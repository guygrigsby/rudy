# ADR 0002: One binary, one protocol

- Status: Accepted
- Date: 2026-09-07
- Deciders: Guy Grigsby

## Context

The harness must run interactively in a terminal, headless in scripts and
CI, and later inside editors. The failure mode to avoid is three separate
paths into the loop that drift apart. The alternative failure mode is a
mandatory daemon: a second process to debug on every launch.

Precedent was checked at source on 2026-09-07. Claude Code is one process:
the TUI and the loop share a Node process, `claude -p` is the same binary
printing stream-json over stdio, and the Agent SDK runs the loop in the
caller's process. Codex moved the other way: its TUI crate depends on
`codex-app-server-client` and `codex-app-server-protocol` and no longer on
`codex-core`. At startup the Codex TUI picks a transport in order: an
explicit remote endpoint, an already-running local daemon over a unix
socket with a 50ms connect timeout, and otherwise an app-server embedded
in its own process over an in-memory channel. Its VS Code extension and
desktop app are other clients of the same server.

See the [context map](../specs/rudy-context-map.md) for the Client
context and its conformist relationship to Session.

## Decision

rudy is one binary and one protocol. The TUI is a client of a JSON-RPC
2.0 protocol served by the kernel. Transports, chosen at startup:

- in-memory channel, the default: `rudy` embeds the server in its own
  process, no IPC in the default path
- unix socket: `rudy serve` runs the same server on a socket; a client
  that finds one attaches to it, so a session outlives a terminal and two
  terminals can watch one session
- stdio: spawned plugins speak the same protocol over their stdin and
  stdout (ADR 0005)

Headless is a client with no screen. An editor integration is an ACP
adapter that is itself a client. No second process exists unless the user
starts one.

## Consequences

- Headless, multi-client and editor support fall out of the seam instead
  of being bolted on.
- The protocol is a contract from day one and gets versioned. Its cost is
  paid in the contracts artifact, not later in code.
- The client renders from entries and stream deltas only, so a resumed
  session and a second attached terminal show the same transcript by
  construction.
- A daemon is a deploy option, not an architecture.

## Alternatives considered

- Single process with no protocol: headless, editor and a second terminal
  each need their own path into the loop.
- Mandatory daemon plus client: a process boundary to debug on every
  launch for a benefit only some sessions need.
- Library plus thin binaries: jess's shape, which failed for lack of
  consumers; rudy has exactly one.
