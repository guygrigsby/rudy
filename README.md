# rudy

A coding agent harness in Go. One binary that runs a model-driven loop over a
workspace with tools, sessions, permissions and plugins, fronted by a
terminal client the owner controls down to the glyph, and runnable headless
or as a server without changing the loop.

This build is the kernel, the terminal client, the headless printer and the
server.

## Install

    go install github.com/guygrigsby/rudy/cmd/rudy@latest

Or take a binary for your platform from the
[releases page](https://github.com/guygrigsby/rudy/releases): each tag builds
darwin and linux on arm64 and amd64, with a `checksums.txt` beside them.

From a checkout:

    make            # bin/rudy
    make check      # fmt, vendor-type greps, vet, lint, test
    make install    # go install ./cmd/rudy, then rudy config sync

Nothing to check out beside it: every dependency resolves from its own module.
Go 1.26 or newer, and `make check` needs cgo for the race detector.

## Configure

`~/.config/rudy/config.toml` names providers and one default model. Until it
names one, a session cannot open and rudy says which file to write.
`rudy config sync` writes that file with every key commented; the smallest one
that works is:

    [default]
    provider = "anthropic"
    model = "claude-opus-5"
    thinking = "off"

    [permissions]
    mode = "strict"        # strict, permissive or off

    [providers.anthropic]
    wire = "anthropic_messages"
    base_url = "https://api.anthropic.com"
    auth = "env:ANTHROPIC_API_KEY"

The model list is not configuration: the registry is discovered from the
provider itself, `/v1/models` or the wire's equivalent, so `model` is any id
that endpoint serves. Any OpenAI-compatible endpoint is a provider too, local
ones included:

    [providers.mlx]
    wire = "openai_chat"
    base_url = "http://localhost:8080/v1"

    [providers.gateway]
    wire = "openai_chat"
    base_url = "https://gateway.example.com/v1"
    auth = "env:GATEWAY_TOKEN"

`auth` is `env:NAME` for an environment variable or `cache:KEY` for a key in an
env-format file of `KEY=value` lines, which `[secrets] file` names. That file
defaults to `~/Library/Caches/op-secrets.env` on macOS, where a 1Password cache
lands, and `$XDG_CACHE_HOME/rudy/secrets.env` everywhere else. An endpoint that
needs no token, a local one or one behind a VPN, leaves `auth` out.

Paths follow XDG: `XDG_CONFIG_HOME`, `XDG_DATA_HOME` and `XDG_CACHE_HOME`
each default to `~/.config`, `~/.local/share` and `~/.cache`, plus `/rudy`.
`config.toml` is yours to edit. The harness only ever adds to it:

    rudy config path                      # the file rudy reads
    rudy config example                   # every key, its default and what it is for
    rudy config sync                      # add the keys your file is missing
    rudy config sync --dry-run            # say what it would add and write nothing

`make install` runs `rudy config sync`, so a key added by a new release reaches your
file with the comment that says what it is for. A value you have already set is never
changed, your comments and ordering are kept, and `examples/config.toml` in this
repository is the same file `rudy config example` prints.

## Run the client

    rudy                                  # a new session in this directory
    rudy "fix the flaky fork test"        # the same, with the prompt in the editor
    rudy --continue                       # the newest session for this directory
    rudy --resume <session id>
    rudy sessions resume <session id>     # the same, as a verb
    rudy sessions fork <session id>       # a fork of it, --at <entry id> for where

The client opens on a header: a greeting, a cat, this session's model and
workspace, a tip or two and what the build carries as news. It materializes once,
any key skips it, and the conversation scrolls it away. `[ui.header]` turns each
part off.

The client opens full screen and keeps every row expandable. Keys are pi's
action ids over the closed set ADR 0013 lists, rebound in a `[keys]` table. Esc
in insert goes to vim normal; in normal with a turn running once steers and
twice cancels. `/permissions` shows or sets how rudy asks before an unsafe tool, `ctrl+l` picks a
model and `ctrl+p` cycles through them, `/scoped-models`
narrows that cycle to the ones you actually use, `shift+tab` cycles thinking, `ctrl+o`
expands the newest tool row, `alt+enter` queues a follow-up behind the running
turn, and `ctrl+d` on an empty editor exits, as does `/exit`. An unsafe tool
asks where its row will be: `y` allows once, `a` for the session, `n` and Esc
deny.

A draft that opens with `!` is a shell command: the composer changes colour, Enter runs
it in the workspace, and the command with its output is recorded so the model reads it
with your next message. No turn starts.

Typing `/` lists the commands above the editor with what each does, filtered as
you type. The arrows move the selection, tab completes the name, Esc dismisses
the list. Enter completes a half-typed name and runs a whole one.

Icons are Nerd Font glyphs by default: the branch in the workspace cell, the
model, the context and one per tool. A terminal without a patched font wants
`ui.icons.set = "unicode"`, and `ui.icons.<name> = ""` turns any single one off.
The status line wears a cat face for the run, which `ui.cats = false` takes away.
Mouse reporting is on, so a click expands a row and the wheel scrolls; that is
also what stops a drag from selecting text, so hold Option on macOS or Shift
elsewhere to select anyway, or set `ui.mouse = "off"` to leave the mouse to the
terminal entirely.

Every render choice is a config field under `[ui]` with a default, listed in
`docs/specs/rudy-contracts.md`; `ui.render = "inline"` swaps the full screen for
the terminal's own scrollback, where a turn's rows are committed when it rests
and a committed row no longer expands.

## The system prompt

    rudy prompt show                      # what a session here would send
    rudy prompt example > ~/.config/rudy/system.md
    rudy prompt path                      # the file it is read from

`system.md` under the config directory replaces the built-in prompt, and
`[prompt] file` names one somewhere else. A template may use `${base}`,
`${tools}`, `${agents}`, `${version}`, `${workspace}`, `${project}`, `${model}`,
`${date}` and `${os}`, and `$${` writes a literal `${`. A name outside that set
is a notice and the built-in prompt, so a typo never leaves a session with a hole
in its instructions.

## Plugins from a repository

A repository can ship its own plugins under `.rudy/plugins/<name>/plugin.toml`.
They run as you, so rudy asks once per workspace before starting any of them,
and remembers what you agreed to: a manifest that later changes what it executes
asks again. A client that cannot ask, `--print` or a pipe, runs none of them and
says so.

    rudy plugins trust            # yes, run this workspace's plugins
    rudy plugins trust --forget   # take it back

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

## Trust model

The boundary is your user account. `rudy serve` listens in a `0700` directory
on a `0600` socket, and every connection's peer uid is checked with
`SO_PEERCRED`, `LOCAL_PEERCRED` on macOS, before a byte is read. Inside that
boundary everything is trusted, deliberately:

- Any process running as you can drive any live session by id: submit a turn,
  interrupt it, compact, close, change the model, and run `session.shell`,
  which runs a command without asking the Gate because the operator typed it
  (ADR 0023). Being attached is not a permission (`rudy-jkz`). Rudy is not
  what stands between a machine that is already compromised and the rest of
  it.
- Permission modes, the dangerous set and the asker govern what the model may
  do, not what a local caller may do. Tools run as you, on your filesystem,
  with your network and your credentials. This is not a sandbox; `rudy-uvo` is
  where that gets answered.
- Installing a plugin runs its code. `rudy plugins install` builds from a
  source you named (ADR 0025), a linked plugin runs in this process, and a
  spawned one is a subprocess holding everything you hold.
- A session log holds what the model saw: prompts, tool inputs, tool outputs
  and whatever a file read put in front of it, `0600` under a `0700`
  directory, unencrypted. Provider keys stay in config or the environment and
  are never written to it.
- `web_fetch` puts somebody else's page into the model's context as text, and
  rudy puts no fence around it: a page can carry instructions and the model
  reads them like any other text. The tool is `unsafe`, so strict mode asks
  before the first one, and it refuses loopback, private and link-local
  addresses at every redirect so a URL cannot reach this machine's own
  services (ADR 0039).
- `--host` runs the kernel on another machine over ssh (ADR 0029). That trust
  is ssh's. Rudy adds no authentication of its own, and the daemon there
  applies this same model as the remote user.

## Inspect

    rudy models list     # the discovered registry with prices
    rudy sessions list    # sessions, newest first

Sessions live under `~/.local/share/rudy/sessions/<id>/entries.jsonl`, one
JSON object per line: messages, tool calls, permission decisions and tool
results, in order.

## Design

`docs/specs/2026-09-07-rudy-design.md` is the design; the context map,
domain model, contracts and ADRs under `docs/specs/` and `docs/adr/` sit
beside it and are normative over the code.

## License

AGPL-3.0-or-later, copyright 2026 Guy Grigsby. See `LICENSE`.

Use it, change it, run it. Convey a copy or a derivative, or run a modified one
where other people reach it over a network, and that version's source goes with
it under the same terms (AGPL sections 5, 6 and 13). A license for other terms
is a conversation: guy@grigsby.dev.
