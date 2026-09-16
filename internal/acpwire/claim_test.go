package acpwire

import (
	"bufio"
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestRequestClaimPreventsScannerReadAhead(t *testing.T) {
	raw := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"unknown\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"unknown\"}\n"
	w := New(io.NopCloser(strings.NewReader(raw)), io.Discard, Options{RequireRequestClaims: true})
	defer w.Close()
	w.Open()
	scanner := bufio.NewScanner(w.Input())
	if !scanner.Scan() {
		t.Fatal("first request missing")
	}
	next := make(chan string, 1)
	go func() { scanner.Scan(); next <- scanner.Text() }()
	select {
	case <-next:
		t.Fatal("second request escaped claim gate")
	case <-time.After(20 * time.Millisecond):
	}
	first := w.ClaimRequest()
	if first == nil || first.ID().Value != "1" {
		t.Fatal("wrong first callback identity")
	}
	select {
	case line := <-next:
		if !strings.Contains(line, `"id":2`) {
			t.Fatal("wrong next request")
		}
	case <-time.After(time.Second):
		t.Fatal("claim gate stuck")
	}
	second := w.ClaimRequest()
	if second == nil || second.ID().Value != "2" {
		t.Fatal("wrong second callback identity")
	}
}
func TestSDKErrorClaimsUndispatchedRequest(t *testing.T) {
	raw := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{}}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"unknown\"}\n"
	w, r, _ := newTestWire(t, raw, Options{RequireRequestClaims: true})
	readLine(t, r)
	done := make(chan string, 1)
	go func() { done <- readLine(t, r) }()
	_, err := w.Output().Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"error\":{\"code\":-32602,\"message\":\"Invalid params\"}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SDK error stranded claim gate")
	}
	r2 := w.ClaimRequest()
	if r2 == nil || r2.ID().Value != "2" {
		t.Fatal("invalid params rebound to next request")
	}
	if _, err := r2.AcquireConstruction(context.Background()); err != nil {
		t.Fatal(err)
	}
}
