# 29. The remote runtime: a box runs the kernel, the client reaches it over ssh

- Status: Accepted
- Date: 2026-09-13
- Deciders: Guy Grigsby

## Context

ADR 0014 made the kernel a process the client attaches to, on one machine over a unix
socket. The operator's layout is two machines: a Mac at the terminal and a Linux box that
holds the checkouts and does the editing. `sand` runs that layout for other agents by
shelling them over ssh per run and moving files and commits around a git ring. Rudy has no
way to run there at all: `--socket` is a local path and every tool touches the filesystem of
the process that built the kernel.

Three things already in the code decide the shape. `protocol.Conn` is a JSON message stream
that `NewStreamConn` runs over any reader and writer. `session.open` detects the workspace
at the client-supplied `cwd` on the server's disk. The asker flow is per connection, and a
turn survives its client leaving. A kernel on another machine is a transport and a workspace
transfer, not a new protocol.

Firecracker sandboxing (rudy-uvo) needs a Linux KVM host, which a Mac reaches only this
way, so this wave goes first.

## Decision

1. **ssh is the fourth transport.** `rudy --host <destination>` runs
   `ssh -- <destination> '<PATH prefix>; command -v rudy || exit 111; exec rudy bridge'` and
   wraps ssh's stdio in `NewStreamConn`. Auth, host keys and the network are ssh's. No TCP
   listener, no rudy-owned credential. The dial order is `--socket`, `--embed`, `--host` or
   `remote.host`, then the local probe; `--host` never falls back to a local server, for the
   reason a deaf `--socket` is fatal: an agent silently running on the wrong machine.

2. **The box runs a daemon.** `rudy bridge` dials the host's socket and starts `rudy serve`
   detached when nothing answers; two bridges racing both try and the loser joins the
   winner. A turn survives the Mac's connection ending; the TUI redials and resumes. The
   daemon, the store, the plugins, the memory bundle and the provider credentials are the
   box's, in the box's own `config.toml`. Sand's rule that the box holds no credential is
   about GitHub push, which stays sand's; the model key has to be where the kernel runs.

3. **The workspace maps by home-relative path, and the box is truth.** A cwd under the
   local home becomes `<host home>/<same relative path>`; `client.hello` gains `home` so
   the client can compute it; outside home `--cwd` names the placement. Before a new
   session opens, a git tree is pushed by URL to a checkout rudy creates at the placement
   (`updateInstead`), with the Mac's uncommitted changes copied on top, when the box is
   clean and behind; a dirty or ahead box is opened as is; diverged refuses. A non-git tree
   is copied over once and copied back by `rudy hosts pull` and on close. Commits come back
   by fetch, which `rudy hosts pull` and sand's `sign` both do.

4. **The box installs rudy from its own checkout at the Mac's revision.** The version
   string carries the commit; `rudy hosts install` checks it out under `remote.source` and
   runs `make install` there. It runs itself when rudy is missing from the box or no daemon
   is running and the versions differ; a running daemon of another version is a notice, not
   a restart. Releases replace the build step later without changing the shape.

5. **The Hosts context lives in the client.** `Host` and `Placement` are value objects,
   `Sync` a domain service, all in `internal/cli` and a `hosts` package under it. The kernel
   has no host concept. `rudy hosts check|install|push|pull` are the operator's verbs and
   `rudy bridge` is a named top-level verb beside `serve`.

## Consequences

- One protocol field (`home`), two config keys (`remote.host`, `remote.source`), one UI
  render key. The server suite is untouched.
- The whole path is testable in `go test` through `RUDY_SSH`, a shim that runs the remote
  line locally, sand's pattern.
- The ring stays in sand. Rudy neither signs nor pushes to GitHub and takes no position on
  what credentials the box holds beyond the model key.
- Images into a prompt, sand's flock interop and Firecracker are follow-up beads; the last
  gets its own spec and ADR.
- Alternatives rejected: ssh socket forwarding without a bridge (nothing starts the daemon
  or names what is missing, and the peer-uid check sees sshd); a TCP listener with rudy's
  own auth (a second trust system beside ssh); embedding a linux build in the darwin
  binary (doubles the binary for a step the box can do itself).
