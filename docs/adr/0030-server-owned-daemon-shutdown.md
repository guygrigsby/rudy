# 30. Server-owned daemon shutdown replaces pid signaling

- Status: Accepted
- Date: 2026-09-14
- Deciders: Guy Grigsby
- Supersedes: ADR 0029 decision 4 and its claim that the server suite is unchanged, only for replacement of a running daemon

## Context

ADR 0029 made `rudy bridge` start a remote daemon and made `rudy hosts install --force`
replace one after installing a new binary. The first implementation put the daemon's process
id in `serve.pid`, checked the socket separately, then sent SIGTERM to that id.

Those facts are not atomic. A daemon can exit after the socket check and the operating system
can reuse its process id before the signal. The valid-looking file then targets an unrelated
same-user process. Exit status from a separate bridge probe also cannot prove that the pid and
socket name the same Server. More validation narrows malformed input but cannot close the
identity race.

The protocol already reaches the exact Server that `hosts install` inspected. The unix
listener already proves the peer uid. The Server owns every runtime resource that must close.
Shutdown belongs to that aggregate and connection, not to a process id observed beside it.

## Decision

1. Add `server.shutdown {}`. Accept it only after `client.hello`, from a non-plugin connection
   whose same-user marker was minted by the accepting unix listener. A claimed client name is
   never authorization. An ssh client qualifies because its host-side bridge connects through
   that listener.

2. Send the successful response before requesting shutdown. Keep that control connection open
   while the process owner stops admission, interrupts work, closes sessions and plugins, and
   removes the socket. Close the control connection only after cleanup completes. Calling
   Server shutdown from the request handler is forbidden because the shutdown wait includes
   that handler's own serve loop.

3. Give each built Server an immutable process-lifetime ULID returned as
   `client.hello.instance_id`. `hosts install --force` keeps the greeted connection it
   inspected, installs, requests shutdown on that same connection, waits for EOF, starts by
   the normal bridge path, and requires the replacement hello to name another instance.

4. Add `rudy bridge --stop` as the direct control path. It never starts a daemon, treats no
   answering daemon as success, and otherwise greets, requests shutdown and waits for cleanup.

5. Remove `serve.pid`, `--pidfile`, pid parsing and process signaling. A legacy daemon that
   lacks `server.shutdown` fails closed. There is no pid fallback.

## Consequences

- Shutdown is bound to the Server connection that answered, so pid reuse cannot redirect it.
- Response flush and cleanup completion become explicit protocol ordering guarantees and need
  server plus real bridge tests.
- Force replacement intentionally interrupts live turns. Non-force install leaves an
  answering daemon alone and reports when the installed binary will take effect.
- The Session kernel gains Server lifecycle state and two internal events. Hosts remains a
  client-side context and no Host type crosses into the kernel.
- `instance_id` is observable but not durable. It replaces process identity checks in tests
  without creating another record to recover or clean.
- Alternatives rejected: stronger pid-file validation, because it cannot bind a reused pid to
  the checked socket; peer credential lookup plus signal, because the process can still exit
  after lookup; disabling force replacement, because an authenticated orderly shutdown is
  available through the existing control plane.
