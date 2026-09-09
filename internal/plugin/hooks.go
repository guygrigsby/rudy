package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// HookPoint is one of the nine places the kernel lets a plugin see or change what it is
// about to do. The set is closed: a plugin cannot invent a point.
type HookPoint string

const (
	HookSessionOpened    HookPoint = "session_opened"
	HookBeforeTurn       HookPoint = "before_turn"
	HookBeforeRequest    HookPoint = "before_request"
	HookAfterResponse    HookPoint = "after_response"
	HookBeforeTool       HookPoint = "before_tool"
	HookAfterTool        HookPoint = "after_tool"
	HookBeforeCompaction HookPoint = "before_compaction"
	HookTurnCompleted    HookPoint = "turn_completed"
	HookSessionClosed    HookPoint = "session_closed"
)

func (p HookPoint) Valid() bool {
	switch p {
	case HookSessionOpened, HookBeforeTurn, HookBeforeRequest, HookAfterResponse, HookBeforeTool,
		HookAfterTool, HookBeforeCompaction, HookTurnCompleted, HookSessionClosed:
		return true
	}
	return false
}

// HookCall is one firing. Payload is the *XPayload struct for Point; a handler returns the
// *XResult struct for Point, or nil for "pass" or for points that return nothing.
type HookCall struct {
	Point     HookPoint
	SessionID string
	TurnID    string // "" for session_opened, before_compaction and session_closed
	Payload   any
}

// HookHandler is one registration. Handlers run in Priority order, lower first, ties in
// plugin load order.
type HookHandler struct {
	Point    HookPoint
	Priority int
	Handle   func(ctx context.Context, call HookCall) (any, error)
}

// OwnedHook is a registered handler with the plugin that owns it, which is what a notice
// names when the handler fails.
type OwnedHook struct {
	Owner string
	HookHandler
}

type SessionOpenedPayload struct {
	SessionID string                `json:"session_id"`
	Workspace session.Workspace     `json:"workspace"`
	Model     session.ModelRef      `json:"model"`
	Mode      session.Mode          `json:"mode"`
	Thinking  session.ThinkingLevel `json:"thinking"`
	Resumed   bool                  `json:"resumed"`
}

type SessionOpenedResult struct {
	Context string `json:"context"`
}

type BeforeTurnPayload struct {
	SessionID string        `json:"session_id"`
	TurnID    string        `json:"turn_id"`
	Message   session.Entry `json:"message"`
}

type BeforeTurnResult struct {
	SystemPromptAdditions []string `json:"system_prompt_additions"`
}

type BeforeRequestPayload struct {
	SessionID string            `json:"session_id"`
	TurnID    string            `json:"turn_id"`
	Provider  string            `json:"provider"`
	Model     session.ModelRef  `json:"model"`
	Headers   map[string]string `json:"headers"`
}

type BeforeRequestResult struct {
	Headers map[string]string `json:"headers"`
}

type AfterResponsePayload struct {
	SessionID string        `json:"session_id"`
	TurnID    string        `json:"turn_id"`
	Message   session.Entry `json:"message"`
}

type BeforeToolPayload struct {
	SessionID string          `json:"session_id"`
	TurnID    string          `json:"turn_id"`
	ToolUseID string          `json:"tool_use_id"`
	Tool      string          `json:"tool"`
	Input     json.RawMessage `json:"input"`
	Safety    tool.Safety     `json:"safety"`
}

// BeforeToolResult is a handler's verdict on one tool call. Decision is pass, allow, deny or
// modify; Input is the replacement bytes for modify, carried verbatim.
type BeforeToolResult struct {
	Decision string          `json:"decision"`
	Input    json.RawMessage `json:"input,omitempty"`
	Reason   string          `json:"reason,omitempty"`
}

// The decisions a before_tool handler may return.
const (
	DecisionPass   = "pass"
	DecisionAllow  = "allow"
	DecisionDeny   = "deny"
	DecisionModify = "modify"
)

type AfterToolPayload struct {
	SessionID string        `json:"session_id"`
	TurnID    string        `json:"turn_id"`
	ToolUseID string        `json:"tool_use_id"`
	Result    session.Entry `json:"result"`
}

type AfterToolResult struct {
	Content []session.Block `json:"content"`
}

