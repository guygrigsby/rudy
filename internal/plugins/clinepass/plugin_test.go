package clinepass_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/plugin/plugintest"
	"github.com/guygrigsby/rudy/internal/plugins/clinepass"
	openaichatplugin "github.com/guygrigsby/rudy/internal/plugins/openaichat"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/provider/httpx"
	"github.com/guygrigsby/rudy/internal/session"
)

func TestRegistersDialectProviderOnceAndSkipsAtOpenAIChat(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "provider", "openaichat", "testdata", "clinepass-nonstream.json")
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)

	providers := map[string]config.ProviderConfig{
		"a": {Wire: "openai_chat", Dialect: "clinepass", BaseURL: srv.URL + "/v1"},
		"b": {Wire: "openai_chat", BaseURL: "https://y/v1"},
	}
	// resolve stands in for config.ResolveSecret bound to a fake environment.
	resolve := func(ref string) (string, error) {
		if ref != "" {
			return "", fmt.Errorf("secret %s: not set", ref)
		}
		return "", nil
	}
	httpc := httpx.New("test")

	hClinepass := &plugintest.Host{}
	cp := clinepass.New(providers, httpc, resolve)
	if cp.Name() != "clinepass" {
		t.Fatalf("name = %s", cp.Name())
	}
	if err := cp.Init(context.Background(), hClinepass); err != nil {
		t.Fatalf("clinepass init: %v", err)
	}

	hOpenAIChat := &plugintest.Host{}
	if err := openaichatplugin.New(providers, httpc, resolve).Init(context.Background(), hOpenAIChat); err != nil {
		t.Fatalf("openai_chat init: %v", err)
	}

	if len(hClinepass.Providers) != 1 || hClinepass.Providers[0].Name() != "a" {
		t.Fatalf("clinepass registered %v", hClinepass.Providers)
	}
	if len(hOpenAIChat.Providers) != 1 || hOpenAIChat.Providers[0].Name() != "b" {
		t.Fatalf("openai_chat registered %v", hOpenAIChat.Providers)
	}

	req := provider.Request{
		Model:     session.ModelRef{Provider: "a", Model: "cline-pass/kimi-k3"},
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: []session.Block{session.TextBlock("hi")}}},
		MaxTokens: 64,
		SessionID: ulid.Make(),
	}
	var parts []provider.Part
	err = hClinepass.Providers[0].Complete(context.Background(), req, func(p provider.Part) error {
		parts = append(parts, p)
		return nil
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var text strings.Builder
	for _, p := range parts {
		if p.Type == provider.PartTextDelta {
			text.WriteString(p.Text)
		}
	}
	if text.String() != "ok" {
		t.Fatalf("text = %q", text.String())
	}
}
