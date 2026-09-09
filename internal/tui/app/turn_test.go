package app

import (
	"strings"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/config"
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
	h.waitFor("the refusal", func(v string) bool { return strings.Contains(v, "command.run") })
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
