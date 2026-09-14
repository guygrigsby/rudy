package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"testing"
	"time"
)

// fakeServer answers requests on conn with handle and lets a test push notifications.
type fakeServer struct {
	conn   Conn
	handle func(req Request) Response
}

func (f *fakeServer) run(ctx context.Context) {
	for {
		raw, err := f.conn.Recv(ctx)
		if err != nil {
			return
		}
		var req Request
		if err := json.Unmarshal(raw, &req); err != nil {
			continue
		}
		if err := f.conn.Send(ctx, f.handle(req)); err != nil {
			return
		}
	}
}

func TestClientCallSuccess(t *testing.T) {
	cc, sc := Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	srv := &fakeServer{conn: sc, handle: func(req Request) Response {
		if req.Method != "echo" {
			t.Errorf("method %q", req.Method)
		}
		resp, _ := NewResponse(req.ID, map[string]any{"got": json.RawMessage(req.Params)})
		return resp
	}}
	go srv.run(ctx)

	c := NewClient(cc)
	defer func() { _ = c.Close() }()
	var out struct {
		Got map[string]int `json:"got"`
	}
	if err := c.Call(ctx, "echo", map[string]int{"n": 7}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Got["n"] != 7 {
		t.Fatalf("got %+v", out)
	}
}

func TestClientCallError(t *testing.T) {
	cc, sc := Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	srv := &fakeServer{conn: sc, handle: func(req Request) Response {
		return NewErrorResponse(req.ID, NewError(CodeNotFound, "no such session", nil))
	}}
	go srv.run(ctx)

	c := NewClient(cc)
	defer func() { _ = c.Close() }()
	err := c.Call(ctx, "session.resume", SessionResumeParams{SessionID: "x"}, nil)
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("want *Error, got %v", err)
	}
	if pe.Code != CodeNotFound || pe.Message != "no such session" {
		t.Fatalf("got %+v", pe)
	}
}

func TestClientNotificationsOrderedWhileCallPending(t *testing.T) {
	cc, sc := Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	srv := &fakeServer{conn: sc, handle: func(req Request) Response {
		for i := range 3 {
			n, _ := NewNotification(NotifyNotice, NoticeParams{Level: "info", Text: string(rune('a' + i))})
			if err := sc.Send(ctx, n); err != nil {
				t.Error(err)
			}
		}
		resp, _ := NewResponse(req.ID, nil)
		return resp
	}}
	go srv.run(ctx)

	c := NewClient(cc)
	defer func() { _ = c.Close() }()
	if err := c.Call(ctx, "anything", nil, nil); err != nil {
		t.Fatal(err)
	}
	var texts []string
	for range 3 {
		select {
		case n := <-c.Notifications():
			var p NoticeParams
			if err := json.Unmarshal(n.Params, &p); err != nil {
				t.Fatal(err)
			}
			if n.Method != NotifyNotice {
				t.Fatalf("method %q", n.Method)
			}
			texts = append(texts, p.Text)
		case <-ctx.Done():
			t.Fatal("notifications not delivered")
		}
	}
	if got := texts[0] + texts[1] + texts[2]; got != "abc" {
		t.Fatalf("order %q", got)
	}
}

func TestClientCallAfterPeerClose(t *testing.T) {
	cc, sc := Pipe()
	c := NewClient(cc)
	defer func() { _ = c.Close() }()
	if err := sc.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Call(ctx, "anything", nil, nil); err == nil {
		t.Fatal("Call on a closed peer must fail")
	}
}

func TestClientWaitReportsPeerEOF(t *testing.T) {
	cc, sc := Pipe()
	c := NewClient(cc)
	defer func() { _ = c.Close() }()
	if err := sc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("Wait = %v, want EOF", err)
	}
}

