package app

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tui/keys"
)

// sessionKeys binds the three session actions, which pi leaves unbound: a user binds them
// in [keys] and a test presses what they bound.
func sessionKeys(t *testing.T) *keys.Table {
	t.Helper()
	table, err := keys.New(map[string][]string{
		string(keys.AppSessionNew):    {"ctrl+n"},
		string(keys.AppSessionFork):   {"ctrl+y"},
		string(keys.AppSessionResume): {"ctrl+r"},
	})
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	return table
}

// altscreen keeps every row on screen, which is what a test about what a switch rebuilt
// has to read: inline commits a rested turn's rows to scrollback instead.
func altscreen(c *config.Config) { c.UI.Render = renderAltscreen }

// filter types into the picker's filter. It is not appHarness.typeText, which leaves the
// editor's normal mode first: a picker takes the keys as they come, an "i" included.
func (h *appHarness) filter(s string) {
	h.t.Helper()
	for _, r := range s {
		h.dispatch(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	h.settle()
}

// sent are the params of every request the pipe harness recorded for method, in order.
func (h *harness) sent(method string) []json.RawMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []json.RawMessage
	for _, r := range h.reqs {
		if r.Method == method {
			out = append(out, r.Params)
		}
	}
	return out
}

func TestModelPickerFiltersAndSetsTheModel(t *testing.T) {
	h := newAppHarnessWith(t, scripted{text("done")}, altscreen)
	h.press("ctrl+l")
	// The picker opens on the snapshot in hand and takes the fresh registry when it
	// lands: fake:m2 is only in the server's.
	h.waitFor("the refreshed model picker", func(v string) bool { return strings.Contains(v, "fake:m2") })
	v := h.view()
	if !strings.Contains(v, "fake:m1  100k ctx  $3.00/$15.00 per Mtok") {
		t.Errorf("a model row carries the context window and the price of a million tokens:\n%s", v)
	}
	if !strings.Contains(v, "> fake:m1") {
		t.Errorf("the picker opens on the session's own model:\n%s", v)
	}
	if strings.Contains(v, "┃") {
		t.Errorf("the editor is hidden while a picker is up:\n%s", v)
	}
	h.filter("m2")
	v = h.view()
	// The status line names the session's model too, so what says the m1 row is gone is
	// the row's own text.
	if strings.Contains(v, "100k ctx") || !strings.Contains(v, "> fake:m2") {
		t.Errorf("the filter leaves the rows it matches, cursor on the first:\n%s", v)
	}
	if !strings.Contains(v, "model  m2") {
		t.Errorf("the picker shows what is being typed, since the editor is hidden:\n%s", v)
	}
	h.press("enter")
	h.wait("the model change", func() bool { return h.m.session.Model == testRef2 })
	if h.m.pick != nil {
		t.Error("a confirm closes the picker")
	}
	if got := ansi.Strip(h.m.statusLine()); !strings.Contains(got, "fake:m2") {
		t.Errorf("the status line follows the model_change entry, got %q", got)
	}
	if !slices.ContainsFunc(h.entries(), func(e session.Entry) bool { return e.Kind == session.KindModelChange }) {
		t.Error("a confirm changes the session's model through session.set_model")
	}
}

// TestPickerTakesEscBeforeTheTurn pins the one key two things want: Esc closes a picker
// and never reaches the turn behind it, which is what the idle row of the TurnControl
// table says (docs/specs/rudy-domain-model.md).
func TestPickerTakesEscBeforeTheTurn(t *testing.T) {
	h := newAppHarness(t, scripted{slowText("thinking", 2*time.Second)})
	h.typeText("go")
	h.press("enter")
	h.waitTurn(stateStreaming)
	h.press("ctrl+l")
	if h.m.pick == nil {
		t.Fatal("ctrl+l opens the model picker")
	}
	h.press("escape")
	if h.m.pick != nil {
		t.Error("escape closes the picker")
	}
	if h.m.turn.state != stateStreaming {
		t.Errorf("the escape a picker took must not steer the turn: %q", h.m.turn.state)
	}
	if !strings.Contains(h.view(), "┃") {
		t.Errorf("the editor is back once the picker closes:\n%s", h.view())
	}
}

func TestModelCyclesForwardAndBackward(t *testing.T) {
	h := newAppHarness(t, scripted{text("done")})
	// The registry the client was handed carries one model; the picker's refresh is what
	// puts the second one in it, as it does in a run.
	h.press("ctrl+l")
	h.wait("the refreshed registry", func() bool { return len(h.m.models) == 2 })
	h.press("escape")
	h.press("ctrl+p")
	h.wait("the next model", func() bool { return h.m.session.Model == testRef2 })
	h.press("ctrl+shift+p")
	h.wait("the previous model", func() bool { return h.m.session.Model == testRef })
	// Backward from the first wraps to the last rather than stopping.
	h.press("ctrl+shift+p")
	h.wait("the wrap", func() bool { return h.m.session.Model == testRef2 })
}

func TestThinkingCyclesThroughTheLevels(t *testing.T) {
	h := newAppHarness(t, scripted{text("done")})
	if h.m.session.Thinking != session.ThinkingOff {
		t.Fatalf("the session opens at the config default, got %q", h.m.session.Thinking)
	}
	for _, want := range []session.ThinkingLevel{session.ThinkingLow, session.ThinkingMedium, session.ThinkingHigh, session.ThinkingOff} {
		h.press("shift+tab")
		h.wait("thinking "+string(want), func() bool { return h.m.session.Thinking == want })
	}
}

func TestThinkingToggleShowsThinkingWithoutWritingConfig(t *testing.T) {
	h := newHarness(t, nil)
	if h.m.cfg.UI.Transcript.Thinking != thinkingHidden {
		t.Fatalf("the default is hidden, got %q", h.m.cfg.UI.Transcript.Thinking)
	}
	h.appended(session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopEndTurn,
		Content: []session.Block{{Type: session.BlockThinking, Text: "the fork test is racy"}, session.TextBlock("looking")},
	})
	if strings.Contains(ansi.Strip(h.view()), "racy") {
		t.Fatalf("thinking is hidden by config:\n%s", h.view())
	}
	h.press("ctrl+t")
	if h.m.cfg.UI.Transcript.Thinking != thinkingShown {
		t.Fatalf("the toggle flips the config value in memory, got %q", h.m.cfg.UI.Transcript.Thinking)
	}
	h.appended(session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopEndTurn,
		Content: []session.Block{{Type: session.BlockThinking, Text: "the second thought"}},
	})
	if !strings.Contains(ansi.Strip(h.view()), "the second thought") {
		t.Errorf("thinking shows from the toggle on:\n%s", h.view())
	}
	h.press("ctrl+t")
	if h.m.cfg.UI.Transcript.Thinking != thinkingHidden {
		t.Errorf("the toggle flips back, got %q", h.m.cfg.UI.Transcript.Thinking)
	}
	// Nothing asks the server: the level the session thinks at is app.thinking.cycle's,
	// and what the transcript draws is the client's own view of the log.
	if got := h.sent(protocol.MethodSessionSetThinking); len(got) != 0 {
		t.Errorf("the toggle is local, it made %d calls", len(got))
	}
}

