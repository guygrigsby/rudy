// SPDX-License-Identifier: AGPL-3.0-or-later

package turn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
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

// ToolState is where one tool call has got to. State is one value for the whole turn and
// several calls run inside it at once (ADR 0028), so this is reported per tool_use id: a turn
// in RunningTool says at least one call is running, and these say which, and which of them is
// the one the operator is being asked about.
type ToolState string

const (
	ToolRunning            ToolState = "running"
	ToolAwaitingPermission ToolState = "awaiting_permission"
	ToolDone               ToolState = "done"
)

// interruptedText is the tool_result content for a tool_use killed while its permission
// question was still with the asker.
const interruptedText = "interrupted while awaiting permission"

// Question is a permission request for one tool_use.
type Question struct {
	ToolUseID string
	Tool      string
	Input     json.RawMessage
	Matcher   session.Matcher
	// Dangerous is the Gate's own verdict.Dangerous for this call (ADR 0011): an Asker must
	// never settle it from another call's session-scope allow, only from an answer to this
	// question itself.
	Dangerous bool
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

// ErrNoAsker is what an Asker returns, directly or wrapped, when nobody is left to answer:
// none was attached when the question was put, or the last one detached while it stood. It is
// not a failure of the Asker, so the runner records the same no_asker denial the Gate writes
// when nothing was attached at all, down to the reason, rather than an asker error.
var ErrNoAsker = errors.New("turn: no asker attached")

// Observer sees every entry, every stream part and every state change. Its methods are called
// from every goroutine a turn runs on, one per tool call as well as the turn's own, so an
// implementation has to be safe for concurrent use.
type Observer interface {
	EntryAppended(e session.Entry)
	Delta(turnID string, p provider.Part)
	StateChanged(turnID string, s State)
	// ToolStateChanged reports one call's progress. Several calls run at once, so this carries
	// the tool_use id that StateChanged cannot.
	ToolStateChanged(turnID, toolUseID, name string, state ToolState)
}

// Tools resolves tool names for a turn.
type Tools interface {
	Tool(name string) (tool.Tool, bool)
	Tools() []tool.Tool
}

// HookFirer runs one hook point's handlers and returns their results in order.
// plugin.HookRunner is the implementation; the interface is what keeps the turn loop free of
// the registry it fires against.
type HookFirer interface {
	Fire(ctx context.Context, call plugin.HookCall) []any
}

// Compactor summarizes part of a session and records the summary.
type Compactor interface {
	// Compact summarizes the request context up to but excluding entries at or after
	// before (zero means everything in the request context) and appends a compaction. It
	// returns the zero Entry and nil when fewer than two entries would be covered.
	Compact(ctx context.Context, s *session.Session, before ulid.ULID, instructions string) (session.Entry, error)
}

// Config wires one Runner.
type Config struct {
	Session     *session.Session
	Provider    provider.Provider
	Model       provider.Model
	Tools       Tools
	Gate        *gate.Gate
	Asker       Asker // nil allowed
	Observer    Observer
	System      string // full system prompt
	MaxTokens   int
	Hooks       HookFirer     // nil means no hooks
	Compactor   Compactor     // nil means never compact
	CompactAt   float64       // fraction of Model.ContextWindow; 0 means never
	ToolTimeout time.Duration // 0 means none
	MaxSteps    int           // provider requests per turn; 0 means unlimited
	// Overrides is what after_tool handlers replaced, by tool_use id, for as long as the
	// session is live: a redaction has to hold for every later request, not just the turn
	// that made it, so the server keeps one map per session and hands it to every runner it
	// builds. Nil means this runner allocates its own, which is what a turn with no session
	// behind it (a test) wants.
	//
	// Every write goes through the Runner's appendToolResult under its mutex, because the
	// calls of one assistant message finish concurrently (ADR 0028). The readers, Assemble
	// here and the Compactor's own, run on the Run goroutine after every call of the message
	// has been joined, so they see every write without taking that mutex; the Compactor holds
	// the same map and could not take it anyway.
	Overrides map[string][]session.Block
}

type noopObserver struct{}

func (noopObserver) EntryAppended(session.Entry)                        {}
func (noopObserver) Delta(string, provider.Part)                        {}
func (noopObserver) StateChanged(string, State)                         {}
func (noopObserver) ToolStateChanged(string, string, string, ToolState) {}

// Runner drives one turn at a time over a session.
type Runner struct {
	cfg Config

	// system is written once, by startTurnState, before the turn's first request, and never
	// again while the turn runs: only the Run goroutine reads it back, so it needs no lock.
	// turnUsage is also the Run goroutine's alone, added to by appendAssistant and read by
	// rest, but it is under mu all the same: a tool call's outcome is now what decides when
	// and how the turn rests (see toolOutcome), and a field whose safety rests on that
	// reasoning is one edit away from a race.
	system string

	mu        sync.Mutex
	state     State
	turn      ulid.ULID         // id of the user_message entry that started the current or most recent turn
	interrupt session.Interrupt // this turn's interrupt; observed by every call, cleared when the turn rests
	turnUsage session.Usage     // sum of this turn's assistant message usages
	// streamCancel cancels the one provider request in flight. It stays a single slot where
	// the tool calls need a set: a turn streams once at a time, and a stream never overlaps
	// the calls of the message it produced. It is never cleared, because a cancel func for a
	// request that already returned is inert.
	streamCancel context.CancelFunc

	// calls is every tool call currently running. Self-locking, and the reference never
	// changes: Run resets its contents instead (see inflight.reset).
	calls *inflight
}

// NewRunner returns an idle runner.
func NewRunner(c Config) *Runner {
	if c.Observer == nil {
		c.Observer = noopObserver{}
	}
	if c.Overrides == nil {
		c.Overrides = map[string][]session.Block{}
	}
	return &Runner{cfg: c, state: Idle, calls: newInflight()}
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
//
// Interrupt touches the session directly only in the Steering case, appending
// turn_interrupted itself: no Run goroutine is executing while the runner is Steering, so
// there is nothing to serialize against there. That append happens under r.mu, the same
// lock Run takes to claim its resume out of Steering (see Run), so the two calls still
// serialize correctly against each other: whichever acquires r.mu first decides the
// outcome, and the other observes the state the first one left behind.
func (r *Runner) Interrupt(how session.Interrupt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state {
	case Idle, Completed, Failed:
		return
	case Steering:
		if how == session.InterruptCancel {
			r.appendLocked(session.TurnInterrupted{TurnID: r.turn, How: session.InterruptCancel})
			// This ends the turn, so sync it here as rest does elsewhere. A failure can
			// only be logged: fail takes r.mu, which this branch already holds.
			if err := r.cfg.Session.Sync(); err != nil {
				slog.Error("turn: sync session log at turn_interrupted", "err", err)
			}
			r.setStateLocked(Idle)
		}
		return
	}
	if r.interrupt != session.InterruptCancel {
		r.interrupt = how
	}
	if r.streamCancel != nil {
		r.streamCancel()
	}
	// Under mu, with the flag, because Run clears the flag and reopens this set under the same
	// lock. Split across two critical sections they could interleave: Run could clear the flag
	// between the two, leaving the next turn's calls cancelled by a latched set with no
	// interrupt to explain it. Nothing in a context's cancel func waits on this runner, so
	// holding mu over them costs only the wakeups the cancellation was for.
	r.calls.cancelAll()
}

// Run appends msg and drives the loop until Completed, Failed, Steering or Idle after a
// cancel. It returns nil for Completed, Steering and an interrupt cancel, the provider or
// internal error for Failed, and ctx.Err() when the caller's context ended the turn.
func (r *Runner) Run(ctx context.Context, msg session.UserMessage) error {
	r.mu.Lock()
	starting := true
	switch r.state {
	case Steering:
		if msg.Source != session.SourceSteer {
			r.mu.Unlock()
			return fmt.Errorf("%w: a steering turn continues only with a steer message", session.ErrInvariant)
		}
		starting = false
		// Claim the resume here, still holding r.mu: this is what serializes against a
		// concurrent Interrupt(cancel), whose Steering branch also runs under r.mu. Once
		// this is set, that branch no longer sees Steering and takes its other path
		// instead of also appending turn_interrupted for the same decision.
		r.setStateLocked(Streaming)
	case Idle, Completed, Failed:
		if msg.Source == session.SourceSteer {
			r.mu.Unlock()
			return fmt.Errorf("%w: steer without a steering turn", session.ErrInvariant)
		}
	default:
		r.mu.Unlock()
		return fmt.Errorf("%w: turn already active", session.ErrInvariant)
	}
	r.interrupt = ""
	// cancelAll latches, so the set the last turn left behind would refuse every call of this
	// one. A steer resume is exactly that case: the interrupt that steered cancelled the set,
	// and the turn it resumes still has tools to run.
	r.calls.reset()
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
		slog.Info("turn: start", "session", r.cfg.Session.ID(), "turn", e.ID, "source", msg.Source)
		r.startTurnState(ctx, e)
	}
	return r.loop(ctx)
}

// startTurnState resets what belongs to one turn and asks the before_turn hook for its
// system prompt additions. Only a fresh turn calls it: a steer resume continues the same
// turn, under the same id, with the additions it already has. The after_tool overrides are
// deliberately not reset: they outlive the turn that made them (see Config.Overrides).
func (r *Runner) startTurnState(ctx context.Context, e session.Entry) {
	r.system = r.cfg.System
	r.mu.Lock()
	r.turnUsage = session.Usage{}
	r.mu.Unlock()
	sid, tid := r.ids()
	for _, res := range r.fire(ctx, plugin.HookBeforeTurn, &plugin.BeforeTurnPayload{SessionID: sid, TurnID: tid, Message: e}) {
		bt, ok := res.(*plugin.BeforeTurnResult)
		if !ok {
			continue
		}
		for _, add := range bt.SystemPromptAdditions {
			if add == "" {
				continue
			}
			if r.system != "" {
				r.system += "\n\n"
			}
			r.system += add
		}
	}
}

// ids are the session and turn ids every hook call and payload from this runner carries.
func (r *Runner) ids() (sessionID, turnID string) {
	return r.cfg.Session.ID().String(), r.TurnID()
}

// fire runs one hook point's handlers, or nothing at all when no hook runner is wired. The
// HookRunner already bounds, isolates and reports each handler, so there is nothing to
// handle here: a hook that failed is simply absent from the results.
//
// A cancelled turn still has hooks to deliver: after_response on the interrupted message and
// after_tool on the killed result both fire on paths where ctx is already done, and a handler
// handed a dead context is skipped by the HookRunner with a false "timed out" notice. Those
// fires get a context that is not cancelled, bounded the way one handler is bounded, so the
// hook sees the entry the log saw.
func (r *Runner) fire(ctx context.Context, point plugin.HookPoint, payload any) []any {
	if r.cfg.Hooks == nil {
		return nil
	}
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), plugin.DefaultHookTimeout)
		defer cancel()
	}
	sid, tid := r.ids()
	return r.cfg.Hooks.Fire(ctx, plugin.HookCall{Point: point, SessionID: sid, TurnID: tid, Payload: payload})
}

