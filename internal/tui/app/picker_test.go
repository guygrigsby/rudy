// SPDX-License-Identifier: AGPL-3.0-or-later

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
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tui/keys"
	"github.com/guygrigsby/rudy/internal/tui/transcript"
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

// inlineRender is the other one: the client opens full screen (ADR 0015), so a test that
// reads what was committed to the terminal's own scrollback has to ask for inline, which
// is the only mode that commits anything.
func inlineRender(c *config.Config) { c.UI.Render = "inline" }

// inlineOver is the same last word for the pipe harness, which takes config overrides as
// a table rather than as a function.
func inlineOver() map[string]any { return map[string]any{"ui.render": "inline"} }

// filter types into the picker's filter. It is not appHarness.typeText, which leaves the
// editor's normal mode first: a picker takes the keys as they come, an "i" included.
func (h *appHarness) filter(s string) {
	h.t.Helper()
	for _, r := range s {
		h.dispatch(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	h.settle()
}

// printedFrom is what a batch of messages asked tea.Println to write, in order.
func printedFrom(msgs []tea.Msg) string {
	var out []string
	for _, msg := range msgs {
		if line, ok := printLine(msg); ok {
			out = append(out, line)
		}
	}
	return ansi.Strip(strings.Join(out, "\n"))
}

// fold folds one appended entry straight into the model, without the pump command Update
// batches beside it: a test that runs what an entry produced must not run the pump too,
// which waits for a notification that is not coming.
func (h *harness) fold(p session.Payload) tea.Cmd {
	h.t.Helper()
	params, err := json.Marshal(protocol.EntryAppended{
		SessionID: h.m.session.SessionID, Entry: entry(h.t, p),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return h.m.notification(protocol.Notification{Method: protocol.NotifyEntryAppended, Params: params})
}

// opened is the head every replay starts with: a log's first line is session_opened or
// fork_point, and a switched-to session's transcript begins there.
func opened() session.SessionOpened {
	return session.SessionOpened{
		SchemaVersion: 1, RudyVersion: "test",
		Workspace: session.Workspace{Root: "/w"},
		Model:     testRef, Thinking: session.ThinkingOff, Mode: session.ModeStrict, Agent: "default",
	}
}

// printedSince is what has been committed to scrollback since the print at index i.
func (h *appHarness) printedSince(i int) string {
	return ansi.Strip(strings.Join(h.prints[i:], "\n"))
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
	h := newAppHarness(t, scripted{heldText("thinking")})
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
		// Nothing on screen carries the level, so the answer says what it set.
		h.waitFor("the notice", func(v string) bool { return strings.Contains(v, "thinking: "+string(want)) })
	}
}

func TestThinkingToggleShowsThinkingWithoutWritingConfig(t *testing.T) {
	h := newHarness(t, nil)
	if h.m.showThinking {
		t.Fatal("the default is hidden")
	}
	h.appended(session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopEndTurn,
		Content: []session.Block{{Type: session.BlockThinking, Text: "the fork test is racy"}, session.TextBlock("looking")},
	})
	if strings.Contains(ansi.Strip(h.view()), "racy") {
		t.Fatalf("thinking is hidden by config:\n%s", h.view())
	}
	h.press("ctrl+t")
	if !h.m.showThinking {
		t.Fatal("the toggle flips the override the transcript is built from")
	}
	if h.m.cfg.UI.Transcript.Thinking != thinkingHidden {
		t.Errorf("the config is shared with the server and is never written, it says %q", h.m.cfg.UI.Transcript.Thinking)
	}
	h.appended(session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopEndTurn,
		Content: []session.Block{{Type: session.BlockThinking, Text: "the second thought"}},
	})
	if !strings.Contains(ansi.Strip(h.view()), "the second thought") {
		t.Errorf("thinking shows from the toggle on:\n%s", h.view())
	}
	h.press("ctrl+t")
	if h.m.showThinking {
		t.Error("the toggle flips back")
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

// TestASlashDraftListsTheCommands is the menu ADR 0015 decision 4 describes: what
// command.list answered, filtered as the draft grows, with what each command does beside
// it. The editor keeps the keyboard the whole time, which is what lets the draft go on
// being typed under the list.
func TestASlashDraftListsTheCommands(t *testing.T) {
	h := newAppHarnessWith(t, scripted{text("done")}, altscreen)
	h.wait("the command list", func() bool { return commandNamed(h.m.commands, "plugins") })
	h.typeText("/")
	view := ansi.Strip(h.view())
	for _, want := range []string{"/plugins", "List loaded plugins and their state", "/exit"} {
		if !strings.Contains(view, want) {
			t.Errorf("a slash draft lists %q:\n%s", want, view)
		}
	}
	if got := h.editor().Text(); got != "/" {
		t.Errorf("the menu is a completion, not a picker: the draft is still being typed, editor holds %q", got)
	}
	h.typeText("pl")
	view = ansi.Strip(h.view())
	if !strings.Contains(view, "/plugins") || strings.Contains(view, "/fork") {
		t.Errorf("the list filters to what the draft matches:\n%s", view)
	}
	// A draft past the name is an argument, which has no completion source.
	h.typeText("ugins x")
	if strings.Contains(ansi.Strip(h.view()), "List loaded plugins") {
		t.Errorf("an argument closes the menu:\n%s", ansi.Strip(h.view()))
	}
}

// TestTheMenuCompletesAndSubmits pins the three keys a menu takes and the one it does not:
// tab and Enter complete a name that is still a prefix, the arrows move the selection, and
// Enter on a name already complete is the submit it always was, so a command typed out in
// full still runs on one press.
func TestTheMenuCompletesAndSubmits(t *testing.T) {
	h := newAppHarnessWith(t, scripted{text("done")}, altscreen)
	h.wait("the command list", func() bool { return commandNamed(h.m.commands, "notice") })
	// /notice and /note both match, listed in the order they were registered.
	h.typeText("/not")
	h.press("tab")
	if got := h.editor().Text(); got != "/notice " {
		t.Fatalf("tab completes the selected row and the space an argument goes after, editor holds %q", got)
	}
	h.editor().Clear()
	h.typeText("/not")
	h.press("down")
	h.press("enter")
	if got := h.editor().Text(); got != "/note " {
		t.Fatalf("the arrows move the selection Enter completes, editor holds %q", got)
	}
	h.editor().Clear()
	h.typeText("/notice hello there")
	h.press("enter")
	h.wait("the notice the command answered with", func() bool {
		return strings.Contains(ansi.Strip(h.view()), "noticed: hello there")
	})
}

// TestEscDismissesTheMenuForThatDraftOnly: the menu is dismissed, not turned off. Another
// letter is another draft, and the list belongs to the draft.
func TestEscDismissesTheMenuForThatDraftOnly(t *testing.T) {
	h := newAppHarnessWith(t, scripted{text("done")}, altscreen)
	h.wait("the command list", func() bool { return commandNamed(h.m.commands, "plugins") })
	h.typeText("/pl")
	if !strings.Contains(ansi.Strip(h.view()), "/plugins") {
		t.Fatalf("the menu stands:\n%s", ansi.Strip(h.view()))
	}
	h.press("escape")
	if strings.Contains(ansi.Strip(h.view()), "List loaded plugins") {
		t.Errorf("escape dismisses the menu:\n%s", ansi.Strip(h.view()))
	}
	if got := h.editor().Text(); got != "/pl" {
		t.Errorf("a dismissal leaves the draft alone, editor holds %q", got)
	}
	h.typeText("u")
	if !strings.Contains(ansi.Strip(h.view()), "List loaded plugins") {
		t.Errorf("another letter is another draft, and the menu belongs to the draft:\n%s", ansi.Strip(h.view()))
	}
}

// commandNamed reports whether the client has been told about a command by this name.
func commandNamed(cmds []protocol.CommandInfo, name string) bool {
	return slices.ContainsFunc(cmds, func(c protocol.CommandInfo) bool { return c.Name == name })
}

// TestASwitchHoldsTheReplayUntilItsAnswer pins the order the contract gives a switch: the
// new session's entries arrive before the answer that names it, so they have to be held
// until there is a transcript for them to land in.
func TestASwitchHoldsTheReplayUntilItsAnswer(t *testing.T) {
	// Altscreen, so what a switch rebuilt is what the view says: inline commits a
	// replayed turn to scrollback, which TestAnInlineSwitchCommitsTheReplay reads.
	h := newHarness(t, map[string]any{"ui.render": renderAltscreen})
	h.m.keys = sessionKeys(t)
	old := h.m.session.SessionID
	h.appended(session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("in the old session")}})
	h.typeText("queued for the old session")
	h.m.ed.Enqueue()
	h.press("ctrl+n")

	next := session.NewID().String()
	for _, p := range []session.Payload{
		opened(),
		session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("in the new session")}},
	} {
		h.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: next, Entry: entry(t, p)})
	}
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
	// A message queued behind the old session's turn was meant for that session: it comes
	// back as an editable draft rather than draining into this one.
	if len(h.m.ed.Queue()) != 0 || h.m.ed.Text() != "queued for the old session" {
		t.Errorf("the queue crosses no switch: draft %q queue %q", h.m.ed.Text(), h.m.ed.Queue())
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
	var calls int
	for _, method := range []string{
		protocol.MethodRegistryRefresh, protocol.MethodSessionSetModel, protocol.MethodSessionSetThinking,
		protocol.MethodSessionOpen, protocol.MethodSessionFork, protocol.MethodSessionList,
	} {
		calls += len(h.sent(method))
	}
	if calls != 0 {
		t.Errorf("nothing was asked of a server that is gone: %d calls", calls)
	}
}

