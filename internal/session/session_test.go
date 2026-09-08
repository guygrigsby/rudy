package session

import (
	"encoding/json"
	"errors"
	"testing"
)

func opened() SessionOpened {
	return SessionOpened{
		SchemaVersion: 1, RudyVersion: "test",
		Workspace: Workspace{Root: "/w", ProjectID: "local/w"},
		Model:     ModelRef{Provider: "aperture", Model: "cline-pass/kimi-k3"},
		Thinking:  ThinkingHigh, Mode: ModeStrict, Agent: "default",
	}
}

func newSession(t *testing.T) (*Store, *Session) {
	t.Helper()
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(st, opened())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return st, s
}

func mustAppend(t *testing.T, s *Session, p Payload) Entry {
	t.Helper()
	e, err := s.Append(p)
	if err != nil {
		t.Fatalf("append %s: %v", p.Kind(), err)
	}
	return e
}

func assistantWithTool(id string) AssistantMessage {
	return AssistantMessage{
		Model: ModelRef{"aperture", "cline-pass/kimi-k3"}, Thinking: ThinkingHigh,
		Content:    []Block{ToolUseBlock(id, "bash", json.RawMessage(`{"command":"ls"}`))},
		StopReason: StopToolUse, StopReasonRaw: "tool_calls",
	}
}

func decision(id string, d Decision, scope Scope) PermissionDecision {
	return PermissionDecision{ToolUseID: id, Tool: "bash", Mode: ModeStrict,
		Matcher: Matcher{Tool: "bash", Prefix: "ls"}, Decision: d, DecidedBy: ByAsker, Scope: scope, Reason: "test"}
}

func TestOpenWritesSessionOpened(t *testing.T) {
	_, s := newSession(t)
	es := s.Entries()
	if len(es) != 1 || es[0].Kind != KindSessionOpened {
		t.Fatalf("entries after Open = %+v", es)
	}
	if s.Model().Model != "cline-pass/kimi-k3" || s.Mode() != ModeStrict || s.Thinking() != ThinkingHigh || s.Agent() != "default" || s.Workspace().Root != "/w" {
		t.Fatal("derived getters do not reflect session_opened")
	}
}

func TestAppendInvariants(t *testing.T) {
	_, s := newSession(t)
	cases := []struct {
		name string
		p    Payload
	}{
		{"second session_opened", opened()},
		{"fork_point after first", ForkPoint{ParentSessionID: NewID(), ParentEntryID: NewID()}},
		{"decision for unknown tool_use", decision("nope", Allow, ScopeOnce)},
		{"result for unknown tool_use", ToolResult{ToolUseID: "nope", Outcome: OutcomeOK}},
		{"invalid enum", ModeChange{Mode: Mode("loose")}},
		{"invalid block", UserMessage{Source: SourceTyped, Content: []Block{{Type: BlockType("audio")}}}},
		{"compaction with unknown ids", Compaction{Summary: "s", FirstEntryID: NewID(), LastEntryID: NewID(), Model: ModelRef{"aperture", "m"}}},
	}
	for _, c := range cases {
		if _, err := s.Append(c.p); !errors.Is(err, ErrInvariant) {
			t.Errorf("%s: err = %v, want ErrInvariant", c.name, err)
		}
	}
}

func TestDecisionThenResultExactlyOnce(t *testing.T) {
	_, s := newSession(t)
	mustAppend(t, s, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("ls")}})
	mustAppend(t, s, assistantWithTool("t1"))
	if got := s.PendingToolUses(); len(got) != 1 || got[0].ID != "t1" {
		t.Fatalf("pending = %+v", got)
	}
	mustAppend(t, s, decision("t1", Allow, ScopeOnce))
	if _, err := s.Append(decision("t1", Allow, ScopeOnce)); !errors.Is(err, ErrInvariant) {
		t.Fatalf("second decision: %v", err)
	}
	mustAppend(t, s, ToolResult{ToolUseID: "t1", Outcome: OutcomeOK, Content: []Block{TextBlock("ok")}, DurationMS: 1})
	if _, err := s.Append(ToolResult{ToolUseID: "t1", Outcome: OutcomeOK}); !errors.Is(err, ErrInvariant) {
		t.Fatalf("second result: %v", err)
	}
	if got := s.PendingToolUses(); len(got) != 0 {
		t.Fatalf("pending after result = %+v", got)
	}
}

