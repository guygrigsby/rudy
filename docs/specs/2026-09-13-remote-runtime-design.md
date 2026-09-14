# The remote workspace: a box runs the agent, the Mac is the control plane

Status: Hosts placement, sync and install design, approved in conversation 2026-09-13. ADR
0029 records the original decisions. ADR 0032 and
`2026-09-14-acp-remote-runtime-design.md` supersede this document's private ssh session carrier
and bridge lifecycle after compatibility. This document remains normative for Host, Placement,
Sync and versioned install. Companion to `github.com/guygrigsby/sand`, whose behavior this wave
brings into Rudy for the workspace half and leaves alone for the git ring.

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

ADR 0029 originally made the remote kernel a fourth private transport plus a workspace
transfer. ADR 0032 replaces that transport with an ACP edge. The workspace transfer below does
not change.

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

### Compatibility bridge

The original box-side process, `rudy bridge`. ADR 0032 replaces it with `rudy acp` after ACP
parity. These rules remain only for installed binaries during the compatibility phase.

- Dials the host's default socket (`Paths.Socket()` on the host, honouring the host's
  environment) with the attach timeout. `ErrNoServer` starts `rudy serve` detached, stderr to
  the host's log file, and redials with backoff up to 30s. Two bridges racing both start a
  daemon; the loser exits on `ErrSocketBusy` and both connect to the winner.
- `CheckSocketOwner` and the peer-uid check on accept run on the host as they do for a local
  client. The bridge is the ssh login user, so they hold.
- Copies messages both ways until either side closes. ssh closing the bridge's stdin closes
  the socket connection, which is the client detaching as ADR 0014 defines it.
- `--no-start` makes `ErrNoServer` an exit rather than a start. `hosts check` uses it.
- `--stop` never starts a daemon. It greets an answering Server, sends `server.shutdown`,
  requires matching `server.stopped` proof followed by EOF after full runtime cleanup, and treats no
  answering Server as success. It never reads or signals a process id.

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

The reply to the probe of step 2 opens with a `rudy-inspect` marker and is read from the last
one: an ssh command runs a shell that reads the operator's rc file, and a box that greets its
commands would otherwise shift every field, read as absent and have step 1 initialise a
repository inside a directory that is someone else's. A reply with no marker is refused rather
than guessed at. On this side, only git's own "not a git repository" makes a cwd a tree to
copy; a checkout git refuses to open (a dubious owner, a broken gitfile, a bare repository) is
an error naming git's message, since copying it would stream the object store over ssh. A
deletion whose path is a directory on the box is never removed, only named: it is work the box
holds, and `rm` would fail on it and take the sync with it.

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
  opened the session over `--host` on a copied tree runs the same pull, and only when the
  turn finished: closing the connection hangs up the bridge and leaves the daemon running,
  so an interrupted client says `rudy hosts pull <host>` rather than take a copy of a tree
  still being written.
- The archive is the box's bytes, so it is unpacked by rudy rather than by the client's
  `tar`, under the rules of `internal/tarx` (no `..`, no absolute path, no backslash, no
  symlink or hard link, an entry cap and a byte cap), into a staging directory inside the
  local root, and moved in file by file only once the whole archive has been accepted. The
  move goes through `os.Root` on the local root and refuses a destination whose path passes
  through a symlink already in the tree, or whose existing entry is of the other kind: the
  archive need carry no link of its own for `sub -> /etc` here to turn `sub/passwd` into a
  write outside the tree. A refusal leaves the working tree untouched.

Git trees come back through `rudy hosts pull`: fetch the branch by URL, fast-forward when the
local tree has no uncommitted changes to tracked files (`git diff-index --quiet HEAD --`,
untracked files ignored: only the box's side counts untracked, where it is work this machine
has not seen) and the local head is an ancestor, refuse otherwise with the fetch already done
and `FETCH_HEAD` named. Sand's `sign` does the same fetch and the ring stays sand's.

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
- `rudy hosts install [host]` runs it on demand. Without `--force`, an answering daemon is
  left running and the command reports that the new binary takes effect on its next start.
  With `--force`, the command keeps the ACP connection it inspected, installs the binary,
  sends negotiated `_rudy/server_shutdown`, requires its terminal `stopped` result, then waits
  for adapter EOF and connects again through the normal ACP start path. EOF alone is never
  cleanup proof. The old and new Server `instanceId` values must differ.
  Compatibility binaries use `rudy bridge --stop` under ADR 0030. A legacy daemon without
  protocol-owned shutdown fails closed and is never replaced by pid signaling.
- `make install` on the host needs Go and the memory checkout beside `remote.source`.
  `hosts check` reports each missing piece by name.

## Reaching the box

Dial precedence for every command that opens a client (`rudy`, `rudy -p`, `rudy sessions
resume|fork`) remains:

