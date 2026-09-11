# rudy contracts

Pass 5, 2026-09-10: the subagents wave (ADR 0028). Tool calls run concurrently, so the Gate coalesces asks, a Tool scheduler joins the domain services and `tool.state` joins the notifications; a session's tool set becomes one narrowing chain that a caller may only shrink, which closes a child's view escaping its parent's; `plugin.register_agent` lets a plugin contribute an agent definition; a child's notifications reach its parent's subscribers without conferring authority over the child. Written before the code, as the rules require. Pass 4, 2026-09-09: the socket transport, attach and the asker rules (ADR 0014). Pass 4 implemented 2026-09-09; its rows were walked against the code and one was corrected: `server.socket` was listed as a config key and is not one, since ADR 0014 makes the socket `Paths.Socket()` with `--socket` overriding it. Pass 3, 2026-09-08. Pass 3 implemented 2026-09-09; the rows below were walked against the code and corrected where they differed. Pass 2 aligned the protocol section with the kernel implementation; pass 3 adds the plugins wave (ADR 0012): child sessions for subagents, `session.compact` and the ninth hook point `before_compaction`, the clinepass dialect key, the memory summary model, skill migration sources, and the `mcp.toml` and `plugins.lock.toml` records. Companion to [rudy-domain-model.md](rudy-domain-model.md) and [rudy-context-map.md](rudy-context-map.md). Three contracts: the protocol, the domain events and the record layer. A transition that appears in one and not the others is listed in the cross-check with a reason.

## Error taxonomy

One closed set. Every request row picks from it. JSON-RPC `error.code` is the numeric column, `error.data.kind` is the name.

| kind | code | means |
|---|---|---|
| `invalid_argument` | -32602 | request shape or value rejected before any domain call |
| `not_found` | -32001 | session, entry, model, plugin, tool, command or pending request does not exist |
| `refused_by_invariant` | -32002 | the aggregate refused the transition; `data.invariant` names it |
| `no_asker` | -32003 | a permission question had nobody to answer it |
| `conflict` | -32004 | a second registration or answer for something already taken |
| `unauthorized` | -32005 | the caller class may not make this assertion |
| `unavailable` | -32006 | a dependency is down: provider unreachable, plugin not ready, session locked by another process (`data.socket` names the socket a daemon holding it would serve on) |
| `provider_error` | -32007 | the provider answered with an error after retries; `data.status`, `data.body` |
| `plugin_error` | -32008 | a plugin returned an error or timed out; `data.plugin` |
| `interrupted` | -32009 | the operation was cancelled by an interrupt before it completed |

## Common types

| type | shape |
|---|---|
| `ModelRef` | `{provider: string, model: string}` both non-empty |
| `Workspace` | `{root: string, git_root: string, project_id: string}`; `git_root` empty means not a git repo |
| `ContentBlock` | one of `{type:"text", text}`, `{type:"image", media_type, sha256}` where `sha256` is the hex digest naming `blobs/<sha256>` in the session dir, stored byte-exact; inline image bytes never appear in the log, `{type:"thinking", text, signature}` where `signature` is the provider's opaque bytes verbatim, `{type:"tool_use", id, name, input}` where `input` is raw JSON bytes verbatim |
| `Span` | `{text: string, role: string}`; `role` is a theme role name |
| `Usage` | `{input: int, output: int, cache_read: int, cache_write: int}` |
| `Entry` | `{id: ulid, at: rfc3339nano, kind: EntryKind, ...payload}`; payloads in the record layer |
| `Model` | `{provider, id, display_name, upstream, context_window: int, max_output: int, pricing}`; `upstream` is who actually serves the model when the endpoint is a proxy: the route it takes, named as the endpoint names that route, comma separated when it serves the id over more than one and absent when it names none; `context_window` and `max_output` zero mean unknown; `pricing` is `{input, output, cache_read, cache_write}` as decimal strings in USD per token and is absent when no source supplied it |
| `SessionSummary` | `{id, opened_at, workspace, model, forked, parent_session_id, last_entry_at, title, entry_count}`; `parent_session_id` empty means root, `forked` is true when the session began as a fork; `last_entry_at`, `title` and `entry_count` are pass 3: not yet reported, the store does not compute them and `session.list` omits them |
| `PermissionMode` | `strict`, `permissive`, `off` |
| `ThinkingLevel` | `off`, `low`, `medium`, `high` |
| `TurnState` | `idle`, `streaming`, `running_tool`, `awaiting_permission`, `steering`, `completed`, `failed` |
| `HookPoint` | `session_opened`, `before_turn`, `before_request`, `after_response`, `before_tool`, `after_tool`, `before_compaction`, `turn_completed`, `session_closed` |
| `StreamPart` | `{type: text_delta, thinking_delta, thinking_signature, tool_use_start, tool_use_delta, tool_use_end, usage or stop, text, id, name, signature, usage, stop_reason, stop_reason_raw}`; one streamed piece of a completion. `text` carries the fragment for `text_delta`, `thinking_delta` and `tool_use_delta`; `id` and `name` the tool use; `signature` the provider's verbatim bytes on `thinking_signature`; `usage`, `stop_reason` and `stop_reason_raw` are set on `usage` and `stop` |
| `ParentRef` | `{session_id: ulid, tool_use_id: string}`; the session and the `tool_use` that spawned a child session |
| `Safety` | `safe`, `unsafe` |

`turn_id` everywhere is the entry id of the `user_message` that started the turn. Turns are not stored; that id is enough to find one in the log.

## 1. Protocol

JSON-RPC 2.0. Requests carry `id`; notifications do not. Both peers may send requests. Three transports:

| transport | who | framing |
|---|---|---|
| in-memory | embedded server inside the `rudy` process; the TUI and linked plugins | Go channels; JSON-RPC envelope kept so the same handlers serve every transport |
| unix socket | `rudy serve` and any client attaching to it | `$XDG_RUNTIME_DIR/rudy/rudy.sock`, or `$TMPDIR/rudy-<uid>/rudy.sock` when `XDG_RUNTIME_DIR` is unset, `--socket` overriding both; directory `0700`, socket `0600`; the server closes a connection whose peer uid is not its own before reading a byte; a socket file that refuses connections is stale and `rudy serve` replaces it; newline-delimited JSON. A client dials an explicit `--socket` or fails, else probes the default with a 50ms timeout and embeds when nothing answers; `--embed` skips the probe; before it connects, the client refuses a socket or a socket directory that is not owned by its uid, is reached through a symlink, or sits in a directory group or other can write, and refuses rather than embedding; its hello is bounded at 2s; `rudy serve` checks an existing socket directory against the same rule and never narrows one it did not create (ADR 0014) |
| stdio | spawned plugins; the server is the parent | newline-delimited JSON on the child's stdin and stdout; stderr is captured into the log |

### Caller classes

| class | transport | authentication | may assert | must never assert |
|---|---|---|---|---|
| TUI client | in-memory, unix socket | in-memory: trusted by construction; socket: directory `0700`, socket `0600`, peer uid equals server uid via `LOCAL_PEERCRED` or `SO_PEERCRED` | user messages, permission answers, model, mode, thinking, title, attach and detach, slash commands | entries of any other kind, tool results, registrations, registry contents |
| headless client | in-memory, unix socket | as TUI client | user messages, commands, attach without asker | permission answers; it declares `asker: false` in hello and the Gate treats it as absent |
| spawned plugin | stdio | spawned by the server from a manifest the user placed in config; identity is the manifest name | registrations under its own name, results for its own tools, hook returns, notes, status and widgets under its own name, child sessions it opens and messages to those | user messages to sessions it did not open, permission answers, items under another plugin's name, entries directly |
| linked plugin | in-memory Go interface | compiled in; trusted by build | as spawned plugin | as spawned plugin |
| ACP adapter | unix socket | as TUI client | as TUI client | as TUI client; deferred, not in v1 |

A port reached by two classes has one authentication story per class, listed above. The authz column below names the class-level rule.

### Requests, client to server

Authn column names the caller class table. Domain column names the aggregate method or says query.

