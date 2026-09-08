# ADR 0009: Permission modes

- Status: Accepted
- Date: 2026-09-07
- Deciders: Guy Grigsby

## Context

Every tool carries a safety class, safe or unsafe, the one switch from gyr
that drives both the gate and the durable-record-before-run rule. The gate
needs a policy above that switch. Claude Code has four modes plus rule
patterns. pi-go has none. rudy starts with three, per the decision of
2026-09-07.

## Decision

`PermissionMode` is an enumeration of three:

- `strict`: an unsafe tool asks the attached asker. No asker means deny.
  Safe tools run.
- `permissive`: everything runs except a built-in dangerous set, which
  asks. The set is the `Dangerous` classifier idea: destructive git,
  recursive delete outside the workspace and the like.
- `off`: everything runs, nothing asks. Headless plus `off` is fully
  autonomous.

In all three modes a `permission_decision` entry is appended and fsynced
before an unsafe tool runs, recording the mode, the decision and who
decided: the asker, the mode or no asker. The mode can change during a
session and the change is itself an entry.

Allow and deny rule patterns such as `Bash(git *)` are not in v1. Modes
only.

Objects and the turn transitions the gate drives are in the
[domain model](../specs/rudy-domain-model.md).

## Consequences

- The log answers "why did this run" for every unsafe call in every mode.
- Headless runs in `strict` deny every unsafe tool, which the model sees
  and adapts to, matching Claude Code's no-prompt behavior.
- The contents of the dangerous set are an open question in the context
  map and get decided with evidence when the classifier is written.
- Rule patterns are added as a fourth input to the gate if modes prove
  too coarse, without changing the entry shape.

## Alternatives considered

- Claude Code's mode set plus rules: more surface than a first cut needs.
- A classifier model deciding, Claude Code's `auto`: a network call in
  the gate and a second model to trust.
- No gate, pi-go's state: unacceptable for unattended runs.
