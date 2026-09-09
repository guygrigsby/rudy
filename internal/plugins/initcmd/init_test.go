package initcmd_test

import (
	"context"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugin/plugintest"
	"github.com/guygrigsby/rudy/internal/plugins/initcmd"
	"github.com/guygrigsby/rudy/internal/session"
)

func TestInitSubmitsThePrompt(t *testing.T) {
	h := &plugintest.Host{}
	if err := initcmd.New().Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if len(h.Commands) != 1 || h.Commands[0].Name != "init" {
		t.Fatalf("commands = %+v", h.Commands)
	}
	act, err := h.Commands[0].Run(context.Background(), plugin.CommandCall{Workspace: session.Workspace{Root: "/tmp/x"}})
	if err != nil {
		t.Fatal(err)
	}
	sp, ok := act.(plugin.SubmitPrompt)
	if !ok {
		t.Fatalf("action %T", act)
	}
	for _, want := range []string{"AGENTS.md", "write tool", "Under 80 lines", "read it first"} {
		if !strings.Contains(sp.Text, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if sp.Text != initcmd.Prompt {
		t.Fatal("prompt differs from the exported constant")
	}
}
