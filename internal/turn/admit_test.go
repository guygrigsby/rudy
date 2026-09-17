package turn

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// calls builds one assistant message asking for each of the given tool calls, the shape a
// hostile provider uses to make the runner allocate: every block arrives in one response.
func callParts(blocks ...[]provider.Part) []provider.Part {
	var out []provider.Part
	for _, b := range blocks {
		out = append(out, b...)
	}
	return append(out, stop(session.StopToolUse, "tool_use"))
}

// manyCalls is n well formed calls on one tool, ids call-0 upward.
func manyCalls(n int, name string) []provider.Part {
	var parts []provider.Part
	for i := range n {
		parts = append(parts, toolCall(idFor(i), name, `{"x":1}`)...)
	}
	return append(parts, stop(session.StopToolUse, "tool_use"))
}

func idFor(i int) string { return "call-" + strings.Repeat("0", 3-len(itoa(i))) + itoa(i) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// nest returns a JSON object nested depth levels deep, for the strict-JSON profile bounds.
func nest(depth int) string {
	return `{"a":` + strings.Repeat(`{"a":`, depth-1) + "1" + strings.Repeat("}", depth-1) + "}"
}

// admitCase is one hostile provider response and what the Turn owes for it.
type admitCase struct {
	name    string
	scripts [][]provider.Part
	// entries the log may hold when the refusal lands, beyond the opening user_message and
	// the turn_failed: a first response that was admitted leaves its own.
	wantAssistant int
	wantRan       bool
}

// TestAProviderResponseOverItsLimitsRunsNothing covers every bound ADR 0034 and the
// terminal-durability contract put on one provider response. None of these may append an
// assistant Entry, create a permission question or start a call: the response is refused
// whole, before any of that, and the Turn fails.
func TestAProviderResponseOverItsLimitsRunsNothing(t *testing.T) {
	big := strings.Repeat("x", 4097)
	for _, tc := range []admitCase{
		{name: "65 calls in one response", scripts: [][]provider.Part{manyCalls(65, "echo")}},
		{
			name: "65 calls across two responses in one turn",
			scripts: [][]provider.Part{
				manyCalls(40, "echo"),
				manyCalls(25, "echo"),
			},
			wantAssistant: 1, wantRan: true,
		},
		{name: "invalid utf8 tool use id", scripts: [][]provider.Part{callParts(toolCall("call-\xff", "echo", `{"x":1}`))}},
		{name: "invalid utf8 tool name", scripts: [][]provider.Part{callParts(toolCall("call-1", "echo\xff", `{"x":1}`))}},
		{name: "tool use id over 4096 bytes", scripts: [][]provider.Part{callParts(toolCall(big, "echo", `{"x":1}`))}},
		{name: "tool name over 4096 bytes", scripts: [][]provider.Part{callParts(toolCall("call-1", big, `{"x":1}`))}},
		{name: "duplicate ids in one response", scripts: [][]provider.Part{callParts(
			toolCall("call-1", "echo", `{"x":1}`),
			toolCall("call-1", "echo", `{"x":2}`),
		)}},
		{
			name: "an id reused by a later response in the same turn",
			scripts: [][]provider.Part{
				callParts(toolCall("call-1", "echo", `{"x":1}`)),
				callParts(toolCall("call-1", "echo", `{"x":2}`)),
			},
			wantAssistant: 1, wantRan: true,
		},
		{name: "duplicate keys at depth", scripts: [][]provider.Part{callParts(toolCall("call-1", "echo", `{"a":{"b":1,"b":2}}`))}},
		{name: "input nested past the profile depth", scripts: [][]provider.Part{callParts(toolCall("call-1", "echo", nest(65)))}},
		{name: "input array over the profile bound", scripts: [][]provider.Part{callParts(toolCall("call-1", "echo",
			`{"a":[`+strings.TrimSuffix(strings.Repeat("1,", 16385), ",")+`]}`))}},
		{name: "input that is not an object", scripts: [][]provider.Part{callParts(toolCall("call-1", "echo", `[1,2,3]`))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestSession(t, session.ModeOff)
			rec := &recorder{}
			p := &scripted{scripts: tc.scripts}
			r := newRunner(t, s, p, toolSet{"echo": echoTool(tool.Safe, "echo")}, nil, rec)
			err := r.Run(context.Background(), userMsg(session.SourceTyped, "go"))
			if err == nil {
				t.Fatal("a response over its limits was accepted")
			}
			if got := rec.count(session.KindAssistantMessage); got != tc.wantAssistant {
				t.Errorf("assistant entries = %d, want %d: the refused response must not append", got, tc.wantAssistant)
			}
			if got := rec.count(session.KindTurnFailed); got != 1 {
				t.Errorf("turn_failed entries = %d, want 1", got)
			}
			ran := rec.count(session.KindToolResult) > 0
			if ran != tc.wantRan {
				t.Errorf("tool results present = %v, want %v: a refused response runs no prefix of itself", ran, tc.wantRan)
			}
			if strings.Contains(failureText(rec), "xxxx") {
				t.Error("the refusal repeated the provider's bytes")
			}
		})
	}
}

// failureText is the message of the turn_failed entry the recorder saw, if any.
func failureText(rec *recorder) string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, e := range rec.entries {
		if f, ok := e.Payload.(session.TurnFailed); ok {
			return f.Message
		}
	}
	return ""
}

