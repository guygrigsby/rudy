package agentdef

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/guygrigsby/rudy/internal/session"
)

func TestLoadAndResolve(t *testing.T) {
	defs, errs := Load([]string{"testdata/agents", "testdata/missing"})
	if len(errs) != 0 {
		t.Fatalf("errs %v", errs)
	}
	d, ok := Resolve(defs, "explorer")
	if !ok || d.Name != "explorer" || d.Description != "Read-only exploration of the workspace" || d.Model != "aperture:cline-pass/kimi-k3" || d.Thinking != session.ThinkingLow || d.MaxTurns != 8 {
		t.Errorf("explorer %+v", d)
	}
	if !reflect.DeepEqual(d.Tools, []string{"read", "grep", "glob"}) || d.Prompt != "You explore the repository and report. You never edit files." {
		t.Errorf("tools %v prompt %q", d.Tools, d.Prompt)
	}
	def, ok := Resolve(defs, "")
	if !ok || def.Name != "default" || def.Tools != nil || def.Prompt != "" {
		t.Errorf("default %+v", def)
	}
	if _, ok := Resolve(defs, "nope"); ok {
		t.Error("unknown agent resolved")
	}
}

func TestLoadRejectsBadFrontmatter(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("bad.md", "---\ndescription: x\nthinking: extreme\n---\nbody")
	write("nodesc.md", "---\ntools: []\n---\nbody")
	write("agent.md", "---\ndescription: d\ntools: [agent, read]\n---\nbody")
	defs, errs := Load([]string{dir})
	if len(errs) != 2 || len(defs) != 1 {
		t.Fatalf("defs %v errs %v", defs, errs)
	}
	if !reflect.DeepEqual(defs["agent"].Tools, []string{"read"}) {
		t.Errorf("agent never in tools: %v", defs["agent"].Tools)
	}
}