1. `--socket`: that socket or fail.
2. `--embed`: this process.
3. `--host`, else `remote.host`: ACP over ssh or fail. No local fallback.
4. Otherwise the 50ms probe of the local socket, then embed.

`--host` with `--socket` or `--embed` is exit 2.

The ACP contract, remote command and reconnect behavior live in
`2026-09-14-acp-remote-runtime-design.md`. Hosts still performs Sync before ACP `session/new`
and passes the absolute Placement as its cwd. `RUDY_SSH` still names the ssh binary for the
test shim and Sync's `GIT_SSH_COMMAND`.

### Disconnect

ssh dropping mid-turn leaves the daemon and Turn running. The ACP adapter detaches; a standing
question is denied `no_asker` when the last asker leaves. The TUI reconnects and uses ACP
`session/load` for the same Rudy ULID so missed entries rebuild its view. `-p` reports the drop
and exits nonzero; `rudy -p --host box --continue` loads the Session.

## Commands, flags, config

- `--host <destination>`, `--cwd <path>`, `--no-sync`: registered beside `--socket` and
  `--embed` on every command that dials. `--cwd` and `--no-sync` require `--host`.
- `rudy hosts check [host]`: the doctor. Concurrent checks, printed in listed order, each gap
  with the command that fixes it: ssh answers; rudy on PATH over ssh; version against ours;
  a daemon answering through `rudy acp --no-start`; `remote.source` is a checkout with the memory
  sibling and Go; the placement for the current cwd exists and is a git checkout when the
  local one is. Exit 1 on any gap. Sand's `init` without the config prompts, which rudy does
  not need: `remote.host` is one line in `config.toml`.
- `rudy hosts install [host] [--force]`: version and install, above.
- `rudy hosts stop [host]`: negotiated ACP shutdown, idempotent when no daemon answers.
- `rudy hosts push [host] [--no-verify]`: the sync, on demand.
- `rudy hosts pull [host]`: the return path, above.
- `rudy acp [--no-start]`: the box-side ACP v1 agent. Top-level and verb-shaped, named in the
  shape test beside `serve`. `rudy bridge` remains only during the compatibility phase.
- Config: `remote.host` (string, `""`), `remote.source` (path, `~/projects/rudy`). Two keys,
  each with a default, a catalogue entry, a contracts row and the regenerated example.
- Protocol: `client.hello` result gains `home`, the server process's home directory, and
  `instance_id`, the process-lifetime Server identity. The local client ignores `home`.
  `server.shutdown {}` is accepted only from a greeted non-plugin connection carrying the
  same-user marker minted by the unix listener. Its tentative claim fences new work without
  changing lifecycle state. The success response is sent before the Server transitions;
  successful cleanup sends `server.stopped {instance_id, state: "stopped"}` on the retained
  connection before EOF. Bare EOF is failure.
- Status bar: the workspace item reads `box:/home/guy/projects/rudy` when the session is
  remote. A render choice, so `ui.status.host` (bool, `true`) governs it.

## Logging

Records on the client: `host: dial` (host, placement), `host: install` (host, rev), `sync:
push` and `sync: pull` (host, mode git or copy, files). `rudy acp` writes fixed adapter
diagnostics and correlation ids to stderr; daemon detail stays in the host's log file.

## Errors

| situation | behaviour |
|---|---|
| ssh cannot connect | ssh's stderr verbatim, exit 1; no fallback |
| rudy not on the host's PATH | install, retry once; a second 111 is exit 1 with the install's output |
| install refused (dirty or dev version) | exit 1, the reason and `rudy hosts install` after a release build |
| daemon fails to start | Fixed ACP adapter stderr with a correlation id, exit 1; detail stays in the box log and is never tailed over ssh |
| placement unmapped (cwd outside home, no `--cwd`) | exit 2 naming `--cwd` |
| placement exists and is not a git checkout while the local tree is | exit 1 naming the path |
| diverged branches | exit 1 naming `rudy hosts pull` |
| dirty box with uncommitted Mac changes | notice; open without copying |
| version mismatch | notice; connect |
| ssh drops mid-session (TUI) | reconnect with backoff, ACP `session/load` |
| ssh drops mid-session (`-p`) | exit nonzero naming `--continue` |

## Testing

- Unit: Host and Placement invariants, version parsing, the sync decision table over fake
  heads, the remote line's text.
- The real path in `go test`: `RUDY_SSH` points at a shim that runs the remote line locally
  under a temp `HOME` with `XDG_RUNTIME_DIR` set, so `rudy -p --host shim '...'` runs the PATH
  line, ACP agent, daemon start, mapped placement, git push and pull against a temp bare
  checkout, then reconnect by load. Every error branch by breaking the shim: exit 111, a daemon
  that refuses to start, a dirty placement and diverged heads.
- The server suite proves shutdown authorization, response-before-transition ordering,
  single transition semantics and completion ordering. The ACP path proves stop with no
  daemon, stop with a daemon and install replacement with a changed Server identity.
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
