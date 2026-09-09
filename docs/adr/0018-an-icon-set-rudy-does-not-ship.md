# 18. An icon set, named not shipped, permissively sourced

- Status: Accepted
- Date: 2026-09-09
- Deciders: Guy Grigsby

## Context

The workspace cell read `rudy main*`, with the branch spelled as a word in a
line that had room for a glyph. The owner asked for the real branch icon and for
a whole set behind it, with one constraint: nothing copyleft.

Three things had to be decided: where the glyphs come from, what happens on a
terminal whose font does not have them, and whether an icon is a fixed part of
the renderer or a config field like every other render choice.

## Decision

1. **rudy names codepoints and ships no font.** A set is a table mapping a name
   to a string; what arrives on screen is whatever the reader's own terminal
   font has at that codepoint. No glyph artwork is copied into this repository,
   so no font licence is being redistributed under.

2. **The default set is Nerd Font ranges from Powerline and Font Awesome 4.**
   Powerline is MIT and Font Awesome 4 is SIL OFL 1.1 for the font with MIT for
   the code. Both are permissive. Nothing is taken from a GPL, AGPL or
   share-alike source, and nothing from a Nerd Fonts constituent whose licence
   is unclear. A test pins every default glyph inside the private use area, so a
   codepoint that wandered out of the patched ranges fails the build rather than
   the reader's terminal.

3. **Two fallbacks, both first class.** `ui.icons.set = "unicode"` is ordinary
   Unicode that any font has, for a terminal without a patched font.
   `ui.icons.set = "ascii"` is plain ASCII, which is what the client drew before
   it had icons. A set that forgot a name fails a test: an icon missing from one
   set only, on the one config that names it, is the worst way to find out.

4. **Every icon is a config field.** `ui.icons.<name>` overrides one glyph, and
   setting it to the empty string turns that icon off, leaving no gap where it
   would have been. An unknown set name or an unknown icon name is a load error
   naming it, and the client does not open, the same as a bad theme role or key
   action id. The icon set is resolved beside the theme and the key table, and
   the same code path resolves it in tests.

5. **Where icons are drawn.** The branch icon sits in front of the branch inside
   the workspace cell, not in front of the whole cell, since the repository name
   is not a branch. The model cell and the context label carry theirs. A tool row
   opens with the icon for its own tool, or a generic one for a tool the set does
   not name, so a plugin's tool still lines up. A notice opens with its level's.

   The cost cell has no icon: the currency symbol the amount opens with is
   already one, and the unicode set proved it by drawing `$ $0.08`.

## Consequences

- Every golden but one runs on the unicode set, so a golden's bytes stay
  readable in a diff. `icons_nerd` pins the default set, and its bytes are
  private use area codepoints on purpose.
- `ui.icons` and `ui.theme` merge the same way, and both now read dotted keys as
  well as tables: a caller that set `ui.icons.set` as an override rather than as
  a table was invisible to viper's map view, which is a bug the icons work found
  in the theme path too.
- A terminal without a patched font draws boxes until its owner sets
  `ui.icons.set = "unicode"`. That is the cost of making the default the thing
  the owner asked for.

## Alternatives considered

- Defaulting to the unicode set: safer for a stranger, and not what was asked
  for. The escape is one config line either way, and the owner is the one
  running it.
- Bundling a patched font: a font is artwork with a licence, and shipping one is
  a redistribution question rudy does not need to have.
- Detecting a Nerd Font at runtime: there is no reliable way to ask a terminal
  what its font has.
