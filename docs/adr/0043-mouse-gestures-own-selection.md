# 43. Mouse gestures own selection

Status: Accepted

Supersedes: ADR 0006's decision that mouse reporting handles row expansion and
wheel scrolling only.

## Context

An altscreen client needs mouse reports to scroll its viewport and identify a
tool row under a click. Terminal mouse reporting also takes an unmodified drag
away from the terminal's selection. Leaving reporting off preserves selection
but makes the wheel and row clicks invisible to Rudy.

The required interaction is one gesture set: wheel to scroll, drag to select
and click to expand a tool row. A drag that begins on a tool row must not expand
it.

## Decision

`ui.mouse` defaults to `click`, Bubble Tea's cell-motion mode. Rudy owns the
left-button gesture until release. Movement makes it a selection: Rudy renders
the selected cells in reverse video and copies their plain text through OSC 52
on release. A release without movement is a click and toggles the tool row under
it. Wheel reports scroll the altscreen viewport.

`ui.mouse = "off"` remains the opt-out that leaves every mouse gesture to the
terminal. `all` adds movement reports when no button is held but otherwise uses
the same gesture semantics as `click`. Inline mode always disables reporting so
its native terminal scrollback and selection keep receiving wheel and drag.

## Consequences

- Wheel scrolling, drag selection and row expansion work together in altscreen.
- Selection copies on release because the terminal does not own the drag.
- Clipboard delivery depends on the terminal's OSC 52 support.
- Operators who prefer terminal-owned selection can set `ui.mouse = "off"` and
  scroll the viewport with its keyboard actions.
- Inline mode keeps native terminal scrolling and selection but has no mouse row
  expansion; its live tool rows remain expandable from the keyboard.