| method | caller | authn | authz | request | response | errors | idempotency | domain |
|---|---|---|---|---|---|---|---|---|
| `client.hello` | TUI, headless, ACP, plugin | per class | first request on a connection; refused otherwise | `{client, version, asker: bool}` | `{server, version}` | `invalid_argument` on protocol mismatch | idempotent per connection; a second hello is `refused_by_invariant` | none; registers the connection as an asker or not. A plugin connection may send it (it needs no introduction, so it usually does not) and its `asker` is ignored: the plugin holding a child session is the one waiting on that child's tool call, so its own question must never route back to it |
| `session.open` | TUI, headless, plugin | per class | any; `parent` only from a plugin, and only naming a `tool_use` pending in a live session whose tool that same plugin registered, in a session that is not itself a child, at most once per `tool_use`, and with `cwd` equal to that session's workspace root | `{cwd, model?: string, mode?: PermissionMode, thinking?: ThinkingLevel, agent?: string, tools?: [string], parent?: ParentRef}`; `model` is `provider:id` or a unique bare id; absent `model`, `mode`, `thinking` take the agent definition's values, then the parent's when `parent` is given, then config defaults; absent `agent` means the default agent. A session's tool set is the agent definition's list, intersected with `tools` when given, intersected with the parent's effective set when `parent` is given, minus `agent` for a child. Every term only removes: `tools` names what to keep and cannot name a tool the definition or the parent withheld, so delegation never widens what the caller holds (ADR 0028). Absent `tools` narrows nothing and an explicit empty list narrows to no tools at all, the same distinction the definition file carries. A name in `tools` that no plugin has registered is dropped before the set is persisted, so a session cannot acquire a tool later by having named one that did not exist when it opened | `SessionInfo`, same shape as `session.resume`; the `session_opened` entry is replayed first as `entry.appended`, this response returns once replay finishes | `invalid_argument` root not a directory, `parent` from a non-plugin, or `cwd` not the parent's workspace root; a name in `tools` that the definition or the parent did not hold is dropped rather than refused, since the set is an intersection and a caller asking for less than it is owed is not an error; `not_found` model, agent, parent session or parent tool_use; `unauthorized` the pending `tool_use` is not a tool of the calling plugin; `refused_by_invariant` the parent is itself a child session; `conflict` that `tool_use` has already opened a child; `unavailable` registry unreachable and no cached snapshot | not idempotent; every call opens a session | `Session.Open` factory; appends `session_opened` with `parent_session_id` and `parent_tool_use_id` |
| `session.resume` | TUI, headless, ACP | per class | any session on this machine | `{session_id}` | `SessionInfo`: `{session_id, workspace: Workspace, model: ModelRef, mode: PermissionMode, thinking: ThinkingLevel, title: string}`; every entry is replayed first as `entry.appended` notifications, then, when a turn is active, the current `turn.state` and any standing `permission.requested` to an asker connection, then this response | `not_found`; `unavailable` locked by another process, `data.socket` | idempotent; re-attaches | `Session.Load` then attach; `Session.Load` runs recovery |
| `session.fork` | TUI, headless, ACP | per class | any session | `{session_id, at_entry_id}`; `at_entry_id` empty means the newest entry | `SessionInfo` for the new session, same shape as `session.resume`; its entries are replayed first as `entry.appended`, this response returns once replay finishes | `not_found` session or entry | not idempotent | `Session.Fork(at)`; appends `fork_point` |
| `session.list` | TUI, headless, ACP | per class | any | `{}` no params | `{sessions: [SessionSummary]}` | none | idempotent | query over `session_opened` and last entries; no mutation |
| `session.close` | TUI, headless, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id}` | `{}` | `not_found` | idempotent | detach; fires `session_closed` hook when the last client detaches; appends nothing, except that a last subscriber leaving a turn parked in `TurnSteering` cancels it, which appends `turn_interrupted` |
| `session.submit` | TUI, headless, plugin | per class | plugin only to sessions it opened; a child session (one opened with `parent`) also refuses any caller, plugin or not, that is not subscribed to it, whatever the caller may be subscribed to instead (ADR 0028 decision 6: watching a session's parent is not the same as being subscribed to it) | `{session_id, content: [ContentBlock text or image], source: typed or steer}`; `shell` is written by `session.shell`, never submitted; pass 3: not yet implemented, `queued` (refused as `invalid_argument`) and `idempotency_key` | `{turn_id}`; pass 3: not yet reported, `entry_id` and `queued_position`, neither of which exists without a queue | `refused_by_invariant` typed while a turn is active, steer while not steering; `invalid_argument` empty content, a content block the log refuses or a source that is neither typed nor steer; `unauthorized` a plugin submitting to a session it did not open, or any caller submitting to a child session it is not subscribed to | not idempotent; pass 3 ignores `idempotency_key` because it does not accept one | `Turn.Start` for typed on idle, `Turn.Resume` for steer; appends `user_message` |
| `session.shell` | TUI, headless | per class | any attached | `{session_id, command}` | `{entry_id, is_error}` | `invalid_argument` empty command; `not_found` unknown session or no `bash` tool in this session's view; `conflict` a turn is active | not idempotent | invokes the registered `bash` tool with the command, ungated, and appends one `user_message` with `source: shell` carrying `$ <command>` and its output. Starts no turn: the model reads it with the next message. The Gate does not run, since the operator typed the command themselves (ADR 0023) |
| `session.interrupt` | TUI, headless, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id, how: steer or cancel}` | `{turn_id, state: TurnState}` | `refused_by_invariant` no active turn | idempotent; repeating returns current state | `Turn.Steer` or `Turn.Cancel`; cancel appends `turn_interrupted` |
| `session.answer` | TUI, ACP | per class | connection declared `asker: true` | `{session_id, tool_use_id, decision: allow or deny, scope: once or session, reason: string}` | `{}` | `unauthorized` not an asker; `not_found` no pending request; `conflict` when another asker answered first | keyed by `tool_use_id`; a second answer is `conflict` | `Turn.Answer` via the Gate; appends `permission_decision` |
| `session.set_model` | TUI, headless, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id, model: ModelRef}` | `{entry_id}` | `conflict` a turn is active; `not_found` model not in registry | same value appends nothing and returns the latest `model_change` id | `Session.SetModel`; appends `model_change` |
| `session.set_mode` | TUI, headless, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id, mode: PermissionMode}` | `{entry_id}` | `conflict` a turn is active; `invalid_argument` | same value appends nothing | `Session.SetMode`; appends `mode_change` |
| `session.set_thinking` | TUI, headless, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id, thinking: ThinkingLevel}` | `{entry_id}` | `conflict` a turn is active; `invalid_argument` | same value appends nothing | `Session.SetThinking`; appends `thinking_change` |
| `session.set_title` | TUI, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id, title: string}` | `{entry_id}` | `conflict` a turn is active; `invalid_argument` empty | same value appends nothing | `Session.SetTitle`; appends `title_change` |
| `session.compact` | TUI, headless, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id, instructions?: string}`; `instructions` steers the model's summary and skips the `before_compaction` hook when present | `{entry_id}` of the `compaction`; `{entry_id: ""}` when the request context holds fewer than two entries to cover | `conflict` a turn is active; `provider_error` the summary request failed; `unavailable` the session's provider is not loaded | not idempotent | Compactor; appends `compaction` |
| `registry.list` | TUI, headless, plugin, ACP | per class | any | `{provider?: string}` | `{fetched_at, models: [Model]}` | none | idempotent | query over the Registry snapshot |
| `registry.refresh` | TUI, headless, ACP | per class | any | `{provider?: string}` | `{fetched_at, models: [Model], failures: [{provider, error}]}` | none; a failing provider lands in `failures` and the rest succeed | idempotent | `Registry.Refresh` |
| `command.list` | TUI, headless, ACP | per class | any | `{}` | `{commands: [{name, description}]}` in registration order | none | idempotent | query over `PluginRegistry.Commands`; the set is fixed once every plugin has answered `plugin.init`, so a client asks once on connect and there is no notification for it. A plugin is refused: it knows its own registrations and has no use for another's. `/exit`, `/quit` and `/scoped-models` are not in it, being the client's own (ADR 0015, ADR 0020) |
| `command.run` | TUI, headless, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id, name, args: string}` | `{turn_id, notice, session_id}`; `turn_id` set for a command that submitted a prompt, `notice` for one that only reports, `session_id` set when the command opened another session, a fork | `not_found` unknown command or unknown session; `conflict` a turn is active and the command's action needs a resting session (`SubmitPrompt`, `Compact`) | not idempotent; the command decides | called whatever the turn is doing: a command is not a message and does not wait behind one, and the refusals above are what stop the two that cannot run mid-turn (ADR 0026). Runs the plugin.Command's `Run`, then maps its `Action` (`SubmitPrompt`, `Notice`, `Compact`, `SetModel`, `SetMode`, `SetTitle`, `Fork`, `NoAction`) onto the matching domain call. `SetTitle` names the session through the same path `session.set_title` takes, refusing an empty title the same way (ADR 0019); `SetMode` sets the permission mode through the same path `session.set_mode` takes, refusing an invalid one the same way |
| `plugin.register_tool` | plugin | per class | own name only | `{name, description, input_schema, safety: Safety}`; `input_schema` raw JSON | `{}` | `conflict` name taken; the plugin stays loaded and a `notice` is emitted | idempotent for the same plugin and identical definition | `PluginRegistry.RegisterTool` |
| `plugin.register_command` | plugin | per class | own name only | `{name, description}`; not yet implemented, `args` and their completion sources: the slash menu completes a command name, never its arguments | `{}` | `conflict` | idempotent for identical definition | `PluginRegistry.RegisterCommand` |
| `plugin.register_hook` | plugin | per class | own name only | `{point: HookPoint, priority: int}` | `{}` | `invalid_argument` unknown point | idempotent | `PluginRegistry.RegisterHook` |
| `plugin.register_widget` | plugin | per class | own name only | `{key, slot: header, above_editor or below_editor, content: [Span]}` | `{}` | `invalid_argument` unknown slot | idempotent; re-registering replaces content | `PluginRegistry.SetWidget` |
| `plugin.set_status` | plugin | per class | own name only | `{key, content: [Span]}`; empty content clears | `{}` | none | idempotent | `PluginRegistry.SetStatus` |
| `plugin.register_provider` | plugin | per class | own name only | `{name, wire: anthropic_messages, openai_chat or custom}`; `custom` means the server calls `provider.complete` on the plugin; a spawned plugin may register only `custom`, since a codec wire needs the endpoint and credential config a linked provider plugin reads | `{}` | `conflict` provider name taken; `invalid_argument` a codec wire from a spawned plugin, or an unknown wire | idempotent for identical definition | `PluginRegistry.RegisterProvider` |
| `plugin.register_agent` | plugin | per class | own name only | `{name, description, prompt: string, tools?: [string], model: string, thinking: ThinkingLevel, max_turns: int}`; the same fields `agents/<name>.md` carries, since a definition is static data and needs no callback. Absent `tools` means every tool, an empty list means none, matching the file | `{}` | `conflict` a plugin already registered that name; `invalid_argument` empty description, or a `thinking` that is not off, low, medium or high | idempotent for identical definition | `PluginRegistry.RegisterAgent`; read by `resolveAgent` after both disk roots, so a user or workspace definition of the same name wins and a plugin cannot take a name an operator is using (ADR 0028) |
| `plugin.append_note` | plugin | per class | any live session; pass 3 has no ownership check here, see the open list | `{session_id, text, role: info, muted, warn or error}` | `{entry_id}` | `not_found` | not idempotent | `Session.Append(note)` |

