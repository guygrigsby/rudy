# rudy

A coding agent harness in Go. One binary that runs a model-driven loop over a
workspace with tools, sessions, permissions and plugins, fronted by a
terminal client the owner controls down to the glyph, and runnable headless
or as a server without changing the loop.

This build is the kernel, the terminal client, the headless printer and the
server.

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

The client opens on a header: a greeting, the wordmark, this session's model and
workspace, a tip or two and what the build carries as news. It materializes once,
any key skips it, and the conversation scrolls it away. `[ui.header]` turns each
part off.

The client opens full screen and keeps every row expandable. Keys are pi's
action ids over the closed set ADR 0013 lists, rebound in a `[keys]` table. Esc
in insert goes to vim normal; in normal with a turn running once steers and
twice cancels. `ctrl+l` picks a model, `shift+tab` cycles thinking, `ctrl+o`
expands the newest tool row, `alt+enter` queues a follow-up behind the running
turn, and `ctrl+d` on an empty editor exits, as does `/exit`. An unsafe tool
asks where its row will be: `y` allows once, `a` for the session, `n` and Esc
deny.

Typing `/` lists the commands above the editor with what each does, filtered as
you type. The arrows move the selection, tab completes the name, Esc dismisses
the list. Enter completes a half-typed name and runs a whole one.

Every render choice is a config field under `[ui]` with a default, listed in
`docs/specs/rudy-contracts.md`; `ui.render = "inline"` swaps the full screen for
the terminal's own scrollback, where a turn's rows are committed when it rests
and a committed row no longer expands.

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

## Run a server

    rudy serve                            # until interrupted

`rudy serve` runs the server this binary already embeds, on a unix socket
instead of an in-process pipe: `$XDG_RUNTIME_DIR/rudy/rudy.sock`, or
`$TMPDIR/rudy-<uid>/rudy.sock` where nothing sets `XDG_RUNTIME_DIR`, macOS
included. It logs to stderr, and on SIGINT or SIGTERM it stops
accepting, cancels every running turn so each records `turn_interrupted`,
closes its sessions and removes the socket.

`rudy`, `rudy -p`, `rudy sessions resume` and `rudy sessions fork` probe that
socket with a 50ms connect and attach to whatever answers; nothing answering
means they serve themselves, as they did before. `--socket <path>` attaches to
that server or fails, and on `rudy serve` it is the path to listen on;
`--embed` skips the probe and serves in this process. The socket directory is
`0700` and the socket `0600`, and a connection whose peer uid is not the
server's is closed before a byte is read.

Attached, a session lives in the daemon rather than in the terminal. A turn
keeps running after the client that started it exits, and `rudy --resume <id>`
picks up the transcript with the answer in it. Two terminals on one session
both see a permission question; the first answer decides and takes the prompt
down in the other. A session some other process is holding answers
`unavailable` and names the socket to attach through.

Nothing installs a service, and rudy never starts one for you. Both of these
are examples to adapt and install yourself.

`~/Library/LaunchAgents/dev.grigsby.rudy.plist`, loaded with
`launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.grigsby.rudy.plist`:

    <?xml version="1.0" encoding="UTF-8"?>
    <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
      "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
    <plist version="1.0">
    <dict>
      <key>Label</key><string>dev.grigsby.rudy</string>
      <key>ProgramArguments</key>
      <array>
        <string>/Users/you/go/bin/rudy</string>
        <string>serve</string>
      </array>
      <key>RunAtLoad</key><true/>
      <key>KeepAlive</key><true/>
      <key>StandardErrorPath</key><string>/Users/you/Library/Logs/rudy.log</string>
    </dict>
    </plist>

launchd gives a user agent the same per-user `TMPDIR` a login shell gets and
sets no `XDG_RUNTIME_DIR`, so the daemon and your terminal resolve the same
socket without either naming it.

`~/.config/systemd/user/rudy.service`, enabled with
`systemctl --user enable --now rudy` and `loginctl enable-linger $USER` so it
survives logout:

    [Unit]
    Description=rudy

    [Service]
    ExecStart=%h/go/bin/rudy serve
    Restart=on-failure

    [Install]
    WantedBy=default.target

systemd sets `XDG_RUNTIME_DIR` to `/run/user/<uid>` for the unit and for your
login session alike, so those agree on the socket too.

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
