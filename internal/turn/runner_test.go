package turn

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// pause is a sentinel part: the scripted provider blocks on release until the test lets
// it continue, or returns ctx.Err() when the runner cancels first.
const pause provider.PartType = "test_pause"

type scripted struct {
	mu       sync.Mutex
	scripts  [][]provider.Part
	calls    int
	requests []provider.Request
	release  chan struct{}
	err      error // returned instead of streaming when set
}

func (p *scripted) Name() string { return "fake" }

func (p *scripted) ListModels(context.Context) ([]provider.Model, error) { return nil, nil }

func (p *scripted) Complete(ctx context.Context, req provider.Request, emit func(provider.Part) error) error {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	if p.err != nil {
		p.mu.Unlock()
		return p.err
	}
	if p.calls >= len(p.scripts) {
		p.mu.Unlock()
		return &provider.Error{Class: session.ErrInternal, Message: "script exhausted"}
	}
	parts := p.scripts[p.calls]
	p.calls++
	p.mu.Unlock()
	for _, part := range parts {
		if part.Type == pause {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-p.release:
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := emit(part); err != nil {
			return err
		}
	}
	return nil
}

func text(s string) provider.Part { return provider.Part{Type: provider.PartTextDelta, Text: s} }

func stop(reason session.StopReason, raw string) provider.Part {
	return provider.Part{Type: provider.PartStop, StopReason: reason, StopReasonRaw: raw}
}

func usage(in, out int64) provider.Part {
	return provider.Part{Type: provider.PartUsage, Usage: session.Usage{Input: in, Output: out}}
}

func toolCall(id, name, input string) []provider.Part {
	return []provider.Part{
		{Type: provider.PartToolUseStart, ID: id, Name: name},
		{Type: provider.PartToolUseDelta, ID: id, Text: input},
		{Type: provider.PartToolUseEnd, ID: id},
	}
}

type recorder struct {
	mu      sync.Mutex
	entries []session.Entry
	states  []State
	deltas  []provider.Part
}

func (r *recorder) EntryAppended(e session.Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, e)
}

func (r *recorder) Delta(_ string, p provider.Part) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deltas = append(r.deltas, p)
}

func (r *recorder) StateChanged(_ string, s State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, s)
}

func (r *recorder) kinds() []session.Kind {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]session.Kind, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e.Kind)
	}
	return out
}

type askerFunc func(ctx context.Context, q Question) (Answer, error)

func (f askerFunc) Ask(ctx context.Context, q Question) (Answer, error) { return f(ctx, q) }

type toolSet map[string]tool.Tool

func (ts toolSet) Tool(name string) (tool.Tool, bool) { t, ok := ts[name]; return t, ok }

func (ts toolSet) Tools() []tool.Tool {
	out := make([]tool.Tool, 0, len(ts))
	for _, t := range ts {
		out = append(out, t)
	}
	return out
}

func echoTool(safety tool.Safety, name string) tool.Tool {
	return tool.Tool{Name: name, Description: "echo", Schema: json.RawMessage(`{"type":"object"}`), Safety: safety,
		Invoke: func(_ context.Context, c tool.Call) (tool.Result, error) {
			return tool.Result{Content: []session.Block{session.TextBlock("echo:" + string(c.Input))}}, nil
		}}
}

func newRunner(t *testing.T, s *session.Session, p *scripted, tools toolSet, asker Asker, rec *recorder) *Runner {
	t.Helper()
	return NewRunner(Config{
		Session:   s,
		Provider:  p,
		Model:     provider.Model{Ref: s.Model(), ContextWindow: 100000},
		Tools:     tools,
		Gate:      gate.New([]string{"rm -rf"}),
		Asker:     asker,
		Observer:  rec,
		System:    "SYSTEM",
		MaxTokens: 1024,
	})
}

func userMsg(src session.Source, s string) session.UserMessage {
	return session.UserMessage{Source: src, Content: []session.Block{session.TextBlock(s)}}
}

func equalKinds(a, b []session.Kind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRunTextOnly(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	p := &scripted{scripts: [][]provider.Part{{text("Hel"), text("lo"), usage(10, 2), stop(session.StopEndTurn, "stop")}}}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{}, nil, rec)

	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "hi")); err != nil {
		t.Fatal(err)
	}
	if r.State() != Completed {
		t.Fatalf("state %s", r.State())
	}
	want := []session.Kind{session.KindUserMessage, session.KindAssistantMessage}
	if got := rec.kinds(); !equalKinds(got, want) {
		t.Fatalf("entries %v", got)
	}
	am := rec.entries[1].Payload.(session.AssistantMessage)
	if len(am.Content) != 1 || am.Content[0].Text != "Hello" || am.Usage.Input != 10 || am.StopReason != session.StopEndTurn || am.StopReasonRaw != "stop" {
		t.Fatalf("assistant message %+v", am)
	}
	if len(rec.deltas) != 4 {
		t.Fatalf("every part must reach the observer, got %d", len(rec.deltas))
	}
	if len(p.requests) != 1 || p.requests[0].System != "SYSTEM" || p.requests[0].Messages[0].Content[0].Text != "hi" {
		t.Fatalf("request %+v", p.requests)
	}
}

