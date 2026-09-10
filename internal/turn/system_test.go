package turn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
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

// TestSystemPromptNamesTheTools: the model is told which tools it has and what each is for,
// from the set the turn will actually offer, so an agent definition's narrower view is what
// the prompt describes.
func TestSystemPromptNamesTheTools(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := session.Workspace{Root: t.TempDir()}
	tools := []tool.Tool{
		{Name: "bash", Description: "run a shell command", Safety: tool.Unsafe},
		{Name: "read", Description: "read a file", Safety: tool.Safe},
		{Name: "nameless"},
	}
	got := SystemPrompt(ws, "0.1.0", tools...)
	for _, want := range []string{"bash: run a shell command", "read: read a file", "nameless"} {
		if !strings.Contains(got, want) {
			t.Errorf("the prompt names %q:\n%s", want, got)
		}
	}
	if bash, read := strings.Index(got, "bash:"), strings.Index(got, "read:"); bash > read {
		t.Errorf("the tools are listed in the order the view gave them:\n%s", got)
	}
	// The list sits above the workspace's own instructions, which are about the repository
	// rather than about the tools.
	if err := os.WriteFile(filepath.Join(ws.Root, "AGENTS.md"), []byte("project rule\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = SystemPrompt(ws, "0.1.0", tools...)
	if strings.Index(got, "bash: run a shell command") > strings.Index(got, "project rule") {
		t.Errorf("the tools come before AGENTS.md:\n%s", got)
	}
}

// TestSystemPromptWithNoToolsSaysNothingAboutThem: a session with no tools must not carry an
// empty heading, and a prompt built without a view is unchanged.
func TestSystemPromptWithNoToolsSaysNothingAboutThem(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got := SystemPrompt(session.Workspace{Root: t.TempDir()}, "0.1.0")
	if strings.Contains(strings.ToLower(got), "available tools") {
		t.Errorf("no tools, no section:\n%s", got)
	}
}
