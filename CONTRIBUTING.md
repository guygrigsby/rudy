# Contributing

## The gate

    make check

That is fmt, the vendor-type greps, vet, lint and the whole test suite, including the server
package a second time over the unix socket. CI runs the same target on Linux and macOS and
nothing else, so a green `make check` locally is the same gate.

Nothing to check out beside this repository: every dependency resolves from its own module.

## What changes before the code

`docs/specs/rudy-contracts.md`, `docs/specs/rudy-domain-model.md` and
`docs/specs/rudy-context-map.md` are normative over the code. A change to a record shape, a
protocol method, a config key or a hook payload edits the contract first and the code second,
and a test in `internal/config` fails when a config key exists in one place and not the four
it belongs in.

Decisions go in `docs/adr/NNNN-kebab-title.md`, append-only: supersede, never rewrite.

## Rules the code already follows

- Every built-in tool, command and provider registers through the plugin interface an
  external plugin would use. The kernel has no private path to its own features.
- No vendor or wire types outside the two codecs and the provider plugins. `make vendor-types`
  is that rule as a grep.
- An unsafe tool runs only after its `permission_decision` allow entry is appended and
  fsynced. No asker means deny.
- Tool inputs and thinking signatures are raw bytes end to end. Never re-marshal them.
- `config.toml` is the operator's. `rudy config sync` adds keys that are absent and changes
  no value, comment or ordering that is already there.
- Every render choice is a config field with a default.
- `for range n`, never a three-clause count loop.

## Tests

A bug is reproduced by a failing test before it is fixed. Concurrency changes run under
`-race`, which is what `make test` does. The TUI's goldens are teatest goldens: regenerate
them deliberately, and read the diff.

## Commits

Terse, verb-first, present tense. No em or en dashes, no Oxford commas, no attribution
trailers. Prefix by area: `session:`, `turn:`, `tui:`, `plugin:`, `provider:`, `web:`,
`remote:`, `docs:`, `build:`.

Say why the change exists, not what the diff already shows.

## Filing something you found and did not fix

Issues live in `.beads/issues.jsonl`, tracked with [beads](https://github.com/gastownhall/beads).
`bd create` writes one. A `TODO` comment is not a tracked issue.
