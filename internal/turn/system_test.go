package turn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/session"
)

func TestSystemPromptBase(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := session.Workspace{Root: t.TempDir(), ProjectID: "local/x"}
	got := SystemPrompt(ws, "0.1.0")
	for _, want := range []string{"rudy 0.1.0", ws.Root, "edit tool", "tests"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "AGENTS.md") {
		t.Fatalf("no AGENTS.md exists, prompt must not mention one:\n%s", got)
	}
}

func TestSystemPromptIncludesAgentsFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".agents", "AGENTS.md"), []byte("global rule one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("project rule two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := SystemPrompt(session.Workspace{Root: root}, "0.1.0")
	pi := strings.Index(got, "project rule two")
	gi := strings.Index(got, "global rule one")
	if pi < 0 || gi < 0 {
		t.Fatalf("both files must appear:\n%s", got)
	}
	if pi > gi {
		t.Fatalf("workspace AGENTS.md must precede the global one:\n%s", got)
	}
	if !strings.Contains(got, "## AGENTS.md ("+filepath.Join(root, "AGENTS.md")+")") {
		t.Fatalf("section header must name the file:\n%s", got)
	}
}
