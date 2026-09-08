# ADR 0006: Bubble Tea v2, and the UI is configuration

- Status: Accepted
- Date: 2026-09-07
- Deciders: Guy Grigsby

## Context

Every recorded complaint about pi is render ownership. rudy's render layer
must be ours, and the way to make that durable is to make each render
decision a config field with a default, not a code path a plugin patches.

Bubble Tea v2 is GA (`charm.land/bubbletea/v2` 2.0.9, GA since
2026-02-24) and imposes no layout: `View()` returns whatever we compose,
with inline versus alt-screen, mouse mode and the kitty keyboard protocol
as per-frame fields. `vimbubble`, the existing vim layer, targets Bubble
Tea v1 and reads the textarea cursor through reflect and unsafe. The
bubbles v2 textarea exposes `Line()`, `Column()`, `SetCursorColumn()` and
a selection API, so the port removes that hack and makes visual mode
possible.

## Decision

The client is Bubble Tea v2 with `bubbles/v2` 2.2.1 and `lipgloss/v2`
2.0.6, every Charm module pinned exactly. `vimbubble` is ported to v2.

Layout is ordered vertical slots, each with an owner: transcript, input,
status by default. No sidebars. A header is a slot a plugin adds. A status
item or widget is registered by a named plugin and placed by config; no
plugin can clear another's and nothing renders that config did not place.

Defaults, all overridable in `[ui]`:

- inline rendering, so the transcript lives in native scrollback
- vim on, insert and normal, pi-vim's Esc layering
- user message: one prefix glyph, no padding
- assistant: markdown through glamour with chroma on fences, one blank
  line between blocks, thinking hidden
- tool calls fold to one row plus a two-line result preview: last two
  lines for bash, first hunk for edits, a one-line count for read, grep
  and glob
- diffs as red and green text, no painted backgrounds anywhere
- status under the input: vim mode, model, permission mode, context
  percent, cost, workspace
- mouse for row expand and wheel scroll only

Escape: in insert goes to normal and is consumed; in visual goes to
normal; in normal with a turn running, once interrupts the current step
and enters steering, twice within the double-press window cancels the
turn. The window defaults to 500ms, an inferred value.

Keys use pi's action ids and pi's defaults, so an existing
`keybindings.json` translates one to one. Colors are theme roles, every
one a config field with a default on black; the accent is a placeholder
until chosen. Theme files live under `~/.config/rudy/themes/`.

Config is read with Viper and never written by the harness.

## Consequences

- A user changes the look by editing TOML, not by patching a component.
- The Charm v2 stack is the biggest churn risk in the dependency set.
  Mitigation: exact pins, crush's go.mod as the known-good set, and
  teatest goldens so a renderer change fails CI instead of the user.
- glamour renders whole documents; streaming re-renders a stable prefix.

## Alternatives considered

- tview or tcell directly: more code for the same result, no v2 momentum.
- Alt-screen by default: loses native scrollback; one config value away.
- Keep vimbubble on v1: blocks the whole v2 stack for 800 lines of port.