func TestForkAndResumeSwitchSessions(t *testing.T) {
	h := newAppHarnessWith(t, scripted{text("first answer")}, altscreen)
	h.m.keys = sessionKeys(t)
	first := h.m.session.SessionID
	h.typeText("fix the flaky fork test")
	h.press("enter")
	h.waitTurn(stateCompleted)

	h.press("ctrl+y")
	h.wait("the fork", func() bool { return h.m.session.SessionID != first })
	fork := h.m.session.SessionID
	if fork == "" {
		t.Fatal("the fork has an id of its own")
	}
	// The fork's own replay rebuilds the transcript, whether it arrived before the answer
	// that named the session or after it.
	h.waitFor("the fork's replay", func(v string) bool { return strings.Contains(v, "first answer") })
	v := h.view()
	if !strings.Contains(v, "fix the flaky fork test") {
		t.Errorf("a fork carries the entries it forked from:\n%s", v)
	}
	if !strings.Contains(v, "switched to session "+fork) {
		t.Errorf("a switch says where it went:\n%s", v)
	}

	h.press("ctrl+r")
	h.wait("the session picker", func() bool { return h.m.pick != nil })
	rows := h.m.pick.rows
	if len(rows) != 2 {
		t.Fatalf("the picker lists both sessions, got %d rows", len(rows))
	}
	// Newest first, the fork above the session it came from. A row says when it was
	// opened, what it works on, its model and that it is a fork; the view clamps it to
	// the terminal's width like every other line the client draws.
	if !strings.Contains(rows[0].text, h.m.session.Workspace.Root) ||
		!strings.Contains(rows[0].text, "fake:m1") || !strings.HasSuffix(rows[0].text, "fork") {
		t.Errorf("the fork's row is %q", rows[0].text)
	}
	if strings.HasSuffix(rows[1].text, "fork") {
		t.Errorf("the session forked from is not itself a fork: %q", rows[1].text)
	}
	if rows[0].id != fork || rows[1].id != first {
		t.Errorf("rows are newest first: %q then %q", rows[0].id, rows[1].id)
	}
	if v = h.view(); !strings.Contains(v, "> "+rows[0].text[:16]) {
		t.Errorf("the cursor is on the session the client is in:\n%s", v)
	}
	// Newest first, so the fork is the row the picker opens on and the session it came
	// from is the next one down.
	h.press("down")
	h.press("enter")
	h.wait("the resume", func() bool { return h.m.session.SessionID == first })
	h.waitFor("the resumed transcript", func(v string) bool { return strings.Contains(v, "fix the flaky fork test") })
}