### Requests, server to plugin

| method | callee | authn | authz | request | response | errors | idempotency | domain |
|---|---|---|---|---|---|---|---|---|
| `plugin.init` | spawned plugin | parent-child | first request the server sends | `{name, version, protocol_version, config: table, workspace_roots: [string]}`; `config` is the plugin's `[plugins.<name>]` table verbatim | `{name, version, protocol_version}`; registrations the plugin sends before it answers are committed with the plugin, so a plugin declares its surface while the server waits for this response | `plugin_error` on mismatch or timeout; plugin marked failed; a `plugin.register_tool`, `plugin.register_command`, `plugin.register_hook`, `plugin.register_provider` or `plugin.register_agent` after the response is `refused_by_invariant`, since the tool set, command set, hook set, provider set and agent set a session was opened with never change under it. `plugin.set_status` and `plugin.register_widget` stay live for the life of the process: they are display, not surface | once per process | `Plugin.Ready` or `Plugin.Fail` |
| `tool.invoke` | owning plugin | parent-child | the plugin that registered the tool | `{session_id, tool_use_id, name, input, workspace: Workspace, timeout_ms: int}` | `{content: [ContentBlock], is_error: bool}` | `plugin_error` timeout or crash; `interrupted` after `tool.cancel` | keyed by `tool_use_id`; a repeat after a lost connection is a new invocation and the old result is discarded | `Turn.RunTool`; the result is appended as `tool_result` |
| `tool.cancel` | owning plugin | parent-child | as above | `{tool_use_id}` | `{}` | none | idempotent | `Turn.Steer` or `Turn.Cancel` reaching a running tool |
| `hook.fire` | registered plugins in priority order | parent-child | registered for that point | `{point, session_id, turn_id, payload}`; payloads in the events contract; `turn_id` is empty for `session_opened`, `before_compaction` and `session_closed` | `{result}` per point; timeout `hook_timeout_ms` from config | `plugin_error` timeout; the hook is skipped and a `notice` emitted | not idempotent | the domain event's consumer list |
| `command.invoke` | owning plugin | parent-child | the plugin that registered the command | `{session_id, name, args: string, mode, model, thinking}`; the session's own facts, so a command can report what it is about to change | `{prompt?: string, notice?: string}`; a non-empty `prompt` is submitted to the session as a user message and its `turn_id` comes back from `command.run`, a non-empty `notice` is shown to the client; both empty means the command did its work itself | `plugin_error` | not idempotent | plugin-defined |
| `provider.complete` | provider plugin with `wire: custom` | parent-child | registered provider | `{request_id, model: ModelRef, system: string, messages: [{role, content?: [ContentBlock], results?: [{tool_use_id, content: [ContentBlock], is_error: bool}]}], tools: [{name, description, input_schema}], thinking: ThinkingLevel, max_tokens: int}`; a message carries `content` for every role but `tool_result`, and `results` for that one. The results of one assistant message arrive as a single message carrying every one of them, because the two shipped codecs need opposite renderings of that group and only the kernel knows it as a group rather than by inferring it from adjacency (ADR 0028). A plugin rendering them must emit whatever its own upstream requires | `{stop_reason, stop_reason_raw, usage: Usage}` after the stream ends | `provider_error`, `interrupted` | keyed by `request_id` | `Provider.Complete` port |
| `provider.list_models` | provider plugin with `wire: custom` | parent-child | registered provider | `{}` | `{models: [Model]}` | `provider_error` | idempotent | `Registry.Refresh` for that provider |

### Notifications, server to clients

Per session, notifications are delivered in entry order. On `session.resume` the client receives every entry as `entry.appended` notifications first, then the `SessionInfo` response, then live notifications from the next entry on. Over in-memory and socket transports delivery is exactly once per connection; after a reconnect the client resumes and de-duplicates by entry id. `stream.delta` is never replayed.

A child session's `entry.appended`, `stream.delta`, `turn.state` and `tool.state` are also delivered to its parent's non-plugin subscribers, so a client watching a session sees the work it delegated (ADR 0028). They carry the child's `session_id`, which is what tells them apart from the parent's own; a client that keys on the session id was already correct and needs no change to stay correct. Depth is one, so the walk does not recurse. The plugin connection that opened the child is excluded, since it is the one waiting on the tool call and has no use for its own echo. A connection already subscribed to the child is excluded too, so a client watching the parent and reading the child receives each notification once rather than twice; `stream.delta` is never replayed and so could not be de-duplicated after the fact.

Receiving a child's notifications is not subscribing to it, and a watcher may not drive what it watches: `session.submit` and `session.interrupt` refuse a child session from a connection that is not subscribed to it. This matters because the routing is what makes a running child's id reachable at all. Before it, that id reached a client only in the parent's `tool_result`, after the call had already returned.

Two things this does not claim. Answering is deliberately not gated, because a child with no asker of its own borrows its parent's, so a parent's asker answering a child's question is the mechanism working rather than a leak. And the remaining session methods are not subscription-gated for a plain client at all, which is pre-existing and applies to every session rather than to children (`rudy-jkz`); the two gated above are the ones this wave put within reach.

| notification | to | payload | delivery |
|---|---|---|---|
| `entry.appended` | every client attached to the session, and the parent's | `{session_id, entry: Entry}` | ordered by entry id; replayed on attach via resume. A child's are not replayed to the parent on attach: the parent's own log carries the `agent` tool's result, and a finished child is read by resuming it |
| `stream.delta` | attached clients, and the parent's | `{session_id, turn_id, part: StreamPart}` | ordered; not replayed; superseded by the `assistant_message` entry |
| `turn.state` | attached clients, and the parent's | `{session_id, turn_id, state: TurnState}` | ordered; the current state is sent to a client attaching while a turn is active, after the replay and before the resume response. `RunningTool` means at least one tool call is running, not that exactly one is: which calls and where each has got to is `tool.state` |
| `tool.state` | attached clients, and the parent's | `{session_id, turn_id, tool_use_id, name, state: running, awaiting_permission or done}` | ordered per `tool_use_id`; every call in flight is sent to a client attaching mid-turn, after the replay and before the resume response. Not replayed afterwards: a finished call is its `tool_result` entry, and the log is what a client reads it from. Exists because tool calls run concurrently (ADR 0028), so a turn-level state cannot say which of several calls is waiting on the operator |
| `permission.requested` | attached asker clients | `{session_id, turn_id, tool_use_id, tool, input, matcher: {tool, prefix}}` | delivered to every attached asker and to an asker attaching while the question stands; the first `session.answer` decides; with no asker attached the Gate denies immediately, and when the last asker detaches while a question stands the Gate denies it with `no_asker` and appends `permission_decision` (ADR 0014) |
| `status.updated` | every client | `{items: [{owner, key, content: [Span]}]}` full set | latest wins; sent on connect |
| `widget.updated` | every client | `{owner, key, slot, content: [Span]}` | latest wins per owner and key; all sent on connect |
| `notice` | every client | `{level: info, warn or error, owner, text}` | best effort; not replayed |
| `plugin.state` | every client | `{name, origin: linked or spawned, state: loading, ready, failed or stopped, reason: string}`; `reason` empty unless failed | latest wins; all sent on connect |
| `registry.updated` | every client | `{fetched_at, providers: [{name, count: int, error: string}]}`; `error` empty on success | latest wins |

### Notifications, plugin to server

| notification | payload | delivery |
|---|---|---|
| `tool.progress` | `{tool_use_id, text}` | pass 3: received by the spawned plugin's adapter and dropped there; forwarding to clients as `stream.delta` needs a stream part type the TUI plan defines |
| `provider.delta` | `{request_id, part: StreamPart}` | ordered per request; the server assembles the `assistant_message` from them |

## 2. Domain events

Delivery inside the process is synchronous and ordered per session. Published events are the hook points; they cross the Plugin boundary and are a versioning obligation. Internal events are consumed inside the kernel and may change freely.

### Entry-append events, emitted by Session on `Append`

