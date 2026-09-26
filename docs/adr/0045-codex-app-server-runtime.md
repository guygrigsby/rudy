# 45. Codex App Server is an agent runtime

Status: Accepted. Extends ADR 0005 and ADR 0010. Does not change ADR 0001's
decision about unsupported third-party subscription OAuth.

Date: 2026-09-26

Deciders: Guy Grigsby

## Context

Rudy's native execution model owns the turn loop, model-visible history, tools
and permission gate. A Provider supplies one completion over an HTTP wire
codec. That boundary fits OpenAI-compatible and Anthropic-compatible model
APIs but does not fit Codex App Server.

The [App Server protocol](https://developers.openai.com/codex/app-server)
owns durable threads, turns, tools, streaming items and approval requests. It
also exposes OpenAI's supported ChatGPT login flow and owns the
resulting credentials. Flattening it into `Provider.Complete` would create two
agent loops, two histories and two approval authorities. Reading Codex tokens
and calling another API directly would make Rudy responsible for undocumented
credential storage and refresh.

The user needs `/login` inside Rudy and wants ChatGPT subscription access. The
installed `codex` binary is an external dependency and may be absent, old or
crash independently of Rudy.

## Decision

Add an `AgentRuntime` port beside `Provider`. An AgentRuntime owns threads,
turns, its model-facing history and tool execution. It registers through the
plugin capability surface, so linked and spawned implementations have the same
kernel path. The first implementation is named `codex`.

Run one lazy `codex app-server` stdio subprocess per Rudy kernel. Require Codex
CLI 0.155.1 or newer. Give its newline-delimited wire protocol a dedicated
bidirectional codec and anti-corruption layer under `internal/provider/`.

Use App Server threads as canonical history. A Rudy runtime session stores its
runtime name and Codex thread id in an atomic mode-0600 link. Transcript rows
are deterministic projections of `thread/read` and live item events. Rudy does
not copy Codex conversation content into its append-only log.

Keep local control and trust-boundary facts in the Rudy session log. In
particular, every App Server approval Rudy allows is appended and fsynced as a
`runtime_permission_decision` before the response reaches App Server. This is
not a second conversation history. It preserves Rudy's rule that no unsafe
action begins before durable consent.

Implement `/login` as a plugin-registered slash command. Start browser login
for a local interactive client. Cancel a failed browser attempt before starting
device-code login. Remote and headless clients use device login directly. App
Server owns every credential and Rudy stores none.

Use `codex:<model>` in the existing model selector. The registry marks each
model's execution owner as Provider or AgentRuntime and session creation derives
execution kind from that owner. A Codex-only configuration is valid even when
no HTTP Provider is configured.

Map Rudy permission modes to App Server approval policies, then fail closed on
every approval request that cannot be bound to the exact Rudy session, Codex
thread, turn, item and upstream request id. Never select a Rudy session from an
identifier supplied by App Server.

On subprocess loss, fail the active operation, deny pending requests, restart
with backoff while a Codex session remains attached and reconcile through
`thread/resume` plus `thread/read`. Never replay a non-idempotent start, fork,
turn or steer request after a lost response.

## Consequences

Rudy can use a ChatGPT subscription without handling OpenAI credentials. Codex
keeps the thread behavior its protocol defines, including its own tools and
canonical resume semantics.

The Session context now dispatches through two execution kinds. Client methods
keep their names but some operations are runtime-specific refusals. Model
records need an execution owner because their string prefix alone no longer
means HTTP Provider.

The plugin protocol gains runtime registration, runtime calls, runtime events
and bound approval requests. The existing one-way protocol client cannot serve
the App Server connection.

Rudy depends on a compatible `codex` executable. Missing or incompatible Codex
disables only that runtime. Version-pinned protocol fixtures and a subprocess
fake become part of the gate.

Rudy retains a small local approval audit beside the thread link. Storing no
local evidence was rejected because it would make the existing durable-consent
invariant unverifiable at the boundary where Rudy releases an unsafe action.

Alternatives rejected:

- Model App Server as a Provider. It cannot preserve one owner for loop,
  history and approvals.
- Run one App Server per session. It multiplies login state, startup cost and
  crash surfaces without adding isolation to the persisted Codex account.
- Connect to a long-lived external App Server. It adds endpoint discovery and
  authentication before one kernel needs shared process lifetime.
- Read Codex credentials and call OpenAI directly. It crosses an unsupported
  secret boundary and duplicates refresh behavior.
- Persist projected Codex transcript entries. It creates two canonical
  histories and makes resume reconciliation ambiguous.
