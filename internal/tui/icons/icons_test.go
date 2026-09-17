// SPDX-License-Identifier: AGPL-3.0-or-later

package icons

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestEverySetNamesEveryIcon: a set that forgot a name would draw nothing where the others
// draw something, and only on the config that named it.
func TestEverySetNamesEveryIcon(t *testing.T) {
	for name, set := range sets {
		if len(set) != len(Names) {
			t.Errorf("%s has %d icons, the set is %d", name, len(set), len(Names))
		}
		for _, n := range Names {
			if _, ok := set[n]; !ok {
				t.Errorf("%s has no %s", name, n)
			}
		}
	}
}

// TestEveryGlyphIsOneCellWide is what keeps a status line and a tool row lining up: a
// two-cell glyph would push everything after it one column right on some terminals and not
// on others. The ascii branch icon is the exception it declares, a word rather than a mark.
func TestEveryGlyphIsOneCellWide(t *testing.T) {
	for name, set := range sets {
		for _, n := range Names {
			g := set[n]
			if g == "" || (name == SetASCII && n == Branch) {
				continue
			}
			if w := ansi.StringWidth(g); w != 1 {
				t.Errorf("%s %s is %d cells wide: %q", name, n, w, g)
			}
		}
	}
}

// TestTheNerdSetIsInThePrivateUseArea pins where the default set's codepoints live: the
// Nerd Font ranges, which is what makes them a patched font's business and not a font rudy
// ships. Anything outside would be a glyph an ordinary font has, and belongs in the
// unicode set instead.
func TestTheNerdSetIsInThePrivateUseArea(t *testing.T) {
	for _, n := range Names {
		for _, r := range nerd[n] {
			if r < 0xE000 || r > 0xF8FF {
				t.Errorf("nerd %s is U+%04X, outside the private use area", n, r)
			}
		}
	}
}

func TestLoadAppliesOverrides(t *testing.T) {
	set, err := Load(SetUnicode, map[string]string{"set": "unicode", "branch": "B", "warn": ""})
	if err != nil {
		t.Fatal(err)
	}
	if got := set.Get(Branch); got != "B" {
		t.Errorf("an override replaces the set's glyph: %q", got)
	}
	if got := set.Get(Warn); got != "" {
		t.Errorf("an empty override turns an icon off: %q", got)
	}
	if got := set.Get(Info); got != unicode[Info] {
		t.Errorf("and leaves the rest of the set alone: %q", got)
	}
}

func TestLoadRefusesWhatItCannotResolve(t *testing.T) {
	if _, err := Load("emoji", nil); err == nil || !strings.Contains(err.Error(), "emoji") {
		t.Errorf("an unknown set is an error naming it: %v", err)
	}
	_, err := Load(SetNerd, map[string]string{"branchh": "x"})
	if err == nil || !strings.Contains(err.Error(), "branchh") {
		t.Errorf("an unknown icon is an error naming it: %v", err)
	}
	if _, err := Load("", nil); err != nil {
		t.Errorf("an empty set name is the default: %v", err)
	}
}

// TestLabelAndForTool pin the two places an icon meets what it labels.
func TestLabelAndForTool(t *testing.T) {
	set := Set{Branch: "⎇", Model: "", Tool: "▸", Bash: "$"}
	if got := set.Label(Branch, "main"); got != "⎇ main" {
		t.Errorf("label: %q", got)
	}
	if got := set.Label(Model, "fake:m1"); got != "fake:m1" {
		t.Errorf("an icon turned off leaves no gap: %q", got)
	}
	if got := set.Label(Branch, ""); got != "⎇" {
		t.Errorf("nothing to label is the glyph alone: %q", got)
	}
	if got := set.ForTool("bash"); got != "$" {
		t.Errorf("a tool the set names gets its own: %q", got)
	}
	if got := set.ForTool("some_plugin_tool"); got != "▸" {
		t.Errorf("a tool it does not name gets the generic one: %q", got)
	}
}
