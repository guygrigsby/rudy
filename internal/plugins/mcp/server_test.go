// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/guygrigsby/rudy/internal/tool"
)

// TestHTTPTransportSendsResolvedHeaders drives the real streamable transport against a real
// go-sdk server behind httptest: the configured header has to arrive on every request the
// client makes to the endpoint, resolved, and the tool has to work through it.
func TestHTTPTransportSendsResolvedHeaders(t *testing.T) {
	srv := sdk.NewServer(&sdk.Implementation{Name: "remote", Version: "0.1.0"}, nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "ping", Description: "Answer pong."},
		func(ctx context.Context, req *sdk.CallToolRequest, in struct{}) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "pong"}}}, nil, nil
		})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil)
	var mu sync.Mutex
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("X-Rudy-Test"))
		mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	// The session holds a standalone SSE stream open, and httptest's Close waits for it, so
	// the server must be torn down after the plugin: cleanups run last registered first.
	t.Cleanup(ts.Close)

	h := loadServers(t, map[string]ServerConfig{
		"remote": {Transport: TransportHTTP, URL: ts.URL, Headers: map[string]string{"X-Rudy-Test": "a literal token"}},
	})
	ping, ok := h.reg.Tool("mcp__remote__ping")
	if !ok {
		t.Fatalf("no ping tool in %v (%v)", toolNames(h.reg), h.notices())
	}
	res, err := ping.Invoke(context.Background(), tool.Call{Name: ping.Name, Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "pong" {
		t.Errorf("result %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the endpoint saw no requests")
	}
	for i, got := range seen {
		if got != "a literal token" {
			t.Errorf("request %d carried X-Rudy-Test %q", i, got)
		}
	}
}

// TestHeaderTransportStopsAtTheEndpointHost is the cross-host redirect rule: a header
// configured for one server is not handed to whatever host it redirects to. The go-sdk's
// streamable transport is not easily made to redirect, so the round tripper is driven
// directly, with a plain http.Client following the redirect the way the SDK's client would.
func TestHeaderTransportStopsAtTheEndpointHost(t *testing.T) {
	var mu sync.Mutex
	var elsewhereSaw, endpointSaw string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		elsewhereSaw = r.Header.Get("X-Rudy-Test")
		mu.Unlock()
	}))
	defer elsewhere.Close()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		endpointSaw = r.Header.Get("X-Rudy-Test")
		mu.Unlock()
		http.Redirect(w, r, elsewhere.URL+"/next", http.StatusFound)
	}))
	defer endpoint.Close()

	u, err := url.Parse(endpoint.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &headerTransport{
		base:    http.DefaultTransport,
		host:    u.Host,
		headers: []entry{{key: "X-Rudy-Test", value: "a literal token"}},
	}}
	resp, err := client.Get(endpoint.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if endpointSaw != "a literal token" {
		t.Errorf("the endpoint saw %q", endpointSaw)
	}
	if elsewhereSaw != "" {
		t.Errorf("the redirect target on another host saw %q", elsewhereSaw)
	}
}
