# Context Map: rudy

## Ubiquitous language

| Term | Means | Lives in |
|------|-------|----------|
| Session | A conversation over one workspace with an append-only log; everything about it is derived from the log | Session |
| Entry | One immutable item appended to a session log; kinds are a closed set | Session |
| Turn | One user message and everything the loop does until it yields; at most one active per session; not stored | Session |
| Steering | The turn state after a single Esc: the prompt is live and the next message continues the turn | Session |
| Workspace | The directory a session acts on: root, git root, project id | Session |
| Safety class | `safe` or `unsafe` on every tool; the one switch driving both the gate and the durable-record rule | Session |
| Permission mode | `strict`, `permissive` or `off`; how the gate treats unsafe tools | Session |
| Asker | Whoever can answer a permission question; an attached client. No asker means deny | Session |
| Dangerous set | The tools and shell shapes that still ask in permissive mode | Session |
| Compaction | A summary entry that stands in for earlier entries when building the next request | Session |
| Request context | The entries after the last compaction plus its summary, translated for a provider | Session, derived |
| Provider | A named endpoint speaking one wire kind, with an auth reference | Provider |
| Wire kind | `anthropic_messages` or `openai_chat`; a codec | Provider |
| Dialect | A provider's deviations from a wire kind, applied by its plugin | Provider |
| Model | An id a provider serves, with context window, capabilities and prices | Provider |
| Registry | The models discovered from every configured provider, with a cached snapshot | Provider |
| Completion | One request to a model and its streamed parts, in domain types | Provider |
| Plugin | A unit that registers capabilities through one interface, linked in or spawned | Plugin |
| Capability | A tool, slash command, hook handler, widget, status item or provider a plugin registers | Plugin |
| Hook point | A named place in the session lifecycle a handler attaches to; a closed set | Plugin |
| Slash command | A client-facing command a plugin registers | Plugin |
| Skill | A `SKILL.md` discovered under a skills root | Plugin |
| Agent definition | A named profile of prompt, tools and model that a subagent session runs under | Plugin |
| Request, Notification | What a client sends the server, what the server sends a client; JSON-RPC words | Protocol |
| Slot | A region of the client screen with an owner: transcript, input, status, header, widget | Client |
| Theme | Named color roles with defaults | Client |

## Contexts

| Context | Subdomain | About |
|---------|-----------|-------|
| Session | core | The log, the turn loop, the gate, the workspace, subagents as child sessions |
| Provider | supporting | Endpoints, models, the registry, completions in domain types |
| Plugin | supporting | How every capability arrives, linked or spawned, plus the hook lifecycle |
| Client | supporting, conformist | Rendering the protocol: the TUI and the headless printer |
| Memory | generic, external | The OKF bundle, reached through the memory-go SDK |

Inside Session, groupings that share one language:

| Grouping | Owns |
|----------|------|
| Log | Session, Entry and every entry payload |
| Loop | Turn, its states, steering |
| Gate | Permission mode, decision, asker, safety class, dangerous set |

## Relationships

| Upstream | Downstream | Pattern | Notes |
|----------|-----------|---------|-------|
| Session | Client | Open Host Service, Published Language | The protocol; clients conform and render from entries plus deltas |
| Provider | Session | Customer/Supplier | Session asks for completions in domain types; never sees a wire shape |
| Vendor SDK, aperture, local servers | Provider | ACL | The two codecs and the provider plugins are the only packages that speak a wire format |
| Plugin | Session | Partnership | Plugins contribute tools, commands and hook handlers; Session fires hook points |
| Session | Plugin | Open Host Service | Spawned plugins are protocol clients with registration rights |
| MCP servers | Plugin | ACL | The MCP plugin adapts go-sdk tools into rudy tools; MCP types stay inside it |
| Memory (memory-go) | Plugin | ACL | The memory plugin is the only importer; fold's model call is a port satisfied from Provider |
| Codex, Claude Code, pi | rudy | Separate Ways | Precedents only; no protocol or format shared |

## Ambiguous terms

| Term | Session meaning | Elsewhere | Resolution |
|------|-----------------|-----------|------------|
| message | An entry with content blocks | Provider: a wire shape per API | `Entry` in Session; the codec owns "message" |
| tool | A callable with a safety class and schema | MCP: a server-declared function | rudy `Tool`; the MCP plugin translates |
| event | would have meant log item, notification and hook lifecycle | | Not a word here: `Entry`, `Notification`, `Hook point` |
| command | | slash command versus shell command | Always qualified: `SlashCommand`, `ShellCommand` |
| agent | | pi: the loop; ADK: a framework object; Claude Code: a subagent | No `Agent` object. A subagent is a child Session under an `AgentDefinition` |

## Stored and derived

- Stored: entries; config; the registry snapshot with its fetch time; discovered skills and agent definitions as files; provider usage on each `assistant_message`, recorded verbatim as an external fact.
- Derived, never stored: current model, mode, thinking level and title of a session; the request context; token totals; cost from usage times registry prices; turn state; the list of sessions.

## Still open

- Dangerous set contents.
- Capabilities for proxy models the enrichment does not match.
- Workspace skills path, inferred as `.agents/skills`.
- Session title mechanism.
- Subagent model selection: price-tier ladder or per definition only.