func TestRunToolFlowStrictAskerAllows(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	var seenAllowOnDisk bool
	unsafe := tool.Tool{Name: "bash", Description: "run", Schema: json.RawMessage(`{"type":"object"}`), Safety: tool.Unsafe,
		Invoke: func(_ context.Context, c tool.Call) (tool.Result, error) {
			entries, err := session.ReadLog(s.Dir())
			if err != nil {
				t.Error(err)
			}
			for _, e := range entries {
				if d, ok := e.Payload.(session.PermissionDecision); ok && d.ToolUseID == c.ID && d.Decision == session.Allow {
					seenAllowOnDisk = true
				}
			}
			return tool.Result{Content: []session.Block{session.TextBlock("ran")}}, nil
		}}
	p := &scripted{scripts: [][]provider.Part{
		append(append([]provider.Part{text("running")}, toolCall("tu1", "bash", `{"command":"go test ./..."}`)...), usage(5, 5), stop(session.StopToolUse, "tool_calls")),
		{text("done"), usage(6, 1), stop(session.StopEndTurn, "stop")},
	}}
	var asked Question
	asker := askerFunc(func(_ context.Context, q Question) (Answer, error) {
		asked = q
		return Answer{Decision: session.Allow, Scope: session.ScopeOnce, Reason: "looks fine"}, nil
	})
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{"bash": unsafe}, asker, rec)

	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "test it")); err != nil {
		t.Fatal(err)
	}
	want := []session.Kind{session.KindUserMessage, session.KindAssistantMessage, session.KindPermissionDecision, session.KindToolResult, session.KindAssistantMessage}
	if got := rec.kinds(); !equalKinds(got, want) {
		t.Fatalf("entries %v", got)
	}
	if !seenAllowOnDisk {
		t.Fatal("the allow decision must be on disk before the tool runs")
	}
	if asked.ToolUseID != "tu1" || asked.Tool != "bash" || string(asked.Input) != `{"command":"go test ./..."}` {
		t.Fatalf("question %+v", asked)
	}
	pd := rec.entries[2].Payload.(session.PermissionDecision)
	if pd.Decision != session.Allow || pd.DecidedBy != session.ByAsker || pd.Scope != session.ScopeOnce || pd.Reason != "looks fine" || pd.Mode != session.ModeStrict {
		t.Fatalf("decision %+v", pd)
	}
	tr := rec.entries[3].Payload.(session.ToolResult)
	if tr.Outcome != session.OutcomeOK || tr.Content[0].Text != "ran" || tr.ToolUseID != "tu1" {
		t.Fatalf("tool result %+v", tr)
	}
	wantStates := []State{Streaming, AwaitingPermission, RunningTool, Streaming, Completed}
	if len(rec.states) != len(wantStates) {
		t.Fatalf("states %v", rec.states)
	}
	for i := range wantStates {
		if rec.states[i] != wantStates[i] {
			t.Fatalf("states %v want %v", rec.states, wantStates)
		}
	}
	if len(p.requests) != 2 || p.requests[1].Messages[2].Role != provider.RoleToolResult {
		t.Fatalf("second request must carry the tool result: %+v", p.requests[1].Messages)
	}
}

func TestRunToolDenied(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	invoked := false
	unsafe := echoTool(tool.Unsafe, "bash")
	unsafe.Invoke = func(context.Context, tool.Call) (tool.Result, error) { invoked = true; return tool.Result{}, nil }
	p := &scripted{scripts: [][]provider.Part{
		append(toolCall("tu1", "bash", `{"command":"ls"}`), stop(session.StopToolUse, "tool_calls")),
		{text("ok, skipping"), stop(session.StopEndTurn, "stop")},
	}}
	asker := askerFunc(func(context.Context, Question) (Answer, error) {
		return Answer{Decision: session.Deny, Scope: session.ScopeOnce, Reason: "not now"}, nil
	})
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{"bash": unsafe}, asker, rec)
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "list")); err != nil {
		t.Fatal(err)
	}
	if invoked {
		t.Fatal("denied tool must not run")
	}
	pd := rec.entries[2].Payload.(session.PermissionDecision)
	if pd.Decision != session.Deny || pd.DecidedBy != session.ByAsker || pd.Reason != "not now" {
		t.Fatalf("decision %+v", pd)
	}
	tr := rec.entries[3].Payload.(session.ToolResult)
	if tr.Outcome != session.OutcomeError || tr.Content[0].Text != "denied: not now" {
		t.Fatalf("tool result %+v", tr)
	}
}

func TestRunStrictWithoutAskerDenies(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	p := &scripted{scripts: [][]provider.Part{
		append(toolCall("tu1", "bash", `{"command":"ls"}`), stop(session.StopToolUse, "tool_calls")),
		{text("understood"), stop(session.StopEndTurn, "stop")},
	}}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{"bash": echoTool(tool.Unsafe, "bash")}, nil, rec)
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "list")); err != nil {
		t.Fatal(err)
	}
	pd := rec.entries[2].Payload.(session.PermissionDecision)
	if pd.Decision != session.Deny || pd.DecidedBy != session.ByNoAsker {
		t.Fatalf("decision %+v", pd)
	}
	for _, st := range rec.states {
		if st == AwaitingPermission {
			t.Fatal("no asker means no waiting")
		}
	}
}

func TestRunSafeToolNeverAsks(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	p := &scripted{scripts: [][]provider.Part{
		append(toolCall("tu1", "read", `{"path":"a"}`), stop(session.StopToolUse, "tool_calls")),
		{text("read it"), stop(session.StopEndTurn, "stop")},
	}}
	asker := askerFunc(func(context.Context, Question) (Answer, error) {
		t.Fatal("safe tool must not ask")
		return Answer{}, nil
	})
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{"read": echoTool(tool.Safe, "read")}, asker, rec)
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "read a")); err != nil {
		t.Fatal(err)
	}
	pd := rec.entries[2].Payload.(session.PermissionDecision)
	if pd.Decision != session.Allow || pd.DecidedBy != session.ByClass {
		t.Fatalf("decision %+v", pd)
	}
}

func TestRunOffModeAllowsByMode(t *testing.T) {
	s := openTestSession(t, session.ModeOff)
	p := &scripted{scripts: [][]provider.Part{
		append(toolCall("tu1", "bash", `{"command":"rm -rf build"}`), stop(session.StopToolUse, "tool_calls")),
		{text("gone"), stop(session.StopEndTurn, "stop")},
	}}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{"bash": echoTool(tool.Unsafe, "bash")}, nil, rec)
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "clean")); err != nil {
		t.Fatal(err)
	}
	pd := rec.entries[2].Payload.(session.PermissionDecision)
	if pd.Decision != session.Allow || pd.DecidedBy != session.ByMode {
		t.Fatalf("decision %+v", pd)
	}
}

func TestRunPermissiveAsksForDangerous(t *testing.T) {
	s := openTestSession(t, session.ModePermissive)
	p := &scripted{scripts: [][]provider.Part{
		append(toolCall("tu1", "bash", `{"command":"rm -rf build"}`), stop(session.StopToolUse, "tool_calls")),
		{text("gone"), stop(session.StopEndTurn, "stop")},
	}}
	asked := false
	asker := askerFunc(func(context.Context, Question) (Answer, error) {
		asked = true
		return Answer{Decision: session.Allow, Scope: session.ScopeSession, Reason: "yes for this session"}, nil
	})
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{"bash": echoTool(tool.Unsafe, "bash")}, asker, rec)
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "clean")); err != nil {
		t.Fatal(err)
	}
	if !asked {
		t.Fatal("dangerous command in permissive mode must ask")
	}
	pd := rec.entries[2].Payload.(session.PermissionDecision)
	if pd.Scope != session.ScopeSession || pd.DecidedBy != session.ByAsker {
		t.Fatalf("decision %+v", pd)
	}
	if len(s.Allowances()) != 1 {
		t.Fatalf("session allowance must be derivable, got %v", s.Allowances())
	}
}

