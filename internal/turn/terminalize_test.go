package turn

import (
	"context"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// danglingIn is every tool_use of these entries with no tool_result, which is what a request
// to the provider would carry as an unanswered call.
func danglingIn(entries []session.Entry) []string {
	var out []string
	for _, b := range session.PendingToolUsesIn(entries) {
		out = append(out, b.ID)
	}
	return out
}

// undecidedIn is every tool_use with no permission_decision.
func undecidedIn(entries []session.Entry) []string {
	decided := map[string]bool{}
	var uses []session.Block
	for _, e := range entries {
		switch v := e.Payload.(type) {
		case session.AssistantMessage:
			for _, b := range v.Content {
				if b.Type == session.BlockToolUse {
					uses = append(uses, b)
				}
			}
		case session.PermissionDecision:
			decided[v.ToolUseID] = true
		}
	}
	var out []string
	for _, b := range uses {
		if !decided[b.ID] {
			out = append(out, b.ID)
		}
	}
	return out
}

// TestACancelledTurnLeavesNoDanglingCall is the log invariant every later request rests on:
// when a Turn is cut, every call it admitted has a decision and a result before the Entry
// that ends the Turn, and none of them is a success.
func TestACancelledTurnLeavesNoDanglingCall(t *testing.T) {
	s := openTestSession(t, session.ModeOff)
	rec := &recorder{}
	started := make(chan struct{}, 2)
	blocked := make(chan struct{})
	slow := tool.Tool{Name: "slow", Description: "blocks", Schema: []byte(`{"type":"object"}`), Safety: tool.Safe,
		Invoke: func(ctx context.Context, _ tool.Call) (tool.Result, error) {
			started <- struct{}{}
			select {
			case <-ctx.Done():
				return tool.Result{}, ctx.Err()
			case <-blocked:
				return tool.Result{Content: []session.Block{session.TextBlock("late")}}, nil
			}
		}}
	p := &scripted{scripts: [][]provider.Part{
		append(append(toolCall("tu_a", "slow", `{"x":1}`), toolCall("tu_b", "slow", `{"y":2}`)...),
			stop(session.StopToolUse, "tool_use")),
		{text("done"), stop(session.StopEndTurn, "end_turn")},
	}}
	r := newRunner(t, s, p, toolSet{"slow": slow}, nil, rec)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, userMsg(session.SourceTyped, "go")) }()
	for range 2 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("both calls should have started")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the cut turn never returned")
	}
	close(blocked)

	entries := s.Entries()
	if got := danglingIn(entries); len(got) != 0 {
		t.Errorf("calls with no result after the cut: %v", got)
	}
	if got := undecidedIn(entries); len(got) != 0 {
		t.Errorf("calls with no decision after the cut: %v", got)
	}
	if got := danglingIn(s.RequestContext()); len(got) != 0 {
		t.Errorf("the next request would carry unanswered calls: %v", got)
	}
	for _, e := range entries {
		if tr, ok := e.Payload.(session.ToolResult); ok && tr.Outcome == session.OutcomeOK {
			t.Errorf("call %s reported success after the turn was cut", tr.ToolUseID)
		}
	}
}

// TestAFailedTurnNamesTheCallThatCausedIt is the other half: the causal call gets an error
// result carrying what went wrong, its peers get killed, and turn_failed comes after both.
func TestAFailedTurnNamesTheCallThatCausedIt(t *testing.T) {
	s := openTestSession(t, session.ModeOff)
	rec := &recorder{}
	panics := tool.Tool{Name: "boom", Description: "panics", Schema: []byte(`{"type":"object"}`), Safety: tool.Safe,
		Invoke: func(context.Context, tool.Call) (tool.Result, error) { panic("plugin fault") }}
	blocked := make(chan struct{})
	slow := tool.Tool{Name: "slow", Description: "blocks", Schema: []byte(`{"type":"object"}`), Safety: tool.Safe,
		Invoke: func(ctx context.Context, _ tool.Call) (tool.Result, error) {
			<-ctx.Done()
			close(blocked)
			return tool.Result{}, ctx.Err()
		}}
	p := &scripted{scripts: [][]provider.Part{
		append(append(toolCall("tu_boom", "boom", `{"x":1}`), toolCall("tu_slow", "slow", `{"y":2}`)...),
			stop(session.StopToolUse, "tool_use")),
	}}
	r := newRunner(t, s, p, toolSet{"boom": panics, "slow": slow}, nil, rec)
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = r.Interrupt(session.InterruptCancel) // release the peer so the turn can end
	}()
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "go")); err == nil {
		t.Fatal("a panicking tool did not fail the turn")
	}
	<-blocked

	entries := s.Entries()
	if got := danglingIn(entries); len(got) != 0 {
		t.Errorf("calls with no result after the failure: %v", got)
	}
	var causal, peer session.ToolResult
	for _, e := range entries {
		if tr, ok := e.Payload.(session.ToolResult); ok {
			switch tr.ToolUseID {
			case "tu_boom":
				causal = tr
			case "tu_slow":
				peer = tr
			}
		}
	}
	if causal.Outcome != session.OutcomeError {
		t.Errorf("the causal call's result = %s, want error", causal.Outcome)
	}
	if peer.Outcome != session.OutcomeKilled {
		t.Errorf("the peer's result = %s, want killed", peer.Outcome)
	}
	last := entries[len(entries)-1]
	if _, ok := last.Payload.(session.TurnFailed); !ok {
		t.Errorf("last entry = %s, want turn_failed after every call was closed out", last.Kind)
	}
}

// TestACallThePoolRefusesStillGetsAResult is the gap the sweep exists for: a call whose
// decision is already in the log and which then never runs, because the pool closed under
// it. Without the sweep that tool_use would sit unanswered in every later request.
func TestACallThePoolRefusesStillGetsAResult(t *testing.T) {
	s := openTestSession(t, session.ModeOff)
	rec := &recorder{}
	p := &scripted{scripts: [][]provider.Part{
		append(toolCall("tu1", "echo", `{"x":1}`), stop(session.StopToolUse, "tool_use")),
	}}
	sched := NewScheduler()
	sched.Close()
	r := newRunner(t, s, p, toolSet{"echo": echoTool(tool.Safe, "echo")}, nil, rec)
	r.cfg.Scheduler = sched

	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "go")); err == nil {
		t.Fatal("a turn whose calls cannot run should fail")
	}
	entries := s.Entries()
	if got := danglingIn(entries); len(got) != 0 {
		t.Errorf("calls with no result: %v", got)
	}
	var res session.ToolResult
	for _, e := range entries {
		if tr, ok := e.Payload.(session.ToolResult); ok && tr.ToolUseID == "tu1" {
			res = tr
		}
	}
	if res.Outcome != session.OutcomeError {
		t.Errorf("the refused call's result = %q, want error", res.Outcome)
	}
}
