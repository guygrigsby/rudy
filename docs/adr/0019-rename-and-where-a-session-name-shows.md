# 19. /rename, and where a session's name shows

- Status: Accepted
- Date: 2026-09-09
- Deciders: Guy Grigsby

## Context

A session could already be named: `session.set_title` is in the contracts, a
`title_change` entry is in the log, and `SessionInfo` carries the result. What
was missing was a way for a person to do it, and anywhere on screen it would
show if they did.

## Decision

1. **`/rename <name>` is a kernel command, not a client one.** It joins /model,
   /help, /fork and /plugins in the commands plugin, registered through the same
   Host interface any plugin would use. A name is session state, so it belongs on
   the server side of the protocol where headless and any later client get it
   too. That is the line ADR 0015 decision 3 drew: /exit is the client's own
   business, a title is the session's.

2. **The Action set grows a `SetTitle`.** A command had no way to name a session,
   and the alternative was for the command to be client-local and call
   `session.set_title` behind the server's back. Both the method and the action
   go through one `setTitle` on the server, so a command cannot name a session
   something the method would refuse: an empty name is `invalid_argument` either
   way, and a name the session already has appends nothing.

3. **The name shows on the composer's upper rule.** The header's box says a lot
   about the session, and it has scrolled away by the time anybody renames one.
   The rule over the composer is on screen for the whole session and was drawing
   nothing, so the name goes there, mirroring the context percentage on the rule
   below.

   Not the status line: it is already six cells wide, and a name is prose, which
   is what a rule with room in the middle of it is for.

## Consequences

- `command.run`'s Action list in the contracts gains `SetTitle`.
- The session picker still lists sessions by workspace and model rather than by
  name: `Store.List` reads the first line of each log, and a `title_change` is
  never the first line. Naming a session helps the person in it, not the one
  choosing between them, until the summary is widened.

## Alternatives considered

- A client-local `/rename` calling `session.set_title`: fewer moving parts, and
  it would have put session state in the client's own vocabulary, where the next
  client would have to reimplement it.
- A `title` status item: a seventh cell, and a name long enough to be worth
  giving would push the rest of the line off the screen.
