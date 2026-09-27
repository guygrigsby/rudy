// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"context"
	"encoding/json"
	"net"
	"testing"
)

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
	peer := newPeer(client, client, client, func(method string, _ json.RawMessage) (any, error) {
		if method != "approval" {
			t.Errorf("method = %q, want approval", method)
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