func TestNewSessionStartsEmpty(t *testing.T) {
	h := newAppHarnessWith(t, scripted{text("done")}, altscreen)
	h.m.keys = sessionKeys(t)
	first := h.m.session.SessionID
	h.typeText("fix the flaky fork test")
	h.press("enter")
	h.waitTurn(stateCompleted)
	h.press("ctrl+n")
	h.wait("the new session", func() bool { return h.m.session.SessionID != first })
	v := h.view()
	if strings.Contains(v, "fix the flaky fork test") {
		t.Errorf("a new session draws none of the old one:\n%s", v)
	}
	if h.m.usage != (session.Usage{}) {
		t.Errorf("a new session spends nothing yet: %+v", h.m.usage)
	}
	if h.m.session.Workspace.Root == "" {
		t.Error("a new session is opened on the client's own directory")
	}
}

// TestSlashForkSwitchesToTheForkOnce pins what a command that opened a session does, and
// that it leaves exactly one subscription behind: the server attaches the fork to the
// connection that ran /fork, so a switch that only resumed it would be subscribed twice
// and every entry after it would be counted twice.
func TestSlashForkSwitchesToTheForkOnce(t *testing.T) {
	h := newAppHarnessWith(t, scripted{text("done")}, altscreen)
	first := h.m.session.SessionID
	h.typeText("/fork")
	h.press("enter")
	h.wait("the fork", func() bool { return h.m.session.SessionID != first })
	if !strings.Contains(h.view(), "switched to session "+h.m.session.SessionID) {
		t.Errorf("a command that forked says where it went:\n%s", h.view())
	}
	h.typeText("go")
	h.press("enter")
	h.waitTurn(stateCompleted)
	if h.m.usage != (session.Usage{Input: 10, Output: 2}) {
		t.Errorf("the fork is subscribed once, so its usage is counted once: %+v", h.m.usage)
	}
}

func TestTabCompletesFromTheHelpNotice(t *testing.T) {
	h := newAppHarness(t, scripted{text("done")})
	h.typeText("/pl")
	h.press("tab")
	if got := h.editor().Text(); got != "/pl" {
		t.Fatalf("with no /help notice yet there is nothing to complete from, editor holds %q", got)
	}
	h.editor().Clear()
	h.typeText("/help")
	h.press("enter")
	h.wait("the command list", func() bool { return slices.Contains(h.m.commands, "plugins") })
	h.typeText("/pl")
	h.press("tab")
	if got := h.editor().Text(); got != "/plugins" {
		t.Errorf("one candidate completes the name, editor holds %q", got)
	}
	h.editor().Clear()
	// Several candidates complete only as far as they agree: /note and /notice.
	h.typeText("/no")
	h.press("tab")
	if got := h.editor().Text(); got != "/not" {
		t.Errorf("several candidates complete to what they share, editor holds %q", got)
	}
}

