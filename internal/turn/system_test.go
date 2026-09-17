// SPDX-License-Identifier: AGPL-3.0-or-later

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

// TestRenderExpandsEveryValue walks the closed set, so a value added without a test is one
// nobody proved renders (ADR 0024).
func TestRenderExpandsEveryValue(t *testing.T) {
	v := Vars{
		Base: "BASE", Tools: "TOOLS", Agents: "AGENTS", Version: "0.1.0",
		Workspace: "/w", Project: "local/w", Model: "p:m", Date: "2026-09-09", OS: "darwin",
	}
	for _, name := range VarNames() {
		got, err := Render("["+"${"+name+"}]", v)
		if err != nil {
			t.Fatalf("${%s}: %v", name, err)
		}
		if strings.TrimSpace(got) == "[]" {
			t.Errorf("${%s} rendered nothing", name)
		}
	}
}

// TestRenderRefusesWhatItCannotName: a typo in a prompt is silent otherwise, and a silent
// one is worse here than anywhere else in the config.
func TestRenderRefusesWhatItCannotName(t *testing.T) {
	if _, err := Render("${toolz}", Vars{}); err == nil || !strings.Contains(err.Error(), "toolz") {
		t.Errorf("an unknown value names itself: %v", err)
	}
	if _, err := Render("${tools", Vars{}); err == nil || !strings.Contains(err.Error(), "not closed") {
		t.Errorf("an unclosed value says so: %v", err)
	}
	got, err := Render("$${tools}", Vars{Tools: "TOOLS"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != "${tools}" {
		t.Errorf("a doubled dollar writes the literal: %q", got)
	}
}

// TestATemplateDecidesTheOrder is the point of overriding one: the operator puts the tools
// where they want them, or leaves them out.
func TestATemplateDecidesTheOrder(t *testing.T) {
	v := Vars{Base: "BASE", Tools: "TOOLS", Agents: "AGENTS"}
	got, err := Build("${agents}\n${tools}\n${base}", v)
	if err != nil {
		t.Fatal(err)
	}
	a, tl, b := strings.Index(got, "AGENTS"), strings.Index(got, "TOOLS"), strings.Index(got, "BASE")
	if a >= tl || tl >= b {
		t.Errorf("the template's order is the prompt's order: %q", got)
	}
	got, err = Build("only this", v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "TOOLS") || strings.Contains(got, "BASE") {
		t.Errorf("a template that names nothing carries nothing: %q", got)
	}
}

// TestAnEmptyValueLeavesNoHole: a session with no tools and no AGENTS.md must not open with
// three blank paragraphs.
func TestAnEmptyValueLeavesNoHole(t *testing.T) {
	got, err := Build(DefaultTemplate, Vars{Base: "BASE"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "\n\n\n") || strings.TrimSpace(got) != "BASE" {
		t.Errorf("empty values collapse: %q", got)
	}
}

// TestTheBuiltInTemplateRenders is what makes SystemPrompt's panic unreachable: the constant
// this package ships is rendered here, so a broken one fails the build rather than a run.
func TestTheBuiltInTemplateRenders(t *testing.T) {
	if _, err := Build(DefaultTemplate, Vars{}); err != nil {
		t.Fatalf("the built-in template must render: %v", err)
	}
}
