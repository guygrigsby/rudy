# 32. ACP replaces the private remote session carrier

- Status: Accepted
- Date: 2026-09-14
- Deciders: Guy Grigsby
- Supersedes: ADR 0029 decisions 1 and 5 for the remote session carrier, ADR 0029's claim that a provider key must exist on the box and ADR 0030 decision 4 after bridge compatibility is removed

## Context

ADR 0029 sends Rudy's private protocol over ssh through `rudy bridge`. The design correctly
puts the durable daemon, session log and tools on the Linux box, but only a Rudy client can
drive it. Agent Client Protocol now has a stable v1 stdio transport and stable session load,
resume, list, close, prompt, cancellation, permission and configuration surfaces.

Replacing Rudy's internal protocol with ACP would move an external wire model into Session,
weaken byte-exact log guarantees and force local clients and plugins through a protocol that
does not express every Rudy invariant. ACP instead fits the remote process boundary.

Provider identity also does not have one credential shape. The box may hold a configured
credential, use ambient identity or need none. Aperture authorizes the box by tailnet identity,
so there is no token ACP could or should carry.

## Decision

1. Add `rudy acp`, a stable ACP v1 agent over stdio. On the box it attaches to the same-user
   Unix socket and starts `rudy serve` when absent. The daemon remains authoritative and
   survives adapter or ssh loss.

2. Put ACP behind two anti-corruption layers. The box-side agent adapter translates ACP into
   Rudy protocol. The Mac-side client adapter implements a `SessionClient` port in Rudy types.
   Embedded, Unix socket and plugin paths keep the internal protocol. ACP types may appear
   only in the two adapters.

3. Use standard ACP v1 wherever it has a method. Negotiate versioned `_rudy` methods and
   metadata only for fork, steer, compact, slash commands, titles, Rudy UI events and
   server-owned shutdown. Model and thinking use standard session config options. Rudy's ULID
   is the ACP `SessionId`; no mapping is stored.

4. Keep workspace sync in Hosts over ssh sideband. The Mac passes the mapped absolute box cwd
   to `session/new`. A generic ACP client passes a cwd meaningful where `rudy acp` runs. The
   agent never uses ACP client filesystem or terminal methods.

5. Let ssh authenticate and encrypt the remote stream. ACP advertises no auth method. Provider
   access resolves in the daemon from configured credentials, ambient host identity or no
   authentication. No provider credential crosses ACP or enters a future tool guest.

6. Pin `github.com/coder/acp-go-sdk` exactly and confine it to the adapters. Cap frames at 8 MiB,
   below the selected SDK's fixed scanner ceiling, admit at most 64 active inbound requests and
   bound outbound queues. Install a sanitized SDK logger before a gated reader releases input.
   On overflow or ssh loss, disconnect the adapter and recover the view with `session/load`;
   never backpressure the daemon with unbounded memory or goroutines.

7. Retire `rudy bridge` after ACP parity. Forced install and `rudy hosts stop` negotiate
   `_rudy/server_shutdown`, which maps to `server.shutdown` over the exact greeted Unix
   connection. It returns `stopped` only after matching internal `server.stopped` and EOF. The
   Server's `instance_id` still proves replacement. There is no pid or signal fallback.

## Consequences

- Generic ACP clients can drive Rudy on the box without understanding the private protocol.
- Rudy keeps its internal contracts, plugin symmetry, durable Gate and byte-exact log.
- Remote TUI parity depends on a small negotiated extension surface and fails clearly when the
  box is too old. It never silently runs locally.
- Session load and resume differ at the adapter: load translates replay, resume suppresses it.
- ssh remains the only remote network and trust boundary. No TCP listener or Rudy token is
  added.
- Bridge removal waits for the ACP path to pass the real remote command, reconnect and
  permission paths.
- Alternatives rejected: replace the internal protocol with ACP, because ACP is an external
  client contract rather than Rudy's kernel language; keep the private bridge permanently,
  because it blocks generic clients and duplicates a standard boundary; expose ACP directly
  from the daemon over TCP, because it creates a second network listener and authentication
  system beside ssh.