// TestStatusAndWidgetsSurviveASwitch pins what a switch keeps: a plugin's status item and
// its widget belong to the connection, not to the session, and nothing re-sends them but
// a hello.
func TestStatusAndWidgetsSurviveASwitch(t *testing.T) {
	h := newHarness(t, map[string]any{
		"ui.render":       renderAltscreen,
		"ui.layout.slots": []string{"header", "transcript", "input", "status"},
		"ui.status.items": []string{"model", "memory:tokens"},
	})
	h.m.keys = sessionKeys(t)
	h.notify(protocol.NotifyStatusUpdated, protocol.StatusUpdated{Items: []protocol.StatusItem{
		{Owner: "memory", Key: "tokens", Content: []protocol.Span{{Text: "12 notes", Role: "muted"}}},
	}})
	h.notify(protocol.NotifyWidgetUpdated, protocol.Widget{
		Owner: "memory", Key: "banner", Slot: protocol.SlotHeader,
		Content: []protocol.Span{{Text: "project memory on", Role: "muted"}},
	})
	h.press("ctrl+n")
	next := session.NewID().String()
	res, err := json.Marshal(protocol.SessionInfo{
		SessionID: next, Workspace: session.Workspace{Root: "/w"},
		Model: testRef, Mode: session.ModeStrict, Thinking: session.ThinkingOff,
	})
	if err != nil {
		t.Fatal(err)
	}
	runAll(t, h.update(CallResultMsg{Method: protocol.MethodSessionOpen, Result: res}))
	v := ansi.Strip(h.view())
	if !strings.Contains(v, "12 notes") {
		t.Errorf("a plugin's status item survives a switch, nothing re-sends it:\n%s", v)
	}
	if !strings.Contains(v, "project memory on") {
		t.Errorf("a plugin's widget survives a switch:\n%s", v)
	}
}

