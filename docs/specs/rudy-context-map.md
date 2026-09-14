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
| Asker | Whoever can answer a permission question; every attached client that declared it. The first answer decides; no asker means deny | Session |
| Dangerous set | The tools and shell shapes that still ask in permissive mode | Session |
| Compaction | A summary entry that stands in for earlier entries when building the next request | Session |
| Request context | The newest compaction's summary, then every entry after the last one it covers, older compactions dropped, translated for a provider | Session, derived |
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
| SessionClient | The Client port for opening, loading and driving Sessions and receiving their events, expressed only in Rudy types | Client |
| ACP agent adapter | The box-side anti-corruption layer that serves ACP v1 and drives the Session protocol through a same-user Unix connection | Client edge |
| ACP client adapter | The Mac-side anti-corruption layer that implements SessionClient by speaking ACP v1 over ssh stdio | Client edge |
| Server | One process-lifetime instance of the Session runtime, identified only while it is running | Session |
| Shutdown | The Server's orderly terminal transition: stop admission, interrupt work, close runtime resources, then close its control connection | Session |
| Same-user connection | A unix socket connection whose peer uid the listener proved equals the server uid; the proof is transport state, never a claim in `client.hello` | Protocol |
| Slot | A region of the client screen with an owner: transcript, input, status, header, widget | Client |
| Theme | Named color roles with defaults | Client |
| Row | One thing on the client screen derived from an entry: user, assistant, tool, prompt, marker | Client |
| Queue | Follow-up messages the client holds while a turn runs; the server never sees them until submitted | Client |

## Contexts

| Context | Subdomain | About |
|---------|-----------|-------|
| Session | core | The process runtime and its lifecycle, the log, the turn loop, the gate, the workspace, subagents as child sessions |
| Provider | supporting | Endpoints, models, the registry, completions in domain types |
| Plugin | supporting | How every capability arrives, linked or spawned, plus the hook lifecycle |
| Client | supporting, conformist | Rendering Sessions through SessionClient: the TUI and the headless printer |
| Memory | generic, external | The OKF bundle, reached through the memory-go SDK |
| Hosts | supporting | Reaching an ACP agent on another machine over ssh, placing the workspace there and moving the tree in and out; client side only (ADR 0029, ADR 0032) |
| ACP | generic, external | Stable agent-client methods, capabilities, content and updates; reached only through two anti-corruption layers |

Inside Session, groupings that share one language:

| Grouping | Owns |
|----------|------|
| Log | Session, Entry and every entry payload |
| Loop | Turn, its states, steering |
| Gate | Permission mode, decision, asker, safety class, dangerous set |
| Runtime | Server, its process-lifetime identity and its orderly shutdown |

## Relationships

| Upstream | Downstream | Pattern | Notes |
|----------|-----------|---------|-------|
| Session | Client | Open Host Service, Published Language | The internal protocol; the local SessionClient implementation conforms and renders from entries plus deltas |
| Provider | Session | Customer/Supplier | Session asks for completions in domain types; never sees a wire shape |
| Vendor SDK, aperture, local servers | Provider | ACL | The two codecs and the provider plugins are the only packages that speak a wire format |
| Plugin | Session | Partnership | Plugins contribute tools, commands and hook handlers; Session fires hook points |
| Session | Plugin | Open Host Service | Spawned plugins are protocol clients with registration rights |
| MCP servers | Plugin | ACL | The `mcp` plugin adapts go-sdk tools into rudy tools; MCP types stay inside it; servers come from `mcp.toml` |
| Session (child) | Plugin (subagents) | Open Host Service | The `agent` tool opens a child session over the protocol like any client and reads its outcome back |
| Memory (memory-go) | Plugin | ACL | The memory plugin is the only importer; fold's model call is a port satisfied from Provider |
| ACP | Client | Conformist, ACL | `acpclient` translates ACP v1 into SessionClient. ACP types stop at the adapter |
| Session | ACP | Open Host Service, ACL | `acpagent` translates ACP v1 into the internal protocol over a same-user Unix connection. Session sees an ACP caller class, never an ACP type |
| Hosts | Client | Customer/Supplier | Client asks Hosts for a remote stdio stream and Placement; `acpclient` wraps the stream. Hosts speaks ssh and git but imports no ACP type. Session never sees a host |
| Session | Hosts | Open Host Service, Published Language | Hosts negotiates `_rudy/server_shutdown`; the ACP agent adapter requests `server.shutdown` over the same greeted connection it used to inspect the remote Server and returns success only after matching `server.stopped` plus internal EOF. No Host type crosses into Session |
| sand | Hosts | Separate Ways | Same box, same checkouts, same ssh alias; rudy takes sand's runtime behaviour (PATH over ssh, the doctor, push by URL, fetch back) and leaves the signing ring to sand |
| Codex, Claude Code, pi | rudy | Separate Ways | Precedents only; no protocol or format shared |

## Ambiguous terms

| Term | Session meaning | Elsewhere | Resolution |
|------|-----------------|-----------|------------|
| message | An entry with content blocks | Provider: a wire shape per API | `Entry` in Session; the codec owns "message" |
| tool | A callable with a safety class and schema | MCP: a server-declared function | rudy `Tool`; the MCP plugin translates |
| event | would have meant log item, notification and hook lifecycle | | Not a word here: `Entry`, `Notification`, `Hook point` |
| command | | slash command versus shell command | Always qualified: `SlashCommand`, `ShellCommand` |
| agent | | pi: the loop; ADK: a framework object; Claude Code: a subagent; ACP: the process serving the agent side of the protocol | No `Agent` object. A subagent is a child Session under an `AgentDefinition`; the external process is always `ACP agent adapter` |

## Stored and derived

- Stored: entries; config; the registry snapshot with its fetch time; discovered skills and agent definitions as files; provider usage on each `assistant_message`, recorded verbatim as an external fact.
- Derived, never stored: current model, mode, thinking level and title of a session; the request context; token totals; cost from usage times registry prices; turn state; the list of sessions; Server identity and state; ACP request correlation, negotiated capabilities, replay suppression and list cursors. No pid file or ACP session map is a record.

## Still open

- Dangerous set contents.
- Capabilities for proxy models the enrichment does not match.
- Workspace skills path, inferred as `.agents/skills`.
- Session title mechanism.
- Subagent model selection: price-tier ladder or per definition only.
