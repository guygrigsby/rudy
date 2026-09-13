# The remote runtime: a box runs the agent, the Mac is the control plane

Status: design, approved in conversation 2026-09-13. ADR 0029 records the decisions.
Contracts pass 7 carries the rows. Companion to `github.com/guygrigsby/sand`, whose
behaviour this wave brings into rudy for the runtime half and leaves alone for the git ring.

## Problem

Rudy runs the kernel where the client runs. `rudy serve` (ADR 0014) already splits them
into two processes, but only on one machine: the daemon listens on a unix socket and the
client probes it. The operator's real layout is a Mac at the keyboard and a Linux box that
does the work, reached by ssh, with the box's checkout the one that gets edited. Sand runs
that layout for Claude Code and pi by shelling the agent over ssh per run. Rudy should run it
natively: `rudy --host box` from a checkout on the Mac, and everything after that is the
same TUI, the same permission prompts, the same `--continue`, with the tools touching the
box's tree.

Firecracker sandboxing (bead rudy-uvo) needs a Linux KVM host. From a Mac the only way to
reach one is this wave, so it ships first and the sandbox gets its own spec after.

## What is already true

- `protocol.Conn` is a JSON message stream with no transport in it. `NewStreamConn` runs the
  codec over any reader and writer; spawned plugins use it over stdio today.
- `session.open` takes `cwd` from the client and detects the workspace at that path on the
  server's filesystem. The kernel never consults its own cwd.
- The asker flow is per connection. A question goes to every attached asker, the first answer
  decides, the last asker leaving denies it `no_asker`. Tested over the socket.
- A turn outlives its client. `session.resume` replays it. A locked session names its socket.

So a remote kernel is a fourth transport and a workspace transfer. The protocol does not
change except for one field.

## Domain

Subdomain: supporting. Bounded context: **Hosts**, on the client side. The kernel learns
nothing about hosts; it serves a `Conn` and detects a workspace at a path, as it does now.

### Host

Value object. An ssh destination as the operator's ssh config resolves it: an alias or
`user@name`.

- From `--host` on the command line, else config `remote.host`, else absent. Absent means no
  remote runtime; today's dial order applies.
- Invariants: non-empty; does not begin with `-`; always passed to ssh after `--`. A value
  that could read as an ssh option is refused at construction, not sanitised.
- Equality is string equality. Two spellings of one machine are two hosts, which is what
  ssh thinks too.

### Placement

Value object. The workspace path on the host for a local cwd.

- Rule: a local cwd under the local `$HOME` maps to `<host home>/<same relative path>`. The
  host home arrives in the `client.hello` result (`home`), so the mapping cannot be computed
  before the hello and is not.
- A cwd outside the local home has no mapping. `--cwd <path on host>` names the placement
  directly; without it the command fails naming the flag. `--cwd` without `--host` is refused.
- Invariant: absolute on the host.

### Bridge

The box-side process, `rudy bridge`. Not a domain object so much as the runtime's edge.

- Dials the host's default socket (`Paths.Socket()` on the host, honouring the host's
  environment) with the attach timeout. `ErrNoServer` starts `rudy serve` detached, stderr to
  the host's log file, and redials with backoff up to 30s. Two bridges racing both start a
  daemon; the loser exits on `ErrSocketBusy` and both connect to the winner.
- `CheckSocketOwner` and the peer-uid check on accept run on the host as they do for a local
  client. The bridge is the ssh login user, so they hold.
- Copies messages both ways until either side closes. ssh closing the bridge's stdin closes
  the socket connection, which is the client detaching as ADR 0014 defines it.
- `--no-start` makes `ErrNoServer` an exit rather than a start. `hosts check` uses it.

### Sync

Domain service. Moves the working tree between the Mac and the placement. Runs on the client
before `session.open` for a new session, never for `resume` or `fork`, skipped by `--no-sync`.

Git tree (the local cwd's workspace root is a git checkout):

1. Ensure a checkout at the placement: absent means `git init` there and
   `receive.denyCurrentBranch=updateInstead` set in it. Present and not a git checkout is a
   refusal naming the path.
2. Compare: the local branch head, the placement's head and whether the placement's tree is
   dirty, read over one ssh round trip.
3. Placement clean and its head is an ancestor of the local head, or the branch does not
   exist there: push the branch by URL (`ssh://<host><placement>`) with `--no-verify`, check it
   out there, then copy the Mac's uncommitted changes on top (modified and untracked files as
   a tar, deletions applied). `--no-verify` for sand's reason: a Mac that signs has a pre-push
   hook that refuses unsigned commits, right for GitHub and wrong for the box.