func TestClientWaitHonorsContext(t *testing.T) {
	cc, sc := Pipe()
	defer func() { _ = sc.Close() }()
	c := NewClient(cc)
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait = %v, want context canceled", err)
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestClientWaitReportsTransportFailure(t *testing.T) {
	want := errors.New("read failed")
	c := NewClient(NewStreamConn(failingReader{err: want}, io.Discard, nil))
	defer func() { _ = c.Close() }()
	if err := c.Wait(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Wait = %v, want %v", err, want)
	}
}

// TestClientCallNeverBlocksBehindNotifications proves the fix for the starvation bug: a
// server that floods 1000 notifications before answering a call must not stall the call
// just because nothing is draining Notifications yet. Once the caller does drain, every
// notification must still arrive, in order, and the channel must close only after Close.
func TestClientCallNeverBlocksBehindNotifications(t *testing.T) {
	cc, sc := Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const n = 1000
	srv := &fakeServer{conn: sc, handle: func(req Request) Response {
		for i := range n {
			note, _ := NewNotification(NotifyNotice, NoticeParams{Level: "info", Text: strconv.Itoa(i)})
			if err := sc.Send(ctx, note); err != nil {
				t.Error(err)
			}
		}
		resp, _ := NewResponse(req.ID, nil)
		return resp
	}}
	go srv.run(ctx)

	c := NewClient(cc)

	// The consumer reads nothing until this returns: with the bug, the reader would be
	// stuck pushing notification 257 into the full 256-slot buffer and the response
	// behind it would never be routed.
	if err := c.Call(ctx, "anything", nil, nil); err != nil {
		t.Fatalf("call blocked behind queued notifications: %v", err)
	}

	for i := range n {
		select {
		case note := <-c.Notifications():
			var p NoticeParams
			if err := json.Unmarshal(note.Params, &p); err != nil {
				t.Fatal(err)
			}
			if note.Method != NotifyNotice {
				t.Fatalf("method %q", note.Method)
			}
			if p.Text != strconv.Itoa(i) {
				t.Fatalf("notification %d out of order: got %q", i, p.Text)
			}
		case <-ctx.Done():
			t.Fatalf("notification %d not delivered", i)
		}
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-c.Notifications():
		if ok {
			t.Fatal("Notifications must not yield again once the queue has drained")
		}
	case <-ctx.Done():
		t.Fatal("Notifications channel did not close after Close")
	}
}

// TestClientAnswersServerRequestWithMethodNotFound covers the server-to-client request
// branch in read: the client answers with CodeMethodNotFound rather than hanging the
// server, since nothing in this plan lets the client handle an inbound request.
func TestClientAnswersServerRequestWithMethodNotFound(t *testing.T) {
	cc, sc := Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	c := NewClient(cc)
	defer func() { _ = c.Close() }()

	req, err := NewRequest(7, "server.ping", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.Send(ctx, req); err != nil {
		t.Fatal(err)
	}

	raw, err := sc.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || resp.Error.Code != CodeMethodNotFound {
		t.Fatalf("got %+v", resp)
	}
	if string(resp.ID) != "7" {
		t.Fatalf("id %s", resp.ID)
	}
}

// TestClientCloseStopsDrainWithNoConsumer proves Close stops drain even when the queue
// holds more than the 256-slot notes buffer and nothing ever reads Notifications before
// Close runs. With the bug, drain is parked on a plain c.notes <- n send for whichever
// notification did not fit in the buffer, and no cond broadcast can interrupt a blocked
// channel send, so it leaks and notes never closes. The fix gives drain's send a select
// against a closed channel Close closes, so it can bail out and discard the rest of the
// queue instead of waiting for a consumer that may never come.
//
// The assertion checks two things: the channel closes within one second, and strictly
// fewer than all n notifications are ever delivered. The second check is load-bearing:
// this test's own receive loop is, unavoidably, a consumer, and a consumer that keeps
// reading would eventually unstick even the buggy blocking send one notification at a
// time until the queue drained naturally — which is fast enough, with no real I/O, to
// finish inside the one-second deadline anyway. Only the discard behavior distinguishes
// the fix: the buffer holds at most 256 notifications when Close runs, so the fixed
// drain can never deliver anywhere near all 1000.
func TestClientCloseStopsDrainWithNoConsumer(t *testing.T) {
	cc, sc := Pipe()
	setupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const n = 1000
	srv := &fakeServer{conn: sc, handle: func(req Request) Response {
		for i := range n {
			note, _ := NewNotification(NotifyNotice, NoticeParams{Level: "info", Text: strconv.Itoa(i)})
			if err := sc.Send(setupCtx, note); err != nil {
				t.Error(err)
			}
		}
		resp, _ := NewResponse(req.ID, nil)
		return resp
	}}
	go srv.run(setupCtx)

	c := NewClient(cc)
	if err := c.Call(setupCtx, "anything", nil, nil); err != nil {
		t.Fatal(err)
	}

	// Nothing has read Notifications yet. All n notifications are already queued (Call
	// only returned after read received the final response behind them), so drain is
	// parked on a full notes channel the instant Close runs.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	received := 0
	for {
		select {
		case _, ok := <-c.Notifications():
			if !ok {
				if received >= n {
					t.Fatalf("drain delivered all %d notifications instead of discarding what was still queued when Close ran", n)
				}
				return
			}
			received++
		case <-deadline.C:
			t.Fatalf("Notifications channel did not close within one second of Close (received %d of %d)", received, n)
		}
	}
}

// TestCallPrefersADeliveredResponseToACancel pins the outcome when a call's answer and its
// caller's cancellation are both ready at once. A select between two ready cases picks at
// random, so before the fix a call whose response had already arrived reported
// context.Canceled about half the time and threw the result away.
//
// The race is staged rather than waited for: send delivers the response the way the reader
// does and then cancels, so Call reaches its select with both cases ready every iteration.
// The loop is what makes a coin flip a certainty.
func TestCallPrefersADeliveredResponseToACancel(t *testing.T) {
	for range 100 {
		ctx, cancel := context.WithCancel(context.Background())
		var c *Client
		send := func(context.Context, any) error {
			c.deliver(Response{ID: json.RawMessage("1"), Result: json.RawMessage(`{"turn_id":"t1"}`)})
			cancel()
			return nil
		}
		c = newClientOn(send, func() error { return nil })
		var out struct {
			TurnID string `json:"turn_id"`
		}
		err := c.Call(ctx, "session.submit", struct{}{}, &out)
		cancel()
		if err != nil {
			t.Fatalf("call = %v, want the response that had already arrived", err)
		}
		if out.TurnID != "t1" {
			t.Fatalf("result = %+v, want the turn id the server answered with", out)
		}
	}
}
