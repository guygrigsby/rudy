package turn

import (
	"context"
	"errors"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

// The fixed text a call gets when the Turn ends underneath it rather than because of it.
// Fixed because it says what happened to the call, which is the same thing however the Turn
// ended, and because it is read by a client that cannot see the Turn's own reason.
const (
	interruptDenyReason = "the turn was interrupted before this call was decided"
	invariantDenyReason = "the turn failed before this call was decided"
	killedByInterrupt   = "killed: the turn was interrupted"
	killedByTurnFailure = "killed: the turn failed"
)

// terminalize closes out every call this Turn admitted and did not finish, before the Entry
// that ends the Turn. A call with no decision gets the Server's own fixed denial, and every
// call with no result gets one, so the next request context holds no tool_use without an
// answer and no client is left watching a call that will never move again (ADR 0034).
//
// The causal call of a failure is the exception: it gets an error result carrying what went
// wrong, where its peers get killed. Called on the Run goroutine, after every call goroutine
// has been joined, so nothing is appending underneath it.
func (r *Runner) terminalize(ctx context.Context, by session.DecidedBy, reason, text, causal, causalText string) error {
	s := r.cfg.Session
	for _, b := range s.PendingToolUses() {
		if _, decided := s.DecisionFor(b.ID); !decided {
			dec := session.PermissionDecision{
				ToolUseID: b.ID,
				Tool:      b.Name,
				Mode:      s.Mode(),
				Matcher:   session.Matcher{Tool: b.Name},
				Decision:  session.Deny,
				DecidedBy: by,
				Scope:     session.ScopeOnce,
				Reason:    reason,
			}
			if _, err := r.append(dec); err != nil {
				return err
			}
		}
		outcome, content := session.OutcomeKilled, text
		if causal != "" && b.ID == causal {
			outcome, content = session.OutcomeError, causalText
		}
		if _, err := r.appendToolResult(ctx, session.ToolResult{
			ToolUseID: b.ID,
			Outcome:   outcome,
			Content:   []session.Block{session.TextBlock(content)},
		}); err != nil {
			return err
		}
	}
	return nil
}

// errText is what a failing call's own result says. A provider error reports its message
// rather than the wrapper's, which is the same string turn_failed carries.
func errText(err error) string {
	var pe *provider.Error
	if errors.As(err, &pe) {
		return pe.Message
	}
	return err.Error()
}
