package theme

import (
	"strings"
	"testing"

	lipgloss "charm.land/lipgloss/v2"
)

func TestDefaultResolvesEveryRole(t *testing.T) {
	th := Default()
	for _, r := range Roles {
		if _, ok := th.Colors[r]; !ok {
			t.Errorf("role %s unresolved", r)
		}
	}
	if th.Chroma != "tokyonight-night" || th.Name != "default" {
		t.Errorf("%+v", th)
	}
	if th.Colors[RoleUser] != th.Colors[RoleAccent] || th.Colors[RoleDiffAdd] != th.Colors[RoleSuccess] {
		t.Error("role indirection")
	}
}

func TestLoadFileAndOverrides(t *testing.T) {
	th, err := Load("testdata", "tokyonight", map[string]string{"accent": "#ff69b4", "tool": "warning"})
	if err != nil {
		t.Fatal(err)
	}
	if lipgloss.Color("#ff69b4") != th.Colors[RoleAccent] || th.Colors[RoleTool] != th.Colors[RoleWarning] {
		t.Errorf("%+v", th.Colors)
	}
	if _, err := Load("testdata", "missing", nil); err == nil {
		t.Error("missing theme must error")
	}
	if _, err := Load("testdata", "default", map[string]string{"text": "nope"}); err == nil || !strings.Contains(err.Error(), "text") {
		t.Errorf("bad value must name the role: %v", err)
	}
	if _, err := Load("testdata", "default", map[string]string{"user": "assistant", "assistant": "user"}); err == nil {
		t.Error("cycle must error")
	}
}

func TestLoadIgnoresNameInOverrides(t *testing.T) {
	// config.Config.UI.Theme, the usual overrides source, carries a "name" key of its
	// own; Load's name argument is authoritative over which theme is loading, so a
	// stray "name" entry in overrides must not error and must not rename the result.
	th, err := Load("testdata", "tokyonight", map[string]string{"name": "bogus", "accent": "#ff0000"})
	if err != nil {
		t.Fatal(err)
	}
	if th.Name != "tokyonight" {
		t.Errorf("name %q, want tokyonight", th.Name)
	}
	if th.Colors[RoleAccent] != lipgloss.Color("#ff0000") {
		t.Errorf("accent override lost: %+v", th.Colors)
	}
}

func TestStyleForUnknownRoleIsText(t *testing.T) {
	th := Default()
	if th.StyleFor("nonsense").GetForeground() != th.Style(RoleText).GetForeground() {
		t.Error("unknown role")
	}
	// NOTE: the brief's literal `!= lipgloss.NoColor{}` does not parse (Go's if-statement
	// composite-literal ambiguity); parenthesized here to compile with the same meaning.
	if th.Style(RoleAccent).GetBackground() != (lipgloss.NoColor{}) {
		t.Error("no backgrounds")
	}
}
