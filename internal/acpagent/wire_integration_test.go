package acpagent_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/guygrigsby/rudy/internal/acpwire"
)

func TestWireNullIDThroughPinnedSDK(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer func() { _ = inW.Close() }()
	defer func() { _ = outR.Close() }()
	var request *acpwire.Request
	w := acpwire.New(inR, outW, acpwire.Options{RequireRequestClaims: true, BeforeDispatch: func(d acpwire.Dispatch) {
		if d.Kind == acpwire.RequestFrame {
			request = d.Request
		}
	}})
	defer func() { _ = w.Close() }()
	sdk := acp.NewConnection(func(ctx context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
		if w.ClaimRequest() != request {
			t.Error("callback claimed wrong request")
		}
		if request == nil {
			t.Error("no request admission")
		}
		return struct{}{}, nil
	}, w.Output(), w.Input())
	sdk.SetLogger(acpwire.NewLogger(slog.NewTextHandler(io.Discard, nil)))
	w.Open()
	go func() { _, _ = inW.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":null,\"method\":\"unknown\"}\n")) }()
	done := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(outR).ReadString('\n'); done <- line }()
	select {
	case line := <-done:
		if !strings.Contains(line, `"id":null`) || !strings.Contains(line, `"result"`) {
			t.Fatal("null request failed round trip")
		}
	case <-time.After(time.Second):
		t.Fatal("SDK misclassified null request as notification")
	}
}
func TestWireConcurrentSDKOutboundIDs(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer func() { _ = inW.Close() }()
	defer func() { _ = outR.Close() }()
	w := acpwire.New(inR, outW, acpwire.Options{})
	defer func() { _ = w.Close() }()
	sdk := acp.NewConnection(func(context.Context, string, json.RawMessage) (any, *acp.RequestError) { return struct{}{}, nil }, w.Output(), w.Input())
	sdk.SetLogger(acpwire.NewLogger(slog.NewTextHandler(io.Discard, nil)))
	w.Open()
	first, err := w.PrepareOutbound("unknown", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.PrepareOutbound("unknown", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 2)
	invoke := func(o *acpwire.Outbound) {
		done <- o.Invoke(ctx, func(callCtx context.Context) error {
			_, err := acp.SendRequest[struct{}](sdk, callCtx, "unknown", struct{}{})
			return err
		})
	}
	go invoke(second)
	peer := bufio.NewReader(outR)
	line, err := peer.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, `"id":2`) {
		t.Fatal("SDK id was not bound to preadmission")
	}
	if err := second.Written().Wait(ctx); err != nil {
		t.Fatal("SDK request physical write not acknowledged")
	}
	go invoke(first)
	line, err = peer.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, `"id":1`) {
		t.Fatal("second SDK id was not bound")
	}
	_, _ = inW.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n"))
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if u := w.Usage(); u.OutboundRequests != 0 || u.Responses != 0 {
		t.Fatal("SDK callbacks retained admission")
	}
}

func TestWireNullCancellationThroughPinnedSDK(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer func() { _ = inW.Close() }()
	defer func() { _ = outR.Close() }()
	started := make(chan struct{})
	w := acpwire.New(inR, outW, acpwire.Options{})
	defer func() { _ = w.Close() }()
	sdk := acp.NewConnection(func(ctx context.Context, _ string, _ json.RawMessage) (any, *acp.RequestError) {
		close(started)
		<-ctx.Done()
		return nil, &acp.RequestError{Code: -32800, Message: "Request cancelled"}
	}, w.Output(), w.Input())
	sdk.SetLogger(acpwire.NewLogger(slog.NewTextHandler(io.Discard, nil)))
	w.Open()
	go func() {
		_, _ = inW.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":null,\"method\":\"unknown\"}\n"))
		<-started
		_, _ = inW.Write([]byte("{\"jsonrpc\":\"2.0\",\"method\":\"$/cancel_request\",\"params\":{\"requestId\":null}}\n"))
	}()
	done := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(outR).ReadString('\n'); done <- line }()
	select {
	case line := <-done:
		if !strings.Contains(line, `"id":null`) || !strings.Contains(line, `-32800`) {
			t.Fatal("null cancellation failed")
		}
	case <-time.After(time.Second):
		t.Fatal("null cancellation stranded handler")
	}
}

func TestWireSDKCancellationLeavesOtherRequestLive(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer func() { _ = inW.Close() }()
	defer func() { _ = outR.Close() }()
	w := acpwire.New(inR, outW, acpwire.Options{})
	defer func() { _ = w.Close() }()
	sdk := acp.NewConnection(func(context.Context, string, json.RawMessage) (any, *acp.RequestError) { return struct{}{}, nil }, w.Output(), w.Input())
	sdk.SetLogger(acpwire.NewLogger(slog.NewTextHandler(io.Discard, nil)))
	w.Open()
	peer := bufio.NewReader(outR)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	one, _ := w.PrepareOutbound("unknown", json.RawMessage(`{}`))
	done := make(chan error, 2)
	invoke := func(o *acpwire.Outbound, ctx context.Context) {
		done <- o.Invoke(ctx, func(c context.Context) error {
			_, err := acp.SendRequest[struct{}](sdk, c, "unknown", struct{}{})
			return err
		})
	}
	go invoke(one, ctx)
	if _, err := peer.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	two, _ := w.PrepareOutbound("unknown", json.RawMessage(`{}`))
	go invoke(two, context.Background())
	if _, err := peer.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	if line, err := peer.ReadString('\n'); err != nil || !strings.Contains(line, `"requestId":1`) {
		t.Fatal("missing exact cancellation")
	}
	if err := <-done; err == nil {
		t.Fatal("cancelled SDK call succeeded")
	}
	_, _ = inW.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n"))
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("unrelated request disconnected")
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated request stuck")
	}
}

