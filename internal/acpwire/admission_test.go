package acpwire

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestCancelRetainsMatchedResponseUntilCallback(t *testing.T) {
	w, r, _ := newTestWire(t, "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n", Options{})
	peer := &sdkPeer{}
	callback := make(chan struct{})
	var wg sync.WaitGroup
	o := peer.hold(t, w, "session/new", callback, &wg)
	readLine(t, r)
	before := w.Usage().InboundBytes
	o.Cancel()
	if u := w.Usage(); u.Responses != 1 || u.InboundBytes != before || u.OutboundRequests != 0 {
		t.Fatal("cancellation released SDK-retained response")
	}
	// Returning from the callback is what Invoke turns into Complete.
	close(callback)
	wg.Wait()
	if u := w.Usage(); u.Responses != 0 || u.InboundBytes != 0 {
		t.Fatal("callback retained response")
	}
}
func TestResponseCountSurvivesCancellation(t *testing.T) {
	var raw strings.Builder
	for i := range 65 {
		fmt.Fprintf(&raw, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{}}\n", i+1)
	}
	w, r, _ := newTestWire(t, raw.String(), Options{})
	peer := &sdkPeer{}
	callback := make(chan struct{})
	var wg sync.WaitGroup
	defer func() { close(callback); wg.Wait() }()
	// Each response is charged while its request is still live, then cancelled: cancelling
	// returns the outbound slot, it does not hand back a response the callback still holds.
	for range MaxItems {
		o := peer.hold(t, w, "session/new", callback, &wg)
		readLine(t, r)
		o.Cancel()
	}
	if u := w.Usage(); u.Responses != MaxItems || u.OutboundRequests != 0 {
		t.Fatalf("cancellation lost the responses their callbacks still hold: %+v", u)
	}
	// Cancelling returned every outbound slot, so one more request is admitted. Its response
	// meets a full response ledger and closes the wire rather than overrunning it.
	peer.hold(t, w, "session/new", callback, &wg)
	if _, err := r.ReadByte(); err == nil {
		t.Fatal("65th retained response dispatched")
	}
}
func TestAggregateInboundByteBudget(t *testing.T) {
	for _, kind := range []string{"requests", "notifications", "responses"} {
		t.Run(kind, func(t *testing.T) {
			var raw strings.Builder
			for i := range 5 {
				frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"unknown"}`, i+1)
				if kind == "notifications" {
					frame = `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s"}}`
				}
				if kind == "responses" {
					frame = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{}}`, i+1)
				}
				raw.WriteString(frame)
				raw.WriteString(strings.Repeat(" ", (8<<20)-len(frame)))
				raw.WriteByte('\n')
			}
			w, r, _ := newTestWire(t, raw.String(), Options{})
			peer := &sdkPeer{}
			callback := make(chan struct{})
			var wg sync.WaitGroup
			defer func() { close(callback); wg.Wait() }()
			if kind == "responses" {
				for range 5 {
					peer.hold(t, w, "session/new", callback, &wg)
				}
			}
			for range 4 {
				readLine(t, r)
			}
			if w.Usage().InboundBytes != 32<<20 {
				t.Fatal("wrong original-frame byte charge")
			}
			if _, err := r.ReadByte(); err == nil {
				t.Fatal("33rd MiB dispatched")
			}
		})
	}
}
func TestOutboundByteBudget(t *testing.T) {
	w, _, _ := newTestWire(t, "", Options{})
	params := json.RawMessage(`{"x":"` + strings.Repeat("x", (8<<20)-256) + `"}`)
	for range 4 {
		if _, err := w.PrepareOutbound("session/request_permission", params); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.PrepareOutbound("session/request_permission", params); err == nil {
		t.Fatal("outbound byte overflow admitted")
	}
}
func TestOutboundHighWaterAndInvalidResponses(t *testing.T) {
	for _, id := range []string{`"1"`, "0", "-1", "2", "1.5", "9007199254740992", "null"} {
		t.Run(id, func(t *testing.T) {
			w, r, _ := newTestWire(t, fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{}}\n", id), Options{})
			if _, err := w.PrepareOutbound("session/new", json.RawMessage(`{}`)); err != nil {
				t.Fatal(err)
			}
			if _, err := r.ReadByte(); err == nil {
				t.Fatal("invalid response reached SDK")
			}
		})
	}
	w, _, _ := newTestWire(t, "", Options{})
	w.high = 9007199254740990
	o, err := w.PrepareOutbound("session/new", json.RawMessage(`{}`))
	if err != nil || o.ID.Value != "9007199254740991" {
		t.Fatal("last safe id refused")
	}
	if _, err := w.PrepareOutbound("session/new", json.RawMessage(`{}`)); err == nil {
		t.Fatal("safe id wrapped")
	}
	for _, code := range []int{-32700, -32600} {
		_, r, _ := newTestWire(t, fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":null,\"error\":{\"code\":%d,\"message\":\"SECRET\"}}\n{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"unknown\"}\n", code), Options{})
		if !strings.Contains(readLine(t, r), "unknown") {
			t.Fatal("uncorrelated null response dispatched")
		}
	}
}
func TestNotificationCountLimit(t *testing.T) {
	raw := strings.Repeat("{\"jsonrpc\":\"2.0\",\"method\":\"session/cancel\",\"params\":{\"sessionId\":\"s\"}}\n", 65)
	_, r, _ := newTestWire(t, raw, Options{})
	for range 64 {
		readLine(t, r)
	}
	if _, err := r.ReadByte(); err == nil {
		t.Fatal("65th notification admitted")
	}
}

func TestSDKCancellationBeforeFirstWriteDoesNotDisconnect(t *testing.T) {
	w, _, out := newTestWire(t, "", Options{})
	o, err := w.PrepareOutbound("unknown", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = o.Invoke(context.Background(), func(context.Context) error {
		o.Cancel()
		_, err := w.Output().Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"unknown\",\"params\":{}}\n"))
		return err
	})
	select {
	case <-w.Failed():
		t.Fatal("cancelled initiation disconnected other work")
	default:
	}
	if out.String() != "" {
		t.Fatal("cancelled request sent")
	}
	if _, err := w.PrepareOutbound("unknown", json.RawMessage(`{}`)); err != nil {
		t.Fatal("unrelated work refused")
	}
}
func TestPrepareOutboundAbsentParams(t *testing.T) {
	w, _, _ := newTestWire(t, "", Options{})
	if _, err := w.PrepareOutbound("unknown", nil); err != nil {
		t.Fatal("absent params refused")
	}
}

func TestCloseReleasesOutboundFrames(t *testing.T) {
	w, _, _ := newTestWire(t, "", Options{})
	o, err := w.PrepareOutbound("unknown", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	if len(o.Frame) != 0 {
		t.Fatal("close retained outbound frame")
	}
}
