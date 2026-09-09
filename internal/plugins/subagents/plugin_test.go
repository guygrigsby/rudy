package subagents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin/plugintest"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

func TestRegistersTheAgentToolNamingTheDefinitions(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	def := "---\ndescription: reads only\n---\nYou explore.\n"
	if err := os.WriteFile(filepath.Join(dir, "agents", "explorer.md"), []byte(def), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &plugintest.Host{Name: "subagents"}
	if err := New(dir).Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if len(h.RegisteredTools) != 1 {
		t.Fatalf("registered %+v", h.RegisteredTools)
	}
	got := h.RegisteredTools[0]
	if got.Name != "agent" || got.Safety != tool.Safe {
		t.Errorf("tool = %s %s", got.Name, got.Safety)
	}
	if !strings.Contains(got.Description, "explorer (reads only)") {
		t.Errorf("description does not name the definitions: %q", got.Description)
	}
	if !json.Valid(got.Schema) {
		t.Errorf("schema is not valid JSON: %s", got.Schema)
	}
}

func TestInvokeRefusesBadInputAndReportsNoServer(t *testing.T) {
	h := &plugintest.Host{Name: "subagents"}
	if err := New(t.TempDir()).Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	invoke := h.RegisteredTools[0].Invoke
	for _, input := range []string{`{}`, `{"agent":"explorer"}`, `{"agent":"","prompt":"go"}`, `{"agent":"explorer","prompt":"  "}`, `not json`} {
		res, err := invoke(context.Background(), tool.Call{ID: "tu1", Input: json.RawMessage(input)})
		if err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		if !res.IsError || !strings.Contains(text(res), "needs agent and prompt") {
			t.Errorf("%s: result %+v", input, res)
		}
	}
	// A host with no server behind it fails the call rather than the turn.
	res, err := invoke(context.Background(), tool.Call{ID: "tu1", Input: json.RawMessage(`{"agent":"explorer","prompt":"look"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(text(res), "no server") {
		t.Errorf("result %+v", res)
	}
}

func text(res tool.Result) string {
	var b strings.Builder
	for _, bl := range res.Content {
		if bl.Type == session.BlockText {
			b.WriteString(bl.Text)
		}
	}
	return b.String()
}