func (r *Runner) loop(ctx context.Context) error {
	steps := 0
	for {
		// The step limit is the agent definition's max_turns: a model that keeps calling
		// tools ends the turn here rather than running until the context window or the
		// caller's patience does. Counted before the request, so MaxSteps is exactly how
		// many completions one turn may ask for.
		steps++
		if r.cfg.MaxSteps > 0 && steps > r.cfg.MaxSteps {
			return r.fail(session.ErrInternal, fmt.Errorf("step limit %d reached", r.cfg.MaxSteps))
		}
		r.setState(Streaming)
		am, err := r.stream(ctx)
		if how := r.observeInterrupt(); how != "" {
			am.StopReason = session.StopInterrupted
			am.StopReasonRaw = ""
			am.Content = dropIncompleteToolUses(am.Content)
			if _, aerr := r.appendAssistant(ctx, am); aerr != nil {
				return r.fail(session.ErrInternal, aerr)
			}
			return r.finishInterrupt(ctx, how)
		}
		if err != nil {
			if ctx.Err() != nil {
				am.StopReason = session.StopInterrupted
				am.StopReasonRaw = ""
				am.Content = dropIncompleteToolUses(am.Content)
				if _, aerr := r.appendAssistant(ctx, am); aerr != nil {
					return r.fail(session.ErrInternal, aerr)
				}
				_ = r.finishInterrupt(ctx, session.InterruptCancel)
				return ctx.Err()
			}
			var pe *provider.Error
			if errors.As(err, &pe) {
				return r.fail(pe.Class, err)
			}
			return r.fail(session.ErrTransport, err)
		}
		malformed := sanitizeToolInputs(am.Content)
		if _, err := r.appendAssistant(ctx, am); err != nil {
			return r.fail(session.ErrInternal, err)
		}
		r.maybeCompact(ctx, am)

		var toolUses []session.Block
		for _, b := range am.Content {
			if b.Type == session.BlockToolUse {
				toolUses = append(toolUses, b)
			}
		}
		if len(toolUses) == 0 {
			return r.rest(ctx, Completed)
		}
		// Every call of this message runs at once (ADR 0028). Malformed inputs are refused
		// first and in order, since they append without running anything and keeping them
		// ordered keeps the log readable.
		var runnable []session.Block
		for _, tu := range toolUses {
			if raw, bad := malformed[tu.ID]; bad {
				if err := r.refuse(ctx, r.classDeny(tu, "malformed input"), session.OutcomeError, "malformed tool input: "+raw); err != nil {
					return r.fail(session.ErrInternal, err)
				}
				continue
			}
			runnable = append(runnable, tu)
		}
		outcomes := make([]toolOutcome, len(runnable))
		var wg sync.WaitGroup
		for i, tu := range runnable {
			wg.Go(func() { outcomes[i] = r.runTool(ctx, tu) })
		}
		wg.Wait()
		// One slot per call, read in call order, so the turn ends on the same outcome
		// whatever order the calls finished in. Each goroutine writes only its own index,
		// and wg.Wait is what publishes the writes, so the slice needs no lock.
		if ended, err := r.endTurn(ctx, outcomes); ended {
			return err
		}
	}
}

