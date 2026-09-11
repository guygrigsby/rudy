package app

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tui/transcript"
)

// childOpened is one session_opened for a subagent, naming the parent call it answers.
func childOpened(t *testing.T, parentSessionID, parentToolUseID string) session.Entry {
	t.Helper()
	return entry(t, session.SessionOpened{
		SchemaVersion: 1, RudyVersion: "test",
		Workspace: testWorkspace, Model: testRef, Mode: session.ModeStrict,
		Thinking: session.ThinkingHigh, Agent: "explorer",
		ParentSessionID: parentSessionID, ParentToolUseID: parentToolUseID,
	})
}

// interruptSessionIDs is the session_id every session.interrupt call the client made
// named, in order.
func interruptSessionIDs(t *testing.T, h *harness) []string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.reqs {
		if r.Method != protocol.MethodSessionInterrupt {
			continue
		}
		var p protocol.SessionInterruptParams
		if err := json.Unmarshal(r.Params, &p); err != nil {
			t.Fatalf("session.interrupt params: %v", err)
		}
		out = append(out, p.SessionID)
	}
	return out
}

// answerRequests is every session.answer call the client made, decoded, in order.
func answerRequests(t *testing.T, h *harness) []protocol.SessionAnswerParams {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []protocol.SessionAnswerParams
	for _, r := range h.reqs {
		if r.Method != protocol.MethodSessionAnswer {
			continue
		}
		var p protocol.SessionAnswerParams
		if err := json.Unmarshal(r.Params, &p); err != nil {
			t.Fatalf("session.answer params: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// TestAChildsPermissionPromptRendersAndAnswers is rudy-ssz end to end: a subagent has no
// asker of its own, so its permission.requested reaches this client borrowing the parent's,
// tagged with the child's session id. The drop-gate must let it through once the mapping is
// known, it must render under the agent call that opened the child, and answering it must
// name the session the question actually came from, never the parent's own.
func TestAChildsPermissionPromptRendersAndAnswers(t *testing.T) {
	h := newHarness(t, nil)
	sid := h.m.session.SessionID
	child := session.NewID().String()

	h.appended(session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopToolUse,
		Content: []session.Block{session.ToolUseBlock("t1", "agent", json.RawMessage(`{"agent":"explorer","prompt":"look"}`))},
	})
	h.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: child, Entry: childOpened(t, sid, "t1")})
	h.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: child, Entry: entry(t, session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopToolUse,
		Content: []session.Block{session.ToolUseBlock("tu_bash", "bash", json.RawMessage(`{"command":"rm -rf /tmp/x"}`))},
	})})

	// tool.state(awaiting_permission) always precedes permission.requested for the same
	// call (contracts); this is the moment rudy-ssz's visibility half covers.
	h.notify(protocol.NotifyToolState, protocol.ToolStateChanged{
		SessionID: child, ToolUseID: "tu_bash", Name: "bash", State: protocol.ToolStateAwaitingPermission,
	})
	h.notify(protocol.NotifyPermissionRequested, protocol.PermissionRequested{
		SessionID: child, ToolUseID: "tu_bash", Tool: "bash",
		Input: json.RawMessage(`{"command":"rm -rf /tmp/x"}`),
	})

	if h.m.focused() == nil || h.m.focused().SessionID != child {
		t.Fatalf("the child's prompt did not become the standing question: %+v", h.m.focused())
	}
	var promptRow *transcript.Row
	for _, r := range h.m.tr.Rows() {
		if r.Kind == transcript.RowPrompt {
			promptRow = r
		}
	}
	if promptRow == nil {
		t.Fatal("no prompt row rendered for the child's question")
	}
	if promptRow.SessionID != child || promptRow.ParentToolUseID != "t1" {
		t.Fatalf("prompt row origin = %+v, want %s/t1", promptRow, child)
	}

	// y: allow once. The standing question's own key handling runs before anything mode
	// dependent, so no insert-to-normal press is needed first.
	runCmd(t, h.press("y"))

	got := answerRequests(t, h)
	if len(got) != 1 {
		t.Fatalf("session.answer calls = %d, want 1: %v", len(got), got)
	}
	if got[0].SessionID != child {
		t.Fatalf("session.answer named %q, want the child %q, never the parent", got[0].SessionID, child)
	}
	if got[0].ToolUseID != "tu_bash" {
		t.Fatalf("session.answer tool_use = %q, want tu_bash", got[0].ToolUseID)
	}
	if got[0].Decision != session.Allow || got[0].Scope != session.ScopeOnce {
		t.Fatalf("session.answer decision = %+v, want allow once", got[0])
	}

	// The decision this client's own answer produced lands on the child's log and is
	// forwarded the ordinary way; the standing question and its row, already taken down
	// the moment the key was pressed, must stay down.
	h.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: child, Entry: entry(t, session.PermissionDecision{
		ToolUseID: "tu_bash", Tool: "bash", Mode: session.ModeStrict,
		Matcher: session.Matcher{Tool: "bash", Prefix: "rm -rf"}, Decision: session.Allow,
		DecidedBy: session.ByAsker, Scope: session.ScopeOnce, Reason: answerReason,
	})})
	if h.m.focused() != nil {
		t.Fatalf("the standing question survived its own decision: %+v", h.m.focused())
	}
	for _, r := range h.m.tr.Rows() {
		if r.Kind == transcript.RowPrompt {
			t.Fatalf("the prompt row survived its own decision: %+v", r)
		}
	}
}

