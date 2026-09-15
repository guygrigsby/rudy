# 35. Preserve Shutdown Proof Notifications

Status: Accepted

## Context

ADR 0033 lets shutdown-control mode discard bounded notifications from an older daemon that
ignores `client.hello.process_events: false`. Read without the later protocol contract, that phrase
could include `server.stopped`, even though shutdown success requires that exact notification.

## Decision

Supersede only ADR 0033's notification-discard wording. Shutdown-control compatibility mode may
discard `status.updated`, `widget.updated`, `plugin.state`, `registry.updated` and `notice`.
It must always route `server.stopped` to the matching shutdown proof waiter. Unknown, malformed or
oversized notifications fail the control connection closed and cannot count as proof.

Every other decision in ADR 0033 remains in force.

## Consequences

- An old daemon cannot fill shutdown control with process UI state.
- Compatibility filtering cannot turn bare EOF into successful shutdown.
- Shutdown proof remains the accepted response, matching `server.stopped` and following EOF.
