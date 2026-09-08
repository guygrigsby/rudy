package protocol

import (
	"context"
	"encoding/json"
	"errors"
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
