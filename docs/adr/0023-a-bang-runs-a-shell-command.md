# 23. A bang runs a shell command, records it, and starts no turn

- Status: Accepted
- Date: 2026-09-09
- Deciders: Guy Grigsby

## Context

Checking something in the middle of a session meant leaving it: another
terminal, or asking the model to run `git status` and paying a turn for it. Every
harness with a `!` has the same answer, and the question each of them settles
differently is what the model sees afterwards.

Only four entry kinds reach a provider request: `user_message`,
`assistant_message`, `tool_result` and `compaction`. A `note` is display and
never leaves the client. So what `!` records decides whether the model can read
it at all.

## Decision

1. **`!<command>` runs the command and records it as one `user_message` with
   `source: shell`.** The model reads it with the next thing the operator sends.
   The transcript shows the command and its output. No turn starts, so peeking
   at `git status` costs nothing until there is something to say.

   The alternatives were a client-only run, which cannot be read by the model and
   is therefore useless in the case that motivates `!`, and running a turn
   immediately, which spends a completion on every peek.

2. **It runs through the registered `bash` tool.** Not a private exec in the
   server: the operator's shell command is the same tool the model calls,
   invoked by hand. A session whose view has no `bash` (an agent definition took
   it away) has nowhere to run one and is told so.

3. **The Gate does not run.** The gate exists so a human answers for what the
   model wants to do. Here the human is the one who typed it, and asking them to
   approve their own keystroke is theatre. The socket's uid check is still what
   decides who may talk to a session at all.

4. **A turn in flight refuses it.** `conflict`, the same as any other write to a
   busy session. The command would otherwise land in the middle of a turn's
   entries and read as though the model had run it.

5. **`shell` is its own source and its own row kind.** A shell command is not
   something the operator said, so it is not a user row and does not wear the
   user prefix. It is drawn in a new `shell` theme role, which also paints the
   composer while the draft starts with a bang: the whole input area changes
   colour, which is the tell that Enter will run rather than send.

   The role defaults to `warning`'s amber rather than red, since red is for
   errors.

## Consequences

- The recorded text is `$ <command>` and then whatever it printed, so a model
  reading the log sees a shell transcript, and a person reading the terminal
  sees what they would have seen in one.
- A long command holds the call open for the tool timeout. That is the same
  bound the model's own `bash` call has.
- The command runs where the session is, which under `rudy serve` is the
  daemon's machine and not necessarily the terminal's. Correct, since the
  workspace is the daemon's, and worth knowing.
- Output is not truncated by this path; whatever the `bash` tool returns is what
  is recorded, so a `!cat` of something enormous costs context. A bound belongs
  in the tool, where the model's calls are bounded too.

## Alternatives considered

- A `/shell` command: commands run through `command.run`, whose Action set has
  no way to append a user message, and a bang is one character rather than
  seven.
- Recording a `note`: display only, never reaches the model, which defeats the
  purpose.
- Running it in the client: the client speaks the protocol and owns no
  workspace, and against a daemon it would run on the wrong machine.