// TestADecisionFromElsewhereTakesDownAChildsPrompt is the other way a child's question can
// be settled: a hook, the Gate, or another asker decides it before this client ever presses
// a key. childNotification's own handling of a subagent's permission_decision entry, not
// the answer-time cleanup TestAChildsPermissionPromptRendersAndAnswers already exercises
// (m.answer takes its own question down immediately, before any entry confirms it), is what
// this isolates: no key is pressed here at all.
func TestADecisionFromElsewhereTakesDownAChildsPrompt(t *testing.T) {
	h := newHarness(t, nil)
	sid := h.m.session.SessionID
	child := session.NewID().String()

	h.appended(session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopToolUse,
		Content: []session.Block{session.ToolUseBlock("t1", "agent", json.RawMessage(`{"agent":"explorer","prompt":"look"}`))},
	})
	h.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: child, Entry: childOpened(t, sid, "t1")})
	h.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: child, Entry: entry(t, session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopToolUse,
		Content: []session.Block{session.ToolUseBlock("tu_bash", "bash", json.RawMessage(`{"command":"rm -rf /tmp/x"}`))},
	})})
	h.notify(protocol.NotifyPermissionRequested, protocol.PermissionRequested{
		SessionID: child, ToolUseID: "tu_bash", Tool: "bash",
		Input: json.RawMessage(`{"command":"rm -rf /tmp/x"}`),
	})
	if h.m.focused() == nil {
		t.Fatal("setup: the child's prompt never became the standing question")
	}

	h.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: child, Entry: entry(t, session.PermissionDecision{
		ToolUseID: "tu_bash", Tool: "bash", Mode: session.ModeStrict,
		Matcher: session.Matcher{Tool: "bash", Prefix: "rm -rf"}, Decision: session.Deny,
		DecidedBy: session.ByHook, Scope: session.ScopeOnce, Reason: "before_tool hook",
	})})

	if h.m.focused() != nil {
		t.Fatalf("a decision from elsewhere left the standing question up: %+v", h.m.focused())
	}
	for _, r := range h.m.tr.Rows() {
		if r.Kind == transcript.RowPrompt {
			t.Fatalf("a decision from elsewhere left the prompt row up: %+v", r)
		}
	}
}

// TestAChildsTurnStateDoesNotBecomeTheParents is ADR 0028 decision 6 on the client side:
// the parent's turn is not the child's turn, so a subagent coming to rest must not rest
// the parent's spinner or offer the composer while the parent is still mid-turn.
func TestAChildsTurnStateDoesNotBecomeTheParents(t *testing.T) {
	h := newHarness(t, nil)
	sid := h.m.session.SessionID
	child := session.NewID().String()
	turn := session.NewID().String()

	h.appended(session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopToolUse,
		Content: []session.Block{session.ToolUseBlock("t1", "agent", json.RawMessage(`{"agent":"explorer","prompt":"look"}`))},
	})
	h.notify(protocol.NotifyTurnState, protocol.TurnStateChanged{SessionID: sid, TurnID: turn, State: stateRunningTool})
	if h.m.turn.state != stateRunningTool {
		t.Fatalf("setup: parent turn state = %q, want %q", h.m.turn.state, stateRunningTool)
	}

	// The child's own session_opened names the call it answers, which is what lets its
	// later notifications through the session-scope gate at all.
	h.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: child, Entry: childOpened(t, sid, "t1")})

	// The child comes to rest. If this reached m.turn, the parent's spinner would stop
	// and the composer would offer to accept input while the parent is still running.
	h.notify(protocol.NotifyTurnState, protocol.TurnStateChanged{SessionID: child, TurnID: turn, State: stateCompleted})
	if h.m.turn.state != stateRunningTool {
		t.Fatalf("a child's turn.state reached the parent's own: %q", h.m.turn.state)
	}
	if !h.m.turn.running() {
		t.Fatal("the parent's turn must still read as running")
	}

	// tool.state passes the same session-scope gate and is just as inert on the parent's
	// own mirror.
	h.notify(protocol.NotifyToolState, protocol.ToolStateChanged{
		SessionID: child, TurnID: turn, ToolUseID: "t2", Name: "bash", State: protocol.ToolStateRunning,
	})
	if h.m.turn.state != stateRunningTool {
		t.Fatalf("a child's tool.state reached the parent's own: %q", h.m.turn.state)
	}
}

