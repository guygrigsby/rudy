package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tui/banner"
	"github.com/guygrigsby/rudy/internal/tui/transcript"
)

// The header the tests draw: a fixed clock, a fixed name and a changelog of its own, so
// what is pinned is the layout rather than the day the suite ran or the build it ran from.
var headerNow = time.Date(2026, 9, 9, 13, 30, 0, 0, time.UTC)

const headerChangelog = "# Changelog\n\n## 0.1.0\n\n- Opens full screen\n- Typing / lists the commands\n"

// headerOver is the config a test that wants the header uses. Animate off: a reveal in
// flight is a clock, and the tests that want one turn it back on.
func headerOver(over map[string]any) map[string]any {
	out := map[string]any{
		"ui.header.show":    true,
		"ui.header.animate": false,
		"ui.header.name":    "Guy",
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// newHeaderHarness is the pipe harness with the header on and everything about it fixed.
func newHeaderHarness(t *testing.T, over map[string]any) *harness {
	t.Helper()
	h := newHarnessWith(t, headerOver(over), func(o *Options) {
		o.Now = headerNow
		o.Changelog = headerChangelog
		o.Version = "0.1.0"
	})
	h.update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return h
}

// TestTheHeaderOpensTheTranscript: the box is the top of what the transcript draws, and it
// says the things a person opens the client to check.
func TestTheHeaderOpensTheTranscript(t *testing.T) {
	h := newHeaderHarness(t, nil)
	view := ansi.Strip(h.view())
	for _, want := range []string{"rudy 0.1.0", "afternoon, Guy", "fake:m1", "Opens full screen"} {
		if !strings.Contains(view, want) {
			t.Errorf("the header says %q:\n%s", want, view)
		}
	}
	lines := h.lines()
	if !strings.HasPrefix(strings.TrimSpace(ansi.Strip(lines[0])), "╭") {
		t.Errorf("the header is the first thing drawn:\n%s", view)
	}
}

// TestTheHeaderIsOffByConfig is the escape: ui.header.show = false opens on an empty
// transcript, which is what the client did before there was a header.
func TestTheHeaderIsOffByConfig(t *testing.T) {
	h := newHeaderHarness(t, map[string]any{"ui.header.show": false})
	if strings.Contains(ansi.Strip(h.view()), "afternoon") {
		t.Errorf("no header was asked for:\n%s", ansi.Strip(h.view()))
	}
}

// TestTheHeaderScrollsAwayRatherThanPinning: it is content, not a slot, so a conversation
// long enough to fill the viewport pushes it off the top.
func TestTheHeaderScrollsAwayRatherThanPinning(t *testing.T) {
	h := newHeaderHarness(t, nil)
	for i := range 40 {
		h.appended(session.UserMessage{
			Source:  session.SourceTyped,
			Content: []session.Block{session.TextBlock("message " + strings.Repeat("x", 3) + string(rune('a'+i%26)))},
		})
	}
	view := ansi.Strip(h.view())
	if strings.Contains(view, "afternoon, Guy") {
		t.Errorf("the conversation scrolls the header away:\n%s", view)
	}
	if !strings.Contains(view, "message") {
		t.Errorf("and what is on screen is the conversation:\n%s", view)
	}
}

// TestTheHeaderFactsFollowTheSession pins that the box reads the session rather than
// remembering it: a model change moves the header the way it moves the status line.
func TestTheHeaderFactsFollowTheSession(t *testing.T) {
	h := newHeaderHarness(t, nil)
	if !strings.Contains(ansi.Strip(h.view()), "fake:m1") {
		t.Fatalf("the header opens on the session's model:\n%s", ansi.Strip(h.view()))
	}
	h.appended(session.ModelChange{Model: session.ModelRef{Provider: "fake", Model: "m2"}})
	if !strings.Contains(ansi.Strip(h.view()), "fake:m2") {
		t.Errorf("a model change moves the header too:\n%s", ansi.Strip(h.view()))
	}
}

// TestAClickBelowTheHeaderLandsOnTheRowItAimedAt: the header's lines belong to no row, so
// the click that expands a tool row has to count from where the rows start.
func TestAClickBelowTheHeaderLandsOnTheRowItAimedAt(t *testing.T) {
	h := newHeaderHarness(t, nil)
	h.appended(session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopToolUse,
		Content: []session.Block{session.ToolUseBlock("t1", "bash", json.RawMessage(`{"command":"go test ./..."}`))},
	})
	var toolLine int
	lines := h.lines()
	for i, l := range lines {
		if strings.Contains(ansi.Strip(l), "go test ./...") {
			toolLine = i
			break
		}
	}
	if toolLine == 0 {
		t.Fatalf("no tool row on screen:\n%s", ansi.Strip(h.view()))
	}
	before := expandedRows(h.m.tr.Rows())
	h.m.click(toolLine)
	if expandedRows(h.m.tr.Rows()) == before {
		t.Errorf("a click under the header lands on the row it aimed at, line %d:\n%s", toolLine, ansi.Strip(h.view()))
	}
}

// expandedRows counts the rows standing expanded, which is what a click toggles.
func expandedRows(rows []*transcript.Row) int {
	n := 0
	for _, r := range rows {
		if r.Expanded {
			n++
		}
	}
	return n
}

// TestTheMarkRevealSettlesAndStops walks the animation the way the program does: the
// first size starts it, each tick advances it, and the last one ends the loop rather than
// asking for another.
func TestTheMarkRevealSettlesAndStops(t *testing.T) {
	h := newHarnessWith(t, headerOver(map[string]any{"ui.header.animate": true}), func(o *Options) {
		o.Now, o.Changelog, o.Version = headerNow, headerChangelog, "0.1.0"
	})
	cmd := h.update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if cmd == nil {
		t.Fatal("the first size starts the reveal")
	}
	if h.m.header.step != 0 {
		t.Fatalf("the reveal opens on nothing drawn yet, step %d", h.m.header.step)
	}
	var ticks int
	for h.m.header.step != banner.Settled {
		ticks++
		if ticks > banner.Frames+2 {
			t.Fatalf("the reveal never settled, step %d", h.m.header.step)
		}
		cmd = h.update(headerTickMsg{})
	}
	if cmd != nil {
		t.Error("a settled reveal asks for no further tick")
	}
	if ticks != banner.Frames+1 {
		t.Errorf("the reveal took %d ticks, want %d", ticks, banner.Frames+1)
	}
	if !strings.Contains(ansi.Strip(h.view()), `( o.o )`) {
		t.Errorf("and the settled cat is drawn:\n%s", ansi.Strip(h.view()))
	}
}

// TestAKeySkipsTheReveal: somebody typing has stopped watching it.
func TestAKeySkipsTheReveal(t *testing.T) {
	h := newHarnessWith(t, headerOver(map[string]any{"ui.header.animate": true}), func(o *Options) {
		o.Now, o.Changelog, o.Version = headerNow, headerChangelog, "0.1.0"
	})
	h.update(tea.WindowSizeMsg{Width: 100, Height: 30})
	h.typeText("h")
	if h.m.header.step != banner.Settled {
		t.Errorf("a keystroke settles the reveal, step %d", h.m.header.step)
	}
	if strings.ContainsAny(ansi.Strip(h.view()), "▓▒") {
		t.Errorf("and nothing is left mid-reveal:\n%s", ansi.Strip(h.view()))
	}
}

// TestInlinePrintsTheHeaderIntoScrollback is the other render mode: the header goes above
// the live region once, so the frame it would have grown into never grows (rudy-wbf).
func TestInlinePrintsTheHeaderIntoScrollback(t *testing.T) {
	h := newHarnessWith(t, headerOver(map[string]any{"ui.render": "inline"}), func(o *Options) {
		o.Now, o.Changelog, o.Version = headerNow, headerChangelog, "0.1.0"
	})
	// The harness's own first size is the one that owes the print, the way a program's is.
	printed := printedFrom([]tea.Msg{runCmd(t, h.startup)})
	if !strings.Contains(ansi.Strip(printed), "afternoon, Guy") {
		t.Errorf("inline prints the header above the live region, printed %q", ansi.Strip(printed))
	}
	if strings.Contains(ansi.Strip(h.view()), "afternoon, Guy") {
		t.Errorf("and does not also draw it in the frame:\n%s", ansi.Strip(h.view()))
	}
	if cmd := h.update(tea.WindowSizeMsg{Width: 90, Height: 30}); cmd != nil {
		t.Error("a second size prints no second header")
	}
}