// TestASwitchHoldsTheReplayUntilItsAnswer pins the order the contract gives a switch: the
// new session's entries arrive before the answer that names it, so they have to be held
// until there is a transcript for them to land in.
func TestASwitchHoldsTheReplayUntilItsAnswer(t *testing.T) {
	h := newHarness(t, nil)
	h.m.keys = sessionKeys(t)
	old := h.m.session.SessionID
	h.appended(session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("in the old session")}})
	h.press("ctrl+n")

	next := session.NewID().String()
	h.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{
		SessionID: next,
		Entry:     entry(t, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("in the new session")}}),
	})
	v := ansi.Strip(h.view())
	if !strings.Contains(v, "in the old session") || strings.Contains(v, "in the new session") {
		t.Fatalf("the replay is held while the switch is in flight:\n%s", v)
	}

	res, err := json.Marshal(protocol.SessionInfo{
		SessionID: next, Workspace: session.Workspace{Root: "/w"},
		Model: testRef, Mode: session.ModeStrict, Thinking: session.ThinkingOff,
	})
	if err != nil {
		t.Fatal(err)
	}
	runAll(t, h.update(CallResultMsg{Method: protocol.MethodSessionOpen, Result: res}))
	v = ansi.Strip(h.view())
	if strings.Contains(v, "in the old session") || !strings.Contains(v, "in the new session") {
		t.Fatalf("the answer replaces the transcript with what was held:\n%s", v)
	}
	if h.m.session.SessionID != next {
		t.Errorf("the session is the new one, got %q", h.m.session.SessionID)
	}
	// The session that was left is still attached until its close lands, and what it says
	// in the meantime is not what is on screen.
	h.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{
		SessionID: old,
		Entry:     entry(t, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("a late word from the old one")}}),
	})
	if strings.Contains(ansi.Strip(h.view()), "a late word") {
		t.Errorf("the session left draws nothing here:\n%s", h.view())
	}
	closes := h.sent(protocol.MethodSessionClose)
	if len(closes) != 1 {
		t.Fatalf("the session left is closed once, got %d closes", len(closes))
	}
	var p protocol.SessionCloseParams
	if err := json.Unmarshal(closes[0], &p); err != nil {
		t.Fatal(err)
	}
	if p.SessionID != old {
		t.Errorf("closed %q, want the session that was left %q", p.SessionID, old)
	}
}

// TestAFailedSwitchKeepsTheSession is the other half: nothing is given up until the new
// session is in hand, and what arrived while the switch was in flight belonged to the
// session the client never left.
func TestAFailedSwitchKeepsTheSession(t *testing.T) {
	h := newHarness(t, nil)
	h.m.keys = sessionKeys(t)
	old := h.m.session.SessionID
	h.press("ctrl+n")
	h.appended(session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("still here")}})
	h.update(CallResultMsg{Method: protocol.MethodSessionOpen, Err: errors.New("no such model")})

	if h.m.session.SessionID != old {
		t.Errorf("a failed switch leaves the session in place, got %q", h.m.session.SessionID)
	}
	v := ansi.Strip(h.view())
	if !strings.Contains(v, "still here") {
		t.Errorf("what was held is folded back:\n%s", v)
	}
	if !strings.Contains(v, "no such model") {
		t.Errorf("a failed switch says why:\n%s", v)
	}
	if got := h.sent(protocol.MethodSessionClose); len(got) != 0 {
		t.Errorf("nothing was closed: %d closes", len(got))
	}
}

func TestPickerActionsWhileDisconnectedSaySo(t *testing.T) {
	h := newHarness(t, nil)
	h.m.keys = sessionKeys(t)
	h.update(DisconnectedMsg{})
	for _, spelling := range []string{"ctrl+l", "ctrl+p", "shift+tab", "ctrl+n", "ctrl+y", "ctrl+r"} {
		h.press(spelling)
	}
	if h.m.pick != nil {
		t.Error("no picker while the server is gone")
	}
	if h.m.switching != nil {
		t.Error("no switch while the server is gone")
	}
	if got := ansi.Strip(h.view()); !strings.Contains(got, "not connected") {
		t.Errorf("a refused action says why:\n%s", got)
	}
	if got := len(h.sent(protocol.MethodRegistryRefresh)) + len(h.sent(protocol.MethodSessionSetModel)); got != 0 {
		t.Errorf("nothing was asked of a server that is gone: %d calls", got)
	}
}