// TestInterruptTargetsTheParentWhileAChildIsOnScreen is ADR 0028 decision 6 the other way:
// task 6 refuses session.interrupt naming a child from a connection not subscribed to it
// directly, which the TUI never is (it only watches a child through the parent's forwarded
// notifications). Esc must interrupt the session this client is actually attached to, the
// parent, whatever a subagent is showing on screen.
func TestInterruptTargetsTheParentWhileAChildIsOnScreen(t *testing.T) {
	h := newHarness(t, nil)
	sid := h.m.session.SessionID
	child := session.NewID().String()
	turn := session.NewID().String()

	h.appended(session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopToolUse,
		Content: []session.Block{session.ToolUseBlock("t1", "agent", json.RawMessage(`{"agent":"explorer","prompt":"look"}`))},
	})
	h.notify(protocol.NotifyTurnState, protocol.TurnStateChanged{SessionID: sid, TurnID: turn, State: stateRunningTool})
	h.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: child, Entry: childOpened(t, sid, "t1")})
	h.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: child, Entry: entry(t, session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopEndTurn,
		Content: []session.Block{session.TextBlock("looked")},
	})})

	h.press("escape") // insert to normal, consumed by the editor
	// normal, turn running: interrupt. runCmd runs the call itself: the raw pipe harness,
	// unlike a real Program's own loop, never runs a returned command for you.
	runCmd(t, h.press("escape"))

	got := interruptSessionIDs(t, h)
	if len(got) != 1 || got[0] != sid {
		t.Fatalf("session.interrupt targeted %v, want [%s]: the parent, never the child on screen", got, sid)
	}
}

// TestTheKeyboardAnswersTheQuestionNearestTheInput is rudy-omc, in the shape one assistant
// message reaches: an agent call and an unsafe call of the parent's own. The parent's question
// is drawn in front of its own tool row, at the bottom; the child's arrives afterwards and is
// inserted under the agent call, ABOVE it. Answering the newest question would send the answer
// the operator typed while reading "rm -rf build" to the child's call instead, which is consent
// for arguments they never saw.
func TestTheKeyboardAnswersTheQuestionNearestTheInput(t *testing.T) {
	h := newHarness(t, nil)
	sid := h.m.session.SessionID
	child := session.NewID().String()
	turn := session.NewID().String()

	h.appended(session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopToolUse,
		Content: []session.Block{
			session.ToolUseBlock("t_agent", "agent", json.RawMessage(`{"agent":"explorer","prompt":"look"}`)),
			session.ToolUseBlock("t_bash", "bash", json.RawMessage(`{"command":"rm -rf build"}`)),
		},
	})
	h.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: child, Entry: childOpened(t, sid, "t_agent")})

	// The parent's own question first, then the child's: arrival order and row order disagree
	// from here on.
	h.notify(protocol.NotifyPermissionRequested, protocol.PermissionRequested{
		SessionID: sid, TurnID: turn, ToolUseID: "t_bash", Tool: "bash",
		Input: json.RawMessage(`{"command":"rm -rf build"}`), Matcher: session.Matcher{Tool: "bash", Prefix: "rm -rf"},
	})
	h.notify(protocol.NotifyPermissionRequested, protocol.PermissionRequested{
		SessionID: child, TurnID: turn, ToolUseID: "tu_child", Tool: "bash",
		Input: json.RawMessage(`{"command":"ls"}`), Matcher: session.Matcher{Tool: "bash", Prefix: "ls"},
	})

	if got := promptRows(h); got != 2 {
		t.Fatalf("setup: %d question rows on screen, want 2:\n%s", got, h.view())
	}
	var order []string
	for _, r := range h.m.tr.Rows() {
		if r.Kind == transcript.RowPrompt {
			order = append(order, r.Prompt.ToolUseID)
		}
	}
	if !slices.Equal(order, []string{"tu_child", "t_bash"}) {
		t.Fatalf("setup: prompt rows top to bottom = %v, want the child's above the parent's", order)
	}
	if focused := h.m.focused(); focused == nil || focused.ToolUseID != "t_bash" || focused.SessionID != sid {
		t.Fatalf("focused = %+v, want the parent's t_bash, the row nearest the input", focused)
	}

	// The bottom row draws the keys, and it is the only one that does: what the operator reads
	// as answerable is what the keyboard answers.
	screen := ansi.Strip(h.view())
	if got := strings.Count(screen, "allow once [y]"); got != 1 {
		t.Fatalf("%d rows drew the keys, want exactly the focused one:\n%s", got, h.view())
	}
	keys := strings.Index(screen, "allow once [y]")
	if rm := strings.Index(screen, "rm -rf build"); rm < 0 || rm > keys {
		t.Fatalf("the keys are not under the command they answer for:\n%s", h.view())
	}

	runCmd(t, h.press("y"))
	got := answerRequests(t, h)
	if len(got) != 1 {
		t.Fatalf("session.answer calls = %d, want 1: %+v", len(got), got)
	}
	if got[0].SessionID != sid || got[0].ToolUseID != "t_bash" {
		t.Fatalf("y answered %s/%s, want the parent's own t_bash: the row the operator was reading", got[0].SessionID, got[0].ToolUseID)
	}
}
