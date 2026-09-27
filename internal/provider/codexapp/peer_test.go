// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestPeerMarksCanceledCallAsNotSent(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	peer := newPeer(client, client, client, nil, nil)
	t.Cleanup(func() { _ = peer.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := peer.Call(ctx, "test/write", map[string]any{}, nil)
	var notSent *callNotSentError
	if !errors.As(err, &notSent) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Call error = %v, want unsent cancellation", err)
	}
}

func TestPeerNeverWritesAllowAfterProcessReadCloses(t *testing.T) {
	var output bytes.Buffer
	p := newPeer(bytes.NewReader(nil), &output, closerFunc(func() error { return nil }), func(_ context.Context, _, _ string, _ json.RawMessage) (any, error) {
		return map[string]string{"decision": "accept"}, nil
	}, nil)
	<-p.done
	p.handleRequest(`"request-1"`, envelope{ID: json.RawMessage(`"request-1"`), Method: methodItemCommandExecutionApproval})
	if got := output.String(); got != "" {
		t.Fatalf("closed peer wrote approval: %q", got)
	}
	if !errors.Is(p.err, io.EOF) {
		t.Fatalf("peer error = %v, want EOF", p.err)
	}
}

func TestPeerWriteRejectsResponseAfterFailure(t *testing.T) {
	reader, writer := io.Pipe()
	var output bytes.Buffer
	p := newPeer(reader, &output, closerFunc(func() error { return reader.Close() }), nil, nil)
	t.Cleanup(func() { _ = p.Close(); _ = writer.Close() })
	p.fail(io.EOF)
	if err := p.write(envelope{ID: json.RawMessage(`1`), Result: json.RawMessage(`{"decision":"accept"}`)}); err == nil {
		t.Fatal("write accepted an approval after peer failure")
	}
	if got := output.String(); got != "" {
		t.Fatalf("failed peer wrote approval: %q", got)
	}
}

func TestPeerResolvedNotificationCancelsMatchingInboundApproval(t *testing.T) {
	client, server := net.Pipe()
	entered := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	p := newPeer(client, client, client, func(ctx context.Context, _, _ string, _ json.RawMessage) (any, error) {
		entered <- struct{}{}
		<-ctx.Done()
		canceled <- struct{}{}
		return map[string]string{"decision": "accept"}, nil
	}, func(string, json.RawMessage) {})
	t.Cleanup(func() { _ = p.Close(); _ = server.Close() })
	if _, err := server.Write([]byte("{\"id\":7,\"method\":\"item/commandExecution/requestApproval\",\"params\":{\"threadId\":\"thread-1\"}}\n")); err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err := server.Write([]byte("{\"method\":\"serverRequest/resolved\",\"params\":{\"threadId\":\"thread-1\",\"requestId\":7}}\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("resolved request did not cancel pending approval")
	}
	if err := server.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var response envelope
	if err := json.NewDecoder(server).Decode(&response); err == nil {
		t.Fatalf("resolved approval still received response: %+v", response)
	}
}

func TestPeerRejectsConcurrentDuplicateInboundRequestID(t *testing.T) {
	reader, writer := io.Pipe()
	entered := make(chan struct{}, 2)
	p := newPeer(reader, io.Discard, closerFunc(func() error { return reader.Close() }), func(ctx context.Context, _, _ string, _ json.RawMessage) (any, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return map[string]string{"decision": "accept"}, nil
	}, nil)
	t.Cleanup(func() { _ = p.Close(); _ = writer.Close() })
	request := []byte("{\"id\":1,\"method\":\"item/commandExecution/requestApproval\",\"params\":{}}\n")
	if _, err := writer.Write(request); err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err := writer.Write(request); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Fatal("duplicate inbound request did not fail the peer")
	}
	if len(entered) != 0 {
		t.Fatal("duplicate inbound request reached approval handler")
	}
}

func TestPeerOmitsJSONRPC(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	peer := newPeer(client, client, client, nil, nil)
	t.Cleanup(func() { _ = peer.Close() })

	errCh := make(chan error, 1)
	go func() {
		var request map[string]json.RawMessage
		if err := json.NewDecoder(server).Decode(&request); err != nil {
			errCh <- err
			return
		}
		if _, found := request["jsonrpc"]; found {
			errCh <- &wireError{Code: -1, Message: "jsonrpc field present"}
			return
		}
		errCh <- json.NewEncoder(server).Encode(map[string]any{
			"id": json.RawMessage(request["id"]), "result": map[string]string{"value": "ok"},
		})
	}()
	var result struct {
		Value string `json:"value"`
	}
	if err := peer.Call(context.Background(), "test/read", map[string]any{}, &result); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if result.Value != "ok" {
		t.Fatalf("value = %q, want ok", result.Value)
	}
}

func TestPeerAnswersInboundRequestWithOriginalStringID(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	peer := newPeer(client, client, client, func(_ context.Context, requestID, method string, _ json.RawMessage) (any, error) {
		if method != "approval" {
			t.Errorf("method = %q, want approval", method)
		}
		if requestID != `"approval-1"` {
			t.Errorf("request id = %q, want canonical string id", requestID)
		}
		return map[string]string{"decision": "decline"}, nil
	}, nil)
	t.Cleanup(func() { _ = peer.Close() })

	if _, err := server.Write([]byte("{\"id\":\"approval-1\",\"method\":\"approval\",\"params\":{}}\n")); err != nil {
		t.Fatal(err)
	}
	var response struct {
		ID     string `json:"id"`
		Result struct {
			Decision string `json:"decision"`
		} `json:"result"`
	}
	if err := json.NewDecoder(server).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.ID != "approval-1" || response.Result.Decision != "decline" {
		t.Fatalf("response = %+v", response)
	}
}
