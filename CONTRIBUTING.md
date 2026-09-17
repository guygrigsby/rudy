# Contributing

## The gate

    make check

That is fmt, the vendor-type greps, vet, lint and the whole test suite, including the server
package a second time over the unix socket. CI runs the same target on Linux and macOS, so a
green `make check` locally is the same gate.

    make vuln

is the second one: `govulncheck ./...` over the module and the standard library. It is not
part of `check` because it asks the vulnerability database over the network and `check`
answers offline. CI runs it on every push and again weekly, since an advisory lands against
code that did not change.

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

## Releasing

The tag is the release. `release.yml` builds darwin and linux on arm64 and amd64 from the
tree the tag points at, writes `checksums.txt` beside them and publishes, and the notes it
publishes are the newest section of `CHANGELOG.md`.

That file is embedded in the binary, so the startup header shows the same bullets the
release page does, which is why the changelog is written and committed before the tag and
not after:

    # 1. add the release to CHANGELOG.md, newest first, as "## 0.2.0" with "- " bullets
    make release-check VERSION=v0.2.0   # refuses a tag the changelog does not name
    git commit -am "docs: 0.2.0 changelog"
    git push
    # 2. tag the commit that carries it
    git tag -s v0.2.0 -m "rudy v0.2.0"
    git push origin v0.2.0

`make release-notes` prints what the release page will carry. The workflow runs
`release-check` before it builds anything, so a tag that disagrees with the changelog fails
in seconds rather than after four cross-compiles.

## The CLA

A first pull request gets a bot asking you to sign
[the CLA](CLA.md) with one comment. You keep the copyright in what you write;
the agreement grants the right to ship it under terms other than the AGPL,
which is what a commercially licensed build needs. A contribution with no
signature behind it can only ever be AGPL, so it would have to be rewritten
before it could go in one.

## Filing something you found and did not fix

Open a GitHub issue. The bug and feature forms ask for what a report needs, and a
vulnerability goes through [private reporting](https://github.com/guygrigsby/rudy/security/advisories/new)
rather than an issue.

Maintainer-side, that issue becomes a bead: the tracker is
[beads](https://github.com/gastownhall/beads), the issues are committed in
`.beads/issues.jsonl`, and `bd create` writes one. Nothing about contributing needs bd
installed. A `TODO` comment is not a tracked issue.
