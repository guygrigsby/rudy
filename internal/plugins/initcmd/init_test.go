package initcmd_test

import (
	"context"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/initcmd"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

type captureHost struct{ cmds []plugin.Command }

func (h *captureHost) RegisterTool(tool.Tool) error             { return nil }
func (h *captureHost) RegisterCommand(c plugin.Command) error   { h.cmds = append(h.cmds, c); return nil }
func (h *captureHost) RegisterProvider(provider.Provider) error { return nil }
func (h *captureHost) Config() map[string]any                   { return nil }
func (h *captureHost) Notice(string)                            {}

func TestInitSubmitsThePrompt(t *testing.T) {
	h := &captureHost{}
	if err := initcmd.New().Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if len(h.cmds) != 1 || h.cmds[0].Name != "init" {
		t.Fatalf("commands = %+v", h.cmds)
	}
	act, err := h.cmds[0].Run(context.Background(), plugin.CommandCall{Workspace: session.Workspace{Root: "/tmp/x"}})
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
