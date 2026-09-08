# ADR 0003: Own the domain model, no agent framework

- Status: Accepted
- Date: 2026-09-07
- Deciders: Guy Grigsby

## Context

Three prior harness attempts sit next to this one. jess wraps
`voocel/agentcore` and exposes gate, ledger, mcp, memory, skill and
subagent packages. Its only real consumer, autophage, imports ledger and
mcp and ignores the rest. gyr is an ops bot whose harness parts migrated
into jess. pi-go, the closest external candidate, sits on Google ADK
(ADR 0001).

Measured on 2026-09-07:

- `jess/gate` is 143 lines and its public type is agentcore's `ToolGate`.
- `jess/mcp` is 392 lines adapting the MCP go-sdk to agentcore tools.
- `jess/ledger` is 510 lines and agentcore-free but duplicates what a
  session event log already records.
- `guygrigsby/llm` has clean native adapters but its message and stream
  types are agentcore's.

The jess audit of 2026-06-04 deleted a 1,500-line anti-corruption layer
because insulating one maintainer against a framework change nobody was
planning was not worth it. The cleaner cut is no framework under the loop.

## Decision

rudy owns its message model, tool interface, loop and session state. No
`agentcore`, no ADK, no `jess` import. Concretely:

- The gate is rewritten against rudy's tool type. It is smaller than
  adapting 143 agentcore-typed lines.
- The MCP bridge is rewritten against rudy's tool type on the MCP go-sdk
  directly. Same size as jess's, no agentcore.
- The ledger's durable-sink idea folds into the session log. There is one
  store, not two (ADR 0004).
- The `llm` adapters are lifted and retyped onto rudy's model as codecs
  (ADR 0010).

Two rules survive from jess as rules, not code:

1. No asker attached means deny. A gate with nobody to ask fails closed.
2. An unsafe tool runs only after its allow decision is appended and
   fsynced to the session log. No durable record, no action.

Aggregate roots and their invariants are in the
[domain model](../specs/rudy-domain-model.md).

## Consequences

- Every type in a signature outside the two provider codecs and the MCP
  bridge is rudy's own. `grep agentcore` returns nothing.
- The loop is written and tested offline against a fake provider, about
  200 lines by ago's precedent.
- autophage keeps using jess, unaffected.

## Alternatives considered

- Import jess for gate and ledger: pulls agentcore, gomlx, chromem and two
  SQL drivers into the module graph for 650 lines.
- Build on agentcore directly: the framework owns the loop and the types,
  the jess problem again.
- Build on ADK: the pi-go problem, 22 packages coupled to a vendor.
