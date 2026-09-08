package turn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// State is the turn's position in its lifecycle.
type State string

const (
	Idle               State = "idle"
	Streaming          State = "streaming"
	RunningTool        State = "running_tool"
	AwaitingPermission State = "awaiting_permission"
	Steering           State = "steering"
	Completed          State = "completed"
	Failed             State = "failed"
)

// Question is a permission request for one tool_use.
type Question struct {
	ToolUseID string
	Tool      string
	Input     json.RawMessage
	Matcher   session.Matcher
}

// Answer is the asker's reply.
type Answer struct {
	Decision session.Decision
	Scope    session.Scope
	Reason   string
}

// Asker answers permission questions. A nil Asker means nobody can answer.
type Asker interface {
	Ask(ctx context.Context, q Question) (Answer, error)
}

// Observer sees every entry, every stream part and every state change.
type Observer interface {
	EntryAppended(e session.Entry)
	Delta(turnID string, p provider.Part)
	StateChanged(turnID string, s State)
}

// Tools resolves tool names for a turn.
type Tools interface {
	Tool(name string) (tool.Tool, bool)
	Tools() []tool.Tool
}

// Config wires one Runner.
type Config struct {
	Session   *session.Session
	Provider  provider.Provider
	Model     provider.Model
	Tools     Tools
	Gate      *gate.Gate
	Asker     Asker // nil allowed
	Observer  Observer
	System    string // full system prompt
	MaxTokens int
}

type noopObserver struct{}

func (noopObserver) EntryAppended(session.Entry) {}
func (noopObserver) Delta(string, provider.Part) {}
func (noopObserver) StateChanged(string, State)  {}

// Runner drives one turn at a time over a session.
type Runner struct {
	cfg Config

	mu        sync.Mutex
	state     State
	turn      ulid.ULID         // id of the user_message entry that started the current or most recent turn
	interrupt session.Interrupt // pending interrupt, cleared by takeInterrupt
	cancel    context.CancelFunc
}

// NewRunner returns an idle runner.
func NewRunner(c Config) *Runner {
	if c.Observer == nil {
		c.Observer = noopObserver{}
	}
	return &Runner{cfg: c, state: Idle}
}

// TurnID is the id of the current or most recent turn: the ULID of the user_message entry
// that started it, as a string. Empty before the first turn.
func (r *Runner) TurnID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.turn.IsZero() {
		return ""
	}
	return r.turn.String()
}

// State is the current state.
func (r *Runner) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// Interrupt stops the current step. Steer leaves the runner in Steering so the next Run
// with a steer message continues the turn; cancel appends turn_interrupted and returns to
// Idle. Cancel overrides a pending steer. Interrupting an idle, completed or failed runner
// changes nothing.
func (r *Runner) Interrupt(how session.Interrupt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state {
	case Idle, Completed, Failed:
		return
	case Steering:
		if how == session.InterruptCancel {
			r.appendLocked(session.TurnInterrupted{TurnID: r.turn, How: session.InterruptCancel})
			r.setStateLocked(Idle)
		}
		return
	}
	if r.interrupt != session.InterruptCancel {
		r.interrupt = how
	}
	if r.cancel != nil {
		r.cancel()
	}
}

// Run appends msg and drives the loop until Completed, Failed, Steering or Idle after a
// cancel. It returns nil for Completed, Steering and an interrupt cancel, the provider or
// internal error for Failed, and ctx.Err() when the caller's context ended the turn.
func (r *Runner) Run(ctx context.Context, msg session.UserMessage) error {
	r.mu.Lock()
	switch r.state {
	case Steering:
		if msg.Source != session.SourceSteer {
			r.mu.Unlock()
			return fmt.Errorf("%w: a steering turn continues only with a steer message", session.ErrInvariant)
		}
	case Idle, Completed, Failed:
		if msg.Source == session.SourceSteer {
			r.mu.Unlock()
			return fmt.Errorf("%w: steer without a steering turn", session.ErrInvariant)
		}
	default:
		r.mu.Unlock()
		return fmt.Errorf("%w: turn already active", session.ErrInvariant)
	}
	starting := r.state != Steering
	r.interrupt = ""
	r.mu.Unlock()

	e, err := r.append(msg)
	if err != nil {
		return r.fail(session.ErrInternal, err)
	}
	if starting {
		// The turn id is the id of the user_message that started it. A steer message
		// continues the turn under the same id.
		r.mu.Lock()
		r.turn = e.ID
		r.mu.Unlock()
	}
	return r.loop(ctx)
}