func TestAllowDecisionIsOnDiskBeforeAppendReturns(t *testing.T) {
	_, s := newSession(t)
	mustAppend(t, s, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("ls")}})
	mustAppend(t, s, assistantWithTool("t1"))
	mustAppend(t, s, decision("t1", Allow, ScopeSession))
	onDisk, err := ReadLog(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	last := onDisk[len(onDisk)-1]
	if last.Kind != KindPermissionDecision {
		t.Fatalf("last entry on disk = %s, want permission_decision", last.Kind)
	}
	if got := s.Allowances(); len(got) != 1 || got[0] != (Matcher{Tool: "bash", Prefix: "ls"}) {
		t.Fatalf("allowances = %+v", got)
	}
}

func TestDerivedGettersFollowChanges(t *testing.T) {
	_, s := newSession(t)
	mustAppend(t, s, ModelChange{Model: ModelRef{"aperture", "gpt-5.6-sol"}})
	mustAppend(t, s, ModeChange{Mode: ModeOff})
	mustAppend(t, s, ThinkingChange{Thinking: ThinkingLow})
	mustAppend(t, s, TitleChange{Title: "renamed"})
	mustAppend(t, s, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("hi")}})
	mustAppend(t, s, AssistantMessage{Model: s.Model(), Thinking: s.Thinking(), Content: []Block{TextBlock("yo")}, Usage: Usage{Input: 10, Output: 5}, StopReason: StopEndTurn, StopReasonRaw: "stop"})
	mustAppend(t, s, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("more")}})
	mustAppend(t, s, AssistantMessage{Model: s.Model(), Thinking: s.Thinking(), Content: []Block{TextBlock("ok")}, Usage: Usage{Input: 20, Output: 7}, StopReason: StopEndTurn, StopReasonRaw: "stop"})
	if s.Model().Model != "gpt-5.6-sol" || s.Mode() != ModeOff || s.Thinking() != ThinkingLow || s.Title() != "renamed" {
		t.Fatalf("derived: model %s mode %s thinking %s title %q", s.Model(), s.Mode(), s.Thinking(), s.Title())
	}
	if got := s.Usage(); got != (Usage{Input: 30, Output: 12}) {
		t.Fatalf("usage = %+v", got)
	}
	es := s.Entries()
	mustAppend(t, s, Compaction{Summary: "summary", FirstEntryID: es[1].ID, LastEntryID: es[len(es)-1].ID, Model: s.Model(), Usage: Usage{Input: 100, Output: 20}})
	mustAppend(t, s, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("after")}})
	rc := s.RequestContext()
	if len(rc) != 2 || rc[0].Kind != KindCompaction || rc[1].Kind != KindUserMessage {
		t.Fatalf("request context = %+v", rc)
	}
}

