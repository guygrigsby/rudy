package acpwire

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestConstructionHeldUntilPhysicalWriteAndClose(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer func() { _ = inW.Close() }()
	defer func() { _ = outR.Close() }()
	w := New(inR, outW, Options{})
	defer func() { _ = w.Close() }()
	w.Open()
	sdkReader := bufio.NewReader(w.Input())
	go func() {
		for i := range 5 {
			_, _ = io.WriteString(inW, fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":\"initialize\"}\n", i+1))
		}
	}()
	var reqs []*Request
	for range 5 {
		readLine(t, sdkReader)
	}
	for i := range 5 {
		reqs = append(reqs, w.Request(numericID(int64(i+1))))
	}
	for _, r := range reqs[:4] {
		if _, err := r.AcquireConstruction(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	waiting := make(chan error, 1)
	go func() { _, err := reqs[4].AcquireConstruction(context.Background()); waiting <- err }()
	select {
	case <-waiting:
		t.Fatal("fifth constructor started")
	case <-time.After(20 * time.Millisecond):
	}
	classified := make(chan struct{})
	w.options.BeforeDispatch = func(d Dispatch) {
		if d.Intent == RequestCancel {
			close(classified)
		}
	}
	go func() {
		_, _ = io.WriteString(inW, "{\"jsonrpc\":\"2.0\",\"method\":\"$/cancel_request\",\"params\":{\"requestId\":5}}\n")
	}()
	readLine(t, sdkReader)
	<-classified
	ack, err := reqs[0].SendResult([]byte(`{}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-waiting:
		t.Fatal("reservation released before physical write")
	case <-time.After(20 * time.Millisecond):
	}
	readLine(t, bufio.NewReader(outR))
	waitAck(t, ack)
	select {
	case err := <-waiting:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("constructor stayed blocked")
	}
	_ = w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, err := w.constructions.Acquire(ctx, Large)
	if err != nil {
		t.Fatal("close retained constructor")
	}
	r.Release()
}
func TestConstructionRefusalReservesFixedError(t *testing.T) {
	for _, result := range []string{`"` + strings.Repeat("x", 1<<20) + `"`, `{"a":[` + strings.Repeat("0,", 16384) + `0]}`} {
		w, r, out := newTestWire(t, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"unknown\"}\n", Options{})
		readLine(t, r)
		req := w.Request(numericID(1))
		if _, err := req.AcquireConstruction(context.Background()); err != nil {
			t.Fatal(err)
		}
		ack, err := req.SendResult([]byte(result), 0)
		if err != nil {
			t.Fatal(err)
		}
		waitAck(t, ack)
		if !strings.Contains(out.String(), `"code":-32603`) || !strings.Contains(out.String(), `Internal error`) {
			t.Fatal("construction overflow did not reserve fixed error")
		}
		select {
		case <-w.Failed():
			t.Fatal("recoverable construction refusal closed wire")
		default:
		}
	}
}

func TestConstructionPoolWholeReservations(t *testing.T) {
	for _, class := range []Class{Large, Small} {
		p := NewConstructionPool()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var held []*Reservation
		count := 4
		if class == Small {
			count = 32
		}
		for range count {
			r, err := p.Acquire(ctx, class)
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, r)
		}
		waiting := make(chan *Reservation, 1)
		go func() { r, _ := p.Acquire(ctx, class); waiting <- r }()
		select {
		case <-waiting:
			t.Fatal("exceeded 32 MiB")
		case <-time.After(20 * time.Millisecond):
		}
		held[0].Release()
		select {
		case r := <-waiting:
			if r == nil {
				t.Fatal("lost reservation")
			}
			r.Release()
		case <-time.After(time.Second):
			t.Fatal("partial-acquire deadlock")
		}
		for _, r := range held {
			r.Release()
		}
	}
}
func TestConstructionOutputBounds(t *testing.T) {
	p := NewConstructionPool()
	for _, tt := range []struct {
		class Class
		size  int
		ok    bool
	}{{Small, 1 << 20, true}, {Small, (1 << 20) + 1, false}, {Large, 7 << 20, true}, {Large, (7 << 20) + 1, false}} {
		r, err := p.Acquire(context.Background(), tt.class)
		if err != nil {
			t.Fatal(err)
		}
		if (r.Check(tt.size) == nil) != tt.ok {
			t.Fatal("incorrect result cap")
		}
		r.Release()
	}
}

// The construction pool is released with the connection. A handler parked on a response the
// departed peer will never send would otherwise hold its whole class until the process ends,
// and the wire's own context is the only thing that cancels a waiter for it.
func TestEndOfInputReleasesConstructionReservations(t *testing.T) {
	var raw strings.Builder
	for i := range 4 {
		fmt.Fprintf(&raw, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":\"session/new\",\"params\":{}}\n", i+1)
	}
	w, r, _ := newTestWire(t, raw.String(), Options{})
	for range 4 {
		readLine(t, r)
	}
	for i := range 4 {
		req := w.Request(numericID(int64(i + 1)))
		if req == nil {
			t.Fatalf("request %d was not admitted", i+1)
		}
		if _, err := req.AcquireConstruction(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("end of input = %v, want io.EOF", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, err := w.constructions.Acquire(ctx, Large)
	if err != nil {
		t.Fatalf("the pool outlived the connection: %v", err)
	}
	res.Release()
}
