package openaichatplugin_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/plugin/plugintest"
	openaichatplugin "github.com/guygrigsby/rudy/internal/plugins/openaichat"
	"github.com/guygrigsby/rudy/internal/provider/httpx"
)

func TestRegistersOnePerOpenAIChatEntryAndSkipsBadSecrets(t *testing.T) {
	providers := map[string]config.ProviderConfig{
		"aperture": {Wire: "openai_chat", BaseURL: "https://ai.example/v1", Auth: ""},
		"mlx":      {Wire: "openai_chat", BaseURL: "http://localhost:8080/v1", Auth: "env:MLX_TOKEN"},
		"broken":   {Wire: "openai_chat", BaseURL: "https://x/v1", Auth: "env:MISSING"},
		"other":    {Wire: "anthropic_messages", BaseURL: "https://y"},
	}
	// resolve stands in for config.ResolveSecret bound to a fake environment.
	resolve := func(ref string) (string, error) {
		switch ref {
		case "":
			return "", nil
		case "env:MLX_TOKEN":
			return "secret", nil
		}
		return "", fmt.Errorf("secret %s: not set", ref)
	}
	h := &plugintest.Host{}
	p := openaichatplugin.New(providers, httpx.New("test"), resolve)
	if p.Name() != "openai_chat" {
		t.Fatalf("name = %s", p.Name())
	}
	if err := p.Init(context.Background(), h); err != nil {
		t.Fatalf("init must not fail on a bad secret: %v", err)
	}
	names := map[string]bool{}
	for _, pr := range h.Providers {
		names[pr.Name()] = true
	}
	if len(h.Providers) != 2 || !names["aperture"] || !names["mlx"] {
		t.Fatalf("registered %v", names)
	}
	if len(h.Notices) != 1 || !strings.Contains(h.Notices[0], "broken") {
		t.Fatalf("notices = %v", h.Notices)
	}
}
