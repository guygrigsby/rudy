package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type hookPlugin struct {
	name string
	hs   []HookHandler
}

func (p hookPlugin) Name() string { return p.name }
func (p hookPlugin) Init(ctx context.Context, h Host) error {
	for _, hh := range p.hs {
		if err := h.RegisterHook(hh); err != nil {
			return err
		}
	}
	return nil
}

func TestHooksOrderTimeoutAndPanic(t *testing.T) {
	var notices []string
	r := NewRegistry(nil, func(s string) { notices = append(notices, s) })
	pass := func(ctx context.Context, c HookCall) (any, error) { return nil, nil }
	r.Load(context.Background(),
		hookPlugin{"b", []HookHandler{
			{Point: HookBeforeTurn, Priority: 10, Handle: func(ctx context.Context, c HookCall) (any, error) {
				return &BeforeTurnResult{SystemPromptAdditions: []string{"b10"}}, nil
			}},
			{Point: HookBeforeTurn, Priority: 0, Handle: func(ctx context.Context, c HookCall) (any, error) {
				<-ctx.Done()
				return &BeforeTurnResult{SystemPromptAdditions: []string{"late"}}, ctx.Err()
			}},
		}},
		hookPlugin{"a", []HookHandler{
			{Point: HookBeforeTurn, Priority: 0, Handle: func(ctx context.Context, c HookCall) (any, error) {
				return &BeforeTurnResult{SystemPromptAdditions: []string{"a0"}}, nil
			}},
			{Point: HookBeforeTurn, Priority: 5, Handle: func(ctx context.Context, c HookCall) (any, error) { panic("boom") }},
			{Point: HookAfterTool, Handle: pass},
		}},
		hookPlugin{"bad", []HookHandler{{Point: "nope", Handle: pass}}},
	)
	if got := r.Statuses()[2]; got.State != StateFailed || !strings.Contains(got.Reason, "unknown hook point") {
		t.Errorf("bad plugin status %+v", got)
	}
	hooks := r.Hooks(HookBeforeTurn)
	if len(hooks) != 4 || hooks[0].Owner != "b" || hooks[1].Owner != "a" || hooks[2].Owner != "a" || hooks[3].Owner != "b" {
		t.Fatalf("order: %+v", hooks)
	}
	runner := NewHookRunner(r, 20*time.Millisecond, func(s string) { notices = append(notices, s) })
	results := runner.Fire(context.Background(), HookCall{Point: HookBeforeTurn, SessionID: "s", TurnID: "t"})
	var adds []string
	for _, res := range results {
		adds = append(adds, res.(*BeforeTurnResult).SystemPromptAdditions...)
	}
	if strings.Join(adds, ",") != "a0,b10" {
		t.Errorf("results %v", adds)
	}
	joined := strings.Join(notices, "\n")
	if !strings.Contains(joined, "hook before_turn: b: ") || !strings.Contains(joined, "hook before_turn: a: panic: boom") {
		t.Errorf("notices:\n%s", joined)
	}
	if got := runner.Fire(context.Background(), HookCall{Point: HookSessionClosed}); len(got) != 0 {
		t.Errorf("no handlers, got %v", got)
	}
}

func TestResultFor(t *testing.T) {
	if _, ok := ResultFor(HookBeforeTool).(*BeforeToolResult); !ok {
		t.Error("before_tool result type")
	}
	if ResultFor(HookAfterResponse) != nil {
		t.Error("after_response returns nothing")
	}
	var _ = errors.New
}
