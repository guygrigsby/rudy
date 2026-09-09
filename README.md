# rudy

A coding agent harness in Go. One binary that runs a model-driven loop over a
workspace with tools, sessions, permissions and plugins, fronted by a
terminal client the owner controls down to the glyph, and runnable headless
or as a server without changing the loop.

This build is the kernel, the terminal client and the headless printer.
Server mode is the next plan.

## Build

    make            # bin/rudy
    make check      # fmt, vendor-type greps, vet, lint, test
    make install    # go install ./cmd/rudy

`go.mod` replaces `github.com/aeryx-ai/memory/memory-go` with
`../memory/memory-go`, so the memory repository has to sit beside this one.

## Configure

`~/.config/rudy/config.toml` names providers and one default model. The
registry itself is discovered from each provider's `/v1/models`.

    [default]
    provider = "aperture"
    model = "cline-pass/kimi-k3"
    thinking = "off"

    [permissions]
    mode = "strict"        # strict, permissive or off

    [providers.aperture]
    wire = "openai_chat"
    base_url = "https://ai.guy.ts.net/v1"

    [providers.mlx]
    wire = "openai_chat"
    base_url = "http://localhost:8080/v1"

`auth = "env:NAME"` or `auth = "cache:KEY"` adds a bearer token from the
environment or from `~/Library/Caches/op-secrets.env`. Neither entry above
needs one: aperture authenticates by tailnet and mlx is local.

Paths follow XDG: `XDG_CONFIG_HOME`, `XDG_DATA_HOME` and `XDG_CACHE_HOME`
each default to `~/.config`, `~/.local/share` and `~/.cache`, plus `/rudy`.
`config.toml` is read, never written.

## Run the client

    rudy                                  # a new session in this directory
    rudy "fix the flaky fork test"        # the same, with the prompt in the editor
    rudy --continue                       # the newest session for this directory
    rudy --resume <session id>
    rudy sessions resume <session id>     # the same, as a verb
    rudy sessions fork <session id>       # a fork of it, --at <entry id> for where

The client draws inline, so the transcript is the terminal's own scrollback:
a turn's rows are committed when it rests. Keys are pi's action ids over the
closed set ADR 0013 lists, rebound in a `[keys]` table. Esc in insert goes to
vim normal; in normal with a turn running once steers and twice cancels.
`ctrl+l` picks a model, `shift+tab` cycles thinking, `ctrl+o` expands the
newest tool row, `alt+enter` queues a follow-up behind the running turn, and
`ctrl+d` on an empty editor exits. An unsafe tool asks inline where its row
will be: `y` allows once, `a` for the session, `n` and Esc deny.

Every render choice is a config field under `[ui]` with a default, listed in
`docs/specs/rudy-contracts.md`; `ui.render = "altscreen"` swaps the inline
scrollback for a viewport where every row stays expandable.

## Run headless

    rudy -p "Reply with exactly: ok"
    rudy -p --output json "Summarize this repository"
    rudy -p --output stream-json "Use the read tool on go.mod"
    git diff main | rudy -p "report typos in this diff"
    rudy -p --continue "now fix the first one"
    rudy -p --mode off "run the tests and fix what fails"
    rudy -p "/init"                       # writes ./AGENTS.md

`--output text` prints the final answer. `--output json` prints one JSON
line with `session_id`, `result`, `usage`, `cost` and `stop_reason`.
`--output stream-json` prints every protocol notification as one JSON line,
including each `permission_decision` and `tool_result`. Exit codes: 0
completed, 1 the turn failed, 2 usage, 130 interrupted.

`--mode` overrides `[permissions] mode` from config for one run: `strict`
asks before an unsafe tool and denies it if nothing can answer, `permissive`
allows by default, `off` allows everything. In strict mode with no client
able to answer, every unsafe tool call is denied and the denial is recorded;
pass `--mode permissive` or `--mode off` for unattended runs.

## Inspect

    rudy models          # the discovered registry with prices
    rudy sessions list    # sessions, newest first

Sessions live under `~/.local/share/rudy/sessions/<id>/entries.jsonl`, one
JSON object per line: messages, tool calls, permission decisions and tool
results, in order.

## Design

`docs/specs/2026-09-07-rudy-design.md` is the design; the context map,
domain model, contracts and ADRs under `docs/specs/` and `docs/adr/` sit
beside it and are normative over the code.

## License

MIT. See `LICENSE`.