func TestForkInheritsByReference(t *testing.T) {
	st, parent := newSession(t)
	mustAppend(t, parent, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("one")}})
	at := mustAppend(t, parent, AssistantMessage{Model: parent.Model(), Thinking: parent.Thinking(), Content: []Block{TextBlock("two")}, StopReason: StopEndTurn, StopReasonRaw: "stop"})
	mustAppend(t, parent, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("three, not inherited")}})

	child, err := parent.Fork(st, at.ID)
	if err != nil {
		t.Fatal(err)
	}
	es := child.Entries()
	if len(es) != 4 || es[0].Kind != KindSessionOpened || es[2].ID != at.ID || es[3].Kind != KindForkPoint {
		t.Fatalf("child entries = %+v", es)
	}
	if child.Model() != parent.Model() || child.Workspace() != parent.Workspace() {
		t.Fatal("child must derive workspace and model through the parent")
	}
	onDisk, err := ReadLog(child.Dir())
	if err != nil || len(onDisk) != 1 || onDisk[0].Kind != KindForkPoint {
		t.Fatalf("child log on disk = %+v, %v; want only fork_point", onDisk, err)
	}
	if _, err := parent.Fork(st, NewID()); !errors.Is(err, ErrInvariant) {
		t.Fatalf("fork at unknown entry: %v", err)
	}
	// A session must be closed, releasing its lock, before it can be loaded
	// again; loading it while still open is exactly what ErrLocked guards.
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Load(st, child.ID())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reloaded.Close() }()
	if got := reloaded.Entries(); len(got) != 4 || got[2].ID != at.ID {
		t.Fatalf("reloaded child entries = %+v", got)
	}
}

func TestLoadRecoversLostResults(t *testing.T) {
	st, s := newSession(t)
	mustAppend(t, s, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("ls")}})
	mustAppend(t, s, AssistantMessage{
		Model: s.Model(), Thinking: s.Thinking(),
		Content: []Block{
			ToolUseBlock("allowed", "bash", json.RawMessage(`{"command":"ls"}`)),
			ToolUseBlock("undecided", "bash", json.RawMessage(`{"command":"pwd"}`)),
		},
		StopReason: StopToolUse, StopReasonRaw: "tool_calls",
	})
	mustAppend(t, s, decision("allowed", Allow, ScopeOnce))
	id := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	re, err := Load(st, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = re.Close() }()
	if got := re.PendingToolUses(); len(got) != 0 {
		t.Fatalf("pending after recovery = %+v", got)
	}
	es := re.Entries()
	tail := es[len(es)-3:]
	if tail[0].Kind != KindToolResult || tail[0].Payload.(ToolResult).Outcome != OutcomeLost || tail[0].Payload.(ToolResult).ToolUseID != "allowed" {
		t.Fatalf("expected lost result for allowed, got %+v", tail[0])
	}
	if tail[1].Kind != KindPermissionDecision || tail[1].Payload.(PermissionDecision).DecidedBy != ByNoAsker {
		t.Fatalf("expected recovery deny for undecided, got %+v", tail[1])
	}
	if tail[2].Kind != KindToolResult || tail[2].Payload.(ToolResult).Outcome != OutcomeLost {
		t.Fatalf("expected lost result for undecided, got %+v", tail[2])
	}
}

func TestLoadRefusesNonMonotonicIDs(t *testing.T) {
	st, s := newSession(t)
	mustAppend(t, s, ModeChange{Mode: ModeOff})
	id := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	dir := st.Dir(id)
	entries, err := ReadLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite the file with the two entries swapped.
	swapped := []Entry{entries[1], entries[0]}
	if err := rewriteLog(dir, swapped); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(st, id); err == nil {
		t.Fatal("Load must refuse non-monotonic ids")
	}
}

// rewriteLog is a test helper: it replaces a log with the given entries in order.
func rewriteLog(dir string, entries []Entry) error {
	l, err := OpenLog(dir)
	if err != nil {
		return err
	}
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	for _, e := range entries {
		if err := l.Append(e); err != nil {
			return err
		}
	}
	return l.Close()
}

// TestAppendAfterCloseIsAnError covers the crash a shutdown could cause: Close used to drop
// the log pointer, so anything still holding the session (a turn the server closed out from
// under after its shutdown budget ran out) panicked on its next Append instead of failing.
func TestAppendAfterCloseIsAnError(t *testing.T) {
	_, s := newSession(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := s.Append(UserMessage{Source: SourceTyped, Content: []Block{TextBlock("after close")}})
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Append after Close = %v, want ErrClosed", err)
	}
	if err := s.Sync(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Sync after Close = %v, want ErrClosed", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}
