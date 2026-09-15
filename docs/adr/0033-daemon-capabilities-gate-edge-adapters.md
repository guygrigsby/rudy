# 33. Daemon capabilities gate edge adapters

Status: Accepted

Date: 2026-09-14

Deciders: Guy Grigsby

## Context

Rudy upgrades its command binary in place while a durable `rudy serve` process may keep running
the previous code. ADR 0029 permits client and daemon version skew because a version string does
not by itself prove protocol incompatibility.

ACP depends on security and durability invariants that older daemons do not implement. In
particular, the old direct Steering cancellation path can publish Idle after its terminal Entry
fails to sync. A new ACP adapter must not infer terminal durability from its own version while it
is attached to that daemon. Requiring exact binary version equality would force needless daemon
restarts for unrelated compatible changes.

## Decision

Extend the internal `client.hello` response with a sorted, duplicate-free capability set from a
closed protocol table. A Server advertises a capability only after the complete invariant is
implemented.

An edge adapter declares the internal capabilities required by the surface it exposes. It does
not commit readiness or admit Session work until hello advertises every required capability. For
the server-facing ACPAgent, a missing capability returns a fixed incompatible-daemon ACP error and
closes after that response is physically written. For the client-facing LocalProtocolAdapter, it
returns a typed unavailable initialization failure and closes its internal connection and event
stream. Version mismatch remains a notice when the capability check passes.

The registered ACP agent in normal or generic Session-serving mode and LocalProtocolAdapter require
`bounded_session_list_v1`, `bounded_session_events_v1`, `bounded_process_events_v1` and
`terminal_turn_durability_v1`. ACPClientAdapter relies on the ACP agent applying that gate before
its initialize response. The terminal capability includes terminal Entry sync, error propagation,
Session quarantine and subscriber closure on sync failure. The Session-event capability includes
the internal oversized reference, replay-safe delivery, non-authorizing per-question ACP asker
decline and exact-question permission cancellation. The process-event capability bounds every
replayable process value plus the complete connect snapshot. Exact non-asker shutdown-control mode
requires none of these four because it exposes no Session or process-event surface; same-user hello
identity and shutdown terminal proof remain mandatory. It sends hello with `process_events: false`
and boundedly discards notifications from an old daemon that ignores that field; overflow fails
closed rather than becoming a capability assumption. The Session-event capability also includes
exact guarded `session.interrupt` with no successor fallthrough,
exact Session, Turn and tool-use matching for every permission answer plus provider tool-use id
uniqueness throughout an active Turn, so an adapter never assumes that a stale callback is harmless
when attached to an older daemon. The process-event capability includes physical hello response
write before snapshot enqueue and uses the maximum sequence width for every public sizing check.

## Consequences

- A new ACP binary cannot claim a guarantee that its live daemon does not implement.
- Compatible client and daemon versions may still differ without restarting working Turns.
- Adding a load-bearing edge invariant requires one internal capability, its implementation gate
  and a compatibility test against a daemon that omits it.
- Existing clients may ignore new hello capabilities. New clients treat an absent required name,
  including an old response with no capability field, as incompatible.
