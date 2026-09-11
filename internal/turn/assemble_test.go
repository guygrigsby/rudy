package turn

import (
	"encoding/json"
	"testing"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

func openTestSession(t *testing.T, mode session.Mode) *session.Session {
	t.Helper()
	st, err := session.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := session.Open(st, session.SessionOpened{
		SchemaVersion: 1,
		RudyVersion:   "test",
		Workspace:     session.Workspace{Root: t.TempDir(), ProjectID: "local/test"},
		Model:         session.ModelRef{Provider: "fake", Model: "m1"},
		Thinking:      session.ThinkingHigh,
		Mode:          mode,
		Agent:         "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAssembleMapsEntriesToMessages(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	mustAppend(t, s, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hi")}})
	mustAppend(t, s, session.AssistantMessage{
		Model: s.Model(), Thinking: s.Thinking(),
		Content:    []session.Block{session.TextBlock("reading"), session.ToolUseBlock("tu1", "read", json.RawMessage(`{"path":"go.mod"}`))},
		StopReason: session.StopToolUse, StopReasonRaw: "tool_calls",
	})
	mustAppend(t, s, session.PermissionDecision{ToolUseID: "tu1", Tool: "read", Mode: session.ModeStrict, Matcher: session.Matcher{Tool: "read"}, Decision: session.Allow, DecidedBy: session.ByClass, Scope: session.ScopeOnce, Reason: "safe"})
	mustAppend(t, s, session.ToolResult{ToolUseID: "tu1", Outcome: session.OutcomeOK, Content: []session.Block{session.TextBlock("module x")}, DurationMS: 3})
	mustAppend(t, s, session.ModeChange{Mode: session.ModeOff})

	tools := []tool.Tool{{Name: "read", Description: "read a file", Schema: json.RawMessage(`{"type":"object"}`), Safety: tool.Safe}}
	req := Assemble(s, tools, "SYSTEM", 4096, nil)

	if req.System != "SYSTEM" || req.MaxTokens != 4096 || req.Model != s.Model() || req.Thinking != session.ThinkingHigh || req.SessionID != s.ID() {
		t.Fatalf("header fields %+v", req)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "read" || string(req.Tools[0].Schema) != `{"type":"object"}` {
		t.Fatalf("tools %+v", req.Tools)
	}
	wantRoles := []provider.Role{provider.RoleUser, provider.RoleAssistant, provider.RoleToolResult}
	if len(req.Messages) != len(wantRoles) {
		t.Fatalf("messages %+v", req.Messages)
	}
	for i, r := range wantRoles {
		if req.Messages[i].Role != r {
			t.Fatalf("message %d role %q want %q", i, req.Messages[i].Role, r)
		}
	}
	res := req.Messages[2].Results
	if len(res) != 1 || res[0].ToolUseID != "tu1" || res[0].Content[0].Text != "module x" || res[0].IsError {
		t.Fatalf("tool result message %+v", req.Messages[2])
	}
	if string(req.Messages[1].Content[1].Input) != `{"path":"go.mod"}` {
		t.Fatalf("tool_use input must be verbatim, got %s", req.Messages[1].Content[1].Input)
	}
}

// TestAssemblePutsOneTurnsResultsInOneMessage is the shape parallel tool use is specified in.
// One message per result is accepted on the wire (the Messages API combines consecutive
// same-role turns, and nothing separates these: the permission decisions in between are
// skipped and a steer's user_message is appended by the next Run), so no error would ever
// appear. What would happen instead is that the model, shown its parallel calls answered one
// turn at a time, stops making them, and the fan-out this wave exists for goes quiet with
// nothing to debug. See the Turn.RunTool row in the contracts.
func TestAssemblePutsOneTurnsResultsInOneMessage(t *testing.T) {
	s := openTestSession(t, session.ModeOff)
	mustAppend(t, s, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("go")}})
	mustAppend(t, s, session.AssistantMessage{
		Model: s.Model(), Thinking: s.Thinking(),
		Content: []session.Block{
			session.ToolUseBlock("tu1", "read", json.RawMessage(`{"path":"a"}`)),
			session.ToolUseBlock("tu2", "read", json.RawMessage(`{"path":"b"}`)),
		},
		StopReason: session.StopToolUse, StopReasonRaw: "tool_calls",
	})
	for _, id := range []string{"tu1", "tu2"} {
		mustAppend(t, s, session.PermissionDecision{ToolUseID: id, Tool: "read", Mode: session.ModeOff, Matcher: session.Matcher{Tool: "read"}, Decision: session.Allow, DecidedBy: session.ByMode, Scope: session.ScopeOnce, Reason: "mode off"})
	}
	// Out of tool_use order on purpose: calls run at once, so the log records whichever
	// finished first, and the pairing is by id.
	mustAppend(t, s, session.ToolResult{ToolUseID: "tu2", Outcome: session.OutcomeError, Content: []session.Block{session.TextBlock("no b")}})
	mustAppend(t, s, session.ToolResult{ToolUseID: "tu1", Outcome: session.OutcomeOK, Content: []session.Block{session.TextBlock("a body")}})

	req := Assemble(s, nil, "S", 100, nil)
	wantRoles := []provider.Role{provider.RoleUser, provider.RoleAssistant, provider.RoleToolResult}
	if len(req.Messages) != len(wantRoles) {
		t.Fatalf("two results must be one message, got %d messages: %+v", len(req.Messages), req.Messages)
	}
	for i, r := range wantRoles {
		if req.Messages[i].Role != r {
			t.Fatalf("message %d role %q want %q", i, req.Messages[i].Role, r)
		}
	}
	res := req.Messages[2].Results
	if len(res) != 2 {
		t.Fatalf("results = %d, want both in the one message: %+v", len(res), res)
	}
	if res[0].ToolUseID != "tu2" || !res[0].IsError || res[0].Content[0].Text != "no b" {
		t.Fatalf("first result %+v", res[0])
	}
	if res[1].ToolUseID != "tu1" || res[1].IsError || res[1].Content[0].Text != "a body" {
		t.Fatalf("second result %+v", res[1])
	}
}