| name | aggregate | transition | payload | consumers | delivery | boundary | owner |
|---|---|---|---|---|---|---|---|
| `SessionOpened` | Session | `Open` | the `session_opened` entry | Turn (idle), memory plugin, skills loader, clients, `session_opened` hook | sync, ordered | published as hook | `Session.Open` |
| `ForkPointRecorded` | Session | `Fork` | the `fork_point` entry | clients | sync | internal | `Session.Fork` |
| `UserMessageAppended` | Session | `Append` | the `user_message` entry | Turn (`Start` or `Resume`), `before_turn` hook when source is typed or queued, RequestAssembler | sync | published as `before_turn` | `Turn.Start`, `Turn.Resume` |
| `AssistantMessageAppended` | Session | `Append` | the `assistant_message` entry | Turn (complete or run tools), Gate for each `tool_use`, `after_response` hook, Compactor, clients | sync | published as `after_response` | `Turn.OnResponse` |
| `PermissionDecided` | Session | `Append` | the `permission_decision` entry | Turn (run or reject the tool), Session allowances when scope is session, clients | sync | internal | Gate |
| `ToolResultAppended` | Session | `Append` | the `tool_result` entry | Turn (next request), `after_tool` hook, Compactor, clients | sync | published as `after_tool` | `Turn.OnToolResult` |
| `ModelChanged` | Session | `SetModel` | the `model_change` entry | RequestAssembler, Compactor (new window), clients | sync | internal | `Session.SetModel` |
| `ModeChanged` | Session | `SetMode` | the `mode_change` entry | Gate, clients | sync | internal | `Session.SetMode` |
| `ThinkingChanged` | Session | `SetThinking` | the `thinking_change` entry | RequestAssembler, clients | sync | internal | `Session.SetThinking` |
| `TitleChanged` | Session | `SetTitle` | the `title_change` entry | clients, session list | sync | internal | `Session.SetTitle` |
| `CompactionRecorded` | Session | `Append` | the `compaction` entry | RequestAssembler, clients | sync | internal | Compactor |
| `TurnInterrupted` | Session | `Append` | the `turn_interrupted` entry | Turn (idle), clients, queued messages returned to the client | sync | internal | `Turn.Cancel` |
| `TurnFailed` | Session | `Append` | the `turn_failed` entry | Turn (idle), clients | sync | internal | `Turn.Fail` |
| `NoteAppended` | Session | `Append` | the `note` entry | clients | sync | internal | `Session.Append` |

### Turn transitions, emitted by Turn

| name | aggregate | transition | payload | consumers | delivery | boundary | owner |
|---|---|---|---|---|---|---|---|
| `TurnStarted` | Turn | idle to streaming | `{session_id, turn_id}` | clients (`turn.state`), RequestAssembler, `before_request` hook | sync | published as `before_request` | `Turn.Start` |
| `ToolRequested` | Turn | streaming to running_tool or awaiting_permission | `{session_id, turn_id, tool_use_id, tool, input}` | Gate, `before_tool` hook | sync | published as `before_tool` | Gate |
| `PermissionRequested` | Turn | streaming to awaiting_permission | `{session_id, turn_id, tool_use_id, matcher, mode}` | asker clients (`permission.requested`) | sync; auto-deny when no asker | internal | Gate |
| `ToolStarted` | Turn | awaiting_permission or streaming to running_tool | `{session_id, turn_id, tool_use_id}` | clients (`turn.state`), owning plugin (`tool.invoke`) | sync | internal | `Turn.RunTool` |
| `ToolFinished` | Turn | running_tool to streaming | `{session_id, turn_id, tool_use_id, outcome}` | Session (`Append tool_result`) | sync | internal | `Turn.OnToolResult` |
| `TurnSteering` | Turn | streaming, running_tool or awaiting_permission to steering | `{session_id, turn_id, partial: [ContentBlock]}` | clients, running tool (`tool.cancel`), Session (partial `assistant_message` with `stop_reason: interrupted`, or `tool_result` with outcome `killed`) | sync | internal | `Turn.Steer` |
| `TurnResumed` | Turn | steering to streaming | `{session_id, turn_id}` | RequestAssembler, `before_request` hook | sync | published as `before_request` | `Turn.Resume` |
| `TurnCompleted` | Turn | streaming to completed | `{session_id, turn_id, usage: Usage}` | clients, `turn_completed` hook, Session (`Dequeue` next queued message) | sync | published as `turn_completed` | `Turn.Complete` |
| `TurnCancelled` | Turn | steering to idle | `{session_id, turn_id}` | Session (`Append turn_interrupted`), clients | sync | internal | `Turn.Cancel` |
| `TurnFailedEvent` | Turn | any active to failed | `{session_id, turn_id, class, message, retries}` | Session (`Append turn_failed`), clients | sync | internal | `Turn.Fail` |
| `CompactionRequested` | Turn | streaming, after an `assistant_message` whose prompt tokens cross `sessions.compact_at` of the context window, or `session.compact` | `{session_id, first_entry_id, last_entry_id, prompt_tokens, context_window}` | `before_compaction` hook (a summary), else Compactor (model summary) | sync | published as `before_compaction` | Compactor |

### Plugin and Registry events

| name | aggregate | transition | payload | consumers | delivery | boundary | owner |
|---|---|---|---|---|---|---|---|
| `PluginLoading` | Plugin | new to loading | `{name, origin}` | clients (`plugin.state`) | sync | internal | `Plugin.Load` |
| `PluginReady` | Plugin | loading to ready | `{name}` | PluginRegistry (capabilities become visible), clients | sync | internal | `Plugin.Ready` |
| `PluginFailed` | Plugin | loading or ready to failed | `{name, reason}` | PluginRegistry (capabilities withdrawn), clients (`notice`, `plugin.state`) | sync | internal | `Plugin.Fail` |
| `PluginStopped` | Plugin | ready to stopped | `{name}` | PluginRegistry, clients | sync | internal | `Plugin.Stop` |
| `CapabilityRegistered` | Plugin | ready, on any `register_*` | `{plugin, kind: tool, command, hook, widget, status, provider or agent, name}` | RequestAssembler (tools), `resolveAgent` (agents), clients (commands, widgets, status) | sync | internal | `PluginRegistry.Register*` |
| `CapabilityRejected` | Plugin | ready, on a `conflict` | `{plugin, kind, name, held_by}` | clients (`notice`) | sync | internal | `PluginRegistry.Register*` |
| `RegistryRefreshed` | Registry | `Refresh` | `{fetched_at, providers: [{name, count, error}]}` | clients (`registry.updated`), model picker, `session.open` validation | sync | internal | `Registry.Refresh` |

### Published events, the hook points

Handlers run in priority order, then plugin load order. Each handler gets `hook_timeout_ms`; a timeout skips that handler and emits a `notice`. Returns are merged in order.

| point | fires on | payload | handler may return |
|---|---|---|---|
| `session_opened` | `SessionOpened`, also on `session.resume` first attach | `{session_id, workspace, model, mode, thinking, resumed: bool, parent_session_id: string}`; `parent_session_id` is the session whose tool call opened this one, empty for a root session, so a handler that writes once per session can tell a subagent apart from the session it belongs to | `{context: string}` appended to the system prompt for the session |
| `before_turn` | `UserMessageAppended` with source typed or queued | `{session_id, turn_id, message: Entry}` | `{system_prompt_additions: [string]}` |
| `before_request` | `TurnStarted`, `TurnResumed` and every subsequent request in the turn | `{session_id, turn_id, provider, model, headers: table}`; no body and no body size: the hook fires while the request is still a `provider.Request`, before any codec has made bytes of it | `{headers: table}` merged over the request headers; body is not exposed in pass 1 |
| `after_response` | `AssistantMessageAppended` | `{session_id, turn_id, message: Entry}` | nothing |
| `before_tool` | `ToolRequested`, before the Gate | `{session_id, turn_id, tool_use_id, tool, input, safety}` | `{decision: pass, allow, deny or modify, input, reason}`; `allow` and `deny` short-circuit the Gate and are recorded with `decided_by: hook`; `modify` replaces `input` |
| `after_tool` | `ToolResultAppended`, before the result reaches the next request | `{session_id, turn_id, tool_use_id, result: Entry}` | `{content: [ContentBlock]}` replacing what the model sees; the stored entry is unchanged. The replacement is held for as long as the session is live, so it applies to every later request in the session and not only the turn that produced it, and it is not persisted: a session loaded from disk sends the stored entry again until a handler replaces it again |
| `before_compaction` | `CompactionRequested`, when the Compactor decides to compact and `session.compact` carried no `instructions` | `{session_id, first_entry_id, last_entry_id, prompt_tokens: int, context_window: int}` | `{summary: string}`; the first non-empty summary in handler order is used and the model is not asked; empty means pass |
| `turn_completed` | `TurnCompleted` | `{session_id, turn_id, usage}` | nothing |
| `session_closed` | last client detaches, or the server exits | `{session_id}` | nothing; the server waits `hook_timeout_ms` |

### Domain services

| service | rule it owns | aggregates it spans |
|---|---|---|
| Gate | given a `tool_use`, the tool's safety class, the session's mode, session allowances and whether an asker is attached, decide allow, deny or ask; classify the input into a matcher; hold the dangerous set for permissive mode. Two rules belong to this service and are enforced at the asker rather than in `Evaluate`, whose verdict is a pure function of its input and holds no session: concurrent calls with the same matcher **and the same input bytes** raise one question and share its answer, and an answer of `session` scope resolves every parked question its allowance covers. The input is part of the key because a matcher is coarse (every tool but `bash` reduces to its name), so matcher-alone coalescing would bind a call to consent given for another call's arguments (ADR 0028) | Turn, Plugin (tool safety), Session (mode, allowances), connection registry (askers) |
| RequestAssembler | build the provider request: `Session.RequestContext`, system prompt from the agent definition, skills index, `session_opened` context and `before_turn` additions, tool definitions from ready plugins, model and thinking | Session, Plugin registry, Registry |
| Compactor | after `AssistantMessageAppended`, when that message's prompt tokens (`usage.input + cache_read + cache_write`) reach `sessions.compact_at` of the model's context window and the window is known, or on `session.compact`: cover every request-context entry before the current turn's `user_message`, ask the `before_compaction` hook for a summary, else summarize with the session's model, append `compaction` | Session, Registry (context window), Provider, Plugin registry (hook) |
| Subagent runner | given an `agent` tool call, open a child session under the named agent definition with `parent` set and the call's `tools` narrowing, submit the prompt, wait for `turn.state` completed or failed, return the final assistant text or the failure as the tool result; a child session exposes no `agent` tool. Several `agent` calls in one assistant message run at once, so the runner holds no state shared between calls | Session (child), Plugin (tool), Registry (model) |
| Tool scheduler | given the tool calls of one assistant message, run them all concurrently, each with its own cancellation, and append each result as it lands; an interrupt is observed by every call in flight rather than consumed by one, so no call records success after the turn was cut. Reentrancy is the tool's own obligation, not a property the scheduler infers from safety (ADR 0028) | Turn, Plugin (tool invoke), Session (append) |
| Recovery | on `Session.Load`, for every `permission_decision` allow without a `tool_result`, append `tool_result` with outcome `lost`; single aggregate, listed here because it runs outside a turn | Session |

