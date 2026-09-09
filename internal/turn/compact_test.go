package turn

import (
	"context"
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

// seedConversation appends n user/assistant pairs directly, without running a turn.
func seedConversation(t *testing.T, s *session.Session, n int) {
	t.Helper()
	for i := range n {
		mustAppend(t, s, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("q" + string(rune('1'+i)))}})
		mustAppend(t, s, session.AssistantMessage{
			Model: s.Model(), Thinking: s.Thinking(),
			Content:    []session.Block{session.TextBlock("a" + string(rune('1'+i)))},
			Usage:      session.Usage{Input: 10, Output: 2},
			StopReason: session.StopEndTurn, StopReasonRaw: "stop",
		})
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