4. Placement dirty, or ahead of the local head: open there as is. The box is truth while it
   holds work. Uncommitted Mac changes are not copied over a dirty box; the command says so
   and opens anyway.
5. Diverged (neither head an ancestor of the other): refuse, naming `rudy hosts pull` and
   `git rebase`. Nothing is written anywhere.

What moves is the workspace root, so the placement the tree lands at is the root's: a cwd
inside a checkout is a subdirectory of what travels, and the same number of segments come off
the placement the cwd maps to. The session still opens at the cwd's placement, which the tree
brings with it. A placement counts as a checkout only when its own toplevel is itself, so a
plain directory inside another checkout on the box is the refusal of step 1 rather than a push
into that other repository. A detached HEAD here refuses before any of it: there is no branch
to push, and `--no-sync` is the way to open on the box anyway.

Non-git tree:

- Placement absent: tar the tree over, everything under the local cwd, and open there.
- Placement present: open there as is. The box is truth.
- Coming back: `rudy hosts pull` tars the placement over the local tree. Files deleted on
  the box stay on the Mac; the tar carries what exists. Session close from a client that
  opened the session over `--host` on a copied tree runs the same pull.

Git trees come back through `rudy hosts pull`: fetch the branch by URL, fast-forward when the
local tree is clean and the local head is an ancestor, refuse otherwise with the fetch already
done and `FETCH_HEAD` named. Sand's `sign` does the same fetch and the ring stays sand's.

No git remote is added to the Mac checkout. Push and fetch go by URL, as sand does, so a
host renamed in ssh config leaves no stale remote behind.

### Version and install

- `client.hello` already returns the server's version. The client compares it with its own.
- Mismatch: a notice in the TUI header and on `-p`'s stderr, a gap in `hosts check`, and the
  connection proceeds. A mismatch is not a refusal because the protocol is versioned by the
  hello, not by the binary.