The dangerous set is checked before session allowances in every mode but off: a dangerous command always asks even when a prior session-scope allow matches, so widening a matcher prefix can never silence a dangerous command. See ADR 0011.

Steering is `Turn.Steer` and `Turn.Resume`, one aggregate, no service.

## 3. Record layer

No database. Files under XDG roots, resolved as `$XDG_CONFIG_HOME` or `~/.config`, `$XDG_DATA_HOME` or `~/.local/share`, `$XDG_RUNTIME_DIR` or the OS temp dir, `$XDG_CACHE_HOME` or `~/.cache`, on every platform including macOS.

| path | owner | holds |
|---|---|---|
| `$XDG_DATA_HOME/rudy/sessions/<ulid>/entries.jsonl` | Session | the log; append-only |
| `$XDG_DATA_HOME/rudy/sessions/<ulid>/blobs/<sha256>` | Session | image bytes referenced by `blob` |
| `$XDG_DATA_HOME/rudy/sessions/<ulid>/lock` | Session | `flock` held by the process serving the session; a second process gets `unavailable` and is told the socket path |
| `$XDG_CACHE_HOME/rudy/registry.json` | Registry | the last discovered snapshot; a snapshot is a cache |
| `$XDG_CACHE_HOME/rudy/rudy.log` | kernel | slog JSON lines; UNSHIPPED (rudy-oat), nothing writes it yet |
| `$XDG_CONFIG_HOME/rudy/config.toml` | user | config; rudy never writes it |
| `$XDG_CONFIG_HOME/rudy/themes/<name>.toml` | user | theme roles |
| `$XDG_DATA_HOME/rudy/trust.toml` | user | the workspaces whose own plugins the operator agreed to run, and what they agreed to (ADR 0025) |
| `$XDG_DATA_HOME/rudy/scope.toml` | user | the models `ctrl+p` and `ctrl+n` cycle through, as `/scoped-models` last left them; absent or empty is the whole registry (ADR 0027) |
| `$XDG_CONFIG_HOME/rudy/system.md` | user | the system prompt template, when `prompt.file` names none |
| `$XDG_CONFIG_HOME/rudy/plugins/<name>/plugin.toml` | user | spawned plugin manifest |
| `$XDG_CONFIG_HOME/rudy/agents/<name>.md` | user | agent definition |
| `<workspace>/.rudy/config.toml` | project | overrides merged over the user config; same keys |
| `<workspace>/.rudy/plugins/<name>/plugin.toml` | project | spawned plugin manifest; loaded only after the operator has trusted this workspace as it stands, and never by a client that cannot ask (ADR 0025) |
| `<workspace>/.rudy/agents/<name>.md` | project | agent definition |
| `~/.agents/skills/<name>/SKILL.md` | standard | skill |
| `<workspace>/.agents/skills/<name>/SKILL.md` | standard | skill |
| `~/.agents/memory` | memory SDK | the OKF bundle; rudy reads and writes only through the SDK |
| `$XDG_CONFIG_HOME/rudy/mcp.toml` | `rudy mcp` | user-scope MCP servers; written only by `rudy mcp add` and `remove` |
| `<workspace>/.rudy/mcp.toml` | `rudy mcp` | project-scope MCP servers, merged over the user scope by name |
| `$XDG_DATA_HOME/rudy/plugins/<name>/` | `rudy plugin` | an installed spawned plugin, a git checkout holding `plugin.toml` |
| `$XDG_DATA_HOME/rudy/plugins.lock.toml` | `rudy plugin` | what is installed, from where, at which commit, enabled or not |

### entries.jsonl

One JSON object per line. Every line has the envelope; the rest is the kind's payload. No field is ever absent unless the row says what absence means. Nothing is re-marshaled: `input`, `signature` and `content` text are stored as the bytes received.

Envelope:

| field | type | null | meaning |
|---|---|---|---|
| `id` | ulid string | no | strictly increasing within the file; the first byte of an entry is the first byte after the previous entry's newline |
| `at` | rfc3339nano string | no | server clock when appended |
| `kind` | EntryKind | no | one of the kinds below |

Invariants, enforced by `Session.Append` and checked by `Session.Load`:

- append-only; a line is never rewritten or removed
- `id` is monotonic; `Load` refuses a file whose ids are not increasing
- the first line is `session_opened` or `fork_point`; `fork_point` appears only as the first line; `session_opened` appears only as the first line of a root session
- an `assistant_message` containing `tool_use` blocks is followed, for each `tool_use.id`, by exactly one `permission_decision` and then exactly one `tool_result` before the next `assistant_message`; `Load` appends `tool_result` outcome `lost` for any allow without a result
- for a tool whose safety is `unsafe`, the `permission_decision` line is fsynced before the tool runs
- `user_message` with source `steer` appears only after an `assistant_message` with `stop_reason: interrupted` or a `tool_result` with outcome `killed`
- `compaction.first_entry_id` and `last_entry_id` name entries in this file or, for a fork, in the parent chain
- a session with children under `fork_point` is refused deletion

Kinds:

**`session_opened`**

| field | type | null | meaning |
|---|---|---|---|
| `schema_version` | int | no | 2. Version 1 lacks `tools` |
| `rudy_version` | string | no | the binary that opened it |
| `workspace` | Workspace | no | `git_root` empty means not a repo |
| `model` | ModelRef | no | initial selection |
| `thinking` | ThinkingLevel | no | initial |
| `mode` | PermissionMode | no | initial |
| `agent` | string | no | agent definition name; `default` when none |
| `parent_session_id` | ulid string | no | the session whose tool call opened this one; empty means a root session |
| `parent_tool_use_id` | string | no | the `tool_use` id in the parent that opened this one; empty exactly when `parent_session_id` is empty |
| `tools` | [string] | yes | the tool set resolved at open, before the `agent` deny. `null` means every tool the registry offers, an empty list means none. The one nullable field in the log, because the distinction it carries is the difference between an unrestricted session and a restricted one |

```json
{"id":"01K4M0A7Q8ZJ3N6R9T2V5X8B1D","at":"2026-09-07T20:30:00.123456789-06:00","kind":"session_opened","schema_version":2,"rudy_version":"0.1.0","workspace":{"root":"/Users/guy/projects/rudy","git_root":"/Users/guy/projects/rudy","project_id":"local/rudy"},"model":{"provider":"aperture","model":"cline-pass/kimi-k3"},"thinking":"high","mode":"strict","agent":"default","parent_session_id":"","parent_tool_use_id":"","tools":null}
```

A pass 1 log lacks the two parent fields; `Load` reads their absence as empty.

`tools` is the session's tool set as resolved at open: the agent definition's list, intersected with the caller's `tools` and with the parent's effective set. It is recorded **before** the `agent` deny that caps subagent depth, because that deny is derived on every rebuild from `parent_session_id` rather than replayed, so recording it would store a rule rather than a fact. A child under a parent that holds `agent` therefore records `agent` here and is still refused it. `null` means every tool the registry offers and an empty list means none, the same distinction the definition file carries. It is written because a session is rebuilt on resume long after its parent is gone, and recomputing it from the definition alone would hand back exactly what the intersection removed (ADR 0028).

This field is why `schema_version` is 2. A version 1 log has no `tools` key, and an absent key is indistinguishable from an explicit `null` once decoded into a slice, so reading one as `null` would hand every pre-existing session the whole registry on its next resume: for a child that is the escalation this field exists to close. `Load` takes the definition's list for a version 1 log and the recorded value from version 2 on.

That leaves one residual, stated rather than left to be discovered: a version 1 child log resumes under its own definition's list, which for any definition wider than its parent's set is wider than the bound a session opened today would get. There is nothing recorded to bound it and its parent is long gone. It is not a new escalation, since a session written before the field ran unbounded while it was live too, but it is looser than anything opened from now on. Refusing to resume such a log was considered and rejected: it would break sessions that predate the field, to retroactively enforce a rule they never ran under.

A version 1 log that does carry `tools` is a different case and is not covered by that argument. It can only come from a build made while this field was landing, and it holds a bound that was genuinely applied when the session was live, so discarding it would resume that session wider than it ran. `Load` falls back to the definition only when the recorded value is absent, never when it is present.

**`fork_point`**

| field | type | null | meaning |
|---|---|---|---|
| `parent_session_id` | ulid | no | |
| `parent_entry_id` | ulid | no | the last entry this session shares with the parent |

```json
{"id":"01K4M0B2...","at":"...","kind":"fork_point","parent_session_id":"01K4M0A7...","parent_entry_id":"01K4M0A9..."}
```

**`user_message`**

| field | type | null | meaning |
|---|---|---|---|
| `source` | `typed`, `queued`, `steer` | no | how it arrived |
| `content` | [ContentBlock text or image] | no | at least one block |

```json
{"id":"01K4M0A8...","at":"...","kind":"user_message","source":"typed","content":[{"type":"text","text":"fix the flaky fork test"}]}
```