func (r *Runner) loop(ctx context.Context) error {
	for {
		r.setState(Streaming)
		am, err := r.stream(ctx)
		if how := r.takeInterrupt(); how != "" {
			am.StopReason = session.StopInterrupted
			am.StopReasonRaw = ""
			am.Content = dropIncompleteToolUses(am.Content)
			if _, aerr := r.append(am); aerr != nil {
				return r.fail(session.ErrInternal, aerr)
			}
			return r.finishInterrupt(how)
		}
		if err != nil {
			if ctx.Err() != nil {
				am.StopReason = session.StopInterrupted
				am.StopReasonRaw = ""
				am.Content = dropIncompleteToolUses(am.Content)
				if _, aerr := r.append(am); aerr != nil {
					return r.fail(session.ErrInternal, aerr)
				}
				_ = r.finishInterrupt(session.InterruptCancel)
				return ctx.Err()
			}
			var pe *provider.Error
			if errors.As(err, &pe) {
				return r.fail(pe.Class, err)
			}
			return r.fail(session.ErrTransport, err)
		}
		if _, err := r.append(am); err != nil {
			return r.fail(session.ErrInternal, err)
		}

		var toolUses []session.Block
		for _, b := range am.Content {
			if b.Type == session.BlockToolUse {
				toolUses = append(toolUses, b)
			}
		}
		if len(toolUses) == 0 {
			r.setState(Completed)
			return nil
		}
		for _, tu := range toolUses {
			done, err := r.runTool(ctx, tu)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		}
	}
}

// stream runs one completion and accumulates the assistant message. The returned message
// is partial when err is non-nil.
func (r *Runner) stream(ctx context.Context) (session.AssistantMessage, error) {
	stepCtx, cancel := context.WithCancel(ctx)
	r.setCancel(cancel)
	defer cancel()

	acc := newAccumulator()
	req := Assemble(r.cfg.Session, r.cfg.Tools.Tools(), r.cfg.System, r.cfg.MaxTokens)
	turnID := r.TurnID()
	err := r.cfg.Provider.Complete(stepCtx, req, func(p provider.Part) error {
		r.cfg.Observer.Delta(turnID, p)
		acc.add(p)
		return nil
	})
	am := session.AssistantMessage{
		Model:         r.cfg.Session.Model(),
		Thinking:      r.cfg.Session.Thinking(),
		Content:       acc.blocks(),
		Usage:         acc.usage,
		StopReason:    acc.stop,
		StopReasonRaw: acc.stopRaw,
	}
	if err == nil && am.StopReason == "" {
		am.StopReason = session.StopOther
	}
	return am, err
}

