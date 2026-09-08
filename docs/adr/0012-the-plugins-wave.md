# 12. The plugins wave: hooks in the kernel, subagents as child sessions, compaction behind a hook, MCP servers under one plugin

- Status: Accepted
- Date: 2026-09-08
- Deciders: Guy Grigsby

## Context

The kernel and headless plan landed a loop with six tools, one provider
codec and one slash command. The design sequences a plugins wave next:
`anthropic_messages`, the clinepass dialect, skills, hooks, subagents,
compaction, MCP, memory, spawned plugins and the `rudy mcp` and
`rudy plugin` commands. Writing the contracts for that wave surfaced
decisions the design left open or contradicted itself on:

- ADR 0005 lists compaction among the plugins; the contracts make the
  Compactor a domain service that appends a `compaction` entry, which a
  plugin may never append.
- The contracts call plugin-opened child sessions a later plan; the design
  sequences subagents into this wave.
- The hook set is closed at eight points, and the memory design wants to
  hand the harness a running summary at compaction time.
- The aperture proxy serves no `/v1/messages` route for any model as of
  this date, so the `anthropic_messages` codec has no live endpoint to
  probe.
- Memory folds read a transcript; rudy's `entries.jsonl` is a third format
  the memory SDK does not know.

## Decision

1. **Hooks are fired by the kernel, handled by plugins.** The turn loop and
   the server fire the hook points; a linked plugin registers a handler
   through `Host.RegisterHook`, a spawned plugin through
   `plugin.register_hook`. The set gains a ninth point,
   `before_compaction`, fired when the Compactor decides to compact; the
   first handler returning a summary supplies it and the model is not
   asked. Nine is the closed set now.

2. **Compaction is kernel.** The Compactor is a domain service in the turn
   loop, as the contracts say; ADR 0005's list is corrected here. It fires
   after every `assistant_message` whose prompt size crosses
   `sessions.compact_at` of the model's context window, and on
   `session.compact`, a new protocol method behind the `/compact` command.
   Without a summary from a hook it summarizes with the session's model.

3. **A subagent is a child session opened by a plugin over the protocol.**
   The `subagents` plugin registers one tool, `agent`, whose input names an
   agent definition and a prompt. It opens a child session with
   `session.open` carrying `parent: {session_id, tool_use_id}`, submits the
   prompt, waits for the turn to end and returns the final assistant text
   as the tool result. `session_opened` records `parent_session_id` and
   `parent_tool_use_id`, empty on a root session. A child session never
   sees the `agent` tool: depth is one. The model comes from the agent
   definition or is inherited; the price-tier ladder stays open.

4. **Every plugin reaches the server the same way.** A linked plugin gets a
   protocol client over the in-memory transport from `Host.Connect`,
   authenticated as caller class plugin, and uses the same requests a
   spawned plugin sends over stdio. The kernel keeps no private path.

5. **MCP servers live under one linked plugin.** `mcp` reads `mcp.toml`
   from the user and workspace scopes, connects each server over stdio or
   streamable HTTP through the pinned go-sdk, and registers every tool as
   `mcp__<server>__<tool>` with safety `unsafe`. A server that fails to
   connect is a notice; the plugin stays ready and the other servers'
   tools stand. `rudy mcp add|remove|list|get` edit `mcp.toml` and nothing
   else.

6. **Installed spawned plugins are git checkouts with a lock file.**
   `rudy plugin install <git url or path>` clones under
   `$XDG_DATA_HOME/rudy/plugins/<name>/` and records source, commit,
   time and enabled state in `plugins.lock.toml` beside them. Manifests are
   discovered from that directory and from the two config roots.

7. **The memory plugin uses memory-go and the memory SDK learns rudy's
   transcript.** A third transcript format, `rudy`, joins pi and Claude
   Code in the memory project (Node and Go, with goldens), detected by a
   first line of kind `session_opened`. The plugin folds after each turn
   with a `Summarizer` backed by the provider port, finalizes on
   `session_closed`, injects the context render on `session_opened`, and
   supplies the running summary on `before_compaction`. Its tools are
   `safe`: they write only the bundle through the SDK's invariants, the
   user opted in with `memory.enabled`, and a permission prompt per
   remembered fact would make the feature unused.

8. **The `anthropic_messages` codec is verified offline.** It is built on
   `anthropic-sdk-go` against recorded fixtures shaped by the Messages API
   documentation; live verification waits for an endpoint that serves the
   route. The clinepass dialect is selected by `providers.<name>.dialect`
   and implemented as a provider wrapper over the `openai_chat` codec.

## Consequences

- The hook set changes once, now, with a versioning note in the contracts.
- Child sessions appear in `rudy sessions list` under their parent; a
  parent with children refuses deletion like a fork parent.
- Memory, skills, subagents, compaction summaries and MCP all arrive
  through the same registration surface, so the surface is exercised by
  six built-ins before any third party touches it.
- Two records join the record layer, `mcp.toml` and `plugins.lock.toml`;
  `config.toml` stays unwritten.
- The memory project carries one more format in both implementations; its
  golden gate covers it.

## Alternatives considered

- Compaction as a plugin appending through a new `plugin.append_compaction`
  request: a second writer of a kernel invariant for no gain.
- Subagents in-process by calling `turn.Runner` directly: a private path,
  and spawned plugins could never do the same.
- One plugin per MCP server: a plugin entity per config line, with nothing
  to say that a server-level notice does not.
- Skipping the `rudy` transcript format and feeding the fold from rudy's
  own reader: the SDK's checkpoint and delta logic would be duplicated in
  the plugin.