type BeforeCompactionPayload struct {
	SessionID     string `json:"session_id"`
	FirstEntryID  string `json:"first_entry_id"`
	LastEntryID   string `json:"last_entry_id"`
	PromptTokens  int64  `json:"prompt_tokens"`
	ContextWindow int64  `json:"context_window"`
}

type BeforeCompactionResult struct {
	Summary string `json:"summary"`
}

type TurnCompletedPayload struct {
	SessionID string        `json:"session_id"`
	TurnID    string        `json:"turn_id"`
	Usage     session.Usage `json:"usage"`
}

type SessionClosedPayload struct {
	SessionID string `json:"session_id"`
}

// ResultFor returns a zero *XResult for point, or nil for points that return nothing. The
// spawned adapter unmarshals a hook.fire response into it.
func ResultFor(point HookPoint) any {
	switch point {
	case HookSessionOpened:
		return &SessionOpenedResult{}
	case HookBeforeTurn:
		return &BeforeTurnResult{}
	case HookBeforeRequest:
		return &BeforeRequestResult{}
	case HookBeforeTool:
		return &BeforeToolResult{}
	case HookAfterTool:
		return &AfterToolResult{}
	case HookBeforeCompaction:
		return &BeforeCompactionResult{}
	}
	return nil
}

// HookRunner fires one point's handlers against a registry. It is the only thing that runs
// plugin hook code, so the timeout, the notice text and the isolation of a failing handler
// live here rather than at each of the nine call sites.
type HookRunner struct {
	reg     *Registry
	timeout time.Duration
	notice  func(string)
}

func NewHookRunner(reg *Registry, timeout time.Duration, notice func(string)) *HookRunner {
	if notice == nil {
		notice = func(string) {}
	}
	return &HookRunner{reg: reg, timeout: timeout, notice: notice}
}

// Fire runs the handlers for call.Point in registry order. Each gets its own deadline; a
// handler that errors, panics or times out is skipped with a notice and never stops the
// others. Results that are nil, or nil pointers, are dropped.
func (h *HookRunner) Fire(ctx context.Context, call HookCall) []any {
	var out []any
	for _, oh := range h.reg.Hooks(call.Point) {
		res, err := h.one(ctx, oh, call)
		if err != nil {
			h.notice(fmt.Sprintf("hook %s: %s: %v", call.Point, oh.Owner, err))
			continue
		}
		if res != nil && !isNilPointer(res) {
			out = append(out, res)
		}
	}
	return out
}

// outcome is what one handler produced. It travels by channel rather than by closing over
// named results: a handler that outran its deadline is still running when one returns, and
// two goroutines writing the same variables would be a data race even though nobody reads
// the late one's answer.
type outcome struct {
	res any
	err error
}

func (h *HookRunner) one(ctx context.Context, oh OwnedHook, call HookCall) (any, error) {
	hctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	done := make(chan outcome, 1) // buffered: a late send must not leak the goroutine
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- outcome{err: fmt.Errorf("panic: %v", p)}
			}
		}()
		res, err := oh.Handle(hctx, call)
		done <- outcome{res: res, err: err}
	}()
	select {
	case o := <-done:
		return o.res, o.err
	case <-hctx.Done():
		// The handler goroutine keeps running until it notices the context; its late result
		// is discarded and the timeout is reported alone.
		return nil, fmt.Errorf("timed out after %s", h.timeout)
	}
}

// isNilPointer reports whether v is a typed nil pointer, the shape a handler that meant to
// pass most easily returns by accident (a nil *XResult in an any is not itself nil).
func isNilPointer(v any) bool {
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// Hooks returns the handlers for point in the order they run: by priority, lower first, and
// ties in plugin load order, which is the order the committed slice already holds.
func (r *Registry) Hooks(point HookPoint) []OwnedHook {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []OwnedHook
	for _, oh := range r.hooks {
		if oh.Point == point {
			out = append(out, oh)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out
}

// RegisterHook stages one handler. Unlike a tool or a command a hook has no name to collide
// on: several plugins, and one plugin several times, may hook the same point.
func (h *host) RegisterHook(hh HookHandler) error {
	if !hh.Point.Valid() {
		return fmt.Errorf("plugin %s: unknown hook point %q", h.name, string(hh.Point))
	}
	if hh.Handle == nil {
		return fmt.Errorf("plugin %s: hook %s has no Handle", h.name, hh.Point)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hooks = append(h.hooks, hh)
	return nil
}
