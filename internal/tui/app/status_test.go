package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	tuispinner "github.com/guygrigsby/rudy/internal/tui/spinner"
	"github.com/guygrigsby/rudy/internal/tui/theme"
)

// TestTheTurnCellIsDrawnOverTheComposer: what a person watches while a turn runs is the
// one status item they should not have to look down for.
func TestTheTurnCellIsDrawnOverTheComposer(t *testing.T) {
	h := newHarness(t, nil)
	if got := ansi.Strip(h.m.aboveEditorLine()); got != "" {
		t.Errorf("a turn at rest draws no cell over the composer: %q", got)
	}
	h.notify(protocol.NotifyTurnState, protocol.TurnStateChanged{
		SessionID: h.m.session.SessionID, TurnID: session.NewID().String(), State: stateStreaming,
	})
	// "thinking" until an answer streams, which is what the turn cell says between the
	// request going out and the first text delta.
	above := ansi.Strip(h.m.aboveEditorLine())
	if !strings.Contains(above, "thinking") {
		t.Fatalf("a running turn says so over the composer: %q", above)
	}
	if strings.Contains(ansi.Strip(h.m.statusLine()), "thinking") {
		t.Errorf("and not under it as well: %q", ansi.Strip(h.m.statusLine()))
	}
	// The status line sits above the composer line, which is the rule over the composer:
	// status, rule, composer, rule.
	lines := h.lines()
	turn, rule, editor := -1, -1, -1
	for i, l := range lines {
		text := ansi.Strip(l)
		switch {
		case strings.Contains(text, "thinking"):
			turn = i
		case strings.Contains(text, "─") && rule < 0:
			rule = i
		case strings.Contains(text, "┃"):
			editor = i
		}
	}
	if turn < 0 || rule < 0 || editor < 0 || turn >= rule || rule >= editor {
		t.Errorf("status %d, composer line %d, composer %d:\n%s", turn, rule, editor, ansi.Strip(h.view()))
	}
}

func TestStatusLineIsTheDesignScreen(t *testing.T) {
	h := newHarness(t, nil)
	h.appended(session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopEndTurn,
		Content: []session.Block{session.TextBlock("done")},
		Usage:   session.Usage{Input: 20000, Output: 1000, CacheRead: 22000},
	})
	got := ansi.Strip(h.m.statusLine())
	// One column in, the gutter the design draws every row and the status line in. No
	// context percentage: the composer's lower rule carries it now (ADR 0017).
	want := " INSERT  ◆ fake:m1  strict  $0.08  rudy ⎇ main*"
	if got != want {
		t.Fatalf("status %q want %q", got, want)
	}
	if !strings.HasSuffix(strings.TrimRight(h.view(), "\n"), h.m.statusLine()) {
		t.Error("the status line is the last thing drawn")
	}
}

func TestStatusItemsRenderOnlyWhatConfigPlaced(t *testing.T) {
	h := newHarness(t, map[string]any{"ui.status.items": []string{"workspace", "vim_mode"}})
	if got := ansi.Strip(h.m.statusLine()); got != " rudy ⎇ main*  INSERT" {
		t.Fatalf("status %q", got)
	}
}

func TestVimModeItem(t *testing.T) {
	h := newHarness(t, map[string]any{"ui.status.items": []string{"vim_mode", "model"}})
	if got := ansi.Strip(h.m.statusLine()); got != " INSERT  ◆ fake:m1" {
		t.Fatalf("insert %q", got)
	}
	h.press("escape") // vim insert to normal
	if got := ansi.Strip(h.m.statusLine()); got != " NORMAL  ◆ fake:m1" {
		t.Fatalf("normal %q", got)
	}
	off := newHarness(t, map[string]any{"ui.vim": false, "ui.status.items": []string{"vim_mode", "model"}})
	if got := ansi.Strip(off.m.statusLine()); got != " ◆ fake:m1" {
		t.Fatalf("vim off must leave no cell and no separator: %q", got)
	}
}

func TestPluginStatusItem(t *testing.T) {
	h := newHarness(t, map[string]any{"ui.status.items": []string{"memory:servers", "model"}})
	if got := ansi.Strip(h.m.statusLine()); got != " ◆ fake:m1" {
		t.Fatalf("an item no plugin set renders nothing: %q", got)
	}
	h.notify(protocol.NotifyStatusUpdated, protocol.StatusUpdated{Items: []protocol.StatusItem{
		{Owner: "memory", Key: "servers", Content: []protocol.Span{
			{Text: "2", Role: "success"}, {Text: " servers", Role: "muted"},
		}},
	}})
	if got := ansi.Strip(h.m.statusLine()); got != " 2 servers  ◆ fake:m1" {
		t.Fatalf("status %q", got)
	}
	// A span is data: escapes and newlines a plugin sends never reach the screen.
	h.notify(protocol.NotifyStatusUpdated, protocol.StatusUpdated{Items: []protocol.StatusItem{
		{Owner: "memory", Key: "servers", Content: []protocol.Span{{Text: "\x1b[31mred\nnext", Role: "muted"}}},
	}})
	if got := ansi.Strip(h.m.statusLine()); got != " rednext  ◆ fake:m1" {
		t.Fatalf("unsanitized span %q", got)
	}
}

