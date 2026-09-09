package anthropicplugin_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/plugin/plugintest"
	anthropicplugin "github.com/guygrigsby/rudy/internal/plugins/anthropic"
	"github.com/guygrigsby/rudy/internal/provider/httpx"
)

func TestRegistersOnePerAnthropicMessagesEntryAndSkipsBadSecrets(t *testing.T) {
	providers := map[string]config.ProviderConfig{
		"anthropic": {Wire: "anthropic_messages", BaseURL: "https://api.anthropic.com", Auth: "env:ANTHROPIC_KEY"},
		"proxy":     {Wire: "anthropic_messages", BaseURL: "https://proxy.example", Auth: ""},
		"broken":    {Wire: "anthropic_messages", BaseURL: "https://x", Auth: "env:MISSING"},
		"other":     {Wire: "openai_chat", BaseURL: "https://y/v1"},
	}
	// resolve stands in for config.ResolveSecret bound to a fake environment.
	resolve := func(ref string) (string, error) {
		switch ref {
		case "":
			return "", nil
		case "env:ANTHROPIC_KEY":
			return "secret", nil
		}
		return "", fmt.Errorf("secret %s: not set", ref)
	}
	h := &plugintest.Host{}
	p := anthropicplugin.New(providers, httpx.New("test"), resolve)
	if p.Name() != "anthropic_messages" {
		t.Fatalf("name = %s", p.Name())
	}
	if err := p.Init(context.Background(), h); err != nil {
		t.Fatalf("init must not fail on a bad secret: %v", err)
	}
	names := map[string]bool{}
	for _, pr := range h.Providers {
		names[pr.Name()] = true
	}
	if len(h.Providers) != 2 || !names["anthropic"] || !names["proxy"] {
		t.Fatalf("registered %v", names)
	}
	if len(h.Notices) != 1 || !strings.Contains(h.Notices[0], "broken") {
		t.Fatalf("notices = %v", h.Notices)
	}
}
