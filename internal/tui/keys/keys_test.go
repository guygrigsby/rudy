// SPDX-License-Identifier: AGPL-3.0-or-later

package keys

import (
	"reflect"
	"slices"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestParseAndFormatRoundTrip(t *testing.T) {
	for _, s := range []string{"escape", "enter", "ctrl+c", "ctrl+shift+f", "alt+enter", "shift+tab", "ctrl+]", "f2", "pageUp", "ctrl+alt+]", "super+k", "space", "a", "shift+l"} {
		k, err := Parse(s)
		if err != nil {
			t.Errorf("%s: %v", s, err)
			continue
		}
		if got := Format(k); got != s {
			t.Errorf("%s -> %s", s, got)
		}
	}
	for _, s := range []string{"", "ctrl+", "hyper+a", "ctrl+unknownkey", "+a"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("%q must fail", s)
		}
	}
}

func TestDefaultsCoverEveryAction(t *testing.T) {
	for _, a := range Actions {
		if _, ok := Defaults[a]; !ok {
			t.Errorf("no default for %s", a)
		}
	}
	if !reflect.DeepEqual(Defaults[AppInterrupt], []string{"escape"}) || !reflect.DeepEqual(Defaults[AppModelSelect], []string{"ctrl+l"}) || !reflect.DeepEqual(Defaults[TUIEditorCursorLineStart], []string{"home", "ctrl+home", "ctrl+a"}) {
		t.Errorf("pi defaults: %v %v %v", Defaults[AppInterrupt], Defaults[AppModelSelect], Defaults[TUIEditorCursorLineStart])
	}
	if !reflect.DeepEqual(Defaults[AppMessageFollowUp], []string{"alt+enter"}) {
		t.Errorf("followUp %v", Defaults[AppMessageFollowUp])
	}
}

func TestTableOverridesAndMatch(t *testing.T) {
	tb, err := New(map[string][]string{"app.interrupt": {"ctrl+g"}, "app.session.fork": {}, "app.tools.expand": {"ctrl+o", "f3"}})
	if err != nil {
		t.Fatal(err)
	}
	esc, _ := Parse("escape")
	if acts := tb.Match(esc); len(acts) != 1 || acts[0] != TUISelectCancel {
		t.Errorf("escape after rebinding interrupt: %v", acts)
	}
	g, _ := Parse("ctrl+g")
	if acts := tb.Match(g); !slices.Contains(acts, AppInterrupt) {
		t.Errorf("ctrl+g %v", acts)
	}
	f3, _ := Parse("f3")
	if acts := tb.Match(f3); !slices.Contains(acts, AppToolsExpand) {
		t.Errorf("f3 %v", acts)
	}
	enter, _ := Parse("enter")
	if acts := tb.Match(enter); !slices.Contains(acts, TUIInputSubmit) || !slices.Contains(acts, TUISelectConfirm) {
		t.Errorf("enter %v", acts)
	}
	for name, over := range map[string]map[string][]string{
		"unknown id": {"app.nope": {"a"}},
		"bad key":    {"app.interrupt": {"ctrl+"}},
	} {
		if _, err := New(over); err == nil {
			t.Errorf("%s must fail", name)
		}
	}
}

func TestNormalizeSpellsAKeyTheWayATerminalReportsIt(t *testing.T) {
	cases := []struct {
		name string
		k    tea.Key
		want string // tea.Key.String, which is what a matcher outside this package reads
	}{
		{"a bound ctrl combo drops the character Parse put in Text", mustParse(t, "ctrl+a"), "ctrl+a"},
		{"and an alt combo the same", mustParse(t, "alt+b"), "alt+b"},
		{"a named key keeps its bubbletea name", mustParse(t, "pageUp"), "pgup"},
		{"escape too", mustParse(t, "escape"), "esc"},
		{"a modifier on a named key survives", mustParse(t, "shift+enter"), "shift+enter"},
		{"a bound shift+letter becomes the shifted letter", mustParse(t, "shift+l"), "L"},
		{"as does a kitty press of it", tea.Key{Code: 'l', Mod: tea.ModShift, ShiftedCode: 'L', Text: "L"}, "L"},
		{"and a legacy press, which reports case alone", tea.Key{Code: 'L', Text: "L"}, "L"},
		{"a shifted letter under another modifier keeps that modifier", mustParse(t, "ctrl+shift+p"), "ctrl+P"},
		{"a plain letter is left alone", mustParse(t, "a"), "a"},
	}
	for _, c := range cases {
		if got := Normalize(c.k).String(); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// The whole point is that both sides agree: a bound shift+letter and every shape a
	// real press of it takes must normalize to one spelling, and ctrl+shift+p must not
	// collapse onto it.
	shiftL := Normalize(mustParse(t, "shift+l")).String()
	if got := Normalize(tea.Key{Code: 'l', Mod: tea.ModShift, Text: "L"}).String(); got != shiftL {
		t.Errorf("a shift+l press spells %q, the binding %q", got, shiftL)
	}
	if got := Normalize(mustParse(t, "ctrl+shift+l")).String(); got == shiftL {
		t.Errorf("ctrl+shift+l and shift+l both spell %q", got)
	}
}

func TestTableKeysIsTheInverseOfMatch(t *testing.T) {
	tb, err := New(map[string][]string{"tui.editor.deleteToLineEnd": {"ctrl+e", "alt+k"}, "app.session.fork": {}})
	if err != nil {
		t.Fatal(err)
	}
	var spelled []string
	for _, k := range tb.Keys(TUIEditorDeleteToLineEnd) {
		spelled = append(spelled, Format(k))
	}
	if !reflect.DeepEqual(spelled, []string{"ctrl+e", "alt+k"}) {
		t.Errorf("an overridden action's keys, in order: %v", spelled)
	}
	if got := tb.Keys(AppSessionFork); len(got) != 0 {
		t.Errorf("an unbound action has no keys: %v", got)
	}
	// Every key Keys reports must match the action back, or the two indexes disagree.
	for _, a := range Actions {
		for _, k := range tb.Keys(a) {
			if !slices.Contains(tb.Match(k), a) {
				t.Errorf("%s is bound to %s, which matches %v", a, Format(k), tb.Match(k))
			}
		}
	}
	// The result is a copy; writing to it cannot reach the table.
	got := tb.Keys(TUIEditorDeleteToLineEnd)
	got[0] = tea.Key{Code: 'z'}
	if Format(tb.Keys(TUIEditorDeleteToLineEnd)[0]) != "ctrl+e" {
		t.Error("Keys handed out the table's own slice")
	}
}

func TestDefaultIsTheUnoverriddenTable(t *testing.T) {
	tb := Default()
	for _, a := range Actions {
		var spelled []string
		for _, k := range tb.Keys(a) {
			spelled = append(spelled, Format(k))
		}
		want := Defaults[a]
		if len(spelled) == 0 && len(want) == 0 {
			continue
		}
		if !reflect.DeepEqual(spelled, want) {
			t.Errorf("%s: %v, want %v", a, spelled, want)
		}
	}
}

// mustParse is Parse for a key string the test wrote itself.
func mustParse(t *testing.T, s string) tea.Key {
	t.Helper()
	k, err := Parse(s)
	if err != nil {
		t.Fatalf("%s: %v", s, err)
	}
	return k
}
