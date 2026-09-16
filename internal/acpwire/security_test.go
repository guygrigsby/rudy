package acpwire

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRejectSDKCaseFoldAliases(t *testing.T) {
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"unknown","Method":"session/prompt","params":{"sessionId":"s","prompt":[]}}`,
		`{"jsonrpc":"2.0","id":1,"ID":2,"method":"unknown"}`,
		`{"jsonrpc":"2.0","id":1,"method":"unknown","Me\u0074hod":"session/prompt"}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s","SessionId":"other","prompt":[]}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s","\u0053essionId":"other","prompt":[]}}`,
		`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":1,"RequestId":2}}`,
		`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":1,"requeſtId":2}}`,
	} {
		e, err := parseEnvelope([]byte(raw), MethodKinds())
		if err == nil {
			err = validateRouting(&e)
		}
		if err == nil {
			t.Fatal("case-fold alias admitted")
		}
	}
	valid := []byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s","prompt":[],"_meta":{"Key":1,"key":2},"input":{"ID":1,"id":2}}}`)
	e, err := parseEnvelope(valid, MethodKinds())
	if err != nil || validateRouting(&e) != nil {
		t.Fatal("opaque case-sensitive data rejected")
	}
}

func TestClaimWaitKeepsIngressCancellationLive(t *testing.T) {
	inR, inW := io.Pipe()
	defer func() { _ = inW.Close() }()
	events := make(chan Dispatch, 8)
	w := New(inR, io.Discard, Options{RequireRequestClaims: true, BeforeDispatch: func(d Dispatch) { events <- d }})
	defer func() { _ = w.Close() }()
	w.Open()
	r := bufio.NewReader(w.Input())
	go func() { _, _ = io.WriteString(inW, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"unknown\"}\n") }()
	readLine(t, r)
	<-events
	readDone := make(chan struct{})
	go func() { _, _ = r.ReadString('\n'); close(readDone) }()
	go func() {
		_, _ = io.WriteString(inW, "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"unknown\"}\n{\"jsonrpc\":\"2.0\",\"method\":\"$/cancel_request\",\"params\":{\"requestId\":1}}\n{\"jsonrpc\":\"2.0\",\"method\":\"session/cancel\",\"params\":{\"sessionId\":\"s\"}}\n")
	}()
	var intents []Intent
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for len(intents) < 2 {
		select {
		case d := <-events:
			if d.Intent != NoIntent {
				intents = append(intents, d.Intent)
			}
		case <-deadline.C:
			t.Fatal("claim gate blocked later cancellation or known notification classification")
		}
	}
	if intents[0] != RequestCancel || intents[1] != SemanticCancel {
		t.Fatal("cancellation ingress order changed")
	}
	if u := w.Usage(); u.Requests != 2 || u.Notifications != 1 {
		t.Fatal("queued ingress was not charged")
	}
	_ = w.Close()
	<-readDone
}

func TestCloseClearsOwnedBuffersImmediately(t *testing.T) {
	w, _, _ := newTestWire(t, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"unknown\"}"+strings.Repeat(" ", 1<<20)+"\n", Options{})
	if _, err := w.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Output().Write([]byte(strings.Repeat(" ", 1<<20))); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	if len(w.pending) != 0 || len(w.output.partial) != 0 {
		t.Fatalf("close retained buffers: input=%d output=%d", len(w.pending), len(w.output.partial))
	}
}

func TestConstructionIncludesEnvelopeBytes(t *testing.T) {
	w, r, out := newTestWire(t, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"unknown\"}\n", Options{})
	readLine(t, r)
	req := w.Request(numericID(1))
	if _, err := req.AcquireConstruction(context.Background()); err != nil {
		t.Fatal(err)
	}
	result := []byte(`"` + strings.Repeat("x", (1<<20)-2) + `"`)
	ack, err := req.SendResult(result, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitAck(t, ack)
	if !strings.Contains(out.String(), `"code":-32603`) {
		t.Fatal("small construction exceeded class after envelope allocation")
	}
}

type signalledReader struct {
	io.ReadCloser
	started chan struct{}
	once    sync.Once
}

func (r *signalledReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	return r.ReadCloser.Read(p)
}

type signalledWriter struct {
	io.WriteCloser
	started chan struct{}
	once    sync.Once
}

func (w *signalledWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	return w.WriteCloser.Write(p)
}
func TestDeliberateCloseDoesNotSignalIOFailure(t *testing.T) {
	t.Run("reader", func(t *testing.T) {
		r, p := io.Pipe()
		defer func() { _ = p.Close() }()
		source := &signalledReader{ReadCloser: r, started: make(chan struct{})}
		w := New(source, io.Discard, Options{})
		w.Open()
		done := make(chan struct{})
		go func() { _, _ = w.Read(make([]byte, 1)); close(done) }()
		<-source.started
		_ = w.Close()
		<-done
		select {
		case err := <-w.Failed():
			t.Fatalf("deliberate close reported %v", err)
		default:
		}
	})
	t.Run("writer", func(t *testing.T) {
		r, p := io.Pipe()
		defer func() { _ = r.Close() }()
		sink := &signalledWriter{WriteCloser: p, started: make(chan struct{})}
		w := New(io.NopCloser(strings.NewReader("")), sink, Options{})
		ack, err := w.Send([]byte(`{"jsonrpc":"2.0","method":"unknown"}`), 0)
		if err != nil {
			t.Fatal(err)
		}
		<-sink.started
		_ = w.Close()
		_ = ack.Wait(context.Background())
		select {
		case err := <-w.Failed():
			t.Fatalf("deliberate close reported %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	})
}

var _ json.RawMessage