func TestRunUnknownToolIsAnErrorResult(t *testing.T) {
	s := openTestSession(t, session.ModeOff)
	p := &scripted{scripts: [][]provider.Part{
		append(toolCall("tu1", "nope", `{}`), stop(session.StopToolUse, "tool_calls")),
		{text("sorry"), stop(session.StopEndTurn, "stop")},
	}}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{}, nil, rec)
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "x")); err != nil {
		t.Fatal(err)
	}
	want := []session.Kind{session.KindUserMessage, session.KindAssistantMessage, session.KindPermissionDecision, session.KindToolResult, session.KindAssistantMessage}
	if got := rec.kinds(); !equalKinds(got, want) {
		t.Fatalf("entries %v", got)
	}
	pd := rec.entries[2].Payload.(session.PermissionDecision)
	if pd.Decision != session.Deny || pd.DecidedBy != session.ByClass {
		t.Fatalf("decision %+v", pd)
	}
	tr := rec.entries[3].Payload.(session.ToolResult)
	if tr.Outcome != session.OutcomeError || tr.Content[0].Text != "unknown tool nope" {
		t.Fatalf("tool result %+v", tr)
	}
}

func TestSteerMidStreamThenContinue(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	p := &scripted{
		release: make(chan struct{}),
		scripts: [][]provider.Part{
			{text("Half"), {Type: pause}, text(" never sent"), stop(session.StopEndTurn, "stop")},
			{text("Steered"), stop(session.StopEndTurn, "stop")},
		},
	}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{}, nil, rec)

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), userMsg(session.SourceTyped, "go")) }()
	waitForDelta(t, rec, 1)
	r.Interrupt(session.InterruptSteer)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after steer")
	}
	if r.State() != Steering {
		t.Fatalf("state %s", r.State())
	}
	am := rec.entries[1].Payload.(session.AssistantMessage)
	if am.StopReason != session.StopInterrupted || am.Content[0].Text != "Half" || am.StopReasonRaw != "" {
		t.Fatalf("partial message %+v", am)
	}
	turnID := r.TurnID()

	if err := r.Run(context.Background(), userMsg(session.SourceSteer, "do it differently")); err != nil {
		t.Fatal(err)
	}
	if r.State() != Completed || r.TurnID() != turnID {
		t.Fatalf("state %s turn %s want %s", r.State(), r.TurnID(), turnID)
	}
	want := []session.Kind{session.KindUserMessage, session.KindAssistantMessage, session.KindUserMessage, session.KindAssistantMessage}
	if got := rec.kinds(); !equalKinds(got, want) {
		t.Fatalf("entries %v", got)
	}
	if len(p.requests) != 2 || len(p.requests[1].Messages) != 3 {
		t.Fatalf("the steer request must carry the partial and the steer: %+v", p.requests[1].Messages)
	}
}

func TestCancelMidStream(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	p := &scripted{release: make(chan struct{}), scripts: [][]provider.Part{{text("Half"), {Type: pause}, stop(session.StopEndTurn, "stop")}}}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{}, nil, rec)

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), userMsg(session.SourceTyped, "go")) }()
	waitForDelta(t, rec, 1)
	r.Interrupt(session.InterruptCancel)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if r.State() != Idle {
		t.Fatalf("state %s", r.State())
	}
	want := []session.Kind{session.KindUserMessage, session.KindAssistantMessage, session.KindTurnInterrupted}
	if got := rec.kinds(); !equalKinds(got, want) {
		t.Fatalf("entries %v", got)
	}
	if ti := rec.entries[2].Payload.(session.TurnInterrupted); ti.How != session.InterruptCancel || ti.TurnID != rec.entries[0].ID {
		t.Fatalf("interrupted %+v want turn %s", ti, rec.entries[0].ID)
	}
}

func TestSteerDuringToolKillsIt(t *testing.T) {
	s := openTestSession(t, session.ModeOff)
	started := make(chan struct{})
	slow := tool.Tool{Name: "bash", Description: "slow", Schema: json.RawMessage(`{"type":"object"}`), Safety: tool.Unsafe,
		Invoke: func(ctx context.Context, _ tool.Call) (tool.Result, error) {
			close(started)
			<-ctx.Done()
			return tool.Result{Content: []session.Block{session.TextBlock("partial output")}}, ctx.Err()
		}}
	p := &scripted{scripts: [][]provider.Part{
		append(toolCall("tu1", "bash", `{"command":"sleep 100"}`), stop(session.StopToolUse, "tool_calls")),
	}}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{"bash": slow}, nil, rec)

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), userMsg(session.SourceTyped, "sleep")) }()
	<-started
	r.Interrupt(session.InterruptSteer)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return")
	}
	if r.State() != Steering {
		t.Fatalf("state %s", r.State())
	}
	tr := rec.entries[3].Payload.(session.ToolResult)
	if tr.Outcome != session.OutcomeKilled || tr.Content[0].Text != "partial output" {
		t.Fatalf("tool result %+v", tr)
	}
}

