package anthropicmsgs

import (
	"encoding/json"
	"testing"

	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/turn"
)

// openWireTestSession is a real session log to assemble from: the shortest way to test the
// whole seam is to write entries the way a turn writes them and read the bytes that leave.
func openWireTestSession(t *testing.T) *session.Session {
	t.Helper()
	st, err := session.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := session.Open(st, session.SessionOpened{
		SchemaVersion: 1,
		RudyVersion:   "test",
		Workspace:     session.Workspace{Root: t.TempDir(), ProjectID: "local/test"},
		Model:         session.ModelRef{Provider: "anth", Model: "claude-sonnet-5"},
		Thinking:      session.ThinkingOff,
		Mode:          session.ModeOff,
		Agent:         "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func appendTo(t *testing.T, s *session.Session, p session.Payload) {
	t.Helper()
	if _, err := s.Append(p); err != nil {
		t.Fatalf("append %T: %v", p, err)
	}
}

// TestASessionsParallelCallsReachTheWireAsOneTurn runs the whole seam: entries in a session
// log, through turn.Assemble, to the bytes that would leave for the Messages API. Each half is
// already covered on its own, turn's TestAssemblePutsOneTurnsResultsInOneMessage for the
// assembler and TestParallelToolResultsAreOneUserMessage for the codec, and that was the gap:
// a change to messagesOf with a matching change here would satisfy both and still put the wrong
// shape on the wire. Only a test that starts at the log and ends at the body sees that, and
// this defect class is one where the wire is the only place the mistake is visible at all.
func TestASessionsParallelCallsReachTheWireAsOneTurn(t *testing.T) {
	s := openWireTestSession(t)
	appendTo(t, s, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("read both")}})
	appendTo(t, s, session.AssistantMessage{
		Model: s.Model(), Thinking: s.Thinking(),
		Content: []session.Block{
			session.TextBlock("on it"),
			session.ToolUseBlock("toolu_a", "read", json.RawMessage(`{"path":"a"}`)),
			session.ToolUseBlock("toolu_b", "read", json.RawMessage(`{"path":"b"}`)),
		},
		StopReason: session.StopToolUse, StopReasonRaw: "tool_use",
	})
	for _, id := range []string{"toolu_a", "toolu_b"} {
		appendTo(t, s, session.PermissionDecision{
			ToolUseID: id, Tool: "read", Mode: session.ModeOff, Matcher: session.Matcher{Tool: "read"},
			Decision: session.Allow, DecidedBy: session.ByMode, Scope: session.ScopeOnce, Reason: "mode off",
		})
	}
	// toolu_b finished first, which is what concurrent calls do. The log keeps that order and
	// the pairing is by id, so the wire must carry both in one turn regardless.
	appendTo(t, s, session.ToolResult{ToolUseID: "toolu_b", Outcome: session.OutcomeError, Content: []session.Block{session.TextBlock("no b")}})
	appendTo(t, s, session.ToolResult{ToolUseID: "toolu_a", Outcome: session.OutcomeOK, Content: []session.Block{session.TextBlock("a body")}})

	w := buildWire(t, turn.Assemble(s, nil, "SYSTEM", 100, nil))

	if len(w.Messages) != 3 {
		t.Fatalf("messages = %d, want the user turn, the assistant turn and one turn answering both calls: %+v", len(w.Messages), w.Messages)
	}
	calls := w.Messages[1]
	if calls.Role != "assistant" || len(calls.Content) != 3 {
		t.Fatalf("assistant turn %+v, want its text and both tool_use blocks", calls)
	}
	for i, id := range []string{"toolu_a", "toolu_b"} {
		if b := calls.Content[i+1]; b.Type != "tool_use" || b.ID != id {
			t.Fatalf("assistant block %d = %+v, want tool_use %s", i+1, b, id)
		}
	}
	results := w.Messages[2]
	if results.Role != "user" {
		t.Fatalf("the results turn is a %q turn, want user", results.Role)
	}
	if len(results.Content) != 2 {
		t.Fatalf("the results turn carries %d blocks, want one tool_result per call: %+v", len(results.Content), results.Content)
	}
	for i, want := range []struct {
		id      string
		isError bool
		text    string
	}{{"toolu_b", true, "no b"}, {"toolu_a", false, "a body"}} {
		b := results.Content[i]
		if b.Type != "tool_result" || b.ToolUseID != want.id || b.IsError != want.isError {
			t.Fatalf("result block %d = %+v, want tool_result %s is_error=%v", i, b, want.id, want.isError)
		}
		if !hasText(b.Content, want.text) {
			t.Fatalf("result block %d carries %+v, want the text %q", i, b.Content, want.text)
		}
	}
}