- Install runs on the host from its own checkout, `remote.source` (default
  `~/projects/rudy`): `git fetch`, `git checkout --detach <rev>`, `make install`. `<rev>` is
  parsed from the Mac's version string, which `make` sets from `git describe --tags --always
  --dirty`: the trailing `g<hash>` component, or the bare hash. A `-dirty` version or `dev`
  refuses to install, saying the Mac's binary is not a commit the box can check out.
- Automatic install happens on connect in two cases: rudy is not on the host's PATH (bridge
  exit 111), or no daemon is running and the versions differ. A running daemon with a
  different version is left alone: restarting it would end live turns.
- `rudy hosts install [host]` runs it on demand. It restarts a daemon that has no live
  session and refuses one that does, unless `--force`.
- `make install` on the host needs Go and the memory checkout beside `remote.source`.
  `hosts check` reports each missing piece by name.

## Reaching the box

Dial order for every command that opens a client (`rudy`, `rudy -p`, `rudy sessions
resume|fork`):

1. `--socket`: that socket or fail.
2. `--embed`: this process.
3. `--host`, else `remote.host`: ssh or fail. No local fallback, for the reason a deaf
   `--socket` is fatal today: a fallback would run the agent on the wrong machine and say
   nothing.
4. Otherwise the 50ms probe of the local socket, then embed.

`--host` with `--socket` or `--embed` is exit 2.

The remote line, one ssh invocation, sand's shape:

```
PATH="$HOME/.local/bin:$HOME/bin:$HOME/go/bin:$PATH"; command -v rudy >/dev/null 2>&1 || exit 111; exec rudy bridge
```

`ssh box '<cmd>'` runs a non-interactive shell with the compiled-in PATH, so the three
user-local bin directories are prepended. Exit 111 is rudy missing; the client answers it by
installing (above) and retrying once. Any other non-zero exit shows ssh's and the bridge's
stderr verbatim, since rudy was there to explain itself.

The client wraps ssh's stdin and stdout in `NewStreamConn` and greets. The greet timeout over
ssh is 30s rather than 2s: the bridge may be starting a daemon that is loading plugins and
refreshing its registry.

`RUDY_SSH` names the ssh binary. It exists for the test shim and is not a config key. The
sync's own git runs with `GIT_SSH_COMMAND` set from it, since a push by URL is the same reach
as the bridge and an ssh honoured for one and ignored for the other would be two transports.

### Disconnect

ssh dropping mid-turn is ADR 0014 unchanged: the turn finishes on the box, entries land in the
box's log, a standing question is denied `no_asker` when the last asker leaves. The TUI, when
the connection ends without the operator quitting, redials with backoff (1s doubling to 30s,
until quit) and sends `session.resume` for the same id, which replays the transcript. `-p`
does not reconnect: it reports the drop and exits non-zero, and `rudy -p --host box
--continue` picks the session up.

## Commands, flags, config

- `--host <destination>`, `--cwd <path>`, `--no-sync`: registered beside `--socket` and
  `--embed` on every command that dials. `--cwd` and `--no-sync` require `--host`.
- `rudy hosts check [host]`: the doctor. Concurrent checks, printed in listed order, each gap
  with the command that fixes it: ssh answers; rudy on PATH over ssh; version against ours;
  a daemon answering (`bridge --no-start`); `remote.source` is a checkout with the memory
  sibling and Go; the placement for the current cwd exists and is a git checkout when the
  local one is. Exit 1 on any gap. Sand's `init` without the config prompts, which rudy does
  not need: `remote.host` is one line in `config.toml`.
- `rudy hosts install [host] [--force]`: version and install, above.
- `rudy hosts push [host] [--no-verify]`: the sync, on demand.
- `rudy hosts pull [host]`: the return path, above.
- `rudy bridge [--no-start]`: the box side. Top-level and verb-shaped, named in the shape
  test beside `serve`: it is the process the transport is made of, and there is no noun it
  acts on.
- Config: `remote.host` (string, `""`), `remote.source` (path, `~/projects/rudy`). Two keys,
  each with a default, a catalogue entry, a contracts row and the regenerated example.
- Protocol: `client.hello` result gains `home`, the server process's home directory. The
  local client ignores it.
- Status bar: the workspace item reads `box:/home/guy/projects/rudy` when the session is
  remote. A render choice, so `ui.status.host` (bool, `true`) governs it.

## Logging

Records on the client: `host: dial` (host, placement), `host: install` (host, rev), `sync:
push` and `sync: pull` (host, mode git or copy, files). On the box: `bridge: connect`,
`bridge: daemon started`, from the bridge process, which uses the host's log file.

## Errors

| situation | behaviour |
|---|---|
| ssh cannot connect | ssh's stderr verbatim, exit 1; no fallback |
| rudy not on the host's PATH | install, retry once; a second 111 is exit 1 with the install's output |
| install refused (dirty or dev version) | exit 1, the reason and `rudy hosts install` after a release build |
| daemon fails to start | bridge's stderr, exit 1 |
| placement unmapped (cwd outside home, no `--cwd`) | exit 2 naming `--cwd` |
| placement exists and is not a git checkout while the local tree is | exit 1 naming the path |
| diverged branches | exit 1 naming `rudy hosts pull` |
| dirty box with uncommitted Mac changes | notice; open without copying |
| version mismatch | notice; connect |
| ssh drops mid-session (TUI) | reconnect with backoff, `session.resume` |
| ssh drops mid-session (`-p`) | exit non-zero naming `--continue` |

## Testing

- Unit: Host and Placement invariants, version parsing, the sync decision table over fake
  heads, the remote line's text.
- The real path in `go test`: `RUDY_SSH` points at a shim that runs the remote line locally
  under a temp `HOME` with `XDG_RUNTIME_DIR` set, so `rudy -p --host shim '...'` runs the PATH
  line, the bridge, the daemon start, the mapped placement, the git push and the pull against
  a temp bare checkout, and the reconnect. Every error branch by breaking the shim: exit 111,
  a daemon that refuses to start, a dirty placement, diverged heads.
- The server suite is unchanged; nothing in the kernel moved.
- One manual run against a real box before any claim of done, reported as what was run.

## Not in this wave

- The git ring: signing, pushing to GitHub, comments and CI pull. Sand's. Rudy runs what the
  box's tools allow and takes no position on credentials there.
- Images from the Mac into a prompt (`sand shot`). Rudy has no image content in the protocol
  or either codec; a bead covers adding it, after which it works local and remote alike.
- Sand's per-checkout flock. A bead: rudy's daemon could take sand's lock while a turn runs
  in a checkout so `sand comments pull` refuses while rudy works.
- Releases. The build-on-box install stands until rudy has release binaries, at which point
  the install step downloads instead and nothing else changes.
- Firecracker. Next spec: one microVM per root session on a KVM host, the six tool plugins
  inside it reached over vsock, the host reached through this wave's `--host`.