// TestAssembleSplitsResultsOfDifferentTurns is the other half: a run ends where the next
// assistant message begins, so two turns never merge into one reply.
func TestAssembleSplitsResultsOfDifferentTurns(t *testing.T) {
	s := openTestSession(t, session.ModeOff)
	mustAppend(t, s, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("go")}})
	for _, id := range []string{"tu1", "tu2"} {
		mustAppend(t, s, session.AssistantMessage{
			Model: s.Model(), Thinking: s.Thinking(),
			Content:    []session.Block{session.ToolUseBlock(id, "read", json.RawMessage(`{}`))},
			StopReason: session.StopToolUse, StopReasonRaw: "tool_calls",
		})
		mustAppend(t, s, session.PermissionDecision{ToolUseID: id, Tool: "read", Mode: session.ModeOff, Matcher: session.Matcher{Tool: "read"}, Decision: session.Allow, DecidedBy: session.ByMode, Scope: session.ScopeOnce, Reason: "mode off"})
		mustAppend(t, s, session.ToolResult{ToolUseID: id, Outcome: session.OutcomeOK, Content: []session.Block{session.TextBlock(id)}})
	}
	req := Assemble(s, nil, "S", 100, nil)
	want := []provider.Role{provider.RoleUser, provider.RoleAssistant, provider.RoleToolResult, provider.RoleAssistant, provider.RoleToolResult}
	if len(req.Messages) != len(want) {
		t.Fatalf("messages %+v, want %v", req.Messages, want)
	}
	for i, r := range want {
		if req.Messages[i].Role != r {
			t.Fatalf("message %d role %q want %q", i, req.Messages[i].Role, r)
		}
	}
	for _, i := range []int{2, 4} {
		if len(req.Messages[i].Results) != 1 {
			t.Fatalf("message %d carries %d results, want its turn's one", i, len(req.Messages[i].Results))
		}
	}
}

func TestAssembleStartsFromCompaction(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	first := mustAppend(t, s, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("old")}})
	last := mustAppend(t, s, session.AssistantMessage{Model: s.Model(), Thinking: s.Thinking(), Content: []session.Block{session.TextBlock("older reply")}, StopReason: session.StopEndTurn, StopReasonRaw: "stop"})
	mustAppend(t, s, session.Compaction{Summary: "we discussed old things", FirstEntryID: first.ID, LastEntryID: last.ID, Model: s.Model()})
	mustAppend(t, s, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("new")}})

	req := Assemble(s, nil, "S", 100, nil)
	if len(req.Messages) != 2 {
		t.Fatalf("want summary then new message, got %+v", req.Messages)
	}
	if req.Messages[0].Role != provider.RoleUser || req.Messages[0].Content[0].Text != "Summary of the conversation so far:\nwe discussed old things" {
		t.Fatalf("summary message %+v", req.Messages[0])
	}
	if req.Messages[1].Content[0].Text != "new" {
		t.Fatalf("second message %+v", req.Messages[1])
	}
}

func mustAppend(t *testing.T, s *session.Session, p session.Payload) session.Entry {
	t.Helper()
	e, err := s.Append(p)
	if err != nil {
		t.Fatalf("append %T: %v", p, err)
	}
	return e
}
