package acpwire

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }
func TestWriterShortWriteFailsOnce(t *testing.T) {
	w := New(io.NopCloser(strings.NewReader("")), shortWriter{}, Options{})
	defer func() { _ = w.Close() }()
	a, err := w.Send([]byte(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s"}}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Wait(context.Background()); !errors.Is(err, ErrWrite) {
		t.Fatalf("short write cause: %v", err)
	}
	select {
	case <-w.Failed():
	case <-time.After(time.Second):
		t.Fatal("no failure")
	}
	_, _ = w.Output().Write([]byte("SECRET\n"))
	select {
	case <-w.Failed():
		t.Fatal("multiple supervisor failures")
	default:
	}
}
func TestWriterQueueAndPartialFrames(t *testing.T) {
	outR, outW := io.Pipe()
	defer func() { _ = outR.Close() }()
	w := New(io.NopCloser(strings.NewReader("")), outW, Options{})
	defer func() { _ = w.Close() }()
	frame := `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s"}}`
	for _, s := range []string{frame[:10], frame[10:] + "\n"} {
		if _, err := w.Output().Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if w.Usage().QueuedFrames != 1 {
		t.Fatal("partial writes not assembled")
	}
	for range 63 {
		if _, err := w.Send([]byte(frame), 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Send([]byte(frame), 0); err == nil {
		t.Fatal("65th queue frame accepted")
	}
}
func TestOutboundStructuralAndByteOverflow(t *testing.T) {
	for _, raw := range []string{strings.Repeat(" ", (8<<20)+1), `{"jsonrpc":"2.0","method":"x","params":[` + strings.Repeat("0,", 16384) + `0]}`} {
		out := new(lockedBuffer)
		w := New(io.NopCloser(strings.NewReader("")), out, Options{})
		if _, err := w.Output().Write([]byte(raw + "\n")); err == nil {
			t.Fatal("invalid outbound accepted")
		}
		if out.String() != "" {
			t.Fatal("invalid bytes physically written")
		}
		_ = w.Close()
	}
}
func TestResponseWrittenSignal(t *testing.T) {
	w, r, _ := newTestWire(t, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"unknown\"}\n", Options{})
	readLine(t, r)
	req := w.Request(numericID(1))
	if _, err := w.Output().Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n")); err != nil {
		t.Fatal(err)
	}
	waitAck(t, req.Written())
}
func TestReaderAndSDKResponsesShareWriter(t *testing.T) {
	outR, outW := io.Pipe()
	defer func() { _ = outR.Close() }()
	w := New(io.NopCloser(strings.NewReader("{bad\n")), outW, Options{})
	defer func() { _ = w.Close() }()
	w.Open()
	notification := []byte(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s"}}`)
	a, err := w.Send(notification, 5)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _, _ = w.Input().Read(make([]byte, 1)); close(done) }()
	reader := bufio.NewReader(outR)
	if got := readLine(t, reader); got != string(notification)+"\n" {
		t.Fatal("notification interleaved")
	}
	waitAck(t, a)
	if got := readLine(t, reader); !strings.Contains(got, `"code":-32700`) {
		t.Fatal("missing parse error")
	}
	<-done
}

func TestWriterQueueByteLimit(t *testing.T) {
	outR, outW := io.Pipe()
	defer func() { _ = outR.Close() }()
	w := New(io.NopCloser(strings.NewReader("")), outW, Options{})
	defer func() { _ = w.Close() }()
	base := `{"jsonrpc":"2.0","method":"unknown"}`
	raw := []byte(base + strings.Repeat(" ", (8<<20)-len(base)))
	for range 4 {
		if _, err := w.Send(raw, 0); err != nil {
			t.Fatal(err)
		}
	}
	if w.Usage().QueuedBytes != 32<<20 {
		t.Fatal("wrong queue byte accounting")
	}
	if _, err := w.Send([]byte(base), 0); err == nil {
		t.Fatal("writer exceeded 32 MiB")
	}
}