// TestForkReplaysTheLogOnce pins the count a double replay would break: the server
// attaches the fork a /fork opened and replays it, and this client closes that
// attachment and resumes, which replays it again. Rows dedupe by key; a usage total has
// no key, so the entry itself is folded once.
func TestForkReplaysTheLogOnce(t *testing.T) {
	h := newAppHarnessWith(t, scripted{text("first answer")}, altscreen)
	first := h.m.session.SessionID
	h.typeText("fix the flaky fork test")
	h.press("enter")
	h.waitTurn(stateCompleted)
	if h.m.usage != (session.Usage{Input: 10, Output: 2}) {
		t.Fatalf("one turn, one completion: %+v", h.m.usage)
	}
	h.typeText("/fork")
	h.press("enter")
	h.wait("the fork", func() bool { return h.m.session.SessionID != first })
	h.waitFor("the fork's replay", func(v string) bool { return strings.Contains(v, "first answer") })
	if h.m.usage != (session.Usage{Input: 10, Output: 2}) {
		t.Errorf("the fork's log is priced once however many times it was replayed: %+v", h.m.usage)
	}
	if got := rowKinds(h.m.tr.Rows()); !slices.Equal(got, []transcript.RowKind{transcript.RowUser, transcript.RowAssistant}) {
		t.Errorf("one row per entry: %v", got)
	}
}

