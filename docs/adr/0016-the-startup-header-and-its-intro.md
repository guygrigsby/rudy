# 16. The startup header: what it says, where it comes from, and why it moves once

- Status: Accepted
- Date: 2026-09-09
- Deciders: Guy Grigsby

## Context

ADR 0006 left one line for this: "a startup banner renders once". ADR 0015
made the client open full screen, which is the moment a person meets it,
and a full screen with nothing on it is a worse first frame than the
inline one it replaced.

The reference the owner named is his own pi header extension: a framed
two-column box, greeting and a block wordmark and the session's facts on
the left, tips and the release's headlines on the right. The second ask
is codex's startup, where the wordmark assembles itself before it
settles.

Three things that box needs are things rudy does not have: a name to
greet, a list of tips, and a release to report. And two things about it
are decisions rather than drawing: whether it belongs to a plugin or to
the client, and whether it stays on screen.

## Decision

1. **The header is the client's own chrome, not a plugin's widget.** It
   draws before any plugin has registered anything, it reads the session
   facts the client already holds, and it is many lines where a widget is
   one. The rule it sits beside, that the kernel has no private path to
   its own features, is about capabilities the server owns: tools,
   commands, providers, hooks. Chrome the client draws for itself is the
   same kind of thing as the status line's built-in items and the notice
   region, both of which are already the client's.

   `ui.layout.slots`'s `header` slot is untouched and still belongs to
   plugin widgets. The startup header is not in a slot: it is the top of
   the transcript.

2. **It draws once and scrolls away.** The header is the first thing in
   the transcript, above the first row, and the conversation pushes it up
   as it grows. It is not pinned: a pinned box costs those rows for the
   life of the session, and what it says (the model, the mode, the
   workspace) is already in the status line, which is pinned and costs
   one row.

   In inline rendering it is printed above the live region at startup,
   the way a committed turn is, so the terminal owns it and the live
   frame never grew to hold it (ADR 0015, rudy-wbf).

3. **The wordmark materializes once, in altscreen only.** The block
   letters reveal column by column with a shimmer at the leading edge and
   settle into the static header, about a second in total. Any key skips
   to the settled frame, and the tick loop ends there: a client at rest
   schedules nothing, which is the same rule the turn spinner follows.

   Inline gets the settled header and no animation. An animation there
   would mean redrawing lines the terminal already owns, or growing and
   shrinking the live frame, which strands rows (rudy-wbf). The two modes
   differing here is the honest version of that constraint.

4. **What is new comes from a CHANGELOG.md this repository keeps**,
   embedded in the binary, newest release's bullets first. A built binary
   carries its own release notes, so an old rudy reports what it actually
   is rather than what the checkout beside it says. The alternatives were
   the ADR titles, which are decisions rather than news, and recent
   commit subjects, which are neither.

5. **The greeting name resolves once, at startup**, from
   `ui.header.name`, then git's `user.name`, then the OS user, first
   token capitalized. The time of day picks the word: morning, afternoon,
   evening, night.

6. **Tips are the client's own list, rotated by the day.** They name keys
   and commands this client actually binds, so a tip is never a lie about
   the build it shipped in.

7. **Every part is a config field with a default**, as ADR 0006 requires:
   `ui.header.show`, `ui.header.animate`, `ui.header.name`,
   `ui.header.tips`, `ui.header.updates`, `ui.header.max_width`. Turning
   the header off leaves the client opening on an empty transcript, which
   is what it did before this ADR.

## Consequences

- `internal/tui/banner` is a package of pure functions with its own
  tests: the box, the column fitting, the changelog parse, the path
  shortening, the greeting, the wordmark's frames. `internal/tui/app`
  composes it and owns the tick.
- The repository root gains `CHANGELOG.md` and one Go file to embed it.
  A release that forgets the changelog shows the previous release's news,
  which is the failure mode the owner accepted when he chose this source.
- The goldens pin the settled header, never a frame of the animation.
  Tests turn the animation off the way they turn vim off.
- A very narrow terminal drops the right column and then the box: below
  the width where two columns fit, the header is one column, and below
  the width where a frame is worth drawing, it is the greeting alone.

## Alternatives considered

- A plugin registering a multi-line header widget: it would need the
  widget contract to grow multi-line content, and it could not draw
  before plugins are ready, which is exactly when a person is waiting at
  an empty screen.
- Pinning the box for the session: the facts it carries are the status
  line's job, and the status line already costs one row instead of
  twelve.
- Animating in inline rendering by growing the live frame: rudy-wbf, and
  the ghost rows would be the first thing a user saw.
