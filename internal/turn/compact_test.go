package turn

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

// textWithUsage is a scripted step that answers with text and reports what the request cost,
// which is what the runner's compaction threshold reads.
func textWithUsage(s string, u session.Usage) []provider.Part {
	return []provider.Part{text(s), {Type: provider.PartUsage, Usage: u}, stop(session.StopEndTurn, "stop")}
}

// appendPair appends one user message and its reply, labelled n, and returns both.
func appendPair(t *testing.T, s *session.Session, n string) (session.Entry, session.Entry) {
	t.Helper()
	u := mustAppend(t, s, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("q" + n)}})
	a := mustAppend(t, s, session.AssistantMessage{
		Model: s.Model(), Thinking: s.Thinking(),
		Content:    []session.Block{session.TextBlock("a" + n)},
		Usage:      session.Usage{Input: 10, Output: 2},
		StopReason: session.StopEndTurn, StopReasonRaw: "stop",
	})
	return u, a
}

// seedConversation appends n user/assistant pairs directly, without running a turn.
func seedConversation(t *testing.T, s *session.Session, n int) {
	t.Helper()
	for i := range n {
		appendPair(t, s, string(rune('1'+i)))
	}
}

// recordingCompactor stands in for the real Compactor and records the before it was given.
type recordingCompactor struct {
	calls []ulid.ULID
}

func (c *recordingCompactor) Compact(_ context.Context, _ *session.Session, before ulid.ULID, _ string) (session.Entry, error) {
	c.calls = append(c.calls, before)
	return session.Entry{}, nil
}

func TestModelCompactorAsksHookFirst(t *testing.T) {
	s, prov, _ := newTurnFixture(t, []provider.Part{text("MODEL SUMMARY")})
	seedConversation(t, s, 3) // three user/assistant pairs
	hooks := &recordingHooks{results: map[plugin.HookPoint][]any{plugin.HookBeforeCompaction: {&plugin.BeforeCompactionResult{Summary: "HOOK SUMMARY"}}}}
	c := &ModelCompactor{Provider: prov, Model: provider.Model{Ref: s.Model(), ContextWindow: 1000}, Hooks: hooks, MaxTokens: 100}
	e, err := c.Compact(context.Background(), s, ulid.ULID{}, "")
	if err != nil {
		t.Fatal(err)
	}
	cp := e.Payload.(session.Compaction)
	if cp.Summary != "HOOK SUMMARY" || len(prov.requests) != 0 {
		t.Errorf("compaction %+v, provider requests %d", cp, len(prov.requests))
	}
	all := s.Entries()
	if cp.FirstEntryID != all[1].ID || cp.LastEntryID != all[6].ID {
		t.Errorf("range %s..%s", cp.FirstEntryID, cp.LastEntryID)
	}
	bc, ok := hooks.calls[0].Payload.(*plugin.BeforeCompactionPayload)
	if !ok || bc.PromptTokens != 10 || bc.ContextWindow != 1000 || bc.FirstEntryID != all[1].ID.String() {
		t.Errorf("before_compaction payload %+v", hooks.calls[0].Payload)
	}
	if got := s.RequestContext(); len(got) != 1 {
		t.Errorf("request context after compaction: %d", len(got))
	}
}

func TestModelCompactorUsesTheModelWithInstructions(t *testing.T) {
	s, prov, _ := newTurnFixture(t, textWithUsage("MODEL SUMMARY", session.Usage{Input: 40, Output: 9}))
	seedConversation(t, s, 2)
	hooks := &recordingHooks{results: map[plugin.HookPoint][]any{plugin.HookBeforeCompaction: {&plugin.BeforeCompactionResult{Summary: "HOOK"}}}}
	c := &ModelCompactor{Provider: prov, Model: provider.Model{Ref: s.Model()}, Hooks: hooks, MaxTokens: 100}
	e, err := c.Compact(context.Background(), s, ulid.ULID{}, "focus on decisions")
	if err != nil {
		t.Fatal(err)
	}
	cp := e.Payload.(session.Compaction)
	if cp.Summary != "MODEL SUMMARY" || len(hooks.calls) != 0 {
		t.Errorf("instructions must skip the hook: %+v, hook calls %d", cp, len(hooks.calls))
	}
	req := prov.requests[0]
	last := req.Messages[len(req.Messages)-1].Content[0].Text
	if !strings.Contains(last, "focus on decisions") || len(req.Tools) != 0 {
		t.Errorf("summary request %+v", req)
	}
	if len(req.Messages) != 5 || req.Messages[0].Content[0].Text != "q1" {
		t.Errorf("the covered conversation must be the request: %+v", req.Messages)
	}
	if cp.Usage.Output == 0 || cp.Model != s.Model() {
		t.Errorf("usage and model recorded: %+v", cp)
	}
}