// TestAnInlineSwitchCommitsTheReplay pins inline rendering's half of a switch: a replayed
// log goes to the terminal's own scrollback turn by turn, in order, and what is left in
// the live region is the newest turn, which the next user message commits like any other.
// The bound is one turn, whether the replay came with a switch or at launch.
func TestAnInlineSwitchCommitsTheReplay(t *testing.T) {
	h := newAppHarnessWith(t, scripted{text("first answer"), text("second answer")}, inlineRender)
	h.m.keys = sessionKeys(t)
	first := h.m.session.SessionID
	h.typeText("first question")
	h.press("enter")
	h.waitTurn(stateCompleted)
	h.waitPrinted("first answer")
	h.typeText("second question")
	h.press("enter")
	h.waitTurnCount(2)
	h.waitPrinted("second answer")

	at := len(h.prints)
	h.press("ctrl+y")
	h.wait("the fork", func() bool { return h.m.session.SessionID != first })
	h.wait("the replay's first turn", func() bool { return strings.Contains(h.printedSince(at), "first answer") })
	got := h.printedSince(at)
	if strings.Contains(got, "second question") {
		t.Errorf("the newest replayed turn stays live:\n%s", got)
	}
	if v := h.view(); strings.Contains(v, "first question") {
		t.Errorf("a committed turn leaves the live region:\n%s", v)
	}
	h.waitFor("the newest turn, live", func(v string) bool { return strings.Contains(v, "second answer") })

	// The next user message commits it, the way it commits any turn before the one it
	// starts, and then the live region holds only what was just typed.
	at = len(h.prints)
	h.typeText("third question")
	h.press("enter")
	h.wait("the last replayed turn", func() bool { return strings.Contains(h.printedSince(at), "second answer") })
	one, two := strings.Index(h.printed(), "first question"), strings.Index(h.printed(), "second question")
	if one < 0 || two < 0 || one > two {
		t.Errorf("the turns reach scrollback in the order they were logged:\n%s", h.printed())
	}
	if v := h.view(); strings.Contains(v, "second answer") || !strings.Contains(v, "third question") {
		t.Errorf("the live region is what happens next:\n%s", v)
	}
}

// TestAnInlineReplayCommitsEachTurnAsTheNextStarts is the same rule where no switch is in
// flight: the launch-time replay of a resumed session (rudy --resume, Task 9). Every turn
// but the newest goes to scrollback as the next one starts, so what stands in the live
// region is bounded by one turn.
func TestAnInlineReplayCommitsEachTurnAsTheNextStarts(t *testing.T) {
	h := newHarness(t, inlineOver())
	h.fold(session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("first question")}})
	h.fold(session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopEndTurn,
		Content: []session.Block{session.TextBlock("first answer")},
	})
	if got := printedFrom(runAll(t, h.fold(session.UserMessage{
		Source: session.SourceTyped, Content: []session.Block{session.TextBlock("second question")},
	}))); !strings.Contains(got, "first question") || !strings.Contains(got, "first answer") {
		t.Fatalf("the turn before the one starting is committed:\n%s", got)
	} else if strings.Contains(got, "second question") {
		t.Errorf("the turn that just started stays live:\n%s", got)
	}
	v := ansi.Strip(h.view())
	if strings.Contains(v, "first question") || strings.Contains(v, "first answer") {
		t.Errorf("a committed turn leaves the live region:\n%s", v)
	}
	if !strings.Contains(v, "second question") {
		t.Errorf("the newest turn is what the live region holds:\n%s", v)
	}
}

// TestASecondSwitchIsRefusedAndItsForkLetGo pins what happens to a fork that arrives
// while another switch is in flight: it is nobody's, and holding it for the life of the
// connection would keep a session open that nothing draws.
func TestASecondSwitchIsRefusedAndItsForkLetGo(t *testing.T) {
	h := newHarness(t, nil)
	h.m.keys = sessionKeys(t)
	h.press("ctrl+n")
	orphan := session.NewID().String()
	res, err := json.Marshal(protocol.CommandRunResult{SessionID: orphan, Notice: "forked to " + orphan})
	if err != nil {
		t.Fatal(err)
	}
	runAll(t, h.update(CallResultMsg{Method: protocol.MethodCommandRun, Name: "fork", Result: res}))
	if got := ansi.Strip(h.view()); !strings.Contains(got, "a session switch is already in flight") {
		t.Errorf("a refused switch says so:\n%s", got)
	}
	var closed []string
	for _, raw := range h.sent(protocol.MethodSessionClose) {
		var p protocol.SessionCloseParams
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		closed = append(closed, p.SessionID)
	}
	if !slices.Contains(closed, orphan) {
		t.Errorf("the fork nobody took is closed, closed %v", closed)
	}
}

