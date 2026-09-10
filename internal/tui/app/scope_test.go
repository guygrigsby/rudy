package app

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

// threeModels is a registry big enough to scope: cycling two of three is what /scoped-models
// is for.
func threeModels() []provider.Model {
	m2 := session.ModelRef{Provider: "fake", Model: "m2"}
	m3 := session.ModelRef{Provider: "fake", Model: "m3"}
	return append(testModels(),
		provider.Model{Ref: m2, DisplayName: "Fake 2", ContextWindow: 100000},
		provider.Model{Ref: m3, DisplayName: "Fake 3", ContextWindow: 100000},
	)
}

// newScopeHarness is the pipe harness on that registry, with the session keys bound so
// ctrl+p and ctrl+n cycle.
func newScopeHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarnessWith(t, nil, func(o *Options) { o.Models = threeModels() })
	h.m.keys = sessionKeys(t)
	return h
}

// sent is every model this client asked the server to switch to, in order.
func sent(t *testing.T, h *harness) []string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.reqs {
		if r.Method != protocol.MethodSessionSetModel {
			continue
		}
		var p protocol.SessionSetModelParams
		if err := json.Unmarshal(r.Params, &p); err != nil {
			t.Fatalf("set_model params: %v", err)
		}
		out = append(out, p.Model)
	}
	return out
}

// TestTheCycleIsTheWholeRegistryUntilItIsScoped is the default: no scope, every model.
func TestTheCycleIsTheWholeRegistryUntilItIsScoped(t *testing.T) {
	h := newScopeHarness(t)
	runAll(t, h.press("ctrl+p"))
	if got := sent(t, h); len(got) != 1 || got[0] != "fake:m2" {
		t.Fatalf("an unscoped cycle steps through the registry, sent %v", got)
	}
}

// TestScopedModelsBoundsTheCycle is the command's whole point: after choosing a set,
// ctrl+p walks that set and nothing else, wrapping inside it.
func TestScopedModelsBoundsTheCycle(t *testing.T) {
	h := newScopeHarness(t)
	h.typeText("/scoped-models")
	runAll(t, h.press("enter"))
	if h.m.pick == nil || h.m.pick.kind != pickerScope {
		t.Fatalf("the command opens the set picker, picker %+v", h.m.pick)
	}
	// The picker opens on the session's own model, so the cursor starts at fake:m1.
	// Toggle it out and take the other two instead.
	h.press("down") // fake:m2
	h.press(" ")
	h.press("down") // fake:m3
	h.press(" ")
	if got := len(h.m.pick.chosen); got != 2 {
		t.Fatalf("two rows are in the set, got %d", got)
	}
	runAll(t, h.press("enter"))
	if h.m.pick != nil {
		t.Fatal("a confirm closes the picker")
	}
	if got := len(h.m.scope); got != 2 {
		t.Fatalf("the scope is what was chosen, got %v", h.m.scope)
	}
	if !strings.Contains(ansi.Strip(h.view()), "cycling 2 models") {
		t.Errorf("the client says what it did:\n%s", ansi.Strip(h.view()))
	}

	// The session is on fake:m1, which is outside the scope, so the first step starts at
	// the scope's own first entry.
	runAll(t, h.press("ctrl+p"))
	h.appended(session.ModelChange{Model: session.ModelRef{Provider: "fake", Model: "m2"}})
	runAll(t, h.press("ctrl+p"))
	h.appended(session.ModelChange{Model: session.ModelRef{Provider: "fake", Model: "m3"}})
	// Past the end of the scope it wraps inside the scope rather than falling into the
	// registry's next model.
	runAll(t, h.press("ctrl+p"))
	want := []string{"fake:m2", "fake:m3", "fake:m2"}
	got := sent(t, h)
	if len(got) != len(want) {
		t.Fatalf("cycled %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cycled %v, want %v", got, want)
		}
	}
}

// TestAnEmptyScopeIsEveryModelAgain: emptying the set is how a person undoes it.
func TestAnEmptyScopeIsEveryModelAgain(t *testing.T) {
	h := newScopeHarness(t)
	h.m.scope = []session.ModelRef{{Provider: "fake", Model: "m3"}}
	h.typeText("/scoped-models")
	runAll(t, h.press("enter"))
	if got := len(h.m.pick.chosen); got != 1 {
		t.Fatalf("the picker opens on the scope that is set, got %d", got)
	}
	// The scoped row is under the cursor's own start, so find it and toggle it out.
	h.m.pick.point("fake:m3")
	h.press(" ")
	runAll(t, h.press("enter"))
	if len(h.m.scope) != 0 {
		t.Fatalf("an empty set clears the scope, got %v", h.m.scope)
	}
	if !strings.Contains(ansi.Strip(h.view()), "cycling every model") {
		t.Errorf("and says so:\n%s", ansi.Strip(h.view()))
	}
}

// TestAScopedModelTheRegistryDroppedIsSkipped: a refresh can take a model away, and the
// cycle must not point at something that no longer answers.
func TestAScopedModelTheRegistryDroppedIsSkipped(t *testing.T) {
	h := newScopeHarness(t)
	h.m.scope = []session.ModelRef{
		{Provider: "fake", Model: "m2"},
		{Provider: "fake", Model: "gone"},
	}
	if got := len(h.m.cycle()); got != 1 {
		t.Fatalf("the cycle is what the registry still carries, got %d", got)
	}
	runAll(t, h.press("ctrl+p"))
	if got := sent(t, h); len(got) != 1 || got[0] != "fake:m2" {
		t.Fatalf("sent %v", got)
	}
}

// TestTheScopePickerMarksTheSet: a person has to see what is in and what is out while the
// cursor moves.
func TestTheScopePickerMarksTheSet(t *testing.T) {
	h := newScopeHarness(t)
	h.update(tea.WindowSizeMsg{Width: 100, Height: 30})
	h.typeText("/scoped-models")
	runAll(t, h.press("enter"))
	h.press(" ")
	view := ansi.Strip(h.view())
	if !strings.Contains(view, "[x] fake:m1") {
		t.Errorf("a chosen row is marked:\n%s", view)
	}
	if !strings.Contains(view, "[ ] fake:m2") {
		t.Errorf("and one that is not is not:\n%s", view)
	}
}

// TestTheScopeOutlivesTheClient: a set chosen through the picker is written where the next
// client reads it, and a client that opens with one cycles it without being told again
// (ADR 0027).
func TestTheScopeOutlivesTheClient(t *testing.T) {
	var saved [][]session.ModelRef
	h := newHarnessWith(t, nil, func(o *Options) {
		o.Models = threeModels()
		o.SaveScope = func(refs []session.ModelRef) error {
			saved = append(saved, refs)
			return nil
		}
	})
	h.m.keys = sessionKeys(t)
	h.m.setScope(map[string]bool{"fake:m2": true, "fake:m3": true})
	if len(saved) != 1 || len(saved[0]) != 2 {
		t.Fatalf("choosing a scope records it: %v", saved)
	}

	next := newHarnessWith(t, nil, func(o *Options) {
		o.Models = threeModels()
		o.Scope = saved[0]
	})
	next.m.keys = sessionKeys(t)
	runAll(t, next.press("ctrl+p"))
	if got := sent(t, next); len(got) != 1 || got[0] != "fake:m2" {
		t.Errorf("the remembered scope is what ctrl+p walks, sent %v", got)
	}
}