// TestResumeVsCancelRace is a regression test for a race between resuming a steered turn
// and cancelling it at the same instant. Both Run (resuming with a steer message) and
// Interrupt(InterruptCancel) check the runner's state and, in the Steering case, act on
// it: Run claims the state (Steering -> Streaming) before appending the steer message;
// Interrupt's Steering branch appends turn_interrupted itself. Before the fix, that check
// and action were not atomic in Run, so both goroutines could independently observe
// Steering and each proceed to append to the session with no serialization between them,
// racing on Session's unprotected internal state.
//
// With the fix, both checks run under the runner's mutex, so exactly one of Run or
// Interrupt observes Steering and resolves it; Run's return value pins down which:
// wrapping ErrInvariant means Interrupt won the claim (Run is refused before it can
// append anything), anything else means Run won it (it always appends the steer message
// and returns nil in this scenario, since nothing here can fail).
//
// A losing Interrupt call is not necessarily a no-op past that point, though: once it
// observes the runner is no longer Steering, it falls through to the same general
// active-state handling TestCancelMidStream exercises, and its cancel signal very often
// still lands on the turn Run just resumed, producing a further turn_interrupted for that
// same turn moments later. That is correct, ordinary cancellation, not corruption: a
// cancel arriving essentially simultaneously with a steer resume legitimately cancels the
// resumed turn instead of silently losing the race to prevent it. What must never happen
// is the two calls both believing they resolved the Steering state itself: Run appending
// the steer message while Interrupt, at the same moment, also appends turn_interrupted
// through its Steering branch believing the state was still Steering. Run's return value
// makes that pairing directly checkable.
//
// Run with: go test -race -run TestResumeVsCancelRace -count=50 ./internal/turn/...
func TestResumeVsCancelRace(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	p := &scripted{
		release: make(chan struct{}),
		scripts: [][]provider.Part{
			{text("Half"), {Type: pause}, text(" never sent"), stop(session.StopEndTurn, "stop")},
			{text("Steered"), stop(session.StopEndTurn, "stop")},
		},
	}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{}, nil, rec)

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), userMsg(session.SourceTyped, "go")) }()
	waitForDelta(t, rec, 1)
	r.Interrupt(session.InterruptSteer)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after steer")
	}
	if r.State() != Steering {
		t.Fatalf("state %s", r.State())
	}
	before := len(s.Entries())

	var wg sync.WaitGroup
	wg.Add(2)
	var runErr error
	go func() {
		defer wg.Done()
		runErr = r.Run(context.Background(), userMsg(session.SourceSteer, "race"))
	}()
	go func() {
		defer wg.Done()
		r.Interrupt(session.InterruptCancel)
	}()
	wg.Wait()

	after := s.Entries()[before:]
	hasUserMessage, hasTurnInterrupted := false, false
	for _, e := range after {
		switch e.Kind {
		case session.KindUserMessage:
			hasUserMessage = true
		case session.KindTurnInterrupted:
			hasTurnInterrupted = true
		}
	}

	if errors.Is(runErr, session.ErrInvariant) {
		// Interrupt won the claim: Run must never have appended the steer message, and
		// Interrupt's own Steering branch must have appended turn_interrupted.
		if hasUserMessage {
			t.Fatalf("Interrupt won the claim but a steer user_message was still appended: %v", after)
		}
		if !hasTurnInterrupted {
			t.Fatalf("Interrupt won the claim but appended no turn_interrupted: %v", after)
		}
		return
	}
	if runErr != nil {
		t.Fatalf("Run won the claim but returned an unexpected error: %v", runErr)
	}
	// Run won the claim: it must have appended the steer message. A turn_interrupted may
	// also be present (the losing Interrupt's cancel signal catching the resumed turn),
	// but only after the steer message, never appended independently of it.
	if !hasUserMessage {
		t.Fatalf("Run won the claim but appended no steer user_message: %v", after)
	}
	sawUserMessage := false
	for _, e := range after {
		if e.Kind == session.KindUserMessage {
			sawUserMessage = true
		}
		if e.Kind == session.KindTurnInterrupted && !sawUserMessage {
			t.Fatalf("turn_interrupted appeared before the steer user_message: %v", after)
		}
	}
}

func TestProviderErrorFailsTurn(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	p := &scripted{err: &provider.Error{Class: session.ErrProvider, Status: 502, Message: "bad gateway", Body: []byte("x"), Attempts: 5}}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{}, nil, rec)
	err := r.Run(context.Background(), userMsg(session.SourceTyped, "hi"))
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("want *provider.Error, got %v", err)
	}
	if r.State() != Failed {
		t.Fatalf("state %s", r.State())
	}
	tf := rec.entries[1].Payload.(session.TurnFailed)
	if tf.Class != session.ErrProvider || tf.Message != "bad gateway" || tf.Retries != 5 {
		t.Fatalf("turn_failed %+v", tf)
	}
	if tf.TurnID != rec.entries[0].ID || r.TurnID() != rec.entries[0].ID.String() {
		t.Fatalf("turn id %s want the user_message id %s", tf.TurnID, rec.entries[0].ID)
	}
}

func TestPanickingToolIsAPluginFault(t *testing.T) {
	s := openTestSession(t, session.ModeOff)
	p := &scripted{scripts: [][]provider.Part{append(toolCall("t1", "boom", `{}`), stop(session.StopToolUse, "tool_calls"))}}
	rec := &recorder{}
	tools := toolSet{"boom": tool.Tool{Name: "boom", Safety: tool.Unsafe, Invoke: func(context.Context, tool.Call) (tool.Result, error) {
		panic("nil map write")
	}}}
	r := newRunner(t, s, p, tools, nil, rec)
	err := r.Run(context.Background(), userMsg(session.SourceTyped, "go"))
	if err == nil || r.State() != Failed {
		t.Fatalf("err %v state %s", err, r.State())
	}
	tf := rec.entries[len(rec.entries)-1].Payload.(session.TurnFailed)
	if tf.Class != session.ErrPlugin || tf.Retries != 0 || !strings.Contains(tf.Message, "nil map write") {
		t.Fatalf("turn_failed %+v", tf)
	}
}

func TestCallerContextCancelIsCancel(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	p := &scripted{release: make(chan struct{}), scripts: [][]provider.Part{{text("Half"), {Type: pause}, stop(session.StopEndTurn, "stop")}}}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{}, nil, rec)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, userMsg(session.SourceTyped, "go")) }()
	waitForDelta(t, rec, 1)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return")
	}
	if r.State() != Idle {
		t.Fatalf("state %s", r.State())
	}
	if got := rec.kinds(); got[len(got)-1] != session.KindTurnInterrupted {
		t.Fatalf("entries %v", got)
	}
}

func TestInterruptWhenIdleIsNoop(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	rec := &recorder{}
	r := newRunner(t, s, &scripted{}, toolSet{}, nil, rec)
	r.Interrupt(session.InterruptCancel)
	if r.State() != Idle || len(rec.entries) != 0 {
		t.Fatalf("idle interrupt must change nothing: %s %v", r.State(), rec.kinds())
	}
}

func waitForDelta(t *testing.T, rec *recorder, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		got := len(rec.deltas)
		rec.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waited for %d deltas", n)
}