// TestAToolInputOverSevenMiBIsRefused is the per-input byte bound on its own, at the real
// contract size rather than a lowered one, since it is the bound a single hostile response
// reaches first.
func TestAToolInputOverSevenMiBIsRefused(t *testing.T) {
	s := openTestSession(t, session.ModeOff)
	rec := &recorder{}
	input := `{"a":"` + strings.Repeat("x", 7<<20) + `"}`
	p := &scripted{scripts: [][]provider.Part{callParts(toolCall("call-1", "echo", input))}}
	r := newRunner(t, s, p, toolSet{"echo": echoTool(tool.Safe, "echo")}, nil, rec)
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "go")); err == nil {
		t.Fatal("a 7 MiB tool input was accepted")
	}
	if got := rec.count(session.KindAssistantMessage); got != 0 {
		t.Errorf("assistant entries = %d, want 0", got)
	}
	if got := rec.count(session.KindToolResult); got != 0 {
		t.Errorf("tool results = %d, want 0", got)
	}
}

// TestTheTurnsInputBudgetsAreCumulative covers the two budgets a single response cannot
// reach: the aggregate across one response and the retained total across the Turn. The
// runner's own limits are lowered for the test, because proving 16 MiB and 32 MiB with real
// bytes costs a minute of allocation to assert arithmetic the contract already fixes.
func TestTheTurnsInputBudgetsAreCumulative(t *testing.T) {
	fill := func(n int) string { return `{"a":"` + strings.Repeat("x", n) + `"}` }
	for _, tc := range []struct {
		name          string
		scripts       [][]provider.Part
		wantAssistant int
	}{
		{
			name: "aggregate input of one response",
			scripts: [][]provider.Part{callParts(
				toolCall("call-1", "echo", fill(600)),
				toolCall("call-2", "echo", fill(600)),
			)},
		},
		{
			name: "retained input across the turn",
			scripts: [][]provider.Part{
				callParts(toolCall("call-1", "echo", fill(900))),
				callParts(toolCall("call-2", "echo", fill(900))),
			},
			wantAssistant: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestSession(t, session.ModeOff)
			rec := &recorder{}
			r := newRunner(t, s, &scripted{scripts: tc.scripts}, toolSet{"echo": echoTool(tool.Safe, "echo")}, nil, rec)
			r.limits.input = 1000
			r.limits.response = 1000
			r.limits.turn = 1500
			if err := r.Run(context.Background(), userMsg(session.SourceTyped, "go")); err == nil {
				t.Fatal("a response over the turn's input budget was accepted")
			}
			if got := rec.count(session.KindAssistantMessage); got != tc.wantAssistant {
				t.Errorf("assistant entries = %d, want %d", got, tc.wantAssistant)
			}
		})
	}
}

// TestAnAcceptedResponseStillRuns is the other side of the bounds: the ordinary case must be
// untouched by them.
func TestAnAcceptedResponseStillRuns(t *testing.T) {
	s := openTestSession(t, session.ModeOff)
	rec := &recorder{}
	p := &scripted{scripts: [][]provider.Part{
		manyCalls(64, "echo"),
		{text("done"), stop(session.StopEndTurn, "end_turn")},
	}}
	r := newRunner(t, s, p, toolSet{"echo": echoTool(tool.Safe, "echo")}, nil, rec)
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "go")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := rec.count(session.KindToolResult); got != 64 {
		t.Errorf("tool results = %d, want 64: the bound is 64 calls, not 63", got)
	}
	if got := rec.count(session.KindTurnFailed); got != 0 {
		t.Errorf("turn_failed entries = %d, want 0", got)
	}
}

