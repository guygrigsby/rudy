# rudy

A coding agent harness in Go: one binary, one JSON-RPC protocol, a Bubble Tea
v2 client, plugins linked in or spawned, sessions as append-only JSONL logs.

## Where things are

- `docs/specs/2026-09-07-rudy-design.md` is the design. Read it first.
- `docs/specs/rudy-context-map.md`, `rudy-domain-model.md`, `rudy-contracts.md`
  are normative over code. A change to a record shape, a protocol method or a
  hook payload edits the contract before the code.
- `docs/adr/` records decisions, append-only. Supersede, never rewrite.
- Work is tracked in beads (`bd`). Bugs fixed in flight belong to the current
  task's commit.

## Rules that shaped the code

- The kernel has no private path to its own features. Every built-in tool,
  command and provider registers through the plugin interface an external
  plugin would use.
- No vendor or wire types outside the two codecs and the provider plugins.
  `grep anthropic\.` and `grep openai` hit only under `internal/provider/`.
  `make vendor-types`, part of `check`, greps the three vendor SDKs the same way:
  `anthropic-sdk-go` only under `internal/provider/anthropicmsgs/`,
  `modelcontextprotocol` only under `internal/plugins/mcp/`, `memory-go` only
  under `internal/plugins/memory/`.
- An unsafe tool runs only after its `permission_decision` allow entry is
  appended and fsynced. No asker means deny.
- Tool inputs and thinking signatures are raw bytes end to end. Never
  re-marshal them.
- `config.toml` is read, never written by the harness. Commands that persist
  state (`mcp add`, `plugin install`) write their own dedicated files. XDG
  paths, `XDG_CONFIG_HOME` honored first, default `~/.config/rudy`.
- CLI is noun then verb (`rudy skills migrate`). Long flags take two dashes.
- No model lists in config beyond a default. The registry comes from
  `/v1/models`.
- No timers for refresh. Registry refreshes on session open, picker open and
  model-not-found.
- Every render choice is a config field with a default. A plugin cannot clear
  another plugin's status or widget.
- charm.land v2 modules and provider SDKs are pinned exactly. Bumps are gated
  on the teatest goldens.
- `for range n`, never a three-clause count loop.
- Commits: terse, verb-first, no em or en dashes, no Oxford commas, no
  attribution trailers. Prefix by area: `session:`, `tui:`, `plugin:`,
  `provider:`, `docs:`.

## Makefile targets

`build` (default), `test`, `lint`, `check`, `fmt-check`, `vendor-types`,
`install`, `redeploy`. CI calls these and nothing else.

`make test` runs the whole suite and then the server package again under
`RUDY_TEST_TRANSPORT=socket`, so every server test runs over both transports:
the in-memory pipe and the unix socket `rudy serve` listens on.

## Building against memory-go

`go.mod` replaces `github.com/aeryx-ai/memory/memory-go` with
`../memory/memory-go`, so the memory repository has to sit beside this one:
`~/projects/rudy` and `~/projects/memory`. CI checks the two out side by side
under the workspace, rudy into `rudy/` and `aeryx-ai/memory` into `memory/`,
and runs `make check` with `working-directory: rudy`. The workflow cannot pass
until the memory repository is pushed to `aeryx-ai/memory`. vimbubble v2 is
required from `github.com/guygrigsby/vimbubble/v2` by pseudo-version; a change
to it is a push there and a `go get` here.


## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
<!-- END BEADS INTEGRATION -->

Beads has no Dolt remote. The committed `.beads/issues.jsonl` export is the
sync path; `bd dolt push` is not part of session close.
