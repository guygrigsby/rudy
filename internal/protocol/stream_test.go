package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// nopCloser is a Close that does nothing, for a conn whose reader and writer are pipes the
// test closes itself.
type nopCloser struct{}

func (nopCloser) Close() error { return nil }

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
