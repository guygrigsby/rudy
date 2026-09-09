package app

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tui/input"
	"github.com/guygrigsby/rudy/internal/tui/transcript"
)

func TestSubmitStreamsAndCommitsInline(t *testing.T) {
	h := newAppHarness(t, scripted{text("Looking at the test first.")})
	h.typeText("fix the flaky fork test")
	h.press("enter")
	h.waitTurn(stateCompleted)
	// During the turn the live region held the user row and the streaming row; at
	// completion both were committed through tea.Println in order and the live region
	// holds only the editor and the status line.
	h.waitPrinted("Looking at the test first.")
	got := h.printed()
	user := strings.Index(got, "› fix the flaky fork test")
	answer := strings.Index(got, "Looking at the test first.")
	if user < 0 || answer < 0 {
		t.Errorf("printed %q", got)
	}
	if user > answer {
		t.Errorf("the user row commits before the answer: %q", got)
	}
	if strings.Contains(h.view(), "Looking at the test") {
		t.Errorf("committed rows must leave the live region:\n%s", h.view())
	}
	if len(h.m.tr.Rows()) != 0 {
		t.Errorf("a committed turn leaves no rows: %+v", h.m.tr.Rows())
	}
	if h.editor().Text() != "" {
		t.Errorf("editor cleared on submit, holds %q", h.editor().Text())
	}
}

func TestEscOnceSteersTwiceCancels(t *testing.T) {
	h := newAppHarness(t, scripted{slowText("thinking...", 2*time.Second)})
	h.typeText("go")
	h.press("enter")
	h.waitTurn(stateStreaming)
	h.press("escape") // insert to normal, consumed by the editor
	if h.editor().Mode() != input.ModeNormal {
		t.Fatalf("first escape only changes mode, mode is %q", h.editor().Mode())
	}
	if h.m.turn.state != stateStreaming {
		t.Fatalf("an escape the editor consumed must not reach the turn: %q", h.m.turn.state)
	}
	h.press("escape") // normal, turn running: steer
	h.waitTurn(stateSteering)
	h.typeText("use the other approach")
	h.press("enter") // continues the turn as a steer message
	h.waitTurn(stateStreaming)
	h.press("escape") // to normal
	h.press("escape") // steer again
	h.waitTurn(stateSteering)
	h.press("escape") // steering with an empty editor: cancel
	h.waitTurn(stateIdle)
	h.waitPrinted("interrupted (cancel)")
	if got := h.printed(); !strings.Contains(got, "› use the other approach") {
		t.Errorf("the steer message commits with the turn it continued: %q", got)
	}
}

// TestDoubleEscWithinWindowCancels pins the window rule where nothing else can stand in
// for it: a steering turn with a draft, where one Esc does nothing at all and only the
// second within the window cancels. The steering state is waited for rather than raced
// through, because a steer and a cancel issued in the same instant race inside the
// server's runner, which is its own problem and not this table's.
func TestDoubleEscWithinWindowCancels(t *testing.T) {
	h := newAppHarnessWith(t, scripted{slowText("x", 2*time.Second)}, func(c *config.Config) {
		c.Permissions.DoublePressMS = 300
	})
	h.typeText("go")
	h.press("enter")
	h.waitTurn(stateStreaming)
	h.press("escape") // insert to normal
	h.press("escape") // steer
	h.waitTurn(stateSteering)
	h.typeText("second thoughts")
	h.press("escape") // insert to normal
	h.press("escape") // steering with a draft: nothing happens
	if h.m.turn.state != stateSteering {
		t.Fatalf("one escape with a draft leaves the turn steering, got %q", h.m.turn.state)
	}
	h.press("escape") // within 300ms of the last: cancel
	h.waitTurn(stateIdle)
	if got := h.editor().Text(); got != "second thoughts" {
		t.Errorf("the draft survives the cancel, got %q", got)
	}
}