// TestAPickerTakesTheKeysAQuestionWants pins the order of the two things that own the
// keyboard: while a picker is up it takes y, a and n as filter text, and the question is
// still standing when it closes.
func TestAPickerTakesTheKeysAQuestionWants(t *testing.T) {
	h := newAppHarness(t, scripted{toolCall("bash", `{"command":"go test ./..."}`), text("done")})
	h.typeText("run the tests")
	h.press("enter")
	h.waitFor("the question", func(v string) bool { return strings.Contains(v, "allow once [y]") })
	h.press("ctrl+l")
	h.press("y")
	if h.m.pick == nil || h.m.pick.filter != "y" {
		t.Fatalf("the picker takes the key the question wanted: picker %+v", h.m.pick)
	}
	if h.m.focused() == nil {
		t.Error("the question was not answered by a key the picker took")
	}
	h.press("escape")
	if h.m.pick != nil {
		t.Fatal("escape closes the picker")
	}
	if h.m.focused() == nil {
		t.Fatal("the question the picker hid is still standing")
	}
	h.press("y")
	h.waitTurn(stateCompleted)
	if dec := findDecision(t, h.entries(), "bash"); dec.Decision != session.Allow {
		t.Errorf("decision %+v", dec)
	}
}

func TestSessionRowsTagForksAndChildren(t *testing.T) {
	at := time.Date(2026, 9, 9, 14, 3, 0, 0, time.UTC)
	rows := sessionRows([]session.Summary{
		{ID: session.NewID(), OpenedAt: at, Workspace: session.Workspace{Root: "/w"}, Model: testRef, Forked: true, ParentSessionID: session.NewID().String()},
		{ID: session.NewID(), OpenedAt: at, Workspace: session.Workspace{Root: "/w"}, Model: testRef, ParentSessionID: session.NewID().String()},
		{ID: session.NewID(), OpenedAt: at, Workspace: session.Workspace{Root: "/w"}, Model: testRef},
	})
	for i, want := range []string{"fork child", "child", "/w  fake:m1"} {
		if !strings.HasSuffix(rows[i].text, want) {
			t.Errorf("row %d is %q, want it to end %q", i, rows[i].text, want)
		}
	}
	if !strings.HasPrefix(rows[0].text, at.Local().Format("2006-01-02 15:04")) {
		t.Errorf("a row opens with when the session was opened: %q", rows[0].text)
	}
}

// TestAnEntryIsFoldedOnce pins the rule TestForkReplaysTheLogOnce depends on, where the
// ordering is the test's own: a log the server sends twice (the fork it attached and this
// client re-attached to) leaves one row and one usage total. The transcript dedupes its
// rows by key; the usage has no key, so the entry itself is folded once.
func TestAnEntryIsFoldedOnce(t *testing.T) {
	h := newHarness(t, map[string]any{"ui.render": renderAltscreen})
	e := entry(t, session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopEndTurn,
		Content: []session.Block{session.TextBlock("counted once")},
		Usage:   session.Usage{Input: 10, Output: 2},
	})
	for range 2 {
		h.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: h.m.session.SessionID, Entry: e})
	}
	if h.m.usage != (session.Usage{Input: 10, Output: 2}) {
		t.Errorf("a replayed entry is priced once: %+v", h.m.usage)
	}
	if got := len(h.m.tr.Rows()); got != 1 {
		t.Errorf("and draws one row, got %d", got)
	}
}