// TestSteerWhileAwaitingPermission and TestCancelWhileAwaitingPermission cover the
// interrupt that lands while the asker still holds the question. Every tool_result needs a
// permission_decision for its tool_use before it, so the killed result the interrupt
// records has to be preceded by a deny for that same tool_use; without one Append refuses
// the result and the turn dies as an internal failure instead of steering or cancelling.
func TestSteerWhileAwaitingPermission(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	p := &scripted{scripts: [][]provider.Part{
		append(toolCall("tu1", "bash", `{"command":"ls"}`), stop(session.StopToolUse, "tool_calls")),
		{text("steered"), stop(session.StopEndTurn, "stop")},
	}}
	asked, release := make(chan struct{}), make(chan struct{})
	asker := askerFunc(func(context.Context, Question) (Answer, error) {
		close(asked)
		<-release
		return Answer{Decision: session.Allow, Scope: session.ScopeOnce, Reason: "too late"}, nil
	})
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{"bash": echoTool(tool.Unsafe, "bash")}, asker, rec)

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), userMsg(session.SourceTyped, "list")) }()
	<-asked
	r.Interrupt(session.InterruptSteer)
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after steer")
	}
	if r.State() != Steering {
		t.Fatalf("state %s", r.State())
	}
	want := []session.Kind{session.KindUserMessage, session.KindAssistantMessage, session.KindPermissionDecision, session.KindToolResult}
	if got := rec.kinds(); !equalKinds(got, want) {
		t.Fatalf("entries %v want %v", got, want)
	}
	pd := rec.entries[2].Payload.(session.PermissionDecision)
	if pd.ToolUseID != "tu1" || pd.Decision != session.Deny || pd.DecidedBy != session.ByAsker || pd.Scope != session.ScopeOnce || pd.Reason != "interrupted" {
		t.Fatalf("decision %+v", pd)
	}
	tr := rec.entries[3].Payload.(session.ToolResult)
	if tr.ToolUseID != "tu1" || tr.Outcome != session.OutcomeKilled {
		t.Fatalf("tool result %+v", tr)
	}
	if err := r.Run(context.Background(), userMsg(session.SourceSteer, "never mind")); err != nil {
		t.Fatal(err)
	}
	if r.State() != Completed {
		t.Fatalf("state after the steer resume %s", r.State())
	}
}

func TestCancelWhileAwaitingPermission(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	p := &scripted{scripts: [][]provider.Part{
		append(toolCall("tu1", "bash", `{"command":"ls"}`), stop(session.StopToolUse, "tool_calls")),
	}}
	asked, release := make(chan struct{}), make(chan struct{})
	asker := askerFunc(func(context.Context, Question) (Answer, error) {
		close(asked)
		<-release
		return Answer{Decision: session.Allow, Scope: session.ScopeOnce, Reason: "too late"}, nil
	})
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{"bash": echoTool(tool.Unsafe, "bash")}, asker, rec)

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), userMsg(session.SourceTyped, "list")) }()
	<-asked
	r.Interrupt(session.InterruptCancel)
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if r.State() != Idle {
		t.Fatalf("state %s", r.State())
	}
	want := []session.Kind{session.KindUserMessage, session.KindAssistantMessage, session.KindPermissionDecision, session.KindToolResult, session.KindTurnInterrupted}
	if got := rec.kinds(); !equalKinds(got, want) {
		t.Fatalf("entries %v want %v", got, want)
	}
	pd := rec.entries[2].Payload.(session.PermissionDecision)
	if pd.Decision != session.Deny || pd.DecidedBy != session.ByAsker || pd.Reason != "interrupted" {
		t.Fatalf("decision %+v", pd)
	}
	if ti := rec.entries[4].Payload.(session.TurnInterrupted); ti.How != session.InterruptCancel || ti.TurnID != rec.entries[0].ID {
		t.Fatalf("interrupted %+v", ti)
	}
}

// TestLogIsDurableWhenTheTurnRests proves a turn's entries are on disk by the time Run
// returns, not whenever the log's write buffer happens to fill or the session is finally
// closed. ReadLog opens the file itself, so it sees only what has actually been written.
func TestLogIsDurableWhenTheTurnRests(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	p := &scripted{scripts: [][]provider.Part{{text("durable"), usage(1, 1), stop(session.StopEndTurn, "stop")}}}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{}, nil, rec)
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "hi")); err != nil {
		t.Fatal(err)
	}
	onDisk, err := session.ReadLog(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range onDisk {
		if am, ok := e.Payload.(session.AssistantMessage); ok && textOf(am.Content) == "durable" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the completed turn's assistant message is not on disk: %v", kindsOf(onDisk))
	}
}

// TestLogIsDurableAfterACancel is the same guarantee for the other resting states: an
// interrupted turn's turn_interrupted must survive a crash right after the interrupt.
func TestLogIsDurableAfterACancel(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	p := &scripted{release: make(chan struct{}), scripts: [][]provider.Part{{text("Half"), {Type: pause}, stop(session.StopEndTurn, "stop")}}}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{}, nil, rec)
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), userMsg(session.SourceTyped, "go")) }()
	waitForDelta(t, rec, 1)
	r.Interrupt(session.InterruptCancel)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
	onDisk, err := session.ReadLog(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if got := kindsOf(onDisk); len(got) == 0 || got[len(got)-1] != session.KindTurnInterrupted {
		t.Fatalf("turn_interrupted is not on disk: %v", got)
	}
}

func kindsOf(entries []session.Entry) []session.Kind {
	out := make([]session.Kind, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Kind)
	}
	return out
}

// TestMalformedToolInputIsAnErrorResult covers a model that streams tool-call JSON the log
// will not accept. The block cannot be appended as sent (Append refuses a tool_use whose
// input is not valid JSON), and dropping the block would leave the model's own message
// disagreeing with the record. The input is recorded as an empty object, the call is
// answered with an error result quoting exactly what arrived, and the turn continues so the
// model can correct itself.
func TestMalformedToolInputIsAnErrorResult(t *testing.T) {
	s := openTestSession(t, session.ModeOff)
	p := &scripted{scripts: [][]provider.Part{
		append(toolCall("tu1", "read", `{"path": `), stop(session.StopToolUse, "tool_calls")),
		{text("sorry, retrying"), stop(session.StopEndTurn, "stop")},
	}}
	invoked := false
	rd := echoTool(tool.Safe, "read")
	rd.Invoke = func(context.Context, tool.Call) (tool.Result, error) { invoked = true; return tool.Result{}, nil }
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{"read": rd}, nil, rec)

	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "read it")); err != nil {
		t.Fatal(err)
	}
	if r.State() != Completed {
		t.Fatalf("state %s", r.State())
	}
	if invoked {
		t.Fatal("a tool_use with malformed input must not run")
	}
	want := []session.Kind{session.KindUserMessage, session.KindAssistantMessage, session.KindPermissionDecision, session.KindToolResult, session.KindAssistantMessage}
	if got := rec.kinds(); !equalKinds(got, want) {
		t.Fatalf("entries %v want %v", got, want)
	}
	am := rec.entries[1].Payload.(session.AssistantMessage)
	if len(am.Content) != 1 || am.Content[0].Type != session.BlockToolUse || string(am.Content[0].Input) != "{}" {
		t.Fatalf("recorded tool_use %+v, want its input replaced with an empty object", am.Content)
	}
	pd := rec.entries[2].Payload.(session.PermissionDecision)
	if pd.ToolUseID != "tu1" || pd.Decision != session.Deny || pd.DecidedBy != session.ByClass || pd.Reason != "malformed input" {
		t.Fatalf("decision %+v", pd)
	}
	tr := rec.entries[3].Payload.(session.ToolResult)
	if tr.ToolUseID != "tu1" || tr.Outcome != session.OutcomeError || tr.Content[0].Text != `malformed tool input: {"path": ` {
		t.Fatalf("tool result %+v", tr)
	}
	if len(p.requests) != 2 {
		t.Fatalf("the model must get a second turn to retry, got %d requests", len(p.requests))
	}
}