// toolOutcome is what one finished call asks the turn to do. A call appends its own
// permission_decision and tool_result, but never the turn's ending: N calls observing one
// interrupt would append N turn_interrupted entries, and two calls failing would race over
// which failure the log records. So each leaves its answer here and the loop acts on the
// answers once, in call order.
type toolOutcome struct {
	interrupt session.Interrupt  // the turn was cut short and finishes this way
	ctxErr    error              // the caller's context ended the turn; Run returns this
	class     session.ErrorClass // with err, the turn_failed to record
	err       error
}

// endTurn walks the calls' outcomes in call order and performs the first ending any of them
// asks for, reporting whether the turn is over. Calls with nothing to say are skipped, which
// is every call that simply ran and appended its result.
func (r *Runner) endTurn(ctx context.Context, outcomes []toolOutcome) (ended bool, err error) {
	for _, o := range outcomes {
		switch {
		case o.err != nil:
			return true, r.fail(o.class, o.err)
		case o.ctxErr != nil:
			_ = r.finishInterrupt(ctx, session.InterruptCancel)
			return true, o.ctxErr
		case o.interrupt != "":
			// The interrupt standing now, not the one this call happened to see. Calls
			// observe whenever they finish, which can be a whole tool call apart, and
			// Interrupt lets a cancel overtake a pending steer in between. Taking the first
			// call's steer would leave the runner parked in Steering, with no
			// turn_interrupted, waiting for a steer the operator who just cancelled is never
			// going to send. Empty is not reachable, since only rest and fail clear it and
			// neither has run; the fallback is there so the turn can never finish on the
			// empty interrupt the log refuses.
			how := r.observeInterrupt()
			if how == "" {
				how = o.interrupt
			}
			return true, r.finishInterrupt(ctx, how)
		}
	}
	return false, nil
}