func TestWireClaimsConcurrentIdenticalSDKRequests(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer func() { _ = inW.Close() }()
	defer func() { _ = outR.Close() }()
	w := acpwire.New(inR, outW, acpwire.Options{RequireRequestClaims: true})
	defer func() { _ = w.Close() }()
	claimed := make(chan string, 2)
	release := make(chan struct{})
	defer close(release)
	sdk := acp.NewConnection(func(ctx context.Context, _ string, _ json.RawMessage) (any, *acp.RequestError) {
		req := w.ClaimRequest()
		if req == nil {
			claimed <- "missing"
			return nil, acp.NewInternalError(nil)
		}
		claimed <- req.ID().Value
		select {
		case <-release:
		case <-ctx.Done():
		}
		return struct{}{}, nil
	}, w.Output(), w.Input())
	sdk.SetLogger(acpwire.NewLogger(slog.NewTextHandler(io.Discard, nil)))
	w.Open()
	go func() {
		_, _ = inW.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"unknown\",\"params\":{}}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"unknown\",\"params\":{}}\n"))
	}()
	for _, want := range []string{"1", "2"} {
		select {
		case got := <-claimed:
			if got != want {
				t.Fatalf("claimed %s, want %s", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("claims serialized whole handlers")
		}
	}
}

func TestWireClosesBeforeSDKRequest65(t *testing.T) {
	inR, inW := io.Pipe()
	defer func() { _ = inW.Close() }()
	w := acpwire.New(inR, io.Discard, acpwire.Options{RequireRequestClaims: true})
	defer func() { _ = w.Close() }()
	started := make(chan string, 65)
	sdk := acp.NewConnection(func(ctx context.Context, _ string, _ json.RawMessage) (any, *acp.RequestError) {
		req := w.ClaimRequest()
		if req == nil {
			started <- "missing"
		} else {
			started <- req.ID().Value
		}
		<-ctx.Done()
		return nil, acp.NewInternalError(nil)
	}, w.Output(), w.Input())
	sdk.SetLogger(acpwire.NewLogger(slog.NewTextHandler(io.Discard, nil)))
	w.Open()
	for i := range 64 {
		_, err := fmt.Fprintf(inW, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":\"unknown\"}\n", i+1)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("admitted callback did not start")
		}
	}
	_, _ = fmt.Fprintln(inW, `{"jsonrpc":"2.0","id":65,"method":"unknown"}`)
	select {
	case <-w.Failed():
	case <-time.After(time.Second):
		t.Fatal("65th request did not fail")
	}
	select {
	case <-started:
		t.Fatal("65th request allocated SDK handler")
	case <-time.After(20 * time.Millisecond):
	}
}

type refusedParams struct{}

func (refusedParams) MarshalJSON() ([]byte, error) { return nil, errors.New("SECRET") }
func TestWireSDKMarshalRefusalDoesNotPoisonNextID(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer func() { _ = inW.Close() }()
	defer func() { _ = outR.Close() }()
	w := acpwire.New(inR, outW, acpwire.Options{})
	defer func() { _ = w.Close() }()
	sdk := acp.NewConnection(func(context.Context, string, json.RawMessage) (any, *acp.RequestError) { return struct{}{}, nil }, w.Output(), w.Input())
	sdk.SetLogger(acpwire.NewLogger(slog.NewTextHandler(io.Discard, nil)))
	w.Open()
	first, _ := w.PrepareOutbound("unknown", json.RawMessage(`{}`))
	err := first.Invoke(context.Background(), func(ctx context.Context) error {
		_, err := acp.SendRequest[struct{}](sdk, ctx, "unknown", refusedParams{})
		return err
	})
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("marshal refusal escaped sanitizer")
	}
	second, _ := w.PrepareOutbound("unknown", json.RawMessage(`{}`))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- second.Invoke(ctx, func(ctx context.Context) error {
			_, err := acp.SendRequest[struct{}](sdk, ctx, "unknown", struct{}{})
			return err
		})
	}()
	line, err := bufio.NewReader(outR).ReadString('\n')
	if err != nil || !strings.Contains(line, `"id":2`) {
		t.Fatal("marshal refusal poisoned SDK id sequence")
	}
	_, _ = io.WriteString(inW, "{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n")
	if err := <-done; err != nil {
		t.Fatal("next request failed")
	}
}

func TestWireUnboundResponseCannotCompleteDifferentSDKCall(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer func() { _ = inW.Close() }()
	defer func() { _ = outR.Close() }()
	w := acpwire.New(inR, outW, acpwire.Options{})
	defer func() { _ = w.Close() }()
	sdk := acp.NewConnection(func(context.Context, string, json.RawMessage) (any, *acp.RequestError) { return nil, nil }, w.Output(), w.Input())
	sdk.SetLogger(acpwire.NewLogger(slog.NewTextHandler(io.Discard, nil)))
	w.Open()
	_, _ = w.PrepareOutbound("unknown", json.RawMessage(`{}`))
	second, _ := w.PrepareOutbound("unknown", json.RawMessage(`{}`))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- second.Invoke(ctx, func(ctx context.Context) error {
			_, err := acp.SendRequest[struct{}](sdk, ctx, "unknown", struct{}{})
			return err
		})
	}()
	if _, err := bufio.NewReader(outR).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(inW, "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n")
	if err := <-done; err == nil {
		t.Fatal("unbound wire response completed another SDK call")
	}
	select {
	case <-w.Failed():
	default:
		t.Fatal("unbound response was not refused")
	}
	if w.Usage().Responses != 0 {
		t.Fatal("unbound response retained charge")
	}
}