// TestAMalformedInputStillRefusesOnlyItsOwnCall keeps the existing behavior for a truncated
// tool input, which is an interrupted stream rather than a hostile response: that call is
// refused and its peers still run.
func TestAMalformedInputStillRefusesOnlyItsOwnCall(t *testing.T) {
	s := openTestSession(t, session.ModeOff)
	rec := &recorder{}
	p := &scripted{scripts: [][]provider.Part{
		callParts(
			toolCall("call-1", "echo", `{"x":`),
			toolCall("call-2", "echo", `{"x":1}`),
		),
		{text("done"), stop(session.StopEndTurn, "end_turn")},
	}}
	r := newRunner(t, s, p, toolSet{"echo": echoTool(tool.Safe, "echo")}, nil, rec)
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "go")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := rec.count(session.KindAssistantMessage); got != 2 {
		t.Errorf("assistant entries = %d, want 2", got)
	}
	var refused, ran int
	rec.mu.Lock()
	for _, e := range rec.entries {
		if tr, ok := e.Payload.(session.ToolResult); ok {
			if tr.Outcome == session.OutcomeError {
				refused++
			} else {
				ran++
			}
		}
	}
	rec.mu.Unlock()
	if refused != 1 || ran != 1 {
		t.Errorf("refused %d and ran %d, want one of each", refused, ran)
	}
}

// TestTheTurnsBudgetsAreForgottenWhenItEnds proves the call count and the byte counter belong
// to one Turn: a second turn on the same session starts from zero rather than inheriting what
// the first one spent. The session log keeps tool_use ids unique across the whole session, so
// the ids differ; what is reused here is the budget.
func TestTheTurnsBudgetsAreForgottenWhenItEnds(t *testing.T) {
	s := openTestSession(t, session.ModeOff)
	rec := &recorder{}
	p := &scripted{scripts: [][]provider.Part{
		callParts(toolCall("a-1", "echo", `{"x":1}`), toolCall("a-2", "echo", `{"x":1}`)),
		{text("done"), stop(session.StopEndTurn, "end_turn")},
		callParts(toolCall("b-1", "echo", `{"x":1}`), toolCall("b-2", "echo", `{"x":1}`)),
		{text("done"), stop(session.StopEndTurn, "end_turn")},
	}}
	r := newRunner(t, s, p, toolSet{"echo": echoTool(tool.Safe, "echo")}, nil, rec)
	r.limits.calls = 2
	r.limits.turn = 40
	for i := range 2 {
		if err := r.Run(context.Background(), userMsg(session.SourceTyped, "go")); err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
	}
	if got := rec.count(session.KindToolResult); got != 4 {
		t.Errorf("tool results = %d, want 4: the second turn spent its own budget", got)
	}
	if got := rec.count(session.KindTurnFailed); got != 0 {
		t.Errorf("turn_failed entries = %d, want 0", got)
	}
}

// decisionFor is the permission_decision the log kept for one tool_use.
func decisionFor(t *testing.T, s *session.Session, id string) session.PermissionDecision {
	t.Helper()
	for _, e := range s.Entries() {
		if d, ok := e.Payload.(session.PermissionDecision); ok && d.ToolUseID == id {
			return d
		}
	}
	t.Fatalf("no permission_decision for %q", id)
	return session.PermissionDecision{}
}

// TestABeforeToolReplacementTakesTheSameAdmission holds a hook's replacement to the rules the
// provider's own input takes. A handler is trusted to change an input, not to put bytes past
// the Turn's limits into the log and in front of the operator.
func TestABeforeToolReplacementTakesTheSameAdmission(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"duplicate keys", `{"a":1,"a":2}`},
		{"not an object", `[1,2,3]`},
		{"past the profile depth", nest(65)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, prov, tools := newTurnFixture(t, callThenDone("tu1", "echo", `{"x":1}`)...)
			hooks := &recordingHooks{results: map[plugin.HookPoint][]any{
				plugin.HookBeforeTool: {&plugin.BeforeToolResult{Decision: "modify", Input: json.RawMessage(tc.input)}},
			}}
			r := NewRunner(Config{Session: s, Provider: prov, Model: provider.Model{Ref: s.Model()}, Tools: tools, Gate: gate.New(nil), MaxTokens: 10, Hooks: hooks})
			if err := r.Run(context.Background(), userMsg(session.SourceTyped, "hi")); err != nil {
				t.Fatal(err)
			}
			dec := decisionFor(t, s, "tu1")
			if dec.Decision != session.Deny || dec.Reason != "hook input refused" {
				t.Errorf("decision = %s by %s reason %q, want a fixed hook denial", dec.Decision, dec.DecidedBy, dec.Reason)
			}
			if len(dec.Input) != 0 {
				t.Errorf("the refused replacement reached the log: %s", dec.Input)
			}
		})
	}
}

