package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugin/plugintest"
	"github.com/guygrigsby/rudy/internal/session"
)

func writeSkill(t *testing.T, root, name, fm string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\n"+fm+"\n---\n# body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSessionOpenedIndexesSkillsAcrossAbsoluteAndRelativeRoots covers the two ways a root can
// be configured: an absolute directory used as is, and a relative one resolved against the
// workspace the hook fires for.
func TestSessionOpenedIndexesSkillsAcrossAbsoluteAndRelativeRoots(t *testing.T) {
	abs := t.TempDir()
	writeSkill(t, abs, "deploy", "name: deploy\ndescription: Ship it")
	wsRoot := t.TempDir()
	writeSkill(t, filepath.Join(wsRoot, ".agents/skills"), "review", "name: review\ndescription: Review a diff")

	reg := plugin.NewRegistry(nil, nil)
	reg.Load(context.Background(), New([]string{abs, ".agents/skills"}))
	if st := reg.Statuses()[0]; st.State != plugin.StateReady {
		t.Fatalf("plugin failed to load: %+v", st)
	}
	runner := plugin.NewHookRunner(reg, 0, nil)
	results := runner.Fire(context.Background(), plugin.HookCall{
		Point:   plugin.HookSessionOpened,
		Payload: &plugin.SessionOpenedPayload{Workspace: session.Workspace{Root: wsRoot}},
	})
	if len(results) != 1 {
		t.Fatalf("results %v", results)
	}
	res, ok := results[0].(*plugin.SessionOpenedResult)
	if !ok {
		t.Fatalf("result type %T", results[0])
	}
	if !strings.HasPrefix(res.Context, "## Skills\n") {
		t.Errorf("context %q", res.Context)
	}
	wantDeploy := "- deploy: Ship it (read " + filepath.Join(abs, "deploy", "SKILL.md") + " before using it)"
	if !strings.Contains(res.Context, wantDeploy) {
		t.Errorf("missing deploy line in %q", res.Context)
	}
	wantReview := "- review: Review a diff (read " + filepath.Join(wsRoot, ".agents/skills", "review", "SKILL.md") + " before using it)"
	if !strings.Contains(res.Context, wantReview) {
		t.Errorf("missing review line in %q", res.Context)
	}
}

// TestSessionOpenedNoSkillsReturnsNoResult covers the brief's "no skills means the handler
// returns nil": the runner drops it, so Fire reports no result for the point at all.
func TestSessionOpenedNoSkillsReturnsNoResult(t *testing.T) {
	reg := plugin.NewRegistry(nil, nil)
	reg.Load(context.Background(), New([]string{t.TempDir()}))
	runner := plugin.NewHookRunner(reg, 0, nil)
	results := runner.Fire(context.Background(), plugin.HookCall{
		Point:   plugin.HookSessionOpened,
		Payload: &plugin.SessionOpenedPayload{Workspace: session.Workspace{Root: t.TempDir()}},
	})
	if len(results) != 0 {
		t.Fatalf("results %v", results)
	}
}

func TestSkillsCommandListsNameDescriptionDir(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "deploy", "name: deploy\ndescription: Ship it")
	h := &plugintest.Host{Name: "skills"}
	if err := New([]string{dir}).Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if len(h.RegisteredCommands) != 1 || h.RegisteredCommands[0].Name != "skills" {
		t.Fatalf("registered %+v", h.RegisteredCommands)
	}
	act, err := h.RegisteredCommands[0].Run(context.Background(), plugin.CommandCall{Workspace: session.Workspace{Root: dir}})
	if err != nil {
		t.Fatal(err)
	}
	n, ok := act.(plugin.Notice)
	want := "deploy  Ship it  " + filepath.Join(dir, "deploy")
	if !ok || n.Text != want {
		t.Fatalf("notice %#v, want text %q", act, want)
	}
}

// TestErrorsFromLoadNoticedOncePerDistinctMessage covers the dedup rule: a broken skill that
// keeps failing to parse across repeated firings costs the operator one notice, not one per
// session.
func TestErrorsFromLoadNoticedOncePerDistinctMessage(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "broken", "description: [")
	h := &plugintest.Host{Name: "skills"}
	if err := New([]string{dir}).Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if len(h.Hooks) != 1 {
		t.Fatalf("hooks %+v", h.Hooks)
	}
	payload := &plugin.SessionOpenedPayload{Workspace: session.Workspace{Root: dir}}
	for range 2 {
		if _, err := h.Hooks[0].Handle(context.Background(), plugin.HookCall{Point: plugin.HookSessionOpened, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.Notices) != 1 {
		t.Fatalf("notices %v", h.Notices)
	}
}
