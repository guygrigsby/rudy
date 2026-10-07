# 54. A same-user socket client is the operator, and a second asker is announced

- Status: Accepted
- Date: 2026-10-07
- Deciders: Guy Grigsby

Records the decision on `rudy-i8ty` and extends the trust model ADR 0014 and
`rudy-jkz` recorded.

## Context

Any connection with the server's uid may declare `asker: true`, resume any
live session and answer its permission questions; the first answer wins.
When a daemon is serving, a bash command the gate has allowed once can start
a process that does exactly that, and every later question is answered by
the model's own process, logged as the operator. The audit named this the
gate's one structural bypass.

## Decision

Accepted as designed, with one addition. The gate is a rail against a model
that has not yet run anything, and a boundary only until it has: a command
that runs as the operator can already edit the config, the trust record and
the session log, and needs no socket to do damage. The uid is the boundary,
as ADR 0014 says, and the model's own child processes are inside it the
moment they exist. Standing between a process and its own user is a sandbox
(`rudy-uvo`), not a permission check.

The addition: when an asker attaches to a session that already has one, every
subscriber of that session is told, naming the client that joined. The
operator who sees a second asker they did not start knows what is happening.
It is a tripwire, not a lock, and the README says which it is.

## Consequences

- The README trust model names the case: in permissive mode with a daemon
  serving, a model that has run one shell command can approve its later ones.
  Strict mode with no daemon, which is what a plain `rudy` runs when nothing
  answers on the socket, is the setting for untrusted content.
- `permission_decision` rows are unchanged. The notice is a `notice`
  notification, not an entry.

## Alternatives considered

- A token the daemon mints for the client that started the turn: readable by
  every same-uid process, including the one being gated.
- Refusing an asker whose peer pid descends from a tool process: a `nohup`
  reparents to init before the question is asked.
- Only the connection that submitted the turn may answer: the attacker
  submits its own turns in its own session.
