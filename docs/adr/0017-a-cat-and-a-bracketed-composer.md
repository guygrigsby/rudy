# 17. The mark is a cat, and the composer is bracketed by rules

- Status: Accepted
- Date: 2026-09-09
- Deciders: Guy Grigsby
- Amends the art of ADR 0016 decision 3 and the `ui.status.items` default of ADR 0006

## Context

ADR 0016 landed the startup header with the word "rudy" in block letters as its
picture, and the owner asked for a cat instead once he had seen it. Two smaller
asks came with it: the composer should read as its own region rather than as the
next transcript row, and the context percentage should be somewhere a person
actually notices. It was already on screen, as a bare `0%` between the
permission mode and the cost, which is easy to miss and says nothing about what
it measures.

## Decision

1. **The mark is a cat, in line art.** Five rows of plain ASCII rather than
   block characters, so it renders the same in any font instead of depending on
   one that has the block glyphs. The reveal is unchanged in shape and better in
   behaviour: it sweeps column by column and leaves the drawing itself behind
   the leading edge, where the block wordmark could only resolve from blocks
   into letters at the final step.

   The package's vocabulary follows: `Wordmark` is `Mark`, `wordmarkWidth` is
   `markWidth`, and the picture is one `var` a future change edits without
   touching the reveal.

2. **The composer sits between two rules.** A line above it and a line below it,
   the width of the terminal, in the same left gutter every other line sits in,
   under `ui.input.rules`. The rules bracket the input slot outside the plugin
   widget rows, so a widget still sits against the editor it belongs to.

3. **The lower rule carries the context percentage, labelled, and it is the only
   place it is drawn.** `42% context` set into the right end of the rule with a
   couple of dashes after it. `context` leaves the default `ui.status.items`: it
   is still a legal item for anyone who wants it in both places, but the default
   draws the number once.

   A session that has sent nothing has nothing to say about its context, and a
   terminal too narrow for the label draws a plain rule rather than a truncated
   sentence.

## Consequences

- Every golden moves: the composer gained two lines and the status line lost a
  cell.
- The real path test now waits on the cat's last row and asserts nothing is left
  mid-reveal, which is a stronger check than the wordmark's was.
- The percentage is drawn from the same numbers the status item used, so a model
  the registry has no context window for still says nothing.

## Alternatives considered

- Keeping the percentage in both places: two copies of one number on adjacent
  lines, which is what made it easy to miss in the first place.
- Labelling the status cell instead of moving it: cheaper, but the composer
  still would not have read as its own region.
- A boxed composer with corners rather than two rules: it would have to reserve
  a column each side, and every other line in the client sits in one gutter.
