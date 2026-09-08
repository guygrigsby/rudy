# ADR 0011: The dangerous set precedes session allowances

- Status: Accepted
- Date: 2026-09-08
- Deciders: Guy Grigsby

## Context

ADR 0009 framed the dangerous set as a permissive-mode construct: everything
runs except the dangerous set, which asks. The gate as first implemented
checked a session-scope allowance before the dangerous set in every mode, and
an allowance matches on the matcher, which for bash is the first two words of
the first command. So an asker's session allow for `git push origin main`
(matcher `git push`) later let `git push --force origin main` run without a
question, and `rm -rf build` allowed for the session covered `rm -rf /`. The
final whole-branch review of the kernel flagged it. The matcher prefix rule
itself is still open in the design; this decision limits what that rule can
cost.

## Decision

The gate evaluates the dangerous set before session allowances in every mode
except `off`. A command that matches a dangerous entry always asks, even when
a prior session-scope allow matches its matcher. Allowances still cover
non-dangerous commands as before. The dangerous set therefore applies in
`strict` as well as `permissive`; in `strict` every unsafe tool asks anyway,
so the only observable change there is that an allowance cannot silence a
dangerous command.

## Consequences

- Widening the matcher prefix rule later can only widen what an allow covers
  among non-dangerous commands. The dangerous set is the floor.
- A user who allowed `git push` for the session is asked again for
  `git push --force`. One extra question, by design.
- ADR 0009's description of the dangerous set as permissive-only is
  superseded by this ordering; the modes themselves are unchanged.
- The contents of the dangerous set and the matcher prefix rule remain open
  in the design.

## Alternatives considered

- Keep allowance-first and narrow the matcher: rejected, the matcher rule is
  undecided and any prefix rule leaves some widening.
- Record allowances against the full command text: rejected, it makes session
  allows nearly useless for repeated commands with varying arguments.
