# 15. Full screen by default, commands a client can list, and the ones it answers itself

- Status: Accepted
- Date: 2026-09-09
- Deciders: Guy Grigsby
- Supersedes the `ui.render` default of ADR 0006 and the default half of ADR 0013 decision 4

## Context

Three things the owner asked for after using the client:

- `rudy` opens in a frame at the bottom of whatever was already in the
  terminal. Every other harness opens owning the screen.
- Typing `/` offers nothing. Tab completes only from the last `/help`
  notice the client happened to run, because the protocol gives a client
  no other list: `command.run` takes a name and answers no set, and
  `plugin.register_command` is the plugin's side of the same wire. A
  client that has never run `/help` completes nothing, which is every
  client at launch.
- `/exit` and `/quit` reach `command.run` and come back
  `unknown command`. Nothing registers them, and nothing could: the
  Action set a command may return (`SubmitPrompt`, `Notice`, `Compact`,
  `SetModel`, `Fork`, `NoAction`) has no member that closes a client.

The first is a reversal. ADR 0006 chose inline rendering so the
transcript lives in the terminal's own scrollback, and listed
alt-screen by default as the alternative it refused. ADR 0013 decision 4
built the machinery that trade needs: a rested turn's rows are printed
above the live region and leave the model, so a committed row can no
longer be expanded.

## Decision

1. **`ui.render` defaults to `altscreen`.** The client opens owning the
   screen, the transcript scrolls in its own viewport and every row stays
   expandable for the life of the session. `ui.render = "inline"` is the
   escape and keeps every line of ADR 0013 decision 4: the commit at turn
   end, the late-row commit, the click offset, the frame that anchors at
   the bottom. What changes is which one a user gets without asking.

   The cost is the one ADR 0006 named: the default transcript is no
   longer the terminal's own scrollback, so it does not survive the
   client exiting and a terminal's own search does not reach it. The
   owner asked for the other side of that trade after using both.

2. **`command.list` is a client query.** It answers the registered
   commands, each `{name, description}`, in registration order, from
   `PluginRegistry.Commands`. Registrations are frozen when a plugin
   answers `plugin.init` (ADR 0005), so the set is fixed for the life of
   the process and the client asks once, on connect. There is no
   notification for it and no live update.

   The alternative, keeping `/help` as the discovery surface, is what
   the client does today and it is why completion is empty at launch. A
   list is not a rendering: `/help` writes prose for a person, and
   parsing prose back into a list is a scraper.

3. **`/exit` and `/quit` are client-local commands.** The client answers
   them itself, ahead of `command.run`. A quit is the client's own
   business: attached to `rudy serve` it detaches and leaves the daemon
   and the session alone, embedded it ends the process that owns both.
   `/quit` is an alias.

   The rejected alternative is a `Quit` Action a plugin's command could
   return. It would let any plugin close any attached client, and with a
   daemon serving several it could not say whose. A server that can close
   a client is a capability nobody asked for.

   Client-local is a client vocabulary, not a private path into the
   kernel: these commands never reach the server, appear in no session
   log, and register nothing. The rule they sit beside, that the kernel
   has no private path to its own features, is about capabilities the
   server owns. A client command is not one.

4. **A slash draft opens a menu above the editor.** A draft whose first
   line opens with `/` and carries no space yet lists the matching
   commands in the `above_editor` region, name and description, filtered
   by what has been typed as it is typed. The editor keeps the keyboard:
   the menu is a completion, not a picker, and a picker would take the
   draft off screen while it stood. `tui.select.up` and
   `tui.select.down` move the selection, `tui.input.tab` and
   `tui.input.submit` complete it, `tui.select.cancel` dismisses the menu
   for that draft. No new action ids: the closed set of ADR 0013
   decision 5 stands.

   Enter completes a name that is still a prefix, so a command is never
   run half-typed, and submits one already whole, so a command typed out
   in full still runs on one press. A completion leaves the name and the
   space an argument goes after, which is also what closes the menu: a
   draft past the name is no longer a name.

## Consequences

- The default screen the design draws is now the altscreen golden. Both
  goldens stay; `default_altscreen` is the default permutation and
  `inline` is the opt-in, which is the reverse of the file names the TUI
  wave left.
- `command.list` joins the requests table for the TUI, headless and ACP
  classes and is refused to plugins, which know their own registrations
  and have no use for another plugin's.
- The command menu draws where a plugin's `above_editor` widget draws.
  Both are in the region the client owns and the menu is transient, so it
  stacks under the widgets rather than replacing them.
- `rudy-wbf`, the inline picker's ghost rows, stops being what a user
  meets on a default install. It is still open and still real in inline
  mode.
- A client-local command shadows a registered one of the same name. The
  client says so in `/help`'s neighbourhood by listing its own commands
  in the menu, and a plugin registering `exit` finds it unreachable from
  the TUI. Accepted: the two names are worth more as a guarantee than as
  a namespace.

## Alternatives considered

- Full screen as a launch flag rather than the default: a flag every
  user would set is a default with extra steps.
- A `command.list` notification so a client tracks registrations live:
  nothing registers after `plugin.init`, so there would be nothing to
  notify about.
- Reusing the picker for command completion: the picker owns the
  keyboard and stands where the editor does, so the draft being completed
  would be off screen while it was completed.