// recordingHooks is a HookFirer that records every call and answers each point from a fixed
// table, standing in for a plugin registry's handlers.
type recordingHooks struct {
	calls   []plugin.HookCall
	fired   []fireCtx // what the context looked like at each fire, in the same order
	results map[plugin.HookPoint][]any
}

// fireCtx is the state of the context at the moment of a fire, recorded then rather than kept:
// a fire's context is cancelled again as soon as it returns, so a test reading it afterwards
// would see every fire as dead.
type fireCtx struct {
	err      error
	deadline bool
}

func (h *recordingHooks) Fire(ctx context.Context, c plugin.HookCall) []any {
	_, hasDeadline := ctx.Deadline()
	h.calls = append(h.calls, c)
	h.fired = append(h.fired, fireCtx{err: ctx.Err(), deadline: hasDeadline})
	return h.results[c.Point]
}

// firedAt is the context state of the first fire of point, and whether it fired at all.
func (h *recordingHooks) firedAt(point plugin.HookPoint) (fireCtx, bool) {
	for i, c := range h.calls {
		if c.Point == point {
			return h.fired[i], true
		}
	}
	return fireCtx{}, false
}

func (h *recordingHooks) points() []plugin.HookPoint {
	out := make([]plugin.HookPoint, 0, len(h.calls))
	for _, c := range h.calls {
		out = append(out, c.Point)
	}
	return out
}

// fixtureTools is the tool set the hook tests use: echo returns its input and sleep waits
// for its duration or the context, whichever comes first. Both are unsafe, so every call
// goes past the gate or a hook decision.
type fixtureTools struct {
	mu    sync.Mutex
	calls int
	sleep time.Duration
}

func (ft *fixtureTools) ran() int {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.calls
}

func (ft *fixtureTools) Tool(name string) (tool.Tool, bool) {
	t := tool.Tool{Name: name, Description: name, Schema: json.RawMessage(`{"type":"object"}`), Safety: tool.Unsafe}
	switch name {
	case "echo":
		t.Invoke = func(_ context.Context, c tool.Call) (tool.Result, error) {
			ft.mu.Lock()
			ft.calls++
			ft.mu.Unlock()
			return tool.Result{Content: []session.Block{session.TextBlock("echo:" + string(c.Input))}}, nil
		}
	case "sleep":
		t.Invoke = func(ctx context.Context, _ tool.Call) (tool.Result, error) {
			ft.mu.Lock()
			ft.calls++
			d := ft.sleep
			ft.mu.Unlock()
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
			}
			return tool.Result{Content: []session.Block{session.TextBlock("slept")}}, nil
		}
	default:
		return tool.Tool{}, false
	}
	return t, true
}

func (ft *fixtureTools) Tools() []tool.Tool {
	echo, _ := ft.Tool("echo")
	sl, _ := ft.Tool("sleep")
	return []tool.Tool{echo, sl}
}

// newTurnFixture opens a session and returns it with a provider scripted to answer each
// request with one of scripts, and the tool set above. The mode is off so the gate allows
// every unsafe call on its own: what these tests measure is what the hooks and the tool
// timeout do instead, and a hook decision that never fired would show up as a decision
// attributed to the mode rather than to the hook.
func newTurnFixture(t *testing.T, scripts ...[]provider.Part) (*session.Session, *scripted, *fixtureTools) {
	t.Helper()
	return openTestSession(t, session.ModeOff), &scripted{scripts: scripts}, &fixtureTools{}
}

func callThenDone(id, name, input string) [][]provider.Part {
	return [][]provider.Part{
		append(toolCall(id, name, input), usage(10, 2), stop(session.StopToolUse, "tool_calls")),
		{text("done"), usage(3, 1), stop(session.StopEndTurn, "stop")},
	}
}

func TestRunnerFiresHooksInOrder(t *testing.T) {
	// One turn: the model calls the unsafe tool "echo" once, then answers with text.
	script := callThenDone("tu1", "echo", `{"x":1}`)
	s, prov, tools := newTurnFixture(t, script...)
	hooks := &recordingHooks{results: map[plugin.HookPoint][]any{
		plugin.HookBeforeTurn:    {&plugin.BeforeTurnResult{SystemPromptAdditions: []string{"ADD"}}},
		plugin.HookBeforeRequest: {&plugin.BeforeRequestResult{Headers: map[string]string{"X-Test": "1"}}},
		plugin.HookBeforeTool:    {&plugin.BeforeToolResult{Decision: "allow", Reason: "hook says yes"}},
		plugin.HookAfterTool:     {&plugin.AfterToolResult{Content: []session.Block{session.TextBlock("REPLACED")}}},
	}}
	r := NewRunner(Config{Session: s, Provider: prov, Model: provider.Model{Ref: s.Model()}, Tools: tools, Gate: gate.New(nil), System: "BASE", MaxTokens: 10, Hooks: hooks})
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "hi")); err != nil {
		t.Fatal(err)
	}
	want := []plugin.HookPoint{plugin.HookBeforeTurn, plugin.HookBeforeRequest, plugin.HookAfterResponse, plugin.HookBeforeTool, plugin.HookAfterTool, plugin.HookBeforeRequest, plugin.HookAfterResponse, plugin.HookTurnCompleted}
	if got := hooks.points(); !reflect.DeepEqual(got, want) {
		t.Errorf("points %v\nwant   %v", got, want)
	}
	last := hooks.calls[len(hooks.calls)-1]
	tc, ok := last.Payload.(*plugin.TurnCompletedPayload)
	if !ok || tc.Usage.Input != 13 || tc.Usage.Output != 3 || tc.TurnID != r.TurnID() {
		t.Errorf("turn_completed payload %+v", last.Payload)
	}
	if !strings.Contains(prov.requests[0].System, "BASE") || !strings.HasSuffix(prov.requests[0].System, "ADD") {
		t.Errorf("system prompt %q", prov.requests[0].System)
	}
	if prov.requests[0].Headers["X-Test"] != "1" {
		t.Errorf("headers %v", prov.requests[0].Headers)
	}
	// The hook decided; no asker was needed and the decision says so.
	var dec session.PermissionDecision
	for _, e := range s.Entries() {
		if d, ok := e.Payload.(session.PermissionDecision); ok {
			dec = d
		}
	}
	if dec.Decision != session.Allow || dec.DecidedBy != session.ByHook || dec.Reason != "hook says yes" {
		t.Errorf("decision %+v", dec)
	}
	if len(dec.Input) != 0 {
		t.Errorf("a decision no hook modified carries an input: %s", dec.Input)
	}
	if tools.ran() != 1 {
		t.Errorf("tool ran %d times", tools.ran())
	}
	// The second request saw the replacement, the log kept the real result.
	msgs := prov.requests[1].Messages
	lastMsg := msgs[len(msgs)-1]
	if lastMsg.Role != provider.RoleToolResult || lastMsg.Content[0].Text != "REPLACED" {
		t.Errorf("request saw %+v", lastMsg)
	}
	for _, e := range s.Entries() {
		if tr, ok := e.Payload.(session.ToolResult); ok && tr.Content[0].Text == "REPLACED" {
			t.Error("log stored the override")
		}
	}
}

