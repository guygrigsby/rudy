# 13. The TUI wave: an embedded client over the protocol, rows from entries, inline scrollback

- Status: Accepted, implemented 2026-09-09
- Date: 2026-09-09
- Deciders: Guy Grigsby

## Context

ADR 0006 fixes the stack (Bubble Tea v2, bubbles v2, lipgloss v2, glamour
v2, vimbubble ported to v2) and the rule that every render choice is a
config field. The plugins wave landed everything the client renders:
entries, stream deltas, turn state, permission questions, notices, status
items, widgets and plugin state, all as protocol notifications. Writing
the plan for the client surfaced decisions ADR 0006 leaves open:

- How a transcript row relates to entries, and what a tool row is.
- What streams and what renders: glamour renders whole documents.
- What inline rendering means for a row the user wants to expand after
  it scrolled into native scrollback.
- Where the vimbubble port lives and how rudy depends on it before it is
  published.
- Queued follow-ups: the server refuses `source: queued` in pass 3.
- Which of pi's action ids rudy binds.
- Whether the TUI embeds the server or attaches to one.

## Decision

1. **The TUI is a client, embedded.** `rudy` with no arguments builds the
   same server `rudy -p` builds and drives it over the in-memory
   transport as caller class TUI with `asker: true`. Attaching to a
   running `rudy serve` is the serve wave's work; nothing in the client
   assumes an embedded server beyond how it dials.

2. **A row is derived from entries, one entity per thing on screen.** A
   `user_message` is a user row; an `assistant_message` is one assistant
   row per text block (thinking blocks render only when
   `ui.transcript.thinking = "shown"`); each `tool_use` block becomes a
   tool row that absorbs its `permission_decision` and `tool_result` by
   tool_use id; `note`, `compaction`, `turn_interrupted` and
   `turn_failed` are marker rows; a `permission.requested` notification is
   a transient prompt row in the tool row's place until answered. Rows
   are keyed by entry id (tool rows by tool_use id) so replay and
   reattach never duplicate.

3. **Stream raw, render at commit.** While an assistant message streams,
   its text deltas append to a live row rendered as plain wrapped text.
   When the `assistant_message` entry arrives the row is rendered once
   through glamour with chroma on fences and replaces the live one.
   Thinking deltas count toward a spinner only. Tool input deltas update
   the pending tool row's one-line summary.

4. **Inline mode commits rows to scrollback at turn end.** Bubble Tea v2
   inline rendering owns only the live region: the current turn's rows,
   the prompt row, widgets, the input and the status line. When a turn
   reaches `completed`, `failed` or `idle`, its rows are printed above the
   live region in order, collapsed or expanded as they stand, and leave
   the model. Expansion (`app.tools.expand`, Enter, a click) applies to
   rows still in the live region. `ui.render = "altscreen"` keeps every
   row in a viewport and expansion applies to all of them. This is the
   trade the design chose with inline scrollback; the config value is the
   escape.

5. **Keys are pi's action ids with pi's defaults, over a closed subset.**
   rudy binds `app.interrupt`, `app.clear`, `app.exit`, `app.suspend`,
   `app.thinking.cycle`, `app.thinking.toggle`, `app.model.select`,
   `app.model.cycleForward`, `app.model.cycleBackward`, `app.tools.expand`,
   `app.message.followUp`, `app.message.dequeue`, `app.session.new`,
   `app.session.fork`, `app.session.resume`, `app.editor.external`, the
   `tui.editor.*` editing actions (in insert mode), `tui.input.newLine`,
   `tui.input.submit`, `tui.select.*` and the `tui.altScreen.*` scrolling
   actions. A `[keys]` entry naming any other id is a config error at
   load, with the id named. Key strings use pi's `modifier+key` grammar.

6. **Queued follow-ups live in the client.** `app.message.followUp`
   appends the editor text to a client-side queue while a turn runs; when
   the turn rests the head is submitted as `typed`. `app.message.dequeue`
   and the double-Esc cancel move the queue back into the editor. The
   server keeps refusing `source: queued`.

7. **vimbubble v2 is a major version in its own repository, replaced
   locally until published.** The port lives at
   `github.com/guygrigsby/vimbubble/v2` (a `v2/` directory in the
   vimbubble repository) on bubbles v2's public cursor API, and gains
   visual mode. rudy requires it through a `replace` to
   `../vimbubble/v2` the same way it requires memory-go; publishing is a
   push the owner makes.

8. **Themes are roles; plugin spans name roles.** The built-in theme is
   the design's TOML block. `themes/<name>.toml` under the config dir
   overrides it; `ui.theme.<role>` overrides per key. A `Span.role` from a
   plugin is a theme role name; an unknown role renders as `text`.

## Consequences

- The client package tree is `internal/tui/{app,transcript,keys,theme,
  input}`; it imports `internal/protocol`, `internal/session`,
  `internal/provider` (for `Model` pricing and context window) and
  nothing under `internal/server`.
- Goldens through teatest v2 cover each config permutation the design
  names: collapsed and expanded tool rows, text and background diffs,
  the permission prompt, each vim mode in the status line, inline and
  altscreen.
- Inline mode cannot re-expand a row once its turn committed; altscreen
  can. Documented in the design's Client section.
- Two local `replace` directives now (memory-go, vimbubble v2) until both
  are pushed; `make check` needs both sibling checkouts.
- A standing permission question takes `y`, `a` and `n` ahead of the editor's
  mode, so the answer lands in vim normal mode as well as in insert. A
  divergence from ADR 0006's Esc layering, where the mode reads a key first,
  accepted because an answer the turn is waiting on must never be swallowed by
  a mode. Esc still answers deny, which is the one key both layers agree on.

## Alternatives considered

- Re-rendering the whole markdown document on every delta: flicker and
  cost proportional to message length; the design's "stable prefix" is
  the same trade in a harder form.
- Keeping every row in the live region in inline mode: the live region
  would grow past the terminal height and inline mode cannot scroll it.
- Vendoring vimbubble into rudy: loses the shared module the design
  names and the v1 users.
- Server-side queue (`source: queued`): the contract row exists; the
  server refusal stands until a second client class needs it.