// TestDoubleEscWhileRunningCancels pins the running row of the TurnControl table against
// a server that only records what it was asked: the second Esc inside the window must send
// cancel rather than a second steer, and must put the queue back where it can be edited.
// Nothing here waits on a turn, so nothing here can race one.
func TestDoubleEscWhileRunningCancels(t *testing.T) {
	h := newHarness(t, nil)
	h.notify(protocol.NotifyTurnState, protocol.TurnStateChanged{
		SessionID: h.m.session.SessionID, TurnID: session.NewID().String(), State: stateStreaming,
	})
	h.typeText("queued")
	runCmd(t, h.press("alt+enter")) // a running turn queues the follow-up
	if len(h.m.ed.Queue()) != 1 || !h.m.ed.Empty() {
		t.Fatalf("queued: draft %q queue %q", h.m.ed.Text(), h.m.ed.Queue())
	}
	runCmd(t, h.press("escape")) // insert to normal, the editor's
	runCmd(t, h.press("escape")) // running: steer
	runCmd(t, h.press("escape")) // running, inside the window: cancel
	want := []session.Interrupt{session.InterruptSteer, session.InterruptCancel}
	if got := h.interrupts(); !slices.Equal(got, want) {
		t.Fatalf("interrupts %v want %v", got, want)
	}
	if got := h.m.ed.Text(); got != "queued" {
		t.Errorf("the cancelled turn's queue comes back as a draft, got %q", got)
	}
	if q := h.m.ed.Queue(); len(q) != 0 {
		t.Errorf("the queue is emptied into the draft: %q", q)
	}
}

// TestAnIdleEscArmsNothing is the other half of the window rule: an Esc the table did not
// act on must not make the next one a double press, or a turn that starts by itself (the
// queue draining) would be cancelled by the Esc that was meant to steer it.
func TestAnIdleEscArmsNothing(t *testing.T) {
	h := newHarness(t, nil)
	runCmd(t, h.press("escape")) // insert to normal, the editor's
	runCmd(t, h.press("escape")) // idle: nothing to interrupt
	if got := h.interrupts(); len(got) != 0 {
		t.Fatalf("an idle escape interrupts nothing, sent %v", got)
	}
	h.notify(protocol.NotifyTurnState, protocol.TurnStateChanged{
		SessionID: h.m.session.SessionID, TurnID: session.NewID().String(), State: stateStreaming,
	})
	runCmd(t, h.press("escape")) // the first Esc of a running turn: steer
	want := []session.Interrupt{session.InterruptSteer}
	if got := h.interrupts(); !slices.Equal(got, want) {
		t.Fatalf("interrupts %v want %v", got, want)
	}
}

func TestAFailedAnswerPutsTheQuestionBack(t *testing.T) {
	h := newHarness(t, nil)
	h.notify(protocol.NotifyPermissionRequested, protocol.PermissionRequested{
		SessionID: h.m.session.SessionID, TurnID: session.NewID().String(), ToolUseID: "t1",
		Tool: "bash", Input: json.RawMessage(`{"command":"go test ./..."}`),
		Matcher: session.Matcher{Tool: "bash", Prefix: "go test"},
	})
	runCmd(t, h.press("y"))
	if strings.Contains(ansi.Strip(h.view()), "allow once [y]") {
		t.Fatalf("the question comes down when the answer goes out:\n%s", h.view())
	}
	h.update(CallResultMsg{Method: protocol.MethodSessionAnswer, Err: errors.New("boom")})
	if !strings.Contains(ansi.Strip(h.view()), "allow once [y]") {
		t.Errorf("an answer that never reached the server puts the question back:\n%s", h.view())
	}
	if h.m.turn.prompt == nil {
		t.Error("and the keyboard answers it again")
	}
	if !strings.Contains(ansi.Strip(h.view()), "session.answer: boom") {
		t.Errorf("the failure is a notice too:\n%s", h.view())
	}
	// The decision arriving anyway is what takes it down for good.
	h.appended(session.PermissionDecision{
		ToolUseID: "t1", Tool: "bash", Mode: session.ModeStrict,
		Matcher:  session.Matcher{Tool: "bash", Prefix: "go test"},
		Decision: session.Allow, DecidedBy: session.ByAsker, Scope: session.ScopeOnce, Reason: "asker",
	})
	h.update(CallResultMsg{Method: protocol.MethodSessionAnswer, Err: errors.New("late")})
	if strings.Contains(ansi.Strip(h.view()), "allow once [y]") {
		t.Errorf("a decided question does not come back:\n%s", h.view())
	}
}

