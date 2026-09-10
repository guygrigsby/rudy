package app

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/cats"
)

// TestTheStatusLineWearsACat: the cell draws the face this run picked, wherever
// ui.status.items placed it.
func TestTheStatusLineWearsACat(t *testing.T) {
	h := newHarnessWith(t, map[string]any{
		"ui.cats": true, "ui.status.items": []string{"model", "cat"},
	}, func(o *Options) { o.Cat = "(=^･ω･^=)" })
	if got := ansi.Strip(h.m.statusLine()); !strings.HasSuffix(got, "(=^･ω･^=)") {
		t.Errorf("the cat sits where config put it: %q", got)
	}
}

// TestOneFaceForTheWholeRun: a status line that changed its face while somebody typed
// would be motion where the design puts none.
func TestOneFaceForTheWholeRun(t *testing.T) {
	h := newHarnessWith(t, map[string]any{"ui.cats": true}, nil)
	first := h.m.cat
	if first == "" {
		t.Fatal("a client with ui.cats on picks a face")
	}
	h.typeText("hello")
	if h.m.cat != first {
		t.Errorf("the face is picked once: %q then %q", first, h.m.cat)
	}
}

// TestCatsOffLeavesNoCell is the switch: no face, and no separator where it would have
// been, the way vim off leaves no mode cell.
func TestCatsOffLeavesNoCell(t *testing.T) {
	h := newHarnessWith(t, map[string]any{
		"ui.cats": false, "ui.status.items": []string{"model", "cat"},
	}, nil)
	if h.m.cat != "" {
		t.Errorf("no face was asked for: %q", h.m.cat)
	}
	if got := ansi.Strip(h.m.statusLine()); got != " ◆ fake:m1" {
		t.Errorf("and no cell and no separator: %q", got)
	}
}

// TestAMangledFaceIsNeverPicked: a few of the faces in internal/cats carry the replacement
// character, and a box in the status line is not a cat.
func TestAMangledFaceIsNeverPicked(t *testing.T) {
	var mangled int
	for _, c := range cats.Cats {
		if strings.ContainsRune(c, '�') {
			mangled++
		}
	}
	if mangled == 0 {
		t.Skip("internal/cats has no mangled faces to skip")
	}
	for range 500 {
		if got := pickCat(); strings.ContainsRune(got, '�') {
			t.Fatalf("picked a mangled face: %q", got)
		}
	}
}
