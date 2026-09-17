// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// answer serves p's incoming requests by replying with {"from": name} until the connection
// ends. Notifications are handed to onNote.
func answer(ctx context.Context, t *testing.T, p *Peer, name string, onNote func(Request)) {
	t.Helper()
	inc := p.Incoming()
	for {
		raw, err := inc.Recv(ctx)
		if err != nil {
			return
		}
		var req Request
		if err := json.Unmarshal(raw, &req); err != nil {
			continue
		}
		if req.IsNotification() {
			if onNote != nil {
				onNote(req)
			}
			continue
		}
		resp, err := NewResponse(req.ID, map[string]string{"from": name})
		if err != nil {
			return
		}
		if err := inc.Send(ctx, resp); err != nil {
			return
		}
	}
}

func TestPeerCallsBothWaysAtOnce(t *testing.T) {
	ac, bc := Pipe()
	a, b := NewPeer(ac), NewPeer(bc)
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go answer(ctx, t, a, "a", nil)
	go answer(ctx, t, b, "b", nil)

	var wg sync.WaitGroup
	var fromB, fromA map[string]string
	var errB, errA error
	wg.Add(2)
	go func() { defer wg.Done(); errB = a.Client().Call(ctx, "ask.b", nil, &fromB) }()
	go func() { defer wg.Done(); errA = b.Client().Call(ctx, "ask.a", nil, &fromA) }()
	wg.Wait()
	if errB != nil || fromB["from"] != "b" {
		t.Fatalf("a's call: %v, %v", fromB, errB)
	}
	if errA != nil || fromA["from"] != "a" {
		t.Fatalf("b's call: %v, %v", fromA, errA)
	}
}

func TestPeerNotificationReachesIncoming(t *testing.T) {
	ac, bc := Pipe()
	a, b := NewPeer(ac), NewPeer(bc)
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	note, err := NewNotification("tool.progress", map[string]string{"tool_use_id": "tu1"})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = a.Incoming().Send(ctx, note) }()
	raw, err := b.Incoming().Recv(ctx)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	var got Request
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Method != "tool.progress" || !got.IsNotification() {
		t.Fatalf("got %+v", got)
	}
}

func TestPeerCloseEndsPeerRecvAndFailsCalls(t *testing.T) {
	ac, bc := Pipe()
	a, b := NewPeer(ac), NewPeer(bc)
	t.Cleanup(func() { _ = a.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	called := make(chan error, 1)
	go func() {
		var out map[string]string
		called <- a.Client().Call(ctx, "never.answered", nil, &out)
	}()
	// Let the call reach b before closing, so it is genuinely pending.
	if _, err := b.Incoming().Recv(ctx); err != nil {
		t.Fatalf("b never saw the call: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := a.Incoming().Recv(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("a's Recv after b closed = %v, want io.EOF", err)
	}
	select {
	case err := <-called:
		if err == nil {
			t.Fatal("the pending call succeeded after the peer closed")
		}
	case <-ctx.Done():
		t.Fatal("the pending call never returned")
	}
}

func TestPeerRefusesAFloodOfUnreadMessages(t *testing.T) {
	ac, bc := Pipe()
	a := NewPeer(ac)
	t.Cleanup(func() { _ = a.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// b is a hostile peer: it sends notifications and never reads anything. Nothing drains
	// a's Incoming queue, so the flood is what the cap is for.
	note, err := NewNotification("tool.progress", map[string]string{"tool_use_id": "tu1"})
	if err != nil {
		t.Fatal(err)
	}
	sendErr := make(chan error, 1)
	go func() {
		for range maxIncoming * 4 {
			if err := bc.Send(ctx, note); err != nil {
				sendErr <- err
				return
			}
		}
		sendErr <- nil
	}()

	// Nothing reads until the flood has been cut off, which is the point: the cap, not the
	// consumer, is what stops it.
	select {
	case err := <-sendErr:
		if err == nil {
			t.Fatal("the flooding peer was never cut off")
		}
	case <-ctx.Done():
		t.Fatal("the flooding peer is still sending")
	}
	// The queue still delivers what it holds, and then reports the overflow.
	var got error
	delivered := 0
	for range maxIncoming * 4 {
		if _, err := a.Incoming().Recv(ctx); err != nil {
			got = err
			break
		}
		delivered++
	}
	if !errors.Is(got, ErrIncomingOverflow) {
		t.Fatalf("Recv ended with %v after %d messages, want the overflow error", got, delivered)
	}
	if delivered > maxIncoming {
		t.Fatalf("queued %d messages, over the %d cap", delivered, maxIncoming)
	}
}

func TestPeerClosesTheConnWhenItsReaderEnds(t *testing.T) {
	ac, bc := Pipe()
	a := NewPeer(ac)
	t.Cleanup(func() { _ = a.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// b goes away. a's reader must close its own end rather than sit on a dead conn: the
	// caller above it (a spawned plugin's adapter) is what decides the plugin has failed, and
	// it hears about it through the closed connection.
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Incoming().Recv(ctx); err == nil {
		t.Fatal("Recv succeeded after the peer went away")
	}
	if err := a.Incoming().Send(ctx, note()); !errors.Is(err, ErrConnClosed) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Send on a dead peer = %v", err)
	}
}

func note() Request {
	r, _ := NewNotification("tool.progress", nil)
	return r
}
