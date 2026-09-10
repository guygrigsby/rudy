# 21. Config is the UX, and the harness may add keys to it

- Status: Accepted
- Date: 2026-09-09
- Deciders: Guy Grigsby
- Supersedes the "read, never written" half of ADR 0006 and the design's `config.toml` rule

## Context

Three things the owner asked for at once, and they are one thing.

The header had a `[ui.header]` table with six keys, but its parts were not
among them: whether the greeting drew, whether the cat drew, which of the
session's facts appeared, whether there was a box at all. UX as config is a
tenet, and a part with no field is a part somebody has to patch code to change.

A config key was described in three places that could disagree: the defaults in
Go, the contracts table, and whatever a reader had in their own file. Nothing
compared them.

And a key added by a release reached nobody: an operator's `config.toml` was
written once, by hand, and every default added after that lived only in the
binary. The rule that made that so is ADR 0006's, restated in the design: the
harness reads `config.toml` and never writes it.

## Decision

1. **Every part of the header is a field.** `frame`, `greeting`, `mark` and
   `facts` join `show`, `animate`, `name`, `tips`, `updates` and `max_width`.
   `facts` is an ordered list from `model`, `thinking`, `mode` and `workspace`,
   so what the box says about the session and the order it says it in are both
   config. An entry outside that set is a load error naming it.

2. **One catalogue describes every key.** `internal/config/docs.go` carries, per
   key, the sentence written above it and an example worth showing beside the
   default. `Defaults()` says what a key is worth; the catalogue says what it is
   for. Both the shipped example and the sync below are generated from them, so a
   key is described once.

3. **The repository ships `examples/config.toml`, generated.** `rudy config
   example` prints it and `make config-example` writes it. A test fails when the
   file is not what the generator prints, so a changed default changes the
   example in the same commit.

4. **The harness may add a missing key to `config.toml`, and may change nothing
   else.** `rudy config sync` adds keys that are absent, each under the table it
   belongs to and under the comment that says what it is for, and leaves every
   value, comment, ordering and blank line that is already there exactly as it
   is. It is text, not a re-marshal, because a re-marshal would take an
   operator's comments away. `make install` runs it. `--dry-run` says what it
   would add.

   This reverses "never written", and the reversal is bounded: sync adds keys
   and only keys. No command changes a value in `config.toml`; the ones that
   persist state still write their own files (`mcp.toml`, the plugin store).

5. **Every fact that lives twice has a guard.** In `internal/config`: every
   default is documented and every documented key is a default; the shipped
   example is what the generator prints; the example loads and means the
   defaults; every default has a row in the contracts table and every `ui.*` row
   in the table is a real key; the README names the commands it tells people to
   run. Each failure names the file to edit.

## Consequences

- Adding a config key now means four things or the build fails: the default, the
  catalogue entry, the contracts row, and the regenerated example. That is the
  point.
- An operator who has deleted a key they do not want will have it added back by
  the next `make install`, with its default, which changes no behaviour. Deleting
  is not the way to say no; setting the value is.
- `config.toml` gains comments it did not have, which is what makes a key
  discoverable in the file rather than only in a spec.

## Alternatives considered

- Generating the contracts table from the catalogue: the contracts are normative
  and hand-written prose about caller classes and errors, not a dump of
  defaults. A guard that compares them keeps both honest without making one a
  build artefact of the other.
- A `rudy config edit` that writes values: the moment the harness owns values,
  an operator's file stops being theirs. Sync adds keys and stops.
- Leaving the file alone and printing "new keys are available": a message
  nobody acts on, every release.
