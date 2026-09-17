package session

// recoveryDenyReason and recoveryLostText are what recovery writes for a call the process
// ended under. Fixed strings: nothing about them varies with the call, and a client reads
// them to explain a tool_use that has no answer of its own.
const (
	recoveryDenyReason = "the process ended before this call was decided"
	recoveryLostText   = "the process ended while this call was outstanding"
)

// Recover appends the entries that make a log consistent after a crash and
// returns how many it appended. See the recovery rule in the plan. It runs inside Load,
// before the session is shared with anyone, and takes no lock of its own: every method it
// calls here is one of Session's self-locking ones.
func Recover(s *Session) (int, error) {
	n := 0
	for _, b := range s.PendingToolUses() {
		if _, decided := s.decisionFor(b.ID); !decided {
			if _, err := s.Append(PermissionDecision{
				ToolUseID: b.ID, Tool: b.Name, Mode: s.Mode(),
				Matcher:  Matcher{Tool: b.Name},
				Decision: Deny, DecidedBy: ByInvariant, Scope: ScopeOnce,
				Reason: recoveryDenyReason,
			}); err != nil {
				return n, err
			}
			n++
		}
		if _, err := s.Append(ToolResult{ToolUseID: b.ID, Outcome: OutcomeLost, Content: []Block{TextBlock(recoveryLostText)}}); err != nil {
			return n, err
		}
		n++
	}
	if n > 0 {
		if err := s.Sync(); err != nil {
			return n, err
		}
	}
	return n, nil
}