func TestContextPercent(t *testing.T) {
	for _, c := range []struct {
		name   string
		window int64
		prompt int64
		want   string
	}{
		{"unknown window", 0, 42000, ""},
		{"none used", 100000, 0, "0%"},
		{"the design screen", 100000, 42000, "42%"},
		{"truncates down", 100000, 42999, "42%"},
		{"full", 100000, 100000, "100%"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := contextPercent(c.window, c.prompt); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestCost(t *testing.T) {
	priced := testModels()[0].Pricing
	for _, c := range []struct {
		name    string
		pricing provider.Pricing
		usage   session.Usage
		want    string
	}{
		{"nothing spent", priced, session.Usage{}, ""},
		{"no prices", provider.Pricing{}, session.Usage{Input: 10}, ""},
		{"input and output", priced, session.Usage{Input: 20000, Output: 1000}, "$0.08"},
		{"cache counts", priced, session.Usage{Input: 20000, Output: 1000, CacheRead: 22000}, "$0.08"},
		{"rounds to cents", priced, session.Usage{Input: 1000000, Output: 100000}, "$4.50"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := cost(c.pricing, c.usage); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestWorkspaceItem(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("commit", "-q", "--allow-empty", "-m", "root")
	want := filepath.Base(dir) + " main"
	if got := detectWorkspace(dir, dir); got != want {
		t.Fatalf("clean %q want %q", got, want)
	}
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := detectWorkspace(dir, dir); got != want+"*" {
		t.Fatalf("dirty %q want %q", got, want+"*")
	}
	// The git root the session already detected is used as is; only the branch and the
	// dirty flag cost a subprocess.
	if got := detectWorkspace("", dir); got != want+"*" {
		t.Fatalf("no root given %q", got)
	}
	if got := detectWorkspace("", t.TempDir()); got != "" {
		t.Fatalf("outside a repository %q", got)
	}
}

// TestTurnItemSpinsWhileTheTurnRuns is ADR 0013 decision 3's spinner: thinking deltas
// count toward it and nothing else does, an answer that begins streaming renames it, and a
// turn at rest leaves the cell empty and the tick loop stopped, so an idle client
// schedules no timer at all.
func TestTurnItemSpinsWhileTheTurnRuns(t *testing.T) {
	// The turn cell is drawn over the composer by default; this test is about the cell
	// itself, so it is put back in the status line and taken off the line above.
	h := newHarness(t, map[string]any{
		"ui.status.items":        []string{"turn"},
		"ui.status.above_editor": []string{},
	})
	// The default preset's frames, whichever preset ui.spinner.name names.
	frames := tuispinner.Default().Frames
	glyph := frames[0]
	if got := ansi.Strip(h.m.statusLine()); got != "" {
		t.Fatalf("a client that has done nothing draws no turn cell: %q", got)
	}
	if h.m.spinning {
		t.Fatal("a client at rest schedules no tick")
	}
	sid, turn := h.m.session.SessionID, session.NewID().String()
	h.notify(protocol.NotifyTurnState, protocol.TurnStateChanged{SessionID: sid, TurnID: turn, State: stateStreaming})
	if !h.m.spinning {
		t.Fatal("a running turn arms the tick loop")
	}
	h.notify(protocol.NotifyStreamDelta, protocol.StreamDelta{
		SessionID: sid, TurnID: turn,
		Part: provider.Part{Type: provider.PartThinkingDelta, Text: "weighing the fork bound"},
	})
	if got := ansi.Strip(h.m.statusLine()); got != " "+glyph+" thinking" {
		t.Errorf("a thinking delta reads as thinking: %q", got)
	}
	h.notify(protocol.NotifyStreamDelta, protocol.StreamDelta{
		SessionID: sid, TurnID: turn,
		Part: provider.Part{Type: provider.PartTextDelta, Text: "Looking at the test first."},
	})
	if got := ansi.Strip(h.m.statusLine()); got != " "+glyph+" streaming" {
		t.Errorf("a text delta reads as streaming: %q", got)
	}
	// A tick advances the glyph and asks for the next one, so the cell animates while the
	// turn runs.
	h.update(spinner.TickMsg{ID: h.m.spin.ID()})
	if got := ansi.Strip(h.m.statusLine()); got != " "+frames[1]+" streaming" {
		t.Errorf("a tick advances the glyph: %q", got)
	}
	for _, c := range []struct{ state, word string }{
		{stateRunningTool, "tool"},
		{stateAwaitingPermission, "waiting"},
		{stateSteering, "steering"},
	} {
		h.notify(protocol.NotifyTurnState, protocol.TurnStateChanged{SessionID: sid, TurnID: turn, State: c.state})
		if got := ansi.Strip(h.m.statusLine()); got != " "+frames[1]+" "+c.word {
			t.Errorf("%s reads as %q: %q", c.state, c.word, got)
		}
	}
	h.notify(protocol.NotifyTurnState, protocol.TurnStateChanged{SessionID: sid, TurnID: turn, State: stateCompleted})
	if got := ansi.Strip(h.m.statusLine()); got != "" {
		t.Errorf("a rested turn leaves the cell empty: %q", got)
	}
	if h.m.spinning {
		t.Error("a rested turn stops the tick loop")
	}
	if cmd := h.m.spinTicked(spinner.TickMsg{ID: h.m.spin.ID()}); cmd != nil {
		t.Error("a tick that arrives after the turn rested asks for no further tick")
	}
}

func TestADisconnectRestsTheTurnCell(t *testing.T) {
	// The turn cell is drawn over the composer by default; this test is about the cell
	// itself, so it is put back in the status line and taken off the line above.
	h := newHarness(t, map[string]any{
		"ui.status.items":        []string{"turn"},
		"ui.status.above_editor": []string{},
	})
	sid, turn := h.m.session.SessionID, session.NewID().String()
	h.notify(protocol.NotifyTurnState, protocol.TurnStateChanged{SessionID: sid, TurnID: turn, State: stateStreaming})
	if !h.m.spinning {
		t.Fatal("a running turn arms the tick loop")
	}
	h.update(DisconnectedMsg{})
	// No turn.state will ever arrive for this turn again: the cell would spin for the
	// life of the process on a turn nobody can finish.
	if got := ansi.Strip(h.m.statusLine()); got != "" {
		t.Errorf("a disconnect empties the turn cell: %q", got)
	}
	if h.m.spinning {
		t.Error("a disconnect stops the tick loop")
	}
	if cmd := h.m.spinTicked(spinner.TickMsg{ID: h.m.spin.ID()}); cmd != nil {
		t.Error("a tick that arrives after the disconnect asks for no further tick")
	}
}

// TestAnItemNamedAboveIsNotDrawnBelow: a config written before ui.status.above_editor
// existed still lists turn in ui.status.items, and drawing it in both places is what the
// screen looked like when this was found.
func TestAnItemNamedAboveIsNotDrawnBelow(t *testing.T) {
	h := newHarness(t, map[string]any{
		"ui.status.above_editor": []string{"turn"},
		"ui.status.items":        []string{"vim_mode", "turn", "model"},
	})
	h.notify(protocol.NotifyTurnState, protocol.TurnStateChanged{
		SessionID: h.m.session.SessionID, TurnID: session.NewID().String(), State: stateStreaming,
	})
	if got := ansi.Strip(h.m.statusLine()); strings.Contains(got, "thinking") {
		t.Errorf("the turn cell is drawn above, so not below as well: %q", got)
	}
	if got := ansi.Strip(h.m.aboveEditorLine()); !strings.Contains(got, "thinking") {
		t.Errorf("and it is drawn above: %q", got)
	}
	// The rest of the line is untouched.
	if got := ansi.Strip(h.m.statusLine()); !strings.Contains(got, "INSERT") || !strings.Contains(got, "fake:m1") {
		t.Errorf("everything else stays: %q", got)
	}
}

// TestTheSpinnerIsPaintedApartFromTheLineItSitsOn: the turn cell is the only thing on the
// status line that moves, and it used to be painted as the chrome around it. The spinner
// carries the spinner role, the word beside it the status role, and neither is the other.
// Named rather than left to a golden, so regenerating one cannot quietly take it back.
func TestTheSpinnerIsPaintedApartFromTheLineItSitsOn(t *testing.T) {
	h := newHarness(t, nil)
	h.notify(protocol.NotifyTurnState, protocol.TurnStateChanged{
		SessionID: h.m.session.SessionID, TurnID: session.NewID().String(), State: stateStreaming,
	})
	cell := h.m.turnCell()
	if ansi.Strip(cell) == "" {
		t.Fatal("a running turn has a cell")
	}
	spin := h.m.th.Style(theme.RoleSpinner).Render("x")
	status := h.m.th.Style(theme.RoleStatus).Render("x")
	open := func(styled string) string { return strings.TrimSuffix(styled, "x"+ansi.ResetStyle) }
	if !strings.HasPrefix(cell, open(spin)) {
		t.Errorf("the spinner is in the spinner role: %q", cell)
	}
	if !strings.Contains(cell, open(status)+"thinking") {
		t.Errorf("the word beside it is the status line's: %q", cell)
	}
	if open(spin) == open(status) {
		t.Error("and the two roles are not the same paint")
	}
}
