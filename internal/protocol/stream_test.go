// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// nopCloser is a Close that does nothing, for a conn whose reader and writer are pipes the
// test closes itself.
type nopCloser struct{}

func (nopCloser) Close() error { return nil }

type cancelAfterWrite struct {
	cancel context.CancelFunc
	buf    bytes.Buffer
}

func (w *cancelAfterWrite) SetWriteDeadline(time.Time) error { return nil }

func (w *cancelAfterWrite) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	w.cancel()
	return n, err
}

func TestStreamConnRoundTrip(t *testing.T) {
	toServer, fromClient := io.Pipe()
	toClient, fromServer := io.Pipe()
	client := NewStreamConn(toClient, fromClient, nopCloser{})
	server := NewStreamConn(toServer, fromServer, nopCloser{})
	ctx := context.Background()
	go func() {
		for i := range 3 {
			if err := client.Send(ctx, Request{JSONRPC: Version, Method: "m" + string(rune('0'+i))}); err != nil {
				t.Errorf("send %d: %v", i, err)
				return
			}
		}
	}()
	for i := range 3 {
		raw, err := server.Recv(ctx)
		if err != nil {
			t.Fatalf("recv %d: %v", i, err)
		}
		var req Request
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatalf("unmarshal %d: %v", i, err)
		}
		if want := "m" + string(rune('0'+i)); req.Method != want {
			t.Fatalf("message %d = %q, want %q", i, req.Method, want)
		}
	}
}

func TestStreamConnRefusesOverlongLine(t *testing.T) {
	r, w := io.Pipe()
	conn := NewStreamConn(r, io.Discard, nopCloser{})
	go func() {
		// One line of 17MB with no newline in it: over the cap, so the reader must give up
		// rather than buffer it.
		chunk := bytes.Repeat([]byte("a"), 1<<20)
		for range 17 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := conn.Recv(ctx)
	if err == nil {
		t.Fatal("recv accepted a 17MB line")
	}
	if !strings.Contains(err.Error(), "too long") {
		t.Fatalf("err = %v, want a line-too-long error", err)
	}
}

func TestStreamConnRecvEOF(t *testing.T) {
	r, w := io.Pipe()
	conn := NewStreamConn(r, io.Discard, nopCloser{})
	go func() { _ = w.Close() }()
	if _, err := conn.Recv(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

func TestStreamConnCloseStopsRecvAndSend(t *testing.T) {
	r, w := io.Pipe()
	conn := NewStreamConn(r, io.Discard, nopCloser{})
	t.Cleanup(func() { _ = w.Close() })
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := conn.Recv(context.Background()); !errors.Is(err, ErrConnClosed) {
		t.Fatalf("recv after close = %v, want ErrConnClosed", err)
	}
	if err := conn.Send(context.Background(), Request{JSONRPC: Version, Method: "m"}); !errors.Is(err, ErrConnClosed) {
		t.Fatalf("send after close = %v, want ErrConnClosed", err)
	}
}

func TestStreamConnSendHonorsContextWhileWriteIsBlocked(t *testing.T) {
	left, right := net.Pipe()
	t.Cleanup(func() { _ = right.Close() })
	conn := NewStreamConn(left, left, left)
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- conn.Send(ctx, Request{JSONRPC: Version, Method: "blocked"})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("blocked Send = %v, want context deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked Send ignored its context")
	}
}

func TestStreamConnSendPrefersACompletedWriteOverSimultaneousCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w := &cancelAfterWrite{cancel: cancel}
	conn := NewStreamConn(strings.NewReader(""), w, nopCloser{})
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.Send(ctx, Request{JSONRPC: Version, Method: "written"}); err != nil {
		t.Fatalf("Send after a complete physical write = %v, want success", err)
	}
	if !strings.Contains(w.buf.String(), `"method":"written"`) {
		t.Fatalf("writer received %q", w.buf.String())
	}
}