func TestModelCompactorRefusesTooLittle(t *testing.T) {
	s, prov, _ := newTurnFixture(t, []provider.Part{text("x")})
	u := mustAppend(t, s, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("only")}})
	c := &ModelCompactor{Provider: prov, Model: provider.Model{Ref: s.Model()}, MaxTokens: 10}
	e, err := c.Compact(context.Background(), s, ulid.ULID{}, "")
	if err != nil || !e.ID.IsZero() {
		t.Errorf("got %+v %v", e, err)
	}
	e, err = c.Compact(context.Background(), s, u.ID, "")
	if err != nil || !e.ID.IsZero() {
		t.Errorf("before the only entry: %+v %v", e, err)
	}
	if len(prov.requests) != 0 {
		t.Errorf("nothing to cover must not reach the provider: %d requests", len(prov.requests))
	}
}

func TestRunnerCompactsWhenPromptTokensCrossTheThreshold(t *testing.T) {
	// Provider reports 900 prompt tokens on a 1000 window with compact_at 0.8. The first
	// assistant message must trigger a compaction covering everything before this turn.
	s, prov, tools := newTurnFixture(t, textWithUsage("answer", session.Usage{Input: 900, Output: 5}))
	seedConversation(t, s, 2)
	fake := &recordingCompactor{}
	r := NewRunner(Config{Session: s, Provider: prov, Model: provider.Model{Ref: s.Model(), ContextWindow: 1000}, Tools: tools, Gate: gate.New(nil), MaxTokens: 10, Compactor: fake, CompactAt: 0.8})
	if err := r.Run(context.Background(), session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("q")}}); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 || fake.calls[0] != r.turn {
		t.Errorf("compactor calls %v, turn %s", fake.calls, r.turn)
	}
}

func TestRunnerLeavesTheThresholdAloneBelowIt(t *testing.T) {
	s, prov, tools := newTurnFixture(t, textWithUsage("answer", session.Usage{Input: 799, Output: 5}))
	seedConversation(t, s, 2)
	fake := &recordingCompactor{}
	r := NewRunner(Config{Session: s, Provider: prov, Model: provider.Model{Ref: s.Model(), ContextWindow: 1000}, Tools: tools, Gate: gate.New(nil), MaxTokens: 10, Compactor: fake, CompactAt: 0.8})
	if err := r.Run(context.Background(), session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("q")}}); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("compacted below the threshold: %v", fake.calls)
	}
}

