# 24. The system prompt is a template an operator can replace

- Status: Accepted
- Date: 2026-09-09
- Deciders: Guy Grigsby

## Context

The system prompt was a Go string. Nothing showed a person what was actually
sent, and changing a word meant a rebuild. Every other render and behaviour
choice in rudy is a config field with a default; the first thing the model reads
was the exception.

The parts are not all the operator's to write, though. The tool list is the
plugin registry's, the `AGENTS.md` sections are the workspace's, and an agent
definition's body is the definition's. What an operator owns is the arrangement:
which parts appear, in what order, with what said around them.

## Decision

1. **The prompt is a template with a closed set of values.** `${base}`,
   `${tools}`, `${agents}`, `${version}`, `${workspace}`, `${project}`,
   `${model}`, `${date}` and `${os}`. `$${` writes a literal `${`. A name outside
   the set is an error naming it and listing the set, the same rule an unknown
   theme role or key action id gets: a typo in a prompt is otherwise silent, and
   silence here is worse than anywhere else in the config.

2. **The built-in prompt is written in that template language.** It is
   `${base}`, `${tools}`, `${agents}`, and `rudy prompt example` prints it. What
   an operator overrides is a file exactly like the one rudy ships, rather than a
   different kind of thing.

3. **`system.md` under the config directory replaces it, and `prompt.file` names
   one elsewhere.** A file nobody named is not an error when it is absent; a file
   named in config that cannot be read is a notice, because the operator asked
   for that file. Either way the built-in prompt is what runs.

4. **A template that fails to render is a notice, not a dead session.** The
   session gets the built-in prompt and the operator is told which value they
   misspelled. A session that silently lost its instructions would be worse than
   one that says so.

5. **`${date}` and `${os}` exist because the model needs them.** A model with a
   training cutoff behind it guesses the date, and guesses wrong; the platform
   decides what a shell command may say. Neither was in the prompt before.

6. **`rudy prompt show` prints what a session here would send.** Template
   expanded, tools listed, `AGENTS.md` included. It builds the same server a
   session does, because the tool list is the plugins' and the sections are the
   workspace's.

## Consequences

- An operator who writes a template that omits `${tools}` gets a model that is
  not told what it has. That is the point of an override, and `rudy prompt show`
  is how they see it.
- The template is read once, when the server is built, so a turn never waits on a
  disk read and an edit takes effect on the next start.
- `turn.SystemPrompt` keeps its signature and now renders the built-in template
  internally, so every existing caller and test is unchanged.

## Alternatives considered

- Go's `text/template`: conditionals and loops over values nobody has, a syntax
  error surface far larger than nine names, and a prompt file that reads like
  code rather than like a prompt.
- Appending an operator's file to the built-in prompt instead of replacing it:
  no way to put anything before the opening paragraph, and no way to drop a
  section, which is most of what an override is for.
- Reading the file per turn so an edit takes effect immediately: a disk read on
  the hot path for a file that changes once a month.