// runTool gates and runs one tool_use. done is true when an interrupt ended the turn.
func (r *Runner) runTool(ctx context.Context, tu session.Block) (done bool, err error) {
	s := r.cfg.Session
	t, ok := r.cfg.Tools.Tool(tu.Name)
	if !ok {
		// Every tool_result must have a preceding permission_decision for its
		// tool_use id, even one that never runs because the tool is unknown.
		if _, err := r.append(session.PermissionDecision{
			ToolUseID: tu.ID,
			Tool:      tu.Name,
			Mode:      s.Mode(),
			Matcher:   session.Matcher{Tool: tu.Name},
			Decision:  session.Deny,
			DecidedBy: session.ByClass,
			Scope:     session.ScopeOnce,
			Reason:    "unknown tool",
		}); err != nil {
			return true, r.fail(session.ErrInternal, err)
		}
		if _, err := r.append(session.ToolResult{
			ToolUseID: tu.ID,
			Outcome:   session.OutcomeError,
			Content:   []session.Block{session.TextBlock("unknown tool " + tu.Name)},
		}); err != nil {
			return true, r.fail(session.ErrInternal, err)
		}
		return false, nil
	}

	verdict := r.cfg.Gate.Evaluate(gate.Input{
		Tool:         tu.Name,
		Safety:       t.Safety,
		Mode:         s.Mode(),
		Args:         tu.Input,
		Allowances:   s.Allowances(),
		AskerPresent: r.cfg.Asker != nil,
	})
	dec := session.PermissionDecision{
		ToolUseID: tu.ID,
		Tool:      tu.Name,
		Mode:      s.Mode(),
		Matcher:   verdict.Matcher,
		Decision:  verdict.Decision,
		DecidedBy: verdict.DecidedBy,
		Scope:     session.ScopeOnce,
		Reason:    verdict.Reason,
	}
	if verdict.Ask {
		r.setState(AwaitingPermission)
		askCtx, cancel := context.WithCancel(ctx)
		r.setCancel(cancel)
		ans, askErr := r.cfg.Asker.Ask(askCtx, Question{ToolUseID: tu.ID, Tool: tu.Name, Input: tu.Input, Matcher: verdict.Matcher})
		cancel()
		if how := r.takeInterrupt(); how != "" {
			if _, err := r.append(session.ToolResult{ToolUseID: tu.ID, Outcome: session.OutcomeKilled,
				Content: []session.Block{session.TextBlock("interrupted while awaiting permission")}}); err != nil {
				return true, r.fail(session.ErrInternal, err)
			}
			return true, r.finishInterrupt(how)
		}
		if ctx.Err() != nil {
			if _, err := r.append(session.ToolResult{ToolUseID: tu.ID, Outcome: session.OutcomeKilled,
				Content: []session.Block{session.TextBlock("interrupted while awaiting permission")}}); err != nil {
				return true, r.fail(session.ErrInternal, err)
			}
			_ = r.finishInterrupt(session.InterruptCancel)
			return true, ctx.Err()
		}
		switch {
		case askErr != nil:
			dec.Decision, dec.DecidedBy, dec.Reason = session.Deny, session.ByNoAsker, "asker failed: "+askErr.Error()
		default:
			dec.Decision, dec.DecidedBy = ans.Decision, session.ByAsker
			dec.Reason = ans.Reason
			if dec.Reason == "" {
				dec.Reason = "asker"
			}
			if ans.Decision == session.Allow && ans.Scope == session.ScopeSession {
				dec.Scope = session.ScopeSession
			}
		}
	}
	if _, err := r.append(dec); err != nil {
		return true, r.fail(session.ErrInternal, err)
	}
	if dec.Decision == session.Deny {
		_, err := r.append(session.ToolResult{
			ToolUseID: tu.ID,
			Outcome:   session.OutcomeError,
			Content:   []session.Block{session.TextBlock("denied: " + dec.Reason)},
		})
		if err != nil {
			return true, r.fail(session.ErrInternal, err)
		}
		return false, nil
	}

	r.setState(RunningTool)
	if t.Invoke == nil {
		return true, r.fail(session.ErrPlugin, fmt.Errorf("tool %s has no Invoke", tu.Name))
	}
	toolCtx, cancel := context.WithCancel(ctx)
	r.setCancel(cancel)
	start := time.Now()
	res, panicked, invokeErr := invokeTool(t, toolCtx, tool.Call{
		ID:        tu.ID,
		Name:      tu.Name,
		Input:     tu.Input,
		Workspace: s.Workspace(),
		SessionID: s.ID(),
	})
	cancel()
	dur := time.Since(start).Milliseconds()
	if panicked != nil {
		// A panic is a plugin fault, not a tool that ran and reported an error.
		return true, r.fail(session.ErrPlugin, fmt.Errorf("tool %s panicked: %v", tu.Name, panicked))
	}
	content := res.Content
	if how := r.takeInterrupt(); how != "" {
		if len(content) == 0 {
			content = []session.Block{session.TextBlock("killed")}
		}
		if _, err := r.append(session.ToolResult{ToolUseID: tu.ID, Outcome: session.OutcomeKilled, Content: content, DurationMS: dur}); err != nil {
			return true, r.fail(session.ErrInternal, err)
		}
		return true, r.finishInterrupt(how)
	}
	if ctx.Err() != nil {
		if len(content) == 0 {
			content = []session.Block{session.TextBlock("killed")}
		}
		if _, err := r.append(session.ToolResult{ToolUseID: tu.ID, Outcome: session.OutcomeKilled, Content: content, DurationMS: dur}); err != nil {
			return true, r.fail(session.ErrInternal, err)
		}
		_ = r.finishInterrupt(session.InterruptCancel)
		return true, ctx.Err()
	}
	outcome := session.OutcomeOK
	switch {
	case invokeErr != nil:
		outcome = session.OutcomeError
		content = []session.Block{session.TextBlock(invokeErr.Error())}
	case res.IsError:
		outcome = session.OutcomeError
	}
	if len(content) == 0 {
		content = []session.Block{session.TextBlock("")}
	}
	if _, err := r.append(session.ToolResult{ToolUseID: tu.ID, Outcome: outcome, Content: content, DurationMS: dur}); err != nil {
		return true, r.fail(session.ErrInternal, err)
	}
	return false, nil
}