// TestModelCompactorChainsFromThePreviousCompaction is the log a mid-turn compaction leaves
// behind: c1 covers u1..a1 but is appended after u2 and a2, the turn that was running. The
// next compaction covers c1, u2 and a2, and c1's entry id is younger than every other entry in
// that set, so taking it as first_entry_id builds a range the log refuses. The new compaction
// starts where c1 started instead.
func TestModelCompactorChainsFromThePreviousCompaction(t *testing.T) {
	s, prov, _ := newTurnFixture(t,
		textWithUsage("SUMMARY ONE", session.Usage{Input: 5, Output: 3}),
		textWithUsage("SUMMARY TWO", session.Usage{Input: 6, Output: 4}))
	u1, a1 := appendPair(t, s, "1")
	u2, a2 := appendPair(t, s, "2")
	c := &ModelCompactor{Provider: prov, Model: provider.Model{Ref: s.Model()}, MaxTokens: 100}
	e1, err := c.Compact(context.Background(), s, u2.ID, "one")
	if err != nil {
		t.Fatalf("first compaction: %v", err)
	}
	if cp := e1.Payload.(session.Compaction); cp.FirstEntryID != u1.ID || cp.LastEntryID != a1.ID {
		t.Fatalf("first compaction range %s..%s", cp.FirstEntryID, cp.LastEntryID)
	}
	u3, a3 := appendPair(t, s, "3")
	e2, err := c.Compact(context.Background(), s, u3.ID, "two")
	if err != nil {
		t.Fatalf("second compaction: %v", err)
	}
	cp := e2.Payload.(session.Compaction)
	if cp.FirstEntryID != u1.ID || cp.LastEntryID != a2.ID {
		t.Errorf("second compaction range %s..%s, want %s..%s", cp.FirstEntryID, cp.LastEntryID, u1.ID, a2.ID)
	}
	if cp.Summary != "SUMMARY TWO" {
		t.Errorf("summary %q", cp.Summary)
	}
	var ids []ulid.ULID
	for _, e := range s.RequestContext() {
		ids = append(ids, e.ID)
	}
	if !reflect.DeepEqual(ids, []ulid.ULID{e2.ID, u3.ID, a3.ID}) {
		t.Errorf("request context %v, want %v", ids, []ulid.ULID{e2.ID, u3.ID, a3.ID})
	}
}

// TestModelCompactorUsesTheAfterToolOverrides: what an after_tool handler replaced is what the
// model saw and is what it must summarize. The stored result, which the log keeps verbatim,
// would otherwise be copied into the summary and persisted there.
func TestModelCompactorUsesTheAfterToolOverrides(t *testing.T) {
	s, prov, _ := newTurnFixture(t, textWithUsage("SUMMARY", session.Usage{Input: 5, Output: 3}))
	mustAppend(t, s, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("run it")}})
	mustAppend(t, s, session.AssistantMessage{
		Model: s.Model(), Thinking: s.Thinking(),
		Content:    []session.Block{session.ToolUseBlock("tu1", "echo", json.RawMessage(`{}`))},
		StopReason: session.StopToolUse, StopReasonRaw: "tool_calls",
	})
	mustAppend(t, s, session.PermissionDecision{
		ToolUseID: "tu1", Tool: "echo", Mode: s.Mode(), Matcher: session.Matcher{Tool: "echo"},
		Decision: session.Allow, DecidedBy: session.ByClass, Scope: session.ScopeOnce, Reason: "test",
	})
	mustAppend(t, s, session.ToolResult{ToolUseID: "tu1", Outcome: session.OutcomeOK, Content: []session.Block{session.TextBlock("SECRET")}})
	mustAppend(t, s, session.AssistantMessage{
		Model: s.Model(), Thinking: s.Thinking(),
		Content:    []session.Block{session.TextBlock("done")},
		StopReason: session.StopEndTurn, StopReasonRaw: "stop",
	})
	c := &ModelCompactor{
		Provider: prov, Model: provider.Model{Ref: s.Model()}, MaxTokens: 100,
		Overrides: map[string][]session.Block{"tu1": {session.TextBlock("REDACTED")}},
	}
	if _, err := c.Compact(context.Background(), s, ulid.ULID{}, "go"); err != nil {
		t.Fatal(err)
	}
	var result *provider.ToolResult
	for i, m := range prov.requests[0].Messages {
		for j := range m.Results {
			result = &prov.requests[0].Messages[i].Results[j]
		}
		for _, b := range slices.Concat(m.Content, blocksOf(m.Results)) {
			if strings.Contains(b.Text, "SECRET") {
				t.Fatalf("the stored result reached the summary request: %+v", m)
			}
		}
	}
	if result == nil || result.Content[0].Text != "REDACTED" {
		t.Fatalf("tool result message %+v", result)
	}
}

// blocksOf is every block the results of a message carry, for a test that checks what reached
// the wire without caring which result it came from.
func blocksOf(results []provider.ToolResult) []session.Block {
	var out []session.Block
	for _, r := range results {
		out = append(out, r.Content...)
	}
	return out
}
