# 14. The serve wave: a unix socket on the same server, clients that attach or embed, questions to every asker

- Status: Accepted, implemented 2026-09-09
- Date: 2026-09-09
- Deciders: Guy Grigsby

## Context

ADR 0002 fixed one binary and one protocol with three transports: in-memory
for the embedded server, a unix socket for `rudy serve`, stdio for spawned
plugins. The kernel, plugins and TUI waves built everything on the first and
third; the socket has a contract row, a path and a caller class but no code.
The design's sequencing item 5 is `rudy serve`, the unix socket and reattach.

Writing the plan surfaced decisions the design and the contracts leave open:

- Which clients attach when a daemon is running, and how they find it.
- What survives a client's exit: the design says sessions do, the server
  today closes a session when its last subscriber leaves and no turn is
  running.
- Who answers a permission question when two terminals watch one session,
  and what happens to a standing question when its asker's terminal closes
  (bead `rudy-k0.23`: the turn parks forever).
- What a client attaching mid-turn is told about the turn.
- Where the socket lives when `XDG_RUNTIME_DIR` is unset: the contract says
  `$TMPDIR/rudy-<uid>`, `config.XDG` says `$TMPDIR/rudy`.
- How "the same suite replayed over unix socket" runs.

## Decision

1. **`rudy serve` is the embedded server on a listener.** It runs `Build`,
   listens on the socket, and serves every accepted connection with the same
   `Server.Serve` the in-memory transport uses, as caller class client. A
   plugin never arrives over the socket. The process is foreground, logs to
   stderr, and on SIGINT or SIGTERM stops accepting, cancels every active
   turn so each records `turn_interrupted`, runs `Shutdown` and removes the
   socket file. A second `rudy serve` on a socket that answers exits 1
   naming it; a socket file that refuses connections is stale and replaced.

2. **The socket is `Paths.Runtime/rudy.sock`, and `Runtime` falls back to
   `$TMPDIR/rudy-<uid>`.** The contract's path wins over `config.XDG`'s
   `$TMPDIR/rudy`; `/tmp` is shared between users on Linux and the uid is
   what keeps one user's socket directory out of another's. The directory is
   `0700`, the socket `0600`, and the server accepts only a connection whose
   peer uid is its own (`LOCAL_PEERCRED` on darwin, `SO_PEERCRED` on linux),
   closing any other before reading a byte. `--socket <path>` overrides the
   path on `rudy serve` and on every client command.

3. **Clients attach when a daemon answers, else embed.** `rudy`, `rudy -p`,
   `rudy sessions resume|fork` dial in this order: an explicit `--socket`
   must connect or the command fails; otherwise the default socket is probed
   with a 50ms connect timeout and used when it answers; otherwise the server
   is embedded as today. `--embed` skips the probe. This is Codex's order
   (ADR 0002) without the remote endpoint.

4. **Session lifetime does not change; the process does.** A session stays
   live while a turn is active or a client is attached, and closes at rest
   when nobody is attached, exactly as the embedded server does. No idle
   timer. What `rudy serve` adds is that the process outlives the terminal,
   so a turn keeps running after its client exits and its entries are in the
   log when the client returns. A reattach is `session.resume`: every entry
   replayed, then, when a turn is active, the current `turn.state` and any
   standing `permission.requested` to an asker, then live notifications. A
   session resumed at rest is a cold load with the same transcript, which is
   ADR 0002's consequence.

5. **A permission question goes to every attached asker; the first answer
   decides.** `permission.requested` is sent to each attached asker
   connection and to an asker that attaches while the question stands. The
   first `session.answer` decides and every later one is `conflict`. When
   the last asker detaches while a question stands, the Gate denies it with
   `no_asker`, which is the design's rule applied at the moment it becomes
   true, and the turn continues. A question never parks a turn on a
   connection that is gone.

6. **A locked session names the socket.** `unavailable` for a session
   another process holds carries `data.socket`, the path a daemon would be
   serving on, and the CLI prints it as the way in. The embedded server
   reports the same when the holder is another embedded `rudy`; the hint is
   then wrong about who holds it and right about what to try, and the
   process holding a session without a listener is the case the design
   already documents as "a second process is kept out".

7. **The server suite is replayed over the socket by a transport switch in
   its dial helper.** `RUDY_TEST_TRANSPORT=socket` makes every server test
   dial a real listener in a temp runtime dir instead of `protocol.Pipe()`;
   `make test` runs the server package once per transport. The peer-uid
   check and the stale-socket replacement have their own tests.

## Consequences

- `rudy` and `rudy -p` behave differently when a daemon is running: their
  sessions live in it, its plugins and registry serve them, and a turn a
  terminal abandons finishes. `--embed` is the escape.
- Two terminals on one session both see the prompt; whoever answers first
  wins and the other sees the `permission_decision` entry take its prompt
  down (the TUI already does this by tool_use id).
- Bead `rudy-k0.23` closes with decision 5.
- `config.XDG`'s runtime fallback changes; nothing read `Paths.Runtime`
  before this wave.
- The ACP adapter stays deferred (contracts caller class table); it is a
  socket client with nothing new to add here.
- `sessions list` keeps reading the store directly; it does not say which
  sessions a daemon holds. A `LIVE` column would need a lock probe that can
  race a real open; a bead if wanted.

## Alternatives considered

- An idle timer that closes unattached sessions after a grace: a clock that
  fires when nothing changed, and the cold resume already gives the same
  transcript. Rejected.
- Keeping a standing question pending until an asker returns: parks the
  turn for as long as the daemon lives on a decision nobody can see.
  Rejected for the design's no-asker rule.
- Routing questions to the first asker only (today): a second terminal
  never sees the prompt and cannot answer. Rejected.
- Attaching only `rudy` and embedding `rudy -p` always: two behaviours for
  one seam, and a script would not benefit from the daemon's warm registry
  and plugins. Rejected; `--embed` covers the script that wants isolation.
- A launchd or systemd unit installed by rudy: ADR 0002 says no second
  process exists unless the user starts one; an example unit in the README
  is enough.