func (r *Runner) finishInterrupt(how session.Interrupt) error {
	if how == session.InterruptSteer {
		r.setState(Steering)
		return nil
	}
	r.mu.Lock()
	turn := r.turn
	r.mu.Unlock()
	if _, err := r.append(session.TurnInterrupted{TurnID: turn, How: session.InterruptCancel}); err != nil {
		return r.fail(session.ErrInternal, err)
	}
	r.setState(Idle)
	return nil
}

// invokeTool runs a tool's Invoke and converts a panic into a returned value so the runner
// can record it as a plugin fault instead of crashing the server.
func invokeTool(t tool.Tool, ctx context.Context, call tool.Call) (res tool.Result, panicked any, err error) {
	defer func() {
		if p := recover(); p != nil {
			panicked = p
		}
	}()
	res, err = t.Invoke(ctx, call)
	return res, nil, err
}

// fail records the failure. Retries and Message come from provider.Error.Attempts and
// Message when the cause is a provider failure (Error.Error() also carries the class and
// status, which turn_failed.Class already reports); otherwise Message is err.Error() and
// Retries is zero. A tool whose Invoke returns a non-nil error is a tool_result with
// outcome error, not a failure; ErrPlugin is reserved for a panic or a nil Invoke, which
// are programming faults in a plugin.
func (r *Runner) fail(class session.ErrorClass, err error) error {
	retries := 0
	msg := err.Error()
	var pe *provider.Error
	if errors.As(err, &pe) {
		retries = pe.Attempts
		msg = pe.Message
	}
	r.mu.Lock()
	turn := r.turn
	r.mu.Unlock()
	if _, aerr := r.cfg.Session.Append(session.TurnFailed{TurnID: turn, Class: class, Message: msg, Retries: retries}); aerr == nil {
		r.notifyLast()
	}
	r.setState(Failed)
	return err
}

func (r *Runner) append(p session.Payload) (session.Entry, error) {
	e, err := r.cfg.Session.Append(p)
	if err != nil {
		return e, err
	}
	r.cfg.Observer.EntryAppended(e)
	return e, nil
}

