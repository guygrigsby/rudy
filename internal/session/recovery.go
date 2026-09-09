package session

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
				Decision: Deny, DecidedBy: ByNoAsker, Scope: ScopeOnce,
				Reason: "recovery: process ended before a decision",
			}); err != nil {
				return n, err
			}
			n++
		}
		if _, err := s.Append(ToolResult{ToolUseID: b.ID, Outcome: OutcomeLost}); err != nil {
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