// TestASwitchCommitsWhatItHeld is the switch's own half of the inline commit, where the
// whole replay was held and the fold's per-turn commits are suppressed: the answer prints
// every turn but the newest, in one ordered block.
func TestASwitchCommitsWhatItHeld(t *testing.T) {
	h := newHarness(t, inlineOver())
	h.m.keys = sessionKeys(t)
	h.press("ctrl+n")
	next := session.NewID().String()
	for _, p := range []session.Payload{
		opened(),
		session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("first question")}},
		session.AssistantMessage{
			Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopEndTurn,
			Content: []session.Block{session.TextBlock("first answer")},
		},
		session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("second question")}},
	} {
		h.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: next, Entry: entry(t, p)})
	}
	res, err := json.Marshal(protocol.SessionInfo{
		SessionID: next, Workspace: session.Workspace{Root: "/w"},
		Model: testRef, Mode: session.ModeStrict, Thinking: session.ThinkingOff,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := printedFrom(runAll(t, h.update(CallResultMsg{Method: protocol.MethodSessionOpen, Result: res})))
	if !strings.Contains(got, "first question") || !strings.Contains(got, "first answer") {
		t.Fatalf("the held replay is committed turn by turn:\n%s", got)
	}
	if strings.Contains(got, "second question") {
		t.Errorf("the newest turn stays live:\n%s", got)
	}
	if v := ansi.Strip(h.view()); strings.Contains(v, "first question") || !strings.Contains(v, "second question") {
		t.Errorf("the live region holds the newest turn and nothing before it:\n%s", v)
	}
}

// TestASwitchWaitsForTheHeadOfTheLog pins where a switched-to session's transcript
// begins. A fork a command opened is replayed twice, once for the attachment the server
// made and once for the one this client takes, and the switch can land part way through
// the first copy. Folding from there would put an answer above its own question, so the
// remains of that copy are dropped and the next copy is taken from its head.
func TestASwitchWaitsForTheHeadOfTheLog(t *testing.T) {
	h := newHarness(t, map[string]any{"ui.render": renderAltscreen})
	h.m.keys = sessionKeys(t)
	h.press("ctrl+n")
	next := session.NewID().String()
	answer := entry(t, session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopEndTurn,
		Content: []session.Block{session.TextBlock("the answer")},
		Usage:   session.Usage{Input: 10, Output: 2},
	})
	question := entry(t, session.UserMessage{
		Source: session.SourceTyped, Content: []session.Block{session.TextBlock("the question")},
	})
	// What is left of the first copy when the switch takes effect: no head, and the
	// answer without the question it belongs to.
	h.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: next, Entry: answer})
	res, err := json.Marshal(protocol.SessionInfo{
		SessionID: next, Workspace: session.Workspace{Root: "/w"},
		Model: testRef, Mode: session.ModeStrict, Thinking: session.ThinkingOff,
	})
	if err != nil {
		t.Fatal(err)
	}
	runAll(t, h.update(CallResultMsg{Method: protocol.MethodSessionResume, Result: res}))
	if got := len(h.m.tr.Rows()); got != 0 {
		t.Fatalf("nothing is folded before the head of the log, got %d rows", got)
	}
	// The second copy, from the head.
	h.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: next, Entry: entry(t, opened())})
	for _, e := range []session.Entry{question, answer} {
		h.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: next, Entry: e})
	}
	if got := rowKinds(h.m.tr.Rows()); !slices.Equal(got, []transcript.RowKind{transcript.RowUser, transcript.RowAssistant}) {
		t.Errorf("the log reads in the order it was written: %v", got)
	}
	if h.m.usage != (session.Usage{Input: 10, Output: 2}) {
		t.Errorf("and is priced once: %+v", h.m.usage)
	}
}

// TestTheModelPickerSaysWhoServesEachModel: an endpoint that fronts several upstreams gives
// every model an id that looks like its own, so the picker names the upstream and the filter
// reads it.
func TestTheModelPickerSaysWhoServesEachModel(t *testing.T) {
	models := []provider.Model{
		{Ref: session.ModelRef{Provider: "aperture", Model: "anthropic/claude-fable-5"}, Upstream: "OpenRouter", ContextWindow: 1000000},
		{Ref: session.ModelRef{Provider: "aperture", Model: "cline-pass/kimi-k3"}, Upstream: "ClinePass", ContextWindow: 1048576},
		{Ref: session.ModelRef{Provider: "mlx", Model: "local-thing"}},
	}
	rows := modelRows(models)
	if len(rows) != 3 {
		t.Fatalf("rows %+v", rows)
	}
	if !strings.Contains(rows[0].text, "OpenRouter") || !strings.Contains(rows[1].text, "ClinePass") {
		t.Errorf("each row names its upstream: %+v", rows)
	}
	// An endpoint that says nothing leaves no gap where the upstream would have been.
	if strings.Contains(rows[2].text, "  ") {
		t.Errorf("a model with no upstream draws no empty column: %q", rows[2].text)
	}
	// The filter reads the row, so the upstream is how a person finds those models.
	p := &picker{kind: pickerModel, rows: rows, filter: "openrouter"}
	if got := p.visible(); len(got) != 1 || got[0].id != "aperture:anthropic/claude-fable-5" {
		t.Errorf("filtering by upstream finds them: %+v", got)
	}
}
