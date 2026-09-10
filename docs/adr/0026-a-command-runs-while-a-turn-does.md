# 26. A command runs while a turn does

- Status: Accepted
- Date: 2026-09-09
- Deciders: Guy Grigsby

## Context

`submit` asked what the turn was doing before it looked at the draft, so a running
turn took everything typed as a follow-up message and put it in the queue. A slash
word went in with the rest and ran when the turn rested, which was right for a
message and wrong for every command: `/permissions`, `/rename`, `/scoped-models`
and `/exit` are all about the client or the session, not about the conversation,
and somebody who types one while the model is thinking means now.

The kernel already knows which commands cannot run mid-turn. `startTurn`,
`handleShell` and `compact` each refuse with `conflict` and the words "a turn is
active". The client was guessing at a rule the server already enforces.

## Decision

1. **Enter on a slash draft runs the command, whatever the turn is doing.** The
   queue is for messages. A command reaches `command.run` immediately.

2. **The server refuses the ones that need a resting session, in its own words.**
   A command whose action submits a prompt or compacts the log gets `conflict`,
   which reaches the user as "a turn is active" the way any command's refusal
   reaches them.

3. **A refusal for that reason puts the draft back in the editor.** The command
   was refused by a race with the model, and retyping it is work the client can
   spare. It goes back only into an editor that is empty: anything typed since is
   the person's own and outranks it.

4. **The client's own commands answer with no server at all.** `/exit`, `/quit`
   and `/scoped-models` are checked before the connection is, so a client whose
   server has gone away can still be closed by the command that closes it.

5. **alt+enter still queues.** Explicitly asking for a follow-up queues a slash
   draft the same as any other, which is the one way to say "when this turn is
   done" and is what ADR 0015's queue was for.

## Consequences

- A command that changes the session's model or mode can now land in the middle of
  a turn. That is what the session log is for: the change is an entry, ordered
  against the turn's own, and the turn already running keeps the model it started
  with.
- The bang stays where it was. A shell command is refused mid-turn by the server
  (`handleShell`), so it queues, which is what ADR 0023 wanted from it anyway.

## Alternatives considered

- Keeping a list in the client of which commands are safe mid-turn: the client
  does not know what a plugin's command does, and the list would be wrong the day
  somebody registers one.
- Queueing a refused command instead of putting it back: it would fire later
  without being asked again, which for `/compact` is a surprise rather than a
  convenience.
