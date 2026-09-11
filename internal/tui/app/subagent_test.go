package app

import (
	"encoding/json"
	"testing"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/session"
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