func TestRunnerHookDenyAndModify(t *testing.T) {
	script := callThenDone("tu1", "echo", `{"x":1}`)
	s, prov, tools := newTurnFixture(t, script...)
	hooks := &recordingHooks{results: map[plugin.HookPoint][]any{
		plugin.HookBeforeTool: {&plugin.BeforeToolResult{Decision: "modify", Input: json.RawMessage(`{"x":2}`)}, &plugin.BeforeToolResult{Decision: "deny", Reason: "no"}},
	}}
	r := NewRunner(Config{Session: s, Provider: prov, Model: provider.Model{Ref: s.Model()}, Tools: tools, Gate: gate.New(nil), MaxTokens: 10, Hooks: hooks})
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "hi")); err != nil {
		t.Fatal(err)
	}
	for _, e := range s.Entries() {
		switch p := e.Payload.(type) {
		case session.PermissionDecision:
			if p.Decision != session.Deny || p.DecidedBy != session.ByHook || p.Reason != "no" {
				t.Errorf("decision %+v", p)
			}
			// The modify still happened, so the log records the bytes the call carried.
			if string(p.Input) != `{"x":2}` {
				t.Errorf("decision input %s, want the modified bytes verbatim", p.Input)
			}
		case session.ToolResult:
			if p.Outcome != session.OutcomeError || !strings.Contains(p.Content[0].Text, "denied: no") {
				t.Errorf("result %+v", p)
			}
		}
	}
	if tools.ran() != 0 {
		t.Error("tool ran despite deny")
	}
	// after_tool fires on the denied result too: the contract is the append, not the run.
	want := []plugin.HookPoint{plugin.HookBeforeTurn, plugin.HookBeforeRequest, plugin.HookAfterResponse, plugin.HookBeforeTool, plugin.HookAfterTool, plugin.HookBeforeRequest, plugin.HookAfterResponse, plugin.HookTurnCompleted}
	if got := hooks.points(); !reflect.DeepEqual(got, want) {
		t.Errorf("points %v\nwant   %v", got, want)
	}
	if got := string(hooks.calls[3].Payload.(*plugin.BeforeToolPayload).Input); got != `{"x":1}` {
		t.Errorf("before_tool saw input %s", got)
	}
}

func TestRunnerHookModifyReachesTheTool(t *testing.T) {
	script := callThenDone("tu1", "echo", `{"x":1}`)
	s, prov, tools := newTurnFixture(t, script...)
	hooks := &recordingHooks{results: map[plugin.HookPoint][]any{
		plugin.HookBeforeTool: {
			&plugin.BeforeToolResult{Decision: "modify", Input: json.RawMessage(`not json`)},
			&plugin.BeforeToolResult{Decision: "modify", Input: json.RawMessage(`{"x": 2}`)},
			&plugin.BeforeToolResult{Decision: "allow"},
		},
	}}
	r := NewRunner(Config{Session: s, Provider: prov, Model: provider.Model{Ref: s.Model()}, Tools: tools, Gate: gate.New(nil), MaxTokens: 10, Hooks: hooks})
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "hi")); err != nil {
		t.Fatal(err)
	}
	var res session.ToolResult
	var dec session.PermissionDecision
	for _, e := range s.Entries() {
		switch p := e.Payload.(type) {
		case session.ToolResult:
			res = p
		case session.PermissionDecision:
			dec = p
		}
	}
	// The invalid modify passed, the valid one replaced the input byte for byte.
	if res.Content[0].Text != `echo:{"x": 2}` {
		t.Errorf("tool saw %q", res.Content[0].Text)
	}
	if dec.Decision != session.Allow || dec.DecidedBy != session.ByHook || dec.Reason != "hook" {
		t.Errorf("decision %+v", dec)
	}
}

func TestRunnerToolTimeout(t *testing.T) {
	script := callThenDone("tu1", "sleep", `{}`)
	s, prov, tools := newTurnFixture(t, script...)
	tools.sleep = 200 * time.Millisecond
	r := NewRunner(Config{Session: s, Provider: prov, Model: provider.Model{Ref: s.Model()}, Tools: tools, Gate: gate.New(nil), MaxTokens: 10, ToolTimeout: 20 * time.Millisecond})
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "hi")); err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, e := range s.Entries() {
		if tr, ok := e.Payload.(session.ToolResult); ok {
			seen = true
			if tr.Outcome != session.OutcomeError || !strings.Contains(tr.Content[0].Text, "timed out after 20ms") {
				t.Errorf("result %+v", tr)
			}
		}
	}
	if !seen {
		t.Fatal("no tool result")
	}
}

