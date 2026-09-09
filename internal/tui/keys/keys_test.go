package keys

import (
	"reflect"
	"slices"
	"testing"
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
