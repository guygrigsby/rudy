# 46. Recover Codex runtime on the next operation

Status: Accepted. Supersedes ADR 0045's subprocess restart policy.

Date: 2026-09-26

Deciders: Guy Grigsby

## Context

ADR 0045 called for a background restart with bounded backoff whenever an
attached Codex session lost its App Server process. The implementation has no
per-session restart coordinator. A background replacement also cannot prove
the outcome of a turn that was running when the process exited.

## Decision

On process loss, emit `runtime_failed` for each active turn. Mark each affected
Rudy session terminal but ambiguous, so it cannot submit or fork. Cancel
pending approvals and fail login challenges owned by the lost process. Start
one replacement process lazily on the next account,
model or thread operation. `session.resume` resumes and reads the linked
thread to clear ambiguity and restore canonical state. Never replay a lost
non-idempotent request.

## Consequences

An attached session does not recover without another operation. Its failed
state is local evidence of process loss, not a claim that Codex ended its
turn. Resume is required before more work on that thread. No background retry
timer or second process runs solely because a client remains attached.
