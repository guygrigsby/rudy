// SPDX-License-Identifier: AGPL-3.0-or-later

package agentdef

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/guygrigsby/rudy/internal/frontmatter"
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
	// The definition is read as written: a root session under it may call agent. Only a child
	// is denied the tool, and the server is what denies it.
	if !reflect.DeepEqual(defs["agent"].Tools, []string{"agent", "read"}) {
		t.Errorf("tools list must be kept as written: %v", defs["agent"].Tools)
	}
}

// TestLoadDistinguishesMissingFromUnterminatedFence covers a regression from moving the
// frontmatter reader to the shared internal/frontmatter package: a file with no opening
// fence and one whose fence never closes must still be told apart, not collapsed into one
// generic "bad frontmatter" message.
func TestLoadDistinguishesMissingFromUnterminatedFence(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("noopen.md", "description: x\nbody")
	write("noclose.md", "---\ndescription: x\nbody")
	_, errs := Load([]string{dir})
	if len(errs) != 2 {
		t.Fatalf("errs %v", errs)
	}
	var noOpen, noClose int
	for _, err := range errs {
		switch {
		case errors.Is(err, frontmatter.ErrNoOpeningFence):
			noOpen++
		case errors.Is(err, frontmatter.ErrNoClosingFence):
			noClose++
		}
	}
	if noOpen != 1 || noClose != 1 {
		t.Errorf("want one of each fence error, got %v", errs)
	}
}

// TestShippedExamplesParse loads every definition this repository ships under
// examples/agents through the same Load a session uses. A file that fails to parse is not
// refused: Load skips it and Resolve falls back to the implicit default, silently, with only
// a warn notice naming the file. examples/agents/default.md shipped with an unquoted
// description containing ": ", which yaml/v3 rejects as "mapping values are not allowed in
// this context"; the file meant to remove the memory tools from a subagent instead vanished
// and left every session that named it holding the whole registry (rudy-review round 1 on
// task 5). This guard fails the moment a shipped example does that again.
//
// It asserts on what Load actually resolved, not on the directory listing: Load only looks at
// top-level *.md files, so a definition dropped into a subdirectory or given another extension
// is invisible to it, and a directory-non-empty check would pass right alongside that (rudy-
// review round 2 on task 5).
func TestShippedExamplesParse(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "agents")
	defs, errs := Load([]string{dir})
	if len(errs) != 0 {
		t.Fatalf("a shipped example under examples/agents does not parse: %v", errs)
	}
	if len(defs) == 0 {
		t.Fatal("examples/agents resolved no definitions at all")
	}
}
