package compactcmd

import (
	"context"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugin/plugintest"
)

func TestCompactCommandAsksTheServerToCompact(t *testing.T) {
	h := &plugintest.Host{Name: "compact"}
	if err := New().Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if len(h.RegisteredCommands) != 1 || h.RegisteredCommands[0].Name != "compact" {
		t.Fatalf("registered %+v", h.RegisteredCommands)
	}
	act, err := h.RegisteredCommands[0].Run(context.Background(), plugin.CommandCall{Args: "  focus on decisions  "})
	if err != nil {
		t.Fatal(err)
	}
	c, ok := act.(plugin.Compact)
	if !ok || c.Instructions != "focus on decisions" {
		t.Fatalf("action %#v", act)
	}
	act, err = h.RegisteredCommands[0].Run(context.Background(), plugin.CommandCall{})
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := act.(plugin.Compact); !ok || c.Instructions != "" {
		t.Fatalf("bare /compact action %#v", act)
	}
}