func TestALateRowCommitsAtOnce(t *testing.T) {
	h := newAppHarness(t, scripted{text("done")})
	h.typeText("go")
	h.press("enter")
	h.waitTurn(stateCompleted)
	h.waitPrinted("› go")
	// A note appended between turns carries the turn that has just gone to scrollback.
	h.typeText("/note the tests are green")
	h.press("enter")
	h.waitPrinted("the tests are green")
	if strings.Contains(h.view(), "the tests are green") {
		t.Errorf("a late row must leave the live region:\n%s", h.view())
	}
	if rows := h.m.tr.Rows(); len(rows) != 0 {
		t.Errorf("nothing is left on screen: %+v", rows)
	}
}

func TestALateRowStaysInAltscreen(t *testing.T) {
	h := newAppHarnessWith(t, scripted{text("done")}, func(c *config.Config) {
		c.UI.Render = renderAltscreen
	})
	h.typeText("go")
	h.press("enter")
	h.waitTurn(stateCompleted)
	h.typeText("/note the tests are green")
	h.press("enter")
	h.waitFor("the note", func(v string) bool { return strings.Contains(v, "the tests are green") })
	if got := h.printed(); got != "" {
		t.Errorf("altscreen commits nothing to scrollback, printed %q", got)
	}
}

func TestAQueuedSlashRunsAsACommand(t *testing.T) {
	h := newAppHarness(t, scripted{slowText("one", 300*time.Millisecond), text("two")})
	h.typeText("first")
	h.press("enter")
	h.waitTurn(stateStreaming)
	h.typeText("/notice from the queue")
	h.press("alt+enter")
	if len(h.editor().Queue()) != 1 {
		t.Fatalf("queued %q", h.editor().Queue())
	}
	h.waitFor("the queued command running", func(v string) bool {
		return strings.Contains(v, "noticed: from the queue")
	})
}

func TestFollowUpAtRestSubmits(t *testing.T) {
	h := newAppHarness(t, scripted{text("done")})
	h.typeText("go")
	h.press("alt+enter") // nothing is running, so there is nothing to queue behind
	if q := h.editor().Queue(); len(q) != 0 {
		t.Fatalf("a follow-up at rest is a submit, not a queue: %q", q)
	}
	h.waitTurn(stateCompleted)
	h.waitPrinted("› go")
}

func TestQueueSubmitsWhenTheTurnRests(t *testing.T) {
	h := newAppHarness(t, scripted{slowText("one", 300*time.Millisecond), text("two")})
	h.typeText("first")
	h.press("enter")
	h.typeText("second")
	h.press("alt+enter") // followUp: queued
	if !h.editor().Empty() || len(h.editor().Queue()) != 1 {
		t.Fatalf("queued: draft %q queue %q", h.editor().Text(), h.editor().Queue())
	}
	h.waitTurnCount(2)
	h.waitPrinted("› second")
	if q := h.editor().Queue(); len(q) != 0 {
		t.Errorf("the queue is emptied as it is sent: %q", q)
	}
}

func TestSubmitWhileRunningQueues(t *testing.T) {
	h := newAppHarness(t, scripted{slowText("one", 300*time.Millisecond), text("two")})
	h.typeText("first")
	h.press("enter")
	h.waitTurn(stateStreaming)
	h.typeText("second")
	h.press("enter") // a running turn takes Enter as a follow-up
	if !h.editor().Empty() || len(h.editor().Queue()) != 1 {
		t.Fatalf("queued: draft %q queue %q", h.editor().Text(), h.editor().Queue())
	}
	h.waitTurnCount(2)
	h.waitPrinted("› second")
}