// stream runs one completion and accumulates the assistant message. The returned message
// is partial when err is non-nil.
func (r *Runner) stream(ctx context.Context) (session.AssistantMessage, error) {
	stepCtx, cancel := context.WithCancel(ctx)
	r.setStreamCancel(cancel)
	defer cancel()

	acc := newAccumulator()
	req := Assemble(r.cfg.Session, r.cfg.Tools.Tools(), r.system, r.cfg.MaxTokens, r.cfg.Overrides)
	sid, turnID := r.ids()
	for _, res := range r.fire(ctx, plugin.HookBeforeRequest, &plugin.BeforeRequestPayload{
		SessionID: sid,
		TurnID:    turnID,
		Provider:  r.cfg.Provider.Name(),
		Model:     req.Model,
		Headers:   maps.Clone(req.Headers),
	}) {
		br, ok := res.(*plugin.BeforeRequestResult)
		if !ok || len(br.Headers) == 0 {
			continue
		}
		if req.Headers == nil {
			req.Headers = map[string]string{}
		}
		// Later handlers win over earlier ones, the same way the results are merged
		// everywhere else: they run in the order the registry decided.
		maps.Copy(req.Headers, br.Headers)
	}
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

// runTool gates and runs one tool_use, appending its permission_decision and its tool_result.
// It runs on a goroutine of its own, beside every other call of the same assistant message
// (ADR 0028), so it never ends the turn itself: what it returns is what it asks the loop to do
// once every call has finished.
func (r *Runner) runTool(ctx context.Context, tu session.Block) toolOutcome {
	s := r.cfg.Session
	turnID := r.TurnID()
	r.cfg.Observer.ToolStateChanged(turnID, tu.ID, tu.Name, ToolRunning)
	defer r.cfg.Observer.ToolStateChanged(turnID, tu.ID, tu.Name, ToolDone)

	t, ok := r.cfg.Tools.Tool(tu.Name)
	if !ok {
		if err := r.refuse(ctx, r.classDeny(tu, "unknown tool"), session.OutcomeError, "unknown tool "+tu.Name); err != nil {
			return toolOutcome{class: session.ErrInternal, err: err}
		}
		return toolOutcome{}
	}

	input, modified, hookDec := r.askHooks(ctx, tu, t.Safety)
	var dec session.PermissionDecision
	ask := false
	dangerous := false
	if hookDec != nil {
		// A hook decided, so the gate never weighs in: what it would have said about the
		// mode, the allowances or an asker no longer applies. The matcher is still the
		// gate's, computed over the input that will actually run, because that is the key
		// an allowance would be matched on.
		dec = session.PermissionDecision{
			ToolUseID: tu.ID,
			Tool:      tu.Name,
			Mode:      s.Mode(),
			Matcher:   r.cfg.Gate.MatcherFor(tu.Name, input),
			Decision:  hookDec.decision,
			DecidedBy: session.ByHook,
			Scope:     session.ScopeOnce,
			Reason:    hookDec.reason,
		}
	} else {
		verdict := r.cfg.Gate.Evaluate(gate.Input{
			Tool:         tu.Name,
			Safety:       t.Safety,
			Mode:         s.Mode(),
			Args:         input,
			Allowances:   s.Allowances(),
			AskerPresent: r.cfg.Asker != nil,
		})
		dec = session.PermissionDecision{
			ToolUseID: tu.ID,
			Tool:      tu.Name,
			Mode:      s.Mode(),
			Matcher:   verdict.Matcher,
			Decision:  verdict.Decision,
			DecidedBy: verdict.DecidedBy,
			Scope:     session.ScopeOnce,
			Reason:    verdict.Reason,
		}
		ask = verdict.Ask
		dangerous = verdict.Dangerous
	}
	if modified {
		// The log records what the tool will actually run with, or nothing about the input
		// at all: a decision whose bytes differ from the tool_use above has to say so.
		dec.Input = input
	}
	if ask {
		r.setState(AwaitingPermission)
		r.cfg.Observer.ToolStateChanged(turnID, tu.ID, tu.Name, ToolAwaitingPermission)
		askCtx, cancel := context.WithCancel(ctx)
		r.calls.add(tu.ID, cancel)
		ans, askErr := r.cfg.Asker.Ask(askCtx, Question{ToolUseID: tu.ID, Tool: tu.Name, Input: input, Matcher: dec.Matcher, Dangerous: dangerous})
		r.calls.remove(tu.ID)
		cancel()
		if how := r.observeInterrupt(); how != "" {
			if err := r.refuse(ctx, interruptedDeny(dec), session.OutcomeKilled, interruptedText); err != nil {
				return toolOutcome{class: session.ErrInternal, err: err}
			}
			return toolOutcome{interrupt: how}
		}
		if ctx.Err() != nil {
			if err := r.refuse(ctx, interruptedDeny(dec), session.OutcomeKilled, interruptedText); err != nil {
				return toolOutcome{class: session.ErrInternal, err: err}
			}
			return toolOutcome{ctxErr: ctx.Err()}
		}
		switch {
		case errors.Is(askErr, ErrNoAsker):
			// Nobody to ask reads the same in the log wherever it was decided: this is the
			// string gate.Evaluate writes for the same verdict, and the domain model fixes
			// it for no_asker.
			dec.Decision, dec.DecidedBy, dec.Reason = session.Deny, session.ByNoAsker, "no asker attached"
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
		return toolOutcome{class: session.ErrInternal, err: err}
	}
	if dec.Decision == session.Deny {
		_, err := r.appendToolResult(ctx, session.ToolResult{
			ToolUseID: tu.ID,
			Outcome:   session.OutcomeError,
			Content:   []session.Block{session.TextBlock("denied: " + dec.Reason)},
		})
		if err != nil {
			return toolOutcome{class: session.ErrInternal, err: err}
		}
		return toolOutcome{}
	}

	r.setState(RunningTool)
	if ask {
		// Back from the operator, so this call is running again rather than waiting on an
		// answer. Only a call that asked has anything to correct here.
		r.cfg.Observer.ToolStateChanged(turnID, tu.ID, tu.Name, ToolRunning)
	}
	if t.Invoke == nil {
		return toolOutcome{class: session.ErrPlugin, err: fmt.Errorf("tool %s has no Invoke", tu.Name)}
	}
	var toolCtx context.Context
	var cancel context.CancelFunc
	if r.cfg.ToolTimeout > 0 {
		toolCtx, cancel = context.WithTimeout(ctx, r.cfg.ToolTimeout)
	} else {
		toolCtx, cancel = context.WithCancel(ctx)
	}
	r.calls.add(tu.ID, cancel)
	start := time.Now()
	res, panicked, invokeErr := invokeTool(t, toolCtx, tool.Call{
		ID:        tu.ID,
		Name:      tu.Name,
		Input:     input,
		Workspace: s.Workspace(),
		SessionID: s.ID(),
	})
	// Read the deadline before cancelling: after cancel every context reads as done, and
	// only the deadline distinguishes a tool that ran out of time from one the turn killed.
	timedOut := errors.Is(toolCtx.Err(), context.DeadlineExceeded)
	r.calls.remove(tu.ID)
	cancel()
	dur := time.Since(start).Milliseconds()
	if panicked != nil {
		// A panic is a plugin fault, not a tool that ran and reported an error.
		return toolOutcome{class: session.ErrPlugin, err: fmt.Errorf("tool %s panicked: %v", tu.Name, panicked)}
	}
	content := res.Content
	// Observed, not consumed: every call that was running has to see the same interrupt, or
	// the ones that did not would record success for a turn that was cut (ADR 0028).
	if how := r.observeInterrupt(); how != "" {
		if len(content) == 0 {
			content = []session.Block{session.TextBlock("killed")}
		}
		if _, err := r.appendToolResult(ctx, session.ToolResult{ToolUseID: tu.ID, Outcome: session.OutcomeKilled, Content: content, DurationMS: dur}); err != nil {
			return toolOutcome{class: session.ErrInternal, err: err}
		}
		return toolOutcome{interrupt: how}
	}
	if ctx.Err() != nil {
		if len(content) == 0 {
			content = []session.Block{session.TextBlock("killed")}
		}
		if _, err := r.appendToolResult(ctx, session.ToolResult{ToolUseID: tu.ID, Outcome: session.OutcomeKilled, Content: content, DurationMS: dur}); err != nil {
			return toolOutcome{class: session.ErrInternal, err: err}
		}
		return toolOutcome{ctxErr: ctx.Err()}
	}
	outcome := session.OutcomeOK
	switch {
	case timedOut:
		// Whatever the tool produced before its deadline is kept: it is often the half of
		// the output that explains why the rest never came.
		outcome = session.OutcomeError
		content = []session.Block{session.TextBlock(fmt.Sprintf("%s\n[timed out after %s]", session.TextOf(content), r.cfg.ToolTimeout))}
	case invokeErr != nil:
		outcome = session.OutcomeError
		content = []session.Block{session.TextBlock(invokeErr.Error())}
	case res.IsError:
		outcome = session.OutcomeError
	}
	if len(content) == 0 {
		content = []session.Block{session.TextBlock("")}
	}
	if _, err := r.appendToolResult(ctx, session.ToolResult{ToolUseID: tu.ID, Outcome: outcome, Content: content, DurationMS: dur}); err != nil {
		return toolOutcome{class: session.ErrInternal, err: err}
	}
	return toolOutcome{}
}

// hookVerdict is a before_tool handler's allow or deny, already normalized: Reason is never
// empty, which the log refuses.
type hookVerdict struct {
	decision session.Decision
	reason   string
}

// askHooks runs before_tool and returns the input the call should use, whether a handler
// replaced it, and the hook's decision, or nil when no handler decided. Handlers are walked in
// order: a modify replaces the input with the bytes it returned, verbatim, and the first allow
// or deny ends the walk, so a later handler can neither soften nor override a decision already
// made. A modify whose bytes are not JSON is a pass, since the log and the provider both refuse
// a tool input that is not an object.
func (r *Runner) askHooks(ctx context.Context, tu session.Block, safety tool.Safety) (json.RawMessage, bool, *hookVerdict) {
	input := tu.Input
	modified := false
	sid, tid := r.ids()
	for _, res := range r.fire(ctx, plugin.HookBeforeTool, &plugin.BeforeToolPayload{
		SessionID: sid,
		TurnID:    tid,
		ToolUseID: tu.ID,
		Tool:      tu.Name,
		Input:     tu.Input,
		Safety:    safety,
	}) {
		bt, ok := res.(*plugin.BeforeToolResult)
		if !ok {
			continue
		}
		reason := bt.Reason
		if reason == "" {
			reason = "hook"
		}
		switch bt.Decision {
		case plugin.DecisionModify:
			if json.Valid(bt.Input) {
				input, modified = bt.Input, true
			}
		case plugin.DecisionAllow:
			return input, modified, &hookVerdict{decision: session.Allow, reason: reason}
		case plugin.DecisionDeny:
			return input, modified, &hookVerdict{decision: session.Deny, reason: reason}
		}
	}
	return input, modified, nil
}

// refuse records a tool_use that will not run: the deny the log requires ahead of any
// tool_result for that tool_use, then the result itself. Every path that answers a tool_use
// without invoking the tool goes through here, so the ordering invariant lives in one place
// rather than being remembered separately at each of them.
func (r *Runner) refuse(ctx context.Context, dec session.PermissionDecision, outcome session.Outcome, text string) error {
	if _, err := r.append(dec); err != nil {
		return err
	}
	_, err := r.appendToolResult(ctx, session.ToolResult{
		ToolUseID: dec.ToolUseID,
		Outcome:   outcome,
		Content:   []session.Block{session.TextBlock(text)},
	})
	return err
}

// maybeCompact compacts the session when the prompt tokens the provider just reported reach
// the configured fraction of the model's context window. The message the model answered is
// what says how full the window is, so the check belongs here, right after it was appended
// and before the next request is assembled.
//
// The current turn is left uncovered: `before` is the turn's own user_message, so the summary
// never swallows the exchange still in flight. The Compactor appends through the session
// rather than through r.append, so the entry is announced here.
func (r *Runner) maybeCompact(ctx context.Context, am session.AssistantMessage) {
	if r.cfg.Compactor == nil || r.cfg.CompactAt <= 0 || r.cfg.Model.ContextWindow <= 0 {
		return
	}
	prompt := am.Usage.Input + am.Usage.CacheRead + am.Usage.CacheWrite
	if float64(prompt) < r.cfg.CompactAt*float64(r.cfg.Model.ContextWindow) {
		return
	}
	r.mu.Lock()
	before := r.turn
	r.mu.Unlock()
	ce, err := r.cfg.Compactor.Compact(ctx, r.cfg.Session, before, "")
	if err != nil {
		// A compaction that failed is not a turn that failed: the turn continues with the
		// context it already has, and the next response over the threshold tries again.
		slog.Error("turn: compaction", "err", err)
		return
	}
	if !ce.ID.IsZero() {
		r.cfg.Observer.EntryAppended(ce)
	}
}

// appendAssistant appends an assistant_message, counts its usage toward the turn and fires
// after_response. Every assistant_message the loop writes, interrupted and cancelled ones
// included, goes through here: the contract fires the hook on the append, not on a happy path.
func (r *Runner) appendAssistant(ctx context.Context, am session.AssistantMessage) (session.Entry, error) {
	e, err := r.append(am)
	if err != nil {
		return e, err
	}
	r.mu.Lock()
	r.turnUsage = r.turnUsage.Add(am.Usage)
	r.mu.Unlock()
	sid, tid := r.ids()
	r.fire(ctx, plugin.HookAfterResponse, &plugin.AfterResponsePayload{SessionID: sid, TurnID: tid, Message: e})
	return e, nil
}

// appendToolResult appends a tool_result and fires after_tool on it. Every tool_result goes
// through here, the refusals and the killed ones as well as a tool that ran, so the hook sees
// what the log saw; the first handler to return content replaces what the model will see of
// it, for as long as the session is live, while the entry keeps what really happened.
func (r *Runner) appendToolResult(ctx context.Context, tr session.ToolResult) (session.Entry, error) {
	e, err := r.append(tr)
	if err != nil {
		return e, err
	}
	slog.Debug("tool: done", "tool_use", tr.ToolUseID, "outcome", tr.Outcome, "ms", tr.DurationMS)
	sid, tid := r.ids()
	for _, res := range r.fire(ctx, plugin.HookAfterTool, &plugin.AfterToolPayload{SessionID: sid, TurnID: tid, ToolUseID: tr.ToolUseID, Result: e}) {
		at, ok := res.(*plugin.AfterToolResult)
		if !ok || len(at.Content) == 0 {
			continue
		}
		// Under mu because this runs on each call's own goroutine and the calls of one
		// message finish together (see Config.Overrides): the map's only concurrency is
		// writer against writer, and this is the only writer.
		r.mu.Lock()
		r.cfg.Overrides[tr.ToolUseID] = at.Content
		r.mu.Unlock()
		break
	}
	return e, nil
}

// classDeny is the decision for a tool_use the gate never got to weigh: nothing about the
// mode, the allowances or an asker entered into it, so it is denied by class.
func (r *Runner) classDeny(tu session.Block, reason string) session.PermissionDecision {
	return session.PermissionDecision{
		ToolUseID: tu.ID,
		Tool:      tu.Name,
		Mode:      r.cfg.Session.Mode(),
		Matcher:   session.Matcher{Tool: tu.Name},
		Decision:  session.Deny,
		DecidedBy: session.ByClass,
		Scope:     session.ScopeOnce,
		Reason:    reason,
	}
}

// interruptedDeny turns a question the asker never answered, because a steer or a cancel
// arrived first, into the deny that has to precede the killed result. It keeps the gate's
// own matcher and mode from the verdict, is attributed to the asker because interrupting is
// the asker's own act, and is scoped once so it never becomes a session allowance.
func interruptedDeny(dec session.PermissionDecision) session.PermissionDecision {
	dec.Decision, dec.DecidedBy, dec.Scope, dec.Reason = session.Deny, session.ByAsker, session.ScopeOnce, "interrupted"
	return dec
}

func (r *Runner) finishInterrupt(ctx context.Context, how session.Interrupt) error {
	if how == session.InterruptSteer {
		return r.rest(ctx, Steering)
	}
	r.mu.Lock()
	turn := r.turn
	r.mu.Unlock()
	if _, err := r.append(session.TurnInterrupted{TurnID: turn, How: how}); err != nil {
		return r.fail(session.ErrInternal, err)
	}
	return r.rest(ctx, Idle)
}

// rest makes everything the turn appended durable, then enters the resting state that ends
// this step: Completed, Steering, or Idle after a cancel. Append leaves entries in the
// log's write buffer, so without this a crash between turns loses replies the user has
// already seen. Every call site here is an otherwise successful turn, so a sync failure is
// the turn's failure and is recorded as a turn_failed; the failing paths sync in fail
// instead, where the error can only be logged.
func (r *Runner) rest(ctx context.Context, s State) error {
	if err := r.cfg.Session.Sync(); err != nil {
		return r.fail(session.ErrInternal, fmt.Errorf("sync session log: %w", err))
	}
	if s == Completed {
		sid, tid := r.ids()
		u := r.usage()
		slog.Info("turn: completed", "session", sid, "turn", tid, "input", u.Input, "output", u.Output)
		r.fire(ctx, plugin.HookTurnCompleted, &plugin.TurnCompletedPayload{SessionID: sid, TurnID: tid, Usage: u})
	}
	r.clearInterrupt()
	r.setState(s)
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
	slog.Error("turn: failed", "turn", turn, "class", class, "err", msg, "retries", retries)
	if e, aerr := r.cfg.Session.Append(session.TurnFailed{TurnID: turn, Class: class, Message: msg, Retries: retries}); aerr == nil {
		r.cfg.Observer.EntryAppended(e)
	}
	// The turn is already failing, so a sync failure here is only logged: err, which the
	// caller is about to receive, says more about what went wrong than a write error on
	// the record of it would.
	if serr := r.cfg.Session.Sync(); serr != nil {
		slog.Error("turn: sync session log at turn_failed", "err", serr)
	}
	r.clearInterrupt()
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
	id := ""
	if !r.turn.IsZero() {
		id = r.turn.String()
	}
	r.cfg.Observer.StateChanged(id, s)
}

// setStreamCancel installs the cancel func for the request stream now starting, and cancels it
// at once when this turn is already under an interrupt: the interrupt is no longer consumed by
// whoever looks first, so a stream starting after one landed has to be stopped on sight.
func (r *Runner) setStreamCancel(c context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.streamCancel = c
	if r.interrupt != "" {
		c()
	}
}

// observeInterrupt reports the interrupt this turn is under, if any. It does not consume it:
// every call in flight has to see it, or the ones that did not would record success for a turn
// that was cut (ADR 0028). It is cleared when the turn rests, not when a call reads it.
func (r *Runner) observeInterrupt() session.Interrupt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.interrupt
}

// clearInterrupt retires this turn's interrupt. Every path that brings the turn to rest calls
// it, because nothing else does any more: an interrupt left standing would cut the next turn
// before it had done anything.
func (r *Runner) clearInterrupt() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.interrupt = ""
}