**`assistant_message`**

| field | type | null | meaning |
|---|---|---|---|
| `model` | ModelRef | no | the model that produced it; recorded because the selection can change mid-session and the usage belongs to this model |
| `thinking` | ThinkingLevel | no | level in effect |
| `content` | [ContentBlock text, thinking or tool_use] | no | may be empty when `stop_reason` is `interrupted` before any delta |
| `usage` | Usage | no | verbatim from the provider; zeros when the provider sent none |
| `stop_reason` | `end_turn`, `tool_use`, `max_tokens`, `interrupted`, `refused`, `other` | no | domain classification |
| `stop_reason_raw` | string | no | the provider's own value; empty for `interrupted` |

`usage` is a verbatim external fact, stored rather than derived because the provider is the only source and totals are summed from these rows.

```json
{"id":"01K4M0A9...","at":"...","kind":"assistant_message","model":{"provider":"aperture","model":"cline-pass/kimi-k3"},"thinking":"high","content":[{"type":"text","text":"Looking at the test first."},{"type":"tool_use","id":"toolu_01","name":"bash","input":{"command":"go test ./internal/session -run TestFork -count=3"}}],"usage":{"input":1200,"output":80,"cache_read":0,"cache_write":0},"stop_reason":"tool_use","stop_reason_raw":"tool_calls"}
```

**`permission_decision`**

| field | type | null | meaning |
|---|---|---|---|
| `tool_use_id` | string | no | |
| `tool` | string | no | |
| `mode` | PermissionMode | no | mode in effect |
| `matcher` | `{tool, prefix}` | no | the Gate's classification of the input; `prefix` is the command's leading words for bash and empty for other tools |
| `decision` | `allow`, `deny` | no | |
| `decided_by` | `class`, `mode`, `allowance`, `hook`, `asker`, `no_asker` | no | `class` means the tool is safe; `mode` means off or permissive let it through; `allowance` means a prior session-scope allow matched; `no_asker` is always a deny |
| `scope` | `once`, `session` | no | `session` only ever comes from an asker allow; every other row says `once` |
| `reason` | string | no | the asker's text, the hook's reason or the rule name; never empty |
| `input` | object | yes | the input the tool ran with when a `before_tool` hook modified it; absent otherwise. Written and read byte for byte, like a `tool_use` input, and the last key on the line |

```json
{"id":"01K4M0AA...","at":"...","kind":"permission_decision","tool_use_id":"toolu_01","tool":"bash","mode":"strict","matcher":{"tool":"bash","prefix":"go test"},"decision":"allow","decided_by":"asker","scope":"session","reason":"allow for session"}
{"id":"01K4M0AB...","at":"...","kind":"permission_decision","tool_use_id":"toolu_02","tool":"bash","mode":"strict","matcher":{"tool":"bash","prefix":"go test"},"decision":"allow","decided_by":"hook","scope":"once","reason":"hook","input":{"command":"go test ./..."}}
```

**`tool_result`**

| field | type | null | meaning |
|---|---|---|---|
| `tool_use_id` | string | no | |
| `outcome` | `ok`, `error`, `killed`, `lost` | no | `killed` by steer or cancel; `lost` written by Recovery |
| `content` | [ContentBlock text or image] | no | empty for `lost` |
| `duration_ms` | int | no | zero for `lost` |

```json
{"id":"01K4M0AB...","at":"...","kind":"tool_result","tool_use_id":"toolu_01","outcome":"ok","content":[{"type":"text","text":"--- FAIL: TestFork (0.01s)\n    fork_test.go:41: want 3 entries, got 2\n"}],"duration_ms":412}
```

**`model_change`**

| field | type | null | meaning |
|---|---|---|---|
| `model` | ModelRef | no | |

```json
{"id":"01K4M0AC...","at":"...","kind":"model_change","model":{"provider":"aperture","model":"gpt-5.6-sol"}}
```

**`mode_change`**

| field | type | null | meaning |
|---|---|---|---|
| `mode` | PermissionMode | no | |

```json
{"id":"01K4M0AD...","at":"...","kind":"mode_change","mode":"permissive"}
```

**`thinking_change`**

| field | type | null | meaning |
|---|---|---|---|
| `thinking` | ThinkingLevel | no | `off`, `low`, `medium`, `high` |

```json
{"id":"01K4M0AD...","at":"...","kind":"thinking_change","thinking":"medium"}
```

**`title_change`**

| field | type | null | meaning |
|---|---|---|---|
| `title` | string | no | non-empty |

```json
{"id":"01K4M0AE...","at":"...","kind":"title_change","title":"fork off-by-one"}
```

**`compaction`**

| field | type | null | meaning |
|---|---|---|---|
| `summary` | string | no | what the model sees in place of the covered entries |
| `first_entry_id` | ulid | no | first covered entry |
| `last_entry_id` | ulid | no | last covered entry; must be present and not before `first_entry_id` |
| `model` | ModelRef | no | the model that wrote the summary |
| `usage` | Usage | no | cost of writing it |

```json
{"id":"01K4M0AF...","at":"...","kind":"compaction","summary":"...","first_entry_id":"01K4M0A8...","last_entry_id":"01K4M0AD...","model":{"provider":"aperture","model":"cline-pass/kimi-k3"},"usage":{"input":40000,"output":900,"cache_read":0,"cache_write":0}}
```

**`turn_interrupted`**

| field | type | null | meaning |
|---|---|---|---|
| `turn_id` | ulid | no | |
| `how` | `steer`, `cancel` | no | `steer` is recorded when the user interrupted and then resumed; `cancel` when the turn ended |

```json
{"id":"01K4M0AG...","at":"...","kind":"turn_interrupted","turn_id":"01K4M0A8...","how":"cancel"}
```

**`turn_failed`**

| field | type | null | meaning |
|---|---|---|---|
| `turn_id` | ulid | no | |
| `class` | `provider`, `transport`, `plugin`, `internal` | no | `provider` answered with an error after retries; `transport` never answered; `plugin` a plugin fault; `internal` a rudy fault |
| `message` | string | no | |
| `retries` | int | no | attempts made before giving up; 0 when not applicable |

```json
{"id":"01K4M0AH...","at":"...","kind":"turn_failed","turn_id":"01K4M0A8...","class":"provider","message":"502 from aperture after 4 attempts","retries":4}
```

**`note`**

| field | type | null | meaning |
|---|---|---|---|
| `plugin` | string | no | owner |
| `text` | string | no | display only; never sent to a model |
| `role` | `info`, `muted`, `warn`, `error` | no | the theme role the client renders it with |

```json
{"id":"01K4M0AI...","at":"...","kind":"note","plugin":"memory","text":"3 concepts folded","role":"muted"}
```

### registry.json

| field | type | null | meaning |
|---|---|---|---|
| `fetched_at` | rfc3339 | no | last successful refresh of any provider |
| `providers` | table of name to `{fetched_at, error, models: [Model]}` | no | `error` empty on success; `models` is the last good list even when `error` is set |

Refreshed on `session.open`, on picker open and on a `not_found` model error from a provider. No timer.

### config.toml

Every key, its type, default and meaning. A missing key takes the default. Unknown keys are
ignored; refusing them at load with the key path named is pass 3: not yet implemented (bead
rudy-k0.27).

The socket is not among them: it is `Paths.Socket()`, overridden by `--socket` and by
nothing in config (ADR 0014 decision 2), so a `server.socket` key is one of the unknown ones.