func TestDequeueRestoresTheEditor(t *testing.T) {
	h := newAppHarness(t, scripted{slowText("one", 2*time.Second)})
	h.typeText("first")
	h.press("enter")
	h.waitTurn(stateStreaming)
	h.typeText("queued one")
	h.press("alt+enter")
	h.typeText("queued two")
	h.press("alt+enter")
	h.typeText("still a draft")
	h.press("alt+up") // app.message.dequeue
	if q := h.editor().Queue(); len(q) != 0 {
		t.Fatalf("the queue is emptied into the draft: %q", q)
	}
	want := "queued one\n\nqueued two\n\nstill a draft"
	if got := h.editor().Text(); got != want {
		t.Fatalf("draft %q want %q", got, want)
	}
}

func TestCancelPutsTheQueueBackInTheEditor(t *testing.T) {
	h := newAppHarness(t, scripted{slowText("one", 2*time.Second)})
	h.typeText("first")
	h.press("enter")
	h.waitTurn(stateStreaming)
	h.typeText("queued")
	h.press("alt+enter")
	h.press("escape") // insert to normal
	h.press("escape") // steer
	h.waitTurn(stateSteering)
	h.press("escape") // steering with an empty editor: cancel
	h.waitTurn(stateIdle)
	if got := h.editor().Text(); got != "queued" {
		t.Fatalf("a cancelled turn's queue comes back as a draft, got %q", got)
	}
}

func TestPermissionPromptInline(t *testing.T) {
	h := newAppHarness(t, scripted{toolCall("bash", `{"command":"go test ./..."}`), text("done")})
	h.typeText("run the tests")
	h.press("enter")
	h.waitFor("the question", func(v string) bool { return strings.Contains(v, "allow once [y]") })
	h.press("y")
	h.waitTurn(stateCompleted)
	dec := findDecision(t, h.entries(), "bash")
	if dec.Decision != session.Allow || dec.DecidedBy != session.ByAsker || dec.Scope != session.ScopeOnce {
		t.Errorf("decision %+v", dec)
	}
	if dec.Reason != "asker" {
		t.Errorf("reason %q", dec.Reason)
	}
	if h.m.turn.prompt != nil {
		t.Error("the answered question is cleared")
	}
	h.waitPrinted("ran go test ./...")
}

func TestPermissionKeys(t *testing.T) {
	for _, tc := range []struct {
		key      string
		decision session.Decision
		scope    session.Scope
	}{
		{"a", session.Allow, session.ScopeSession},
		{"n", session.Deny, session.ScopeOnce},
		{"escape", session.Deny, session.ScopeOnce},
	} {
		t.Run(tc.key, func(t *testing.T) {
			h := newAppHarness(t, scripted{toolCall("bash", `{"command":"go test ./..."}`), text("done")})
			h.typeText("run the tests")
			h.press("enter")
			h.waitFor("the question", func(v string) bool { return strings.Contains(v, "allow once [y]") })
			h.press(tc.key)
			h.waitTurn(stateCompleted)
			dec := findDecision(t, h.entries(), "bash")
			if dec.Decision != tc.decision || dec.DecidedBy != session.ByAsker || dec.Scope != tc.scope {
				t.Errorf("decision %+v", dec)
			}
			if strings.Contains(h.view(), "allow once [y]") {
				t.Errorf("the answered question must go:\n%s", h.view())
			}
		})
	}
}

func TestAltscreenKeepsRowsExpandable(t *testing.T) {
	for _, render := range []string{renderAltscreen, "inline"} {
		t.Run(render, func(t *testing.T) {
			h := newAppHarnessWith(t, scripted{toolCall("read", `{"path":"a.go"}`), text("done")}, func(c *config.Config) {
				c.UI.Render = render
			})
			for i, prompt := range []string{"first", "second"} {
				h.typeText(prompt)
				h.press("enter")
				h.waitTurnCount(i + 1)
			}
			h.press("ctrl+o") // app.tools.expand
			var tools []*transcript.Row
			for _, r := range h.m.tr.Rows() {
				if r.Kind == transcript.RowTool {
					tools = append(tools, r)
				}
			}
			if render != renderAltscreen {
				if len(h.m.tr.Rows()) != 0 {
					t.Fatalf("inline commits every rested turn's rows: %+v", h.m.tr.Rows())
				}
				return
			}
			if len(tools) != 2 {
				t.Fatalf("altscreen keeps both turns' tool rows, got %d", len(tools))
			}
			if !tools[1].Expanded || tools[0].Expanded {
				t.Fatalf("ctrl+o expands the newest tool row: %v %v", tools[0].Expanded, tools[1].Expanded)
			}
		})
	}
}