func (r *Runner) usage() session.Usage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turnUsage
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
	case provider.PartThinkingSignature:
		a.signThinking(p.Signature)
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

// signThinking attaches a signature to the thinking block it covers, which is the one that
// just streamed. A signature that arrives with no thinking block open opens an empty one
// rather than being dropped: the provider will refuse the block on the next request if the
// signature it issued does not come back.
func (a *accumulator) signThinking(sig string) {
	n := len(a.blocksOut)
	if n > 0 && a.blocksOut[n-1].Type == session.BlockThinking {
		a.blocksOut[n-1].Signature = sig
		return
	}
	a.blocksOut = append(a.blocksOut, session.Block{Type: session.BlockThinking, Signature: sig})
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

// sanitizeToolInputs replaces every tool_use input that is not valid JSON with an empty
// object, in place, and returns what the model actually sent, keyed by tool_use id. The log
// refuses a tool_use whose input is not JSON, so a model that truncates or mangles its
// arguments would otherwise kill the turn as an internal failure. Dropping the block instead
// would leave the recorded message disagreeing with what the model said; keeping it with an
// empty input, and answering it with an error result naming the raw bytes (see
// rejectMalformed), lets the model see its own mistake and retry.
func sanitizeToolInputs(blocks []session.Block) map[string]string {
	var raw map[string]string
	for i, b := range blocks {
		if b.Type != session.BlockToolUse || len(b.Input) == 0 || json.Valid(b.Input) {
			continue
		}
		if raw == nil {
			raw = map[string]string{}
		}
		raw[b.ID] = string(b.Input)
		blocks[i].Input = json.RawMessage("{}")
	}
	return raw
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
