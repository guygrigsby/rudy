# 37. Linearize Session durability quarantine

Status: Accepted

## Context

A terminal Entry sync failure or permission decision-batch failure makes the in-memory Session
unsafe to continue. A separate availability check is insufficient: an attachment, plugin Host call
or Turn append can pass the check, pause and commit after another path records the durability
failure. Clearing quarantine on detach or reload would also let the same Server process reuse state
whose durable prefix is uncertain.

## Decision

Give Server one runtime-only `SessionAdmission` fence per Session ULID. Every immediate Session read,
mutation, append, attachment and detachment commit runs through that fence. Turn, Gate, protocol and
plugin Host paths receive the fenced operation rather than calling a live Session directly.

Provider I/O and transport waits remain outside the fence and reenter for each commit. When a
fenced operation returns a durability cause, Server installs the first cause before releasing the
fence. A racing operation therefore commits first and joins the cancellation or subscriber
snapshot, installs quarantine itself or observes quarantine and changes nothing.

Quarantine cancels remaining work, closes the captured subscribers with fixed text and makes later
Session-targeting operations unavailable. Detach, unload and reload do not clear it. Only a new
Server process starts with an empty map and runs ordinary log recovery.

## Consequences

- Durability failure and admission have one linearization boundary.
- A stale precheck cannot authorize a later commit.
- Quarantine affects only the failed Session.
- Long provider and transport operations do not hold the fence.
- Server restart is the explicit recovery boundary.