// TestRunnerOverridesOutliveTheTurn covers the after_tool replacement staying in place for
// every later request: a redaction that lapsed at the turn boundary would put the secret back
// in the next request.
func TestRunnerOverridesOutliveTheTurn(t *testing.T) {
	s, prov, tools := newTurnFixture(t,
		append(toolCall("tu1", "echo", `{"x":1}`), stop(session.StopToolUse, "tool_calls")),
		[]provider.Part{text("done"), stop(session.StopEndTurn, "stop")},
		[]provider.Part{text("still done"), stop(session.StopEndTurn, "stop")},
	)
	hooks := &recordingHooks{results: map[plugin.HookPoint][]any{
		plugin.HookAfterTool: {&plugin.AfterToolResult{Content: []session.Block{session.TextBlock("REDACTED")}}},
	}}
	r := NewRunner(Config{Session: s, Provider: prov, Model: provider.Model{Ref: s.Model()}, Tools: tools, Gate: gate.New(nil), MaxTokens: 10, Hooks: hooks})
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "hi")); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "again")); err != nil {
		t.Fatal(err)
	}
	if len(prov.requests) != 3 {
		t.Fatalf("%d requests", len(prov.requests))
	}
	for i, req := range prov.requests[1:] {
		found := false
		for _, m := range req.Messages {
			if m.Role == provider.RoleToolResult {
				found = true
				if m.Content[0].Text != "REDACTED" {
					t.Errorf("request %d carried %q", i+1, m.Content[0].Text)
				}
			}
		}
		if !found {
			t.Errorf("request %d has no tool result", i+1)
		}
	}
}

// TestRunnerSharesTheServerOverrideMap covers Config.Overrides being the caller's map: the
// server keeps one per session and every runner it builds writes into that one.
func TestRunnerSharesTheServerOverrideMap(t *testing.T) {
	s, prov, tools := newTurnFixture(t, callThenDone("tu1", "echo", `{"x":1}`)...)
	shared := map[string][]session.Block{}
	hooks := &recordingHooks{results: map[plugin.HookPoint][]any{
		plugin.HookAfterTool: {&plugin.AfterToolResult{Content: []session.Block{session.TextBlock("REDACTED")}}},
	}}
	r := NewRunner(Config{Session: s, Provider: prov, Model: provider.Model{Ref: s.Model()}, Tools: tools, Gate: gate.New(nil), MaxTokens: 10, Hooks: hooks, Overrides: shared})
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "hi")); err != nil {
		t.Fatal(err)
	}
	got, ok := shared["tu1"]
	if !ok || got[0].Text != "REDACTED" {
		t.Errorf("shared map holds %v", shared)
	}
}

// cancelOnDelta cancels the turn as soon as the provider has streamed anything, so the cancel
// lands mid-stream where the runner appends the partial assistant message.
type cancelOnDelta struct {
	*recorder
	once   sync.Once
	cancel context.CancelFunc
}

func (o *cancelOnDelta) Delta(id string, p provider.Part) {
	o.recorder.Delta(id, p)
	o.once.Do(o.cancel)
}

// TestCancelledTurnStillFiresAfterResponse covers the hooks on the cancel paths: the
// interrupted assistant_message and the killed tool_result are appended with the turn's own
// context, which is already done, and a handler handed a dead context is skipped by the
// HookRunner with a false "timed out" notice. The fire has to happen on a live, bounded
// context instead.
func TestCancelledTurnStillFiresAfterResponse(t *testing.T) {
	s := openTestSession(t, session.ModeOff)
	p := &scripted{scripts: [][]provider.Part{{text("partial"), {Type: pause}}}, release: make(chan struct{})}
	hooks := &recordingHooks{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	obs := &cancelOnDelta{recorder: &recorder{}, cancel: cancel}
	r := NewRunner(Config{Session: s, Provider: p, Model: provider.Model{Ref: s.Model()}, Tools: toolSet{}, Gate: gate.New(nil), Observer: obs, MaxTokens: 10, Hooks: hooks})
	if err := r.Run(ctx, userMsg(session.SourceTyped, "hi")); !errors.Is(err, context.Canceled) {
		t.Fatalf("run = %v, want context canceled", err)
	}
	f, ok := hooks.firedAt(plugin.HookAfterResponse)
	if !ok {
		t.Fatalf("after_response never fired: %v", hooks.points())
	}
	if f.err != nil {
		t.Errorf("after_response fired on a dead context: %v", f.err)
	}
	if !f.deadline {
		t.Error("the replacement context must be bounded")
	}
}

func TestRunnerStepLimit(t *testing.T) {
	// The model calls a tool forever; MaxSteps 3 ends the turn as failed after three requests.
	call := func(id string) []provider.Part {
		return append(toolCall(id, "echo", `{}`), usage(1, 1), stop(session.StopToolUse, "tool_calls"))
	}
	s, prov, tools := newTurnFixture(t, call("tu1"), call("tu2"), call("tu3"), call("tu4"))
	r := NewRunner(Config{Session: s, Provider: prov, Model: provider.Model{Ref: s.Model()}, Tools: tools, Gate: gate.New(nil), MaxTokens: 10, MaxSteps: 3})
	err := r.Run(context.Background(), session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("go")}})
	if err == nil || r.State() != Failed || len(prov.requests) != 3 {
		t.Errorf("err %v state %s requests %d", err, r.State(), len(prov.requests))
	}
	last := s.Entries()[len(s.Entries())-1].Payload.(session.TurnFailed)
	if last.Class != session.ErrInternal || !strings.Contains(last.Message, "step limit 3 reached") {
		t.Errorf("turn_failed %+v", last)
	}
}

func TestAccumulatorFoldsThinkingSignature(t *testing.T) {
	a := newAccumulator()
	a.add(provider.Part{Type: provider.PartThinkingDelta, Text: "a"})
	a.add(provider.Part{Type: provider.PartThinkingDelta, Text: "b"})
	a.add(provider.Part{Type: provider.PartThinkingSignature, Signature: "SIG"})
	want := []session.Block{{Type: session.BlockThinking, Text: "ab", Signature: "SIG"}}
	if got := a.blocks(); !reflect.DeepEqual(got, want) {
		t.Fatalf("blocks %+v\nwant   %+v", got, want)
	}
}

func TestAccumulatorSignatureWithoutThinkingOpensABlock(t *testing.T) {
	a := newAccumulator()
	a.add(provider.Part{Type: provider.PartTextDelta, Text: "hi"})
	a.add(provider.Part{Type: provider.PartThinkingSignature, Signature: "SIG"})
	want := []session.Block{
		{Type: session.BlockText, Text: "hi"},
		{Type: session.BlockThinking, Signature: "SIG"},
	}
	if got := a.blocks(); !reflect.DeepEqual(got, want) {
		t.Fatalf("blocks %+v\nwant   %+v", got, want)
	}
}
