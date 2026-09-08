package protocol

import (
	"context"
	"encoding/json"
	"errors"
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
	defer c.Close()
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
	defer c.Close()
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
	defer c.Close()
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
	defer c.Close()
	if err := sc.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Call(ctx, "anything", nil, nil); err == nil {
		t.Fatal("Call on a closed peer must fail")
	}
}
