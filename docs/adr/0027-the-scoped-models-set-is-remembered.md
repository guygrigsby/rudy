# 27. The scoped models set is remembered, in a file of its own

- Status: Accepted
- Date: 2026-09-09
- Deciders: Guy Grigsby

## Context

ADR 0020 made the set `ctrl+p` cycles through the client's own state, never the
session's: the cycle is a keyboard habit, not something a log should carry. It
was left in memory, so choosing it was a per-run chore. A shortlist a person
picks out of sixty models is worth picking once.

Where it goes is the question ADR 0021 already answered for everything else:
`config.toml` is the operator's, `rudy config sync` is the only thing that
writes it, and a command that persists state writes its own file, as `mcp add`
and `plugin install` do.

## Decision

1. **`$XDG_DATA_HOME/rudy/scope.toml` holds it**, beside `trust.toml` and the
   plugin lock. It is written by `/scoped-models` and read when a client opens.
   Nothing else writes it, and `config.toml` never carries it.

2. **The scope is the person's, not the workspace's.** One shortlist follows
   them between repositories, the way the default model does. A per-workspace
   scope can be added later by a file under `.rudy`; nothing here forecloses it,
   and nobody has asked.

3. **Clearing it is a choice that persists too.** An empty set means the whole
   registry, and it is written rather than left as "no file yet", so going back
   to everything survives the client like any other choice.

4. **A file that will not parse is printed, not swallowed.** The client opens on
   the whole registry and says why. Somebody chose those models; losing the
   choice quietly would be worse than a line on stderr.

5. **A ref the registry no longer carries is dropped when the cycle is built**,
   which is where ADR 0020 already dropped it. The file keeps what was chosen; a
   model that comes back to the registry is in the cycle again.

## Consequences

- A client with nowhere to write (a test, an embedded run) still takes the
  choice for its own run: the save is a function the client may not have.
- `/scoped-models` now touches the disk on confirm. It is one small file written
  on a deliberate action, not on a keystroke.

## Alternatives considered

- `ui.models.scope` in `config.toml`: the harness would be writing the
  operator's file, which ADR 0021 forbids for good reasons (comments, ordering,
  values that are theirs).
- Session state: the cycle is the client's, and a scope in the log would follow
  a session to another client that never asked for it (ADR 0020).
- Remembering nothing, and letting a person retype the picker each run: which is
  the thing that was wrong.