// appendLocked is append for callers holding r.mu; it is only called from the Steering
// branch of Interrupt, which is safe because no Run is executing while the runner is
// Steering.
func (r *Runner) appendLocked(p session.Payload) {
	if e, err := r.cfg.Session.Append(p); err == nil {
		r.cfg.Observer.EntryAppended(e)
	}
}

func (r *Runner) notifyLast() {
	entries := r.cfg.Session.Entries()
	if len(entries) > 0 {
		r.cfg.Observer.EntryAppended(entries[len(entries)-1])
	}
}

func (r *Runner) setState(s State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setStateLocked(s)
}

func (r *Runner) setStateLocked(s State) {
	if r.state == s {
		return
	}
	r.state = s
	if s != Streaming && s != RunningTool && s != AwaitingPermission {
		r.cancel = nil
	}
	id := ""
	if !r.turn.IsZero() {
		id = r.turn.String()
	}
	r.cfg.Observer.StateChanged(id, s)
}

func (r *Runner) setCancel(c context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancel = c
	if r.interrupt != "" {
		c()
	}
}

func (r *Runner) takeInterrupt() session.Interrupt {
	r.mu.Lock()
	defer r.mu.Unlock()
	how := r.interrupt
	r.interrupt = ""
	return how
}

// accumulator folds stream parts into content blocks in arrival order.
type accumulator struct {
	blocksOut []session.Block
	inputs    map[string]*strings.Builder
	open      map[string]int // tool_use id to index in blocksOut
	usage     session.Usage
	stop      session.StopReason
	stopRaw   string
}

func newAccumulator() *accumulator {
	return &accumulator{inputs: map[string]*strings.Builder{}, open: map[string]int{}}
}

func (a *accumulator) add(p provider.Part) {
	switch p.Type {
	case provider.PartTextDelta:
		a.appendText(session.BlockText, p.Text)
	case provider.PartThinkingDelta:
		a.appendText(session.BlockThinking, p.Text)
	case provider.PartToolUseStart:
		a.open[p.ID] = len(a.blocksOut)
		a.inputs[p.ID] = &strings.Builder{}
		a.blocksOut = append(a.blocksOut, session.Block{Type: session.BlockToolUse, ID: p.ID, Name: p.Name})
	case provider.PartToolUseDelta:
		if b, ok := a.inputs[p.ID]; ok {
			b.WriteString(p.Text)
		}
	case provider.PartToolUseEnd:
		a.finishToolUse(p.ID)
	case provider.PartUsage:
		a.usage = a.usage.Add(p.Usage)
	case provider.PartStop:
		a.stop = p.StopReason
		a.stopRaw = p.StopReasonRaw
	}
}

func (a *accumulator) appendText(t session.BlockType, s string) {
	n := len(a.blocksOut)
	if n > 0 && a.blocksOut[n-1].Type == t {
		a.blocksOut[n-1].Text += s
		return
	}
	a.blocksOut = append(a.blocksOut, session.Block{Type: t, Text: s})
}

func (a *accumulator) finishToolUse(id string) {
	i, ok := a.open[id]
	if !ok {
		return
	}
	in := a.inputs[id].String()
	if strings.TrimSpace(in) == "" {
		in = "{}"
	}
	a.blocksOut[i].Input = json.RawMessage(in)
	delete(a.open, id)
	delete(a.inputs, id)
}

// blocks finalizes any still-open tool_use with whatever input has arrived.
func (a *accumulator) blocks() []session.Block {
	for id := range a.open {
		a.finishToolUse(id)
	}
	return a.blocksOut
}

// dropIncompleteToolUses removes tool_use blocks whose input is not valid JSON. They
// only occur in an interrupted message and never run.
func dropIncompleteToolUses(blocks []session.Block) []session.Block {
	out := blocks[:0]
	for _, b := range blocks {
		if b.Type == session.BlockToolUse && !json.Valid(b.Input) {
			continue
		}
		out = append(out, b)
	}
	return out
}