| key | type | default | meaning |
|---|---|---|---|
| `default.provider` | string | required when `providers` is non-empty | the provider of the one model in config; everything else comes from the registry |
| `default.model` | string | required | the model id under that provider. A model spec on the CLI or in the protocol is `provider:id`, or a bare id that is unique across providers |
| `default.thinking` | ThinkingLevel | `high` | |
| `agent` | string | `default` | agent definition for new sessions |
| `hook_timeout_ms` | int | 5000 | per handler |
| `tool_timeout_ms` | int | 600000 | per tool invocation |
| `max_tokens` | int | 8192 | output tokens one request may produce |
| `permissions.mode` | PermissionMode | `strict` | |
| `permissions.dangerous` | [string] | see open list | matchers that always ask unless the mode is off, ahead of any session allowance; each is `tool` or `tool:prefix`. Pass 1 entries are plain shell command prefixes for bash; the `tool:prefix` form is deferred |
| `permissions.double_press_ms` | int | 500 | the Esc window; lives here because the client reads it; must be positive, since zero is a window no two presses fall inside and the double-Esc cancel would be unreachable |
| `prompt.file` | path | `` | a system prompt template of the operator's own; empty reads `system.md` under the config directory when it exists, and the built-in template when it does not. `${base}`, `${tools}`, `${agents}`, `${version}`, `${workspace}`, `${project}`, `${model}`, `${date}`, `${os}` are the values a template may name, `$${` writes a literal `${`, and a name outside the set is a warning notice and the built-in prompt (ADR 0024) |
| `sessions.dir` | path | `$XDG_DATA_HOME/rudy/sessions` | |
| `sessions.compact_at` | float | 0.8 | fraction of the context window that triggers the Compactor |
| `log.level` | `debug`, `info`, `warn`, `error` | `info` | UNSHIPPED (rudy-oat): nothing logs yet, so neither key exists |
| `log.file` | path | `$XDG_CACHE_HOME/rudy/rudy.log` | UNSHIPPED (rudy-oat): the file in the record layer table above is written by nobody until this is |
| `ui.header.frame` | bool | true | false draws the header's lines with no box around them |
| `ui.header.greeting` | bool | true | the time of day and a name in the header |
| `ui.header.mark` | bool | true | the cat in the header |
| `ui.header.facts` | [string] | `["model", "thinking", "workspace"]` | the session's facts the header carries, in the order they draw; legal entries are `model`, `thinking`, `mode`, `workspace` |
| `ui.mouse` | `off`, `click`, `all` | `off` | what the client asks the terminal to report. `off` leaves the mouse alone, so a drag selects text as it does anywhere else; `click` reports clicks and the wheel, which is what expands a tool row and scrolls the transcript; `all` reports movement too. With reporting on, a terminal's own selection is a modifier away: Shift in Ghostty, WezTerm and most others, Option in Terminal.app and iTerm2 (ADR 0006) |
| `ui.render` | `inline`, `altscreen` | `altscreen` | `altscreen` owns the screen and keeps every row expandable; `inline` draws in the terminal's own scrollback and commits a rested turn's rows out of the live region, where they can no longer be expanded (ADR 0015) |
| `ui.vim` | bool | true | |
| `ui.layout.slots` | [string] | `["transcript", "input", "status"]` | order top to bottom; `header` may be added |
| `ui.transcript.tool_collapsed` | bool | true | |
| `ui.transcript.tool_preview_lines` | int | 2 | |
| `ui.transcript.thinking` | `hidden`, `shown` | `hidden` | |
| `ui.transcript.user_prefix` | string | `›` | |
| `ui.transcript.block_gap` | int | 1 | blank lines between assistant blocks; zero or positive |
| `ui.diff.style` | `text`, `background` | `text` | |
| `ui.status.above_editor` | [string] | `["turn"]` | the status line, drawn above the composer's upper line, same vocabulary as `ui.status.items`: what is worth seeing while typing rather than after. An item is drawn once, in the first list that names it, so a file that carried `turn` under the composer before this key existed does not draw it twice |
| `ui.status.items` | [string] | `["vim_mode", "model", "permission_mode", "cost", "workspace", "cat"]` | built-in keys (`vim_mode`, `model`, `permission_mode`, `context`, `cost`, `workspace`, `turn`, `cat`) plus `plugin:key` for plugin items; `context` is still a legal item and is no longer a default, since the composer's lower rule carries it (ADR 0017); `turn` is a spinner and one word (`thinking`, `streaming`, `tool`, `steering`, `waiting`) while a turn runs and nothing at rest |
| `ui.input.rules` | bool | true | the two composer lines, one above the composer and one below; the upper one carries the session's name once it has one (ADR 0019) and the lower one the context percentage, labelled, which is the only place it is drawn (ADR 0017) |
| `ui.header.show` | bool | true | the startup header: the greeting, the mark, the session facts, the tips and what is new. Drawn once at the top of the transcript and scrolled away by it (ADR 0016) |
| `ui.header.animate` | bool | true | the mark materializes once on startup, then settles. Altscreen only: inline draws the settled header, since an inline frame that grows and shrinks strands its rows |
| `ui.header.name` | string | `` | the name the greeting uses; empty resolves git `user.name`, then the OS user, first token capitalized |
| `ui.header.tips` | int | 2 | tips drawn in the right column, rotated by the day; `0` draws none |
| `ui.header.updates` | int | 3 | bullets from the newest `CHANGELOG.md` release the binary was built with; `0` draws none |
| `ui.header.max_width` | int | 120 | the widest the box draws; a wider terminal leaves the rest of the line empty rather than stretching |
| `ui.spinner.name` | `arc`, `blocks`, `pulse`, `paw`, `dots` | `arc` | the glyph the turn cell animates; `dots` is the braille spinner every other CLI uses |
| `ui.spinner.frames` | [string] | `[]` | frames of your own, in order, each one cell wide; empty uses the preset's |
| `ui.spinner.interval_ms` | int | 0 | how long each frame is on screen; `0` uses the preset's own timing |
| `ui.notices.ttl_ms` | int | 8000 | how long a notice stays on screen before it goes; `0` keeps it until a newer one pushes it out |
| `ui.notices.max` | int | 3 | notice lines the client draws under the transcript, newest first; `0` draws none |
| `ui.cats` | bool | true | a random cat face from `internal/cats` in the status line's `cat` cell, one for the life of the client; false leaves the cell empty wherever `ui.status.items` placed it |
| `ui.icons.set` | `nerd`, `unicode`, `ascii` | `nerd` | the glyph set: `nerd` is Nerd Font codepoints (Powerline, Font Awesome 4) and needs a patched font, `unicode` needs none, `ascii` is what the client drew before icons (ADR 0018) |
| `ui.icons.<name>` | string | per set | overrides one glyph; the empty string draws none and leaves no gap. Names: `branch`, `model`, `context`, `collapsed`, `expanded`, `tool`, `bash`, `edit`, `read`, `write`, `grep`, `glob`, `fetch`, `agent`, `info`, `warn`, `error`; an unknown set or name is a load error naming it |
| `ui.theme.name` | string | `default` | a file under `themes/` or the built-in |
| `ui.theme.<role>` | color or role name | per theme | overrides; roles listed under themes. `shell` paints the composer while a draft is a shell command and the row it becomes, and defaults to `warning`. `status` paints the status line's cells and `spinner` the spinner in its turn cell: the one thing on that line that moves is the only news on it, so it is not painted as the line around it |
| `keys.<action id>` | string or [string] | pi defaults | one of the ids ADR 0013 decision 5 lists; a value replaces the default for that action; `[]` unbinds; an unknown id or an unparseable key is a load error naming it |
| `providers.<name>.wire` | `anthropic_messages`, `openai_chat`, `custom` | required | `custom` is a provider a plugin serves over `provider.complete`; the two codec wires are the linked provider plugins |
| `providers.<name>.base_url` | string | required | |
| `providers.<name>.auth` | string | `` | `env:NAME` reads the environment; `cache:KEY` reads a `KEY=value` line from the 1Password cache file; empty means no auth header |
| `providers.<name>.headers` | table | `{}` | sent on every request |
| `providers.<name>.models_path` | string | `/v1/models` | discovery endpoint relative to `base_url`; pass 3: not yet implemented, the key is unread and each codec asks its own fixed path |
| `providers.<name>.dialect` | `` or `clinepass` | `` | a dialect plugin that wraps the wire codec for this endpoint; the `openai_chat` plugin skips a provider that names one |
| `plugins.disabled` | [string] | `[]` | linked or spawned plugins not to load |
| `plugins.<name>` | table | `{}` | handed to the plugin verbatim on `plugin.init` |
| `skills.dirs` | [path] | `["~/.agents/skills", ".agents/skills"]` | relative paths resolve against the workspace |
| `memory.dir` | path | `~/.agents/memory` | passed to the memory SDK |
| `memory.enabled` | bool | true | |
| `memory.summary_model` | string | `` | model spec for fold summaries; empty means the session's model |
| `memory.fold.<key>` | int | the SDK's own | `observe_after_tokens`, `reflect_after_tokens`, `observations_max_tokens`, `observations_target_tokens`, `observer_max_tokens`; a missing key takes the SDK default |
| `skills.migrate_from` | [path] | `["~/.claude/skills", "~/.pi/agent/skills"]` | roots `rudy skills migrate` copies from, one directory per skill |
| `mcp.connect_timeout_ms` | int | 10000 | per server at boot |

### themes/<name>.toml

Roles, every one required in a theme file or the built-in default applies: `accent`, `text`, `muted`, `user`, `assistant`, `tool`, `success`, `error`, `warning`, `diff_add`, `diff_del`, `shell`, `status`, `spinner`, `code`. A value is a hex color, a role name, or for `code` a `chroma:<style>` name. No role paints a background.

### plugins/<name>/plugin.toml

| key | type | default | meaning |
|---|---|---|---|
| `name` | string | required | equals the directory name |
| `version` | string | required | |
| `protocol_version` | int | required | must equal the server's |
| `command` | string | required | executable |
| `build` | string | `` | a command run once in the checkout after staging and before the manifest is accepted, so a plugin can be cloned and built. It runs arbitrary code, which is what installing from a source means |
| `args` | [string] | `[]` | |
| `env` | table | `{}` | added to the child environment |
| `description` | string | `` | shown in `/plugins` |

### mcp.toml

One table per server. `rudy mcp add` writes the user file or, with `--scope project`, the workspace file; a name present in both takes the project entry. Written whole, never appended.

| key | type | default | meaning |
|---|---|---|---|
| `servers.<name>.transport` | `stdio`, `http` | required | |
| `servers.<name>.command` | string | required for `stdio` | executable |
| `servers.<name>.args` | [string] | `[]` | |
| `servers.<name>.env` | table | `{}` | added to the child environment |
| `servers.<name>.url` | string | required for `http` | streamable HTTP endpoint |
| `servers.<name>.headers` | table | `{}` | sent on every request |

```toml
[servers.github]
transport = "stdio"
command = "npx"
args = ["-y", "@modelcontextprotocol/server-github"]
env = { GITHUB_TOKEN = "cache:GITHUB_TOKEN" }
```

Values in `env` and `headers` are secret references resolved like `providers.<name>.auth`: `env:NAME`, `cache:KEY`, or a literal.

### plugins.lock.toml

| key | type | default | meaning |
|---|---|---|---|
| `plugins.<name>.source` | string | required | the git URL or absolute path `rudy plugins install` was given |
| `plugins.<name>.commit` | string | required | the checked-out commit; empty for a path source that is not a repository |
| `plugins.<name>.installed_at` | rfc3339 | required | |
| `plugins.<name>.enabled` | bool | true | `rudy plugins disable` sets false; a disabled plugin is not spawned |