// TestAnAcceptedReplacementIsChargedToTheTurn proves the accepted replacement counts against
// the Turn's retained input budget, which is what stops a handler from being the way around
// it.
func TestAnAcceptedReplacementIsChargedToTheTurn(t *testing.T) {
	s, prov, tools := newTurnFixture(t, callThenDone("tu1", "echo", `{"x":1}`)...)
	big := `{"a":"` + strings.Repeat("y", 400) + `"}`
	hooks := &recordingHooks{results: map[plugin.HookPoint][]any{
		plugin.HookBeforeTool: {&plugin.BeforeToolResult{Decision: "modify", Input: json.RawMessage(big)}},
	}}
	r := NewRunner(Config{Session: s, Provider: prov, Model: provider.Model{Ref: s.Model()}, Tools: tools, Gate: gate.New(nil), MaxTokens: 10, Hooks: hooks})
	r.limits.turn = 100
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "hi")); err != nil {
		t.Fatal(err)
	}
	dec := decisionFor(t, s, "tu1")
	if dec.Decision != session.Deny {
		t.Errorf("decision = %s, want deny: the replacement was over the turn's remaining budget", dec.Decision)
	}
}

// TestADecisionReasonIsBounded covers both reasons a caller can write into the log: a hook's
// and an asker's. Neither may put invalid UTF-8 or an unbounded string in front of every
// subscriber, and neither is truncated, which would publish most of it anyway.
func TestADecisionReasonIsBounded(t *testing.T) {
	long := strings.Repeat("r", 4097)
	t.Run("hook", func(t *testing.T) {
		s, prov, tools := newTurnFixture(t, callThenDone("tu1", "echo", `{"x":1}`)...)
		hooks := &recordingHooks{results: map[plugin.HookPoint][]any{
			plugin.HookBeforeTool: {&plugin.BeforeToolResult{Decision: "deny", Reason: long}},
		}}
		r := NewRunner(Config{Session: s, Provider: prov, Model: provider.Model{Ref: s.Model()}, Tools: tools, Gate: gate.New(nil), MaxTokens: 10, Hooks: hooks})
		if err := r.Run(context.Background(), userMsg(session.SourceTyped, "hi")); err != nil {
			t.Fatal(err)
		}
		if got := decisionFor(t, s, "tu1").Reason; got != "hook" {
			t.Errorf("reason kept %d bytes, want the fixed word", len(got))
		}
	})
	t.Run("asker", func(t *testing.T) {
		s := openTestSession(t, session.ModeStrict)
		prov := &scripted{scripts: callThenDone("tu1", "danger", `{"x":1}`)}
		rec := &recorder{}
		asker := askerFunc(func(context.Context, Question) (Answer, error) {
			return Answer{Decision: session.Allow, Scope: session.ScopeOnce, Reason: "bad\xffreason"}, nil
		})
		r := newRunner(t, s, prov, toolSet{"danger": echoTool(tool.Unsafe, "danger")}, asker, rec)
		if err := r.Run(context.Background(), userMsg(session.SourceTyped, "hi")); err != nil {
			t.Fatal(err)
		}
		if got := decisionFor(t, s, "tu1").Reason; got != "asker" {
			t.Errorf("reason = %q, want the fixed word for an answer that is not valid UTF-8", got)
		}
	})
}

// TestAQuestionThatCannotBePublishedDeniesAndFailsTheTurn is the runner's half of the
// permission preflight. Nobody was asked, so nobody can answer: the call is denied by the
// Server itself and the Turn fails rather than waiting on a question that does not exist.
func TestAQuestionThatCannotBePublishedDeniesAndFailsTheTurn(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	rec := &recorder{}
	prov := &scripted{scripts: callThenDone("tu1", "danger", `{"x":1}`)}
	asker := askerFunc(func(context.Context, Question) (Answer, error) {
		return Answer{}, ErrPermissionOversized
	})
	r := newRunner(t, s, prov, toolSet{"danger": echoTool(tool.Unsafe, "danger")}, asker, rec)
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "go")); err == nil {
		t.Fatal("the turn survived a question it could not ask")
	}
	dec := decisionFor(t, s, "tu1")
	if dec.Decision != session.Deny || dec.DecidedBy != session.ByInvariant {
		t.Errorf("decision = %s by %s, want a deny by invariant", dec.Decision, dec.DecidedBy)
	}
	if got := rec.count(session.KindTurnFailed); got != 1 {
		t.Errorf("turn_failed entries = %d, want 1", got)
	}
	var ran bool
	for _, e := range s.Entries() {
		if tr, ok := e.Payload.(session.ToolResult); ok && tr.Outcome == session.OutcomeOK {
			ran = true
		}
	}
	if ran {
		t.Error("a call ran without the consent nobody was asked for")
	}
}