func TestSlashRunsACommand(t *testing.T) {
	h := newAppHarness(t, scripted{text("done")})
	h.typeText("/notice hello there")
	h.press("enter")
	h.waitFor("the command's notice", func(v string) bool { return strings.Contains(v, "noticed: hello there") })
	if h.editor().Text() != "" {
		t.Errorf("the editor is cleared by a command too, holds %q", h.editor().Text())
	}
	if h.m.turn.turnID != "" {
		t.Errorf("a command that produced no turn starts none: %q", h.m.turn.turnID)
	}
	h.typeText("/ask about the fork test")
	h.press("enter")
	h.waitTurn(stateCompleted)
	h.waitPrinted("› about the fork test")
}

func TestUnknownCommandNotices(t *testing.T) {
	h := newAppHarness(t, scripted{text("done")})
	h.typeText("/nope")
	h.press("enter")
	// The server's own words, with neither the method nor the JSON-RPC code in front of
	// them: what came back is the command the user typed.
	h.waitFor("the refusal", func(v string) bool { return strings.Contains(v, "unknown command /nope") })
	if strings.Contains(h.view(), "command.run") {
		t.Errorf("a command's refusal names the command, not the method:\n%s", h.view())
	}
	if h.m.turn.state != "" {
		t.Errorf("no turn started: %q", h.m.turn.state)
	}
}

func TestSubmitWhileDisconnectedIsRefused(t *testing.T) {
	h := newAppHarness(t, scripted{text("done")})
	h.dispatch(DisconnectedMsg{})
	h.typeText("fix the flaky fork test")
	h.press("enter")
	if got := h.editor().Text(); got != "fix the flaky fork test" {
		t.Fatalf("a refused submit keeps the draft, editor holds %q", got)
	}
	if !strings.Contains(h.view(), "not connected") {
		t.Errorf("a refused submit says so:\n%s", h.view())
	}
}

// findDecision is the permission_decision the log recorded for tool.
func findDecision(t *testing.T, entries []session.Entry, tool string) session.PermissionDecision {
	t.Helper()
	for _, e := range entries {
		if d, ok := e.Payload.(session.PermissionDecision); ok && d.Tool == tool {
			return d
		}
	}
	t.Fatalf("no permission_decision for %s in %d entries", tool, len(entries))
	return session.PermissionDecision{}
}

// TestATurnRestingDuringAReplayDoesNotPrint is the guard commitTurns already keeps, on the
// other path a commit can be reached by. A turn.state buffered while a session switch is
// in flight is folded during the replay, and the one ordered print at the end of that
// replay is what puts those rows in scrollback; a second print from turnChanged would be
// batched against it, and a tea.Batch does not order its commands.
func TestATurnRestingDuringAReplayDoesNotPrint(t *testing.T) {
	h := newHarness(t, nil) // inline is the default
	h.fold(session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("first question")}})
	h.fold(session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopEndTurn,
		Content: []session.Block{session.TextBlock("first answer")},
	})
	turn := h.m.tr.Turn()
	rested := protocol.TurnStateChanged{SessionID: h.m.session.SessionID, TurnID: turn, State: stateCompleted}

	h.m.replaying = true
	for _, msg := range runAll(t, h.m.turnChanged(rested)) {
		if line, ok := printLine(msg); ok {
			t.Errorf("a turn resting inside a replay must not print: %q", line)
		}
	}
	if rows := h.m.tr.Rows(); len(rows) != 2 {
		t.Fatalf("the rows are left for the replay's own print: %+v", rows)
	}

	// Off the replay the same notification commits, which is what the guard must not have
	// broken.
	h.m.replaying = false
	var printed string
	for _, msg := range runAll(t, h.m.turnChanged(rested)) {
		if line, ok := printLine(msg); ok {
			printed = ansi.Strip(line)
		}
	}
	if !strings.Contains(printed, "first answer") {
		t.Errorf("a turn resting outside a replay commits, printed %q", printed)
	}
}