### agents/<name>.md

YAML frontmatter then the system prompt body. A plugin contributes the same fields through `plugin.register_agent` without a file; `resolveAgent` reads the user's root, then the workspace's, then the plugins, and the first definition of a name wins, so an operator's file always beats a plugin's registration.

| key | type | default | meaning |
|---|---|---|---|
| `name` | string | file stem | |
| `description` | string | required | |
| `tools` | [string] | all | tool names exposed; empty list means none. A ceiling, not a grant: the session gets these intersected with the caller's `tools` and with its parent's set, so naming a tool here never adds one the parent withheld |
| `model` | string | inherit | `provider:id`, a bare id unique across providers, or empty to inherit the parent's |
| `thinking` | ThinkingLevel | inherit | |
| `max_turns` | int | 0 | zero means unlimited |

### SKILL.md

The agentskills format as published: frontmatter `name`, `description`, optional `allowed-tools`; body is the skill. rudy reads and never writes these.

## Cross-check

Every transition traced through protocol, event and record, else a recorded reason.

| transition | protocol | event | record |
|---|---|---|---|
| Session.Open | `session.open` | `SessionOpened` | `session_opened` |
| Session.Open, child | `session.open` with `parent` from a plugin | `SessionOpened`; the subagent runner consumes `TurnCompleted` and `TurnFailed` of the child | `session_opened` with `parent_session_id` and `parent_tool_use_id` |
| Session.Fork | `session.fork` | `ForkPointRecorded` | `fork_point` |
| Session.Load and Recovery | `session.resume` | none; recovery appends through `Append` | `tool_result` outcome `lost` |
| Session.SetModel | `session.set_model` | `ModelChanged` | `model_change` |
| Session.SetThinking | `session.set_thinking` | `ThinkingChanged` | `thinking_change` |
| Session.SetMode | `session.set_mode` | `ModeChanged` | `mode_change` |
| Session.SetTitle | `session.set_title` | `TitleChanged` | `title_change` |
| Session.Enqueue | pass 3: not yet implemented; `session.submit` refuses a queued source with `invalid_argument` and there is no queue behind it | none until dequeued; queued messages would be in memory only, returned to the client on cancel | none; a queued message becomes a `user_message` only when it starts a turn |
| Session.Append(note) | `plugin.append_note` | `NoteAppended` | `note` |
| Turn.Start | `session.submit` source typed | `UserMessageAppended`, `TurnStarted` | `user_message` |
| Turn.OnResponse | none; provider stream | `AssistantMessageAppended` | `assistant_message` |
| Gate decide | `session.answer` when asked | `ToolRequested`, `PermissionRequested`, `PermissionDecided` | `permission_decision` |
| Turn.RunTool | `tool.invoke` to plugin, `tool.state` to clients | `ToolStarted` | none until finished; the decision row precedes the run. Every call of one assistant message runs at once, so the `tool_result` rows of a turn may be in a different order from its `tool_use` blocks; both codecs pair them by id, and the invariant below is per `tool_use`, not positional. The results of one assistant message are assembled into a single message carrying every block, not one message per result: Anthropic's parallel tool use requires that shape, and splitting them is accepted on the wire but teaches the model to stop calling tools in parallel, which is the behaviour this wave exists to enable |
| Turn.OnToolResult | none | `ToolFinished`, `ToolResultAppended` | `tool_result` |
| Turn.Steer | `session.interrupt` how steer | `TurnSteering` | partial `assistant_message` stop_reason `interrupted`, or `tool_result` outcome `killed` |
| Turn.Resume | `session.submit` source steer | `UserMessageAppended`, `TurnResumed` | `user_message` source steer, then `turn_interrupted` how steer |
| Turn.Cancel | `session.interrupt` how cancel | `TurnCancelled` | `turn_interrupted` how cancel |
| Turn.Complete | none | `TurnCompleted` | none beyond the final `assistant_message`; completion is derived from `stop_reason` |
| Turn.Fail | none | `TurnFailedEvent`, `TurnFailed` | `turn_failed` |
| Compactor | `session.compact`, implicit on threshold | `CompactionRequested`, `CompactionRecorded` | `compaction` |
| MCP server connect | none; boot | none; a failure is a `notice` | `mcp.toml` read, nothing written |
| Plugin install, enable, disable, update | `rudy plugin` CLI, not protocol | none | `plugins.lock.toml`, the checkout |
| Plugin.Load, Ready, Fail, Stop | `plugin.init`; state via `plugin.state` | `PluginLoading`, `PluginReady`, `PluginFailed`, `PluginStopped` | none; plugin state is runtime, rebuilt at boot from config |
| PluginRegistry.Register* | `plugin.register_*`, `plugin.set_status` | `CapabilityRegistered`, `CapabilityRejected` | none; capabilities are runtime |
| Registry.Refresh | `registry.refresh`, implicit on open | `RegistryRefreshed` | `registry.json` |
| session close | `session.close` | `session_closed` hook | none; closing is a connection fact, not a conversation fact |

Invariants and where they are enforced:

| invariant | aggregate | record layer |
|---|---|---|
| unsafe tool runs only after a durable allow | Gate then `Session.Append` with fsync | line order plus fsync; `Load` checks order |
| one `tool_result` per `tool_use` | `Turn.OnToolResult` | Recovery writes `lost` |
| `fork_point` only first | `Session.Fork` | `Load` refuses otherwise |
| ids monotonic | `Session.Append` | `Load` refuses otherwise |
| steer only in steering state | `Turn.Resume` | `Load` checks the preceding entry |
| one process writes a session | lock file | `flock` on `lock` |
| tool names unique | `PluginRegistry.RegisterTool` | none; runtime |
| a session's tool set never exceeds its parent's | `Session.Open` intersects definition, `tools` and parent before stamping the view | the resolved set is written on `session_opened` at `schema_version` 2 and replayed on resume, with a version 1 log falling back to the definition's list. It is recorded rather than recomputed because a child is cold by the time anyone resumes it, its parent may be gone, and recomputing from the definition alone hands back exactly what the intersection removed. What a session ran under is a fact about that session, so the log is where it belongs |
| a call never runs on consent given for another call's arguments | the asker coalesces only calls whose matcher and input bytes are both equal; a `session` answer resolves parked questions its allowance covers, never one the Gate marked dangerous, which keeps ADR 0011's ordering that puts the dangerous set ahead of allowances | one `permission_decision` per `tool_use`, so each call records its own decision row citing the answer that bound it |
| an interrupted turn records no call as succeeding | every in-flight call observes the interrupt rather than one consuming it | `tool_result` outcome `killed` for each call that was running |

## Completeness gate

- [x] every request has every field
- [x] every notification has payload and delivery
- [x] every event has all eight fields
- [x] every record kind has every field with nullability stated; `session_opened.tools` is the only nullable one, because `null` there means every tool and an empty list means none, and collapsing the two would make an unrestricted session indistinguishable from a fully restricted one
- [x] every caller class reaches at least one method
- [x] no method admits a class not enumerated
- [x] every transition traces through all three or carries a reason
- [ ] `before_request` exposes headers only; body mutation is deferred and marked open
- [ ] the dangerous set is not enumerated
- [x] pass 3: child sessions, `session.compact`, `before_compaction`, `mcp.toml`, `plugins.lock.toml` trace through all three

## Open

- the contents of `permissions.dangerous`; proposed shape is `tool` or `tool:prefix`, contents undecided
- whether `before_request` may mutate the request body, or only headers, in pass 2
- paging for `session.resume` on very large logs; pass 1 returns every entry
- whether `session` scope allowances should survive a fork
- the exact prefix rule the Gate uses to build `matcher.prefix` for bash; leading words up to the first operator is the candidate
- live verification of `anthropic_messages`: no configured endpoint serves the route; the codec is proven against recorded fixtures until one does
- the subagent model ladder from registry prices; pass 3 takes the model from the agent definition or inherits
- concurrent tool calls have no cap: a model asking for fifty gets fifty. Whether that needs a bound, and whether the bound belongs to the scheduler or to a tool that knows its own cost, is undecided
- a `tools` narrowing that names a tool the caller does not hold is dropped silently, so an orchestrator learns what its child actually got only from what the child says. Whether `SessionInfo` should report the resolved set is undecided
- whether a tool should be able to declare that it must not run beside another call of itself, so the scheduler serializes it rather than the tool locking internally. Pass 5 puts the obligation on the tool, which is the smaller change and keeps the scheduler free of a second classification
- the socket path on macOS when neither `XDG_RUNTIME_DIR` nor `TMPDIR` is set
- how a spawned provider plugin authenticates to its upstream; pass 1 leaves it to the plugin's own config table
- image content from an MCP tool result is rendered as a text placeholder, `[image <media_type>, <n> bytes]`; the bytes should go to the session's blob store and come back as an `image` content block
- MCP servers are per process: the plugin loads `mcp.toml` once at boot with the project scope of the workspace rudy was started in. Per-session project servers wait for `rudy serve` holding many workspaces at once
- a spawned plugin may register only `wire: custom` providers; `openai_chat` and `anthropic_messages` from a spawned plugin are refused, so a spawned plugin cannot yet stand up an endpoint of its own with config
- `tool.progress` from a spawned plugin is received by the adapter and dropped; forwarding it to clients needs the stream part type the TUI plan defines
- `plugin.append_note` is not in the own-session set, so a plugin may append a note to any live session, not only the ones it opened. Whether that is the contract or an omission is undecided; the note carries the plugin's name either way
