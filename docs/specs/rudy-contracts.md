# rudy contracts

Pass 10, 2026-09-26: Codex App Server joins as an AgentRuntime (ADR 0045,
revised by ADR 0046 for process recovery).
Runtime sessions keep canonical conversation history outside Rudy, project it
for clients and retain only local control and approval evidence. The Pass 10
rows below replace same-method native assumptions only when
`SessionExecution.kind` is `runtime`; unchanged rows remain normative for
native sessions. Written before code.

## Pass 10: AgentRuntime contract

### Runtime protocol types

| type | shape |
|---|---|
| `SessionExecution` | one of `{kind:"native", provider:string}` or `{kind:"runtime", runtime:string}`; the selected model owner fixes it at session open |
| `SessionInfo` | `{session_id, workspace:Workspace, model:ModelRef, mode:PermissionMode, thinking:ThinkingLevel, title:string, execution:SessionExecution, thread_linked:bool}`; `thread_linked` is false for native and for a runtime session before its first successful thread start |
| `AccountState` | `{runtime, authenticated:bool, auth_mode:string, plan_type:string}`; the two strings are empty when unauthenticated and no credential field exists |
| `AuthChallenge` | one of `{type:"browser", runtime, login_id, url}` or `{type:"device", runtime, login_id, verification_url, user_code}`; caller-private and never logged |
| `RuntimeThread` | `{runtime, thread_id, turns:[RuntimeTurn]}`; returned history is authoritative and contains no Rudy session id |
| `RuntimeTurn` | `{turn_id, status, items:[RuntimeItem], usage:Usage}`; `status` is `running`, `completed`, `interrupted` or `failed` |
| `RuntimeItem` | `{item_id, type, status, content, command, cwd, output, changes, error}`; every field is present, unused strings and lists are empty, `content` is `[ContentBlock]`, `changes` is `[{path, kind}]`; supported types are `user_message`, `agent_message`, `reasoning`, `command`, `file_change`, `tool`, `plan`, `diff`, `warning`, `error` |
| `ProjectedEntry` | `{id, at, kind, runtime, thread_id, turn_id, item_id, content, status, usage}`; `id` is a deterministic ULID-shaped digest, `at` is the runtime time or the projection time when absent, unused content is empty; derived and never an `entries.jsonl` record |
| `RuntimeEvent` | one of `thread_started`, `thread_status`, `turn_started`, `turn_completed`, `runtime_failed`, `item_started`, `item_delta`, `item_completed`, `diff_updated`, `plan_updated`, `usage_updated`, `warning`, `error`, `request_resolved`; each carries `thread_id`, an empty or non-empty `turn_id`, an empty or non-empty `item_id`, `request_id` empty except `request_resolved`, adapter-assigned receive `sequence`, `item:RuntimeItem`, `text`, `status`, `usage`; `runtime_failed` names the active thread and turn with `status:"failed"`; fields not used by the variant are the stated empty value |
| `RuntimeApprovalQuestion` | `{runtime, request_id, thread_id, turn_id, item_id, kind, summary, command, cwd, reason, changes:[{path,kind}], network:[{host,protocol,port}], permissions:[string], allowed_scopes:[Scope]}`; `kind` is `command`, `file_change` or `permissions`; display fields may be empty and confer no authority |
| `RuntimeApprovalAnswer` | `{decision, scope, reason, granted:[string]}`; `decision` is `allow` or `deny`; allow scope must be in `allowed_scopes`, deny always uses `once`; `granted` is used only for a permissions allow and is a subset of the requested permissions |

### Runtime session behavior

| method | runtime behavior | additional errors | record |
|---|---|---|---|
| `session.open` | resolves the selected model's registry owner and writes `SessionExecution{runtime}`. A configured runtime model may open before login or discovery. It creates no thread. Runtime execution accepts only a root Session under the default agent with absent `tools` narrowing | `unavailable` only when the named runtime is not registered; absence of an HTTP Provider is not an error; `refused_by_invariant` for `parent`, a non-default agent or explicit `tools`, since runtime-owned tools cannot honor Rudy delegation constraints | `session_opened` schema 3 |
| `session.resume` | loads local control records, resumes a linked runtime thread, reads authoritative history and sends `runtime.entry` projections before the response. An unlinked pre-login session has no runtime history | `runtime_error` when the link exists but cannot be resumed or read | no conversation write |
| `session.fork` | requires a linked rested thread and `at_entry_id` empty or equal to the newest projected id; calls runtime fork, reads and validates the distinct child thread and projects its canonical history before binding it | `refused_by_invariant` for an older projection, active turn or reused thread id; `ambiguous` on lost fork response; `runtime_error` if the child cannot be read faithfully | child `session_opened`, `fork_point`, `runtime.toml` and replayed child projection |
| `session.submit` | on the first submit, starts and durably binds a thread before starting a turn; later typed input starts a turn and steer input steers the verified active runtime turn; a process-failed session must resume and read its canonical thread first | `runtime_error` unauthenticated or runtime failure; `ambiguous` on lost non-idempotent response or while canonical reconciliation is required, with no automatic retry | no user or assistant conversation entry |
| `session.interrupt` | requests runtime turn interruption. The response is acknowledgment; terminal state waits for `turn_completed`. `how:steer` enters client steering and the next submit calls runtime steer only while that same runtime turn remains active; `how:cancel` does not accept later steer | `conflict` when expected runtime turn changed | no conversation write |
| `session.answer` | native questions only. A runtime question answered here is `not_found` | none | `permission_decision` only |
| `runtime.approval.answer` | verifies asker, session, active turn and pending request binding, appends the runtime decision, fsyncs an allow, then answers the owning runtime | `unauthorized`, `not_found` or `conflict` on the same rules as `session.answer`, plus `conflict` for any binding mismatch | `runtime_permission_decision` |
| `session.set_model` | permits only another model owned by the same AgentRuntime and applies it to the next turn. Changing execution kind is refused | `refused_by_invariant` for a different owner or owner kind | `model_change` |
| `session.set_mode`, `session.set_thinking`, `session.set_title` | append local control facts and apply mode and effort to the next runtime turn | unchanged | corresponding existing entry |
| `session.compact` | refused because runtime history is canonical and App Server exposes no matching operation | `refused_by_invariant` | none |
| `session.shell` | refused because a local shell result cannot be inserted into canonical runtime history without starting a model turn | `refused_by_invariant` | none |
| `session.close` | detaches and closes live projection resources; does not delete or mutate the runtime thread | runtime process cleanup errors become notices | none |

Native-only hook points `session_opened`, `before_turn`, `before_request`,
`after_response`, `before_tool`, `after_tool` and `before_compaction` do not fire
for runtime-owned execution because their returns cannot be applied faithfully
to canonical runtime history. `turn_completed` and `session_closed` still fire
with domain payloads. Runtime approval translation is not a Tool hook.

### Runtime plugin requests

The linked Go interface has the same operations and domain shapes. For a
spawned runtime the parent calls these methods on the plugin connection. Every
row's callee is the plugin that registered `request.runtime`, authentication is
the parent-child stdio connection, authorization requires that exact registered
name and the domain operation is the same-named AgentRuntime behavior. A name
owned by another plugin is `unauthorized` before the call.

| method | request | response | errors | idempotency |
|---|---|---|---|---|
| `runtime.account.read` | `{runtime}` | `AccountState` | `runtime_error`, `unavailable` | idempotent |
| `runtime.login.start` | `{runtime, mode}` where mode is `browser` or `device` | `AuthChallenge` | `runtime_error`, `unavailable` | not idempotent; completion may race the response and is matched by `login_id` |
| `runtime.login.cancel` | `{runtime, login_id}` | `{}` | `not_found`, `runtime_error` | idempotent |
| `runtime.model.list` | `{runtime, cursor}`; empty cursor starts | `{models:[Model], next_cursor}`; empty next cursor ends | `runtime_error`, `unavailable` | idempotent per cursor |
| `runtime.thread.start` | `{runtime, session_id, cwd, model, thinking, mode}` | `{thread_id}` | `runtime_error`, `unavailable`, `ambiguous` | not idempotent and never retried after ambiguity |
| `runtime.thread.resume` | `{runtime, session_id, thread_id}` | `{}` | `not_found`, `runtime_error` | idempotent |
| `runtime.thread.fork` | `{runtime, session_id, thread_id}` | `{thread_id}` for a distinct child | `not_found`, `runtime_error`, `ambiguous` | not idempotent and never retried after ambiguity |
| `runtime.thread.read` | `{runtime, session_id, thread_id}` | `RuntimeThread` | `not_found`, `runtime_error` | idempotent |
| `runtime.turn.start` | `{runtime, session_id, thread_id, content:[ContentBlock], model, thinking, mode}` | `{turn_id}` | `runtime_error`, `ambiguous` | not idempotent and never retried after ambiguity |
| `runtime.turn.steer` | `{runtime, session_id, thread_id, turn_id, content:[ContentBlock]}` | `{turn_id}` equal to the expected turn | `conflict`, `runtime_error`, `ambiguous` | not idempotent and never retried after ambiguity |
| `runtime.turn.interrupt` | `{runtime, session_id, thread_id, turn_id}` | `{}` acknowledgment only | `not_found`, `runtime_error` | idempotent request; terminal proof is a turn event |

The runtime plugin calls the fully specified `plugin.register_runtime` and
`runtime.approval.request` server methods in the client-to-server table below.

The runtime plugin sends `runtime.event` with `{runtime, event:RuntimeEvent}`.
The server rejects an event whose thread, turn or item binding does not match
its verified state. Delivery is ordered per runtime connection. Repeated final
events are de-duplicated by their binding and projection id.

### Runtime client notifications

| notification | to | payload | delivery |
|---|---|---|---|
| `runtime.entry` | clients attached to the bound session and the parent's eligible watchers | `{session_id, entry:ProjectedEntry}` | authoritative history before `session.resume` response, then completed live items; reconnect de-duplicates by deterministic id |
| `runtime.delta` | clients attached to the bound session and the parent's eligible watchers | `{session_id, turn_id, item_id, kind, text, replace}` | live only; `replace: false` appends a fragment, `replace: true` replaces the item's live text and `runtime.entry` supersedes either form for the completed item |
| `runtime.usage.updated` | clients attached to the bound session | `{session_id, turn_id, usage:Usage}` | latest cumulative usage for the runtime turn; live only and replaces the prior value for that turn |
| `runtime.permission.requested` | attached asker clients | `{session_id, turn_id, request_id, item_id, kind, summary, command, cwd, reason, changes, network, permissions, allowed_scopes}` | every current asker and an asker attaching while pending; first `runtime.approval.answer` wins; no asker denies immediately |
| `runtime.permission.resolved` | clients attached to the bound session | `{session_id, request_id}` | sent after `serverRequest/resolved` or terminal turn completion; clears the pending approval UI |
| `runtime.account.updated` | every client | `AccountState` | latest wins; sent on connect and after verified `account/read` |
| `runtime.login.challenge` | invoking connection only | `AuthChallenge` | live only; used when browser fallback creates a device attempt |
| `runtime.login.completed` | invoking connection only | `{runtime, login_id, success, error}` | exactly once for the attempt; `error` is redacted and empty on success |

Initial `/login` returns its AuthChallenge in `command.run`. Challenge and
completion never broadcast, enter transcript projection or reach logs. Client
disconnect cancels its live LoginAttempt. A local TUI requests browser mode; a
headless or SSH client requests device mode because the server cannot infer SSH
after `rudy bridge` terminates it.
The headless client keeps the connection open after printing the device code
until its matching completion or interruption, so disconnect does not cancel
an unredeemed challenge. A later challenge for the same runtime supersedes the
prior id in the TUI; a late browser-opener result for that id is ignored.

The visible command is `/login`; the client sends hidden args `browser` or
`device` from its own transport. A user-supplied arg outside those values is
`invalid_argument`. Headless and remote clients replace even an explicit
`browser` arg with `device` and never invoke a local opener for a browser
challenge. Starting a new mode cancels the invoking connection's prior
attempt first. Browser completion failure starts device mode and emits
`runtime.login.challenge`. A platform opener accepts only an `https` URL whose
lowercased host is `openai.com`, `chatgpt.com` or a dot-delimited subdomain of
one of those suffixes, passes the URL as one argv element and never invokes a
shell. An opener failure immediately runs `/login device`, which cancels the
browser attempt before returning the new challenge.

For `rudy --print /login`, text output writes the device verification URL and
code. JSON output keeps the ordinary no-turn result and adds optional
`auth_challenge:AuthChallenge`. Stream JSON writes one
`{"method":"runtime.login.challenge","params":AuthChallenge}` object. These
outputs contain the live challenge but never persist it.
The headless command exits successfully only on matching successful completion.

Rudy fences each in-flight runtime `thread/read` with a local session revision.
Live events received during the read win on matching projected ids and remain if
absent from the snapshot; the older snapshot cannot roll back active turn state.
Codex model image-input metadata does not set Rudy's `vision` capability while
the runtime submit path accepts text only.

### Codex App Server ACL

The built-in `codex` runtime translates the generic operations above to App
Server. It sends `initialize` then `initialized`, pages `model/list`, uses
`account/read`, `account/login/start`, `account/login/cancel`, `thread/start`,
`thread/resume`, `thread/fork`, `thread/read`, `turn/start`, `turn/steer` and
`turn/interrupt`, then translates documented events and inbound requests.

Rudy sends App Server approval policy `untrusted`, `on-request` and `never` for
permission mode `strict`, `permissive` and `off`. Strict sends sandbox mode
`read-only` on `thread/start` and sandbox policy
`{type:"readOnly",networkAccess:false}` on every `turn/start`. Permissive and
off do not override the operator's App Server sandbox configuration.
Thinking levels target `minimal`, `low`, `medium` and `high` in that order;
`off` targets `minimal`. If the target is absent, choose the nearest advertised
lower effort, or the lowest advertised effort when none is lower. Rudy never
selects `xhigh` for a `high` setting.

It keys login completion by `loginId`, pending approvals by the App Server
JSON-RPC request id and turn routing by verified `threadId` plus `turnId`.
Unknown inbound requests receive an immediate method error. The supported
`item/tool/requestUserInput` request receives `{answers:{}}`; the supported
`item/tool/call` request receives `{contentItems:[],success:false}`. These
schema-valid cancellations expose neither user input nor dynamic tool execution
and do not call Rudy's approval asker. MCP elicitation is unsupported.
Command and file requests deny on failure. Permission requests grant an empty
set on failure. Pending approval UI clears only on request-resolved or terminal
turn completion. The prompt renders every opaque requested permission member
before an allow key can grant it.

Pass 9, 2026-09-14: daemon shutdown completion gains explicit terminal proof (ADR 0031). After successful cleanup the retained control connection receives `server.stopped {instance_id, state: "stopped"}` before EOF. Bare EOF means crash, transport loss or failed cleanup and never authorizes replacement. A tentative shutdown claim fences work without moving public Server state. Recorded with the security hardening before final verification.

Pass 8, 2026-09-14: protocol-owned Server shutdown replaces pid signaling for daemon upgrades (ADR 0030). `server.shutdown` is confined to greeted non-plugin connections whose same-user identity the unix listener proved. Its response is flushed before cleanup begins, its connection closes only after cleanup completes, and `client.hello.instance_id` proves replacement without a process id. `rudy bridge --stop` exposes the same path. No pid file remains. Written before the code.

Pass 7, 2026-09-13: the remote runtime (ADR 0029). ssh joins the transports as a fourth row, carried by `rudy bridge` on the host; `client.hello` returns `home` so the client can place the workspace; `remote.host`, `remote.source` and `ui.status.host` join the config keys. The kernel does not change. Written before the code. Pass 6, 2026-09-11: the plugin install wave (ADR 0025). `plugins.lock.toml` gains `kind`, `ref` and `digest` so an install is reproducible per source kind; the source vocabulary is `go:`, `git:`, `https://` and a path; the manifest's `build` row, normative since pass 5 but never run, is implemented at install and update; `rudy install` is the top-level spelling. The trust model was already implemented and documented before this pass. Written before the code. Pass 5, 2026-09-10: the subagents wave (ADR 0028). Tool calls run concurrently, so the Gate coalesces asks, a Tool scheduler joins the domain services and `tool.state` joins the notifications; a session's tool set becomes one narrowing chain that a caller may only shrink, which closes a child's view escaping its parent's; `plugin.register_agent` lets a plugin contribute an agent definition; a child's notifications reach its parent's subscribers without conferring authority over the child. Written before the code, as the rules require. Pass 4, 2026-09-09: the socket transport, attach and the asker rules (ADR 0014). Pass 4 implemented 2026-09-09; its rows were walked against the code and one was corrected: `server.socket` was listed as a config key and is not one, since ADR 0014 makes the socket `Paths.Socket()` with `--socket` overriding it. Pass 3, 2026-09-08. Pass 3 implemented 2026-09-09; the rows below were walked against the code and corrected where they differed. Pass 2 aligned the protocol section with the kernel implementation; pass 3 adds the plugins wave (ADR 0012): child sessions for subagents, `session.compact` and the ninth hook point `before_compaction`, the clinepass dialect key, the memory summary model, skill migration sources, and the `mcp.toml` and `plugins.lock.toml` records. Companion to [rudy-domain-model.md](rudy-domain-model.md) and [rudy-context-map.md](rudy-context-map.md). Three contracts: the protocol, the domain events and the record layer. A transition that appears in one and not the others is listed in the cross-check with a reason.

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
| `runtime_error` | -32010 | an AgentRuntime returned a redacted error; `data.runtime` |
| `ambiguous` | -32011 | a non-idempotent runtime request lost its response and may have committed; it was not replayed |

## Common types

| type | shape |
|---|---|
| `ModelRef` | `{provider: string, model: string}` both non-empty |
| `Workspace` | `{root: string, git_root: string, project_id: string}`; `git_root` empty means not a git repo |
| `ContentBlock` | one of `{type:"text", text}`, `{type:"image", media_type, sha256}` where `sha256` is the hex digest naming `blobs/<sha256>` in the session dir, stored byte-exact; inline image bytes never appear in the log, `{type:"thinking", text, signature}` where `signature` is the provider's opaque bytes verbatim, `{type:"tool_use", id, name, input}` where `input` is raw JSON bytes verbatim |
| `Span` | `{text: string, role: string}`; `role` is a theme role name |
| `Usage` | `{input: int, output: int, cache_read: int, cache_write: int}`; the input buckets are disjoint: `input` is uncached prompt tokens, so `input + cache_read + cache_write` is the full prompt |
| `Entry` | `{id: ulid, at: rfc3339nano, kind: EntryKind, ...payload}`; payloads in the record layer |
| `Model` | `{provider, owner_kind, id, display_name, upstream, context_window: int, max_output: int, reasoning_efforts:[string], pricing}`; `provider` is the stable model namespace and may name a Provider or AgentRuntime, `owner_kind` says which; `upstream` is absent when none; numeric zero means unknown; `reasoning_efforts` is empty when unreported; `pricing` is absent when no source supplied it |
| `SessionSummary` | `{id, opened_at, workspace, model, execution, thread_linked, forked, parent_session_id, last_entry_at, title, entry_count}`; `parent_session_id` empty means root, `forked` is true when the session began as a fork; runtime projections do not contribute to `entry_count`; `last_entry_at`, `title` and `entry_count` are pass 3: not yet reported |
| `PermissionMode` | `strict`, `permissive`, `off` |
| `ThinkingLevel` | `off`, `low`, `medium`, `high` |
| `TurnState` | `idle`, `streaming`, `running_tool`, `awaiting_permission`, `steering`, `completed`, `failed` |
| `ServerState` | `running`, `shutting_down`, `stopped` |
| `HookPoint` | `session_opened`, `before_turn`, `before_request`, `after_response`, `before_tool`, `after_tool`, `before_compaction`, `turn_completed`, `session_closed` |
| `StreamPart` | `{type: text_delta, thinking_delta, thinking_signature, tool_use_start, tool_use_delta, tool_use_end, usage or stop, text, id, name, signature, usage, stop_reason, stop_reason_raw}`; one streamed piece of a completion. `text` carries the fragment for `text_delta`, `thinking_delta` and `tool_use_delta`; `id` and `name` the tool use; `signature` the provider's verbatim bytes on `thinking_signature`; `usage`, `stop_reason` and `stop_reason_raw` are set on `usage` and `stop` |
| `ParentRef` | `{session_id: ulid, tool_use_id: string}`; the session and the `tool_use` that spawned a child session |
| `Safety` | `safe`, `unsafe` |

For native execution, `turn_id` is the entry id of the `user_message` that
started the turn. For runtime execution it is the AgentRuntime turn id and is
never presented as a stored Entry id.

## 1. Protocol

JSON-RPC 2.0. Requests carry `id`; notifications do not. Both peers may send requests. Four transports:

| transport | who | framing |
|---|---|---|
| in-memory | embedded server inside the `rudy` process; the TUI and linked plugins | Go channels; JSON-RPC envelope kept so the same handlers serve every transport |
| unix socket | `rudy serve` and any client attaching to it | `$XDG_RUNTIME_DIR/rudy/rudy.sock`, or `$TMPDIR/rudy-<uid>/rudy.sock` when `XDG_RUNTIME_DIR` is unset, `--socket` overriding both; directory `0700`, socket `0600`; the server closes a connection whose peer uid is not its own before reading a byte; an accepted same-uid connection carries an unforgeable in-process same-user marker minted by the listener, never by `client.hello`; a socket file that refuses connections is stale and `rudy serve` replaces it; newline-delimited JSON. A client dials an explicit `--socket` or fails, else probes the default with a 50ms timeout and embeds when nothing answers; `--embed` skips the probe; before it connects, the client refuses a socket or a socket directory that is not owned by its uid, is reached through a symlink, or sits in a directory group or other can write, and refuses rather than embedding; its hello is bounded at 2s; `rudy serve` checks an existing socket directory against the same rule and never narrows one it did not create (ADR 0014, ADR 0030) |
| stdio | spawned plugins; the server is the parent | newline-delimited JSON on the child's stdin and stdout; stderr is captured into the log |
| ssh | a client on another machine reaching `rudy serve` on the host named by `--host` or `remote.host` | the client runs `ssh -- <host> '<PATH prefix>; command -v rudy >/dev/null 2>&1 \|\| exit 111; exec rudy bridge'` and speaks newline-delimited JSON on ssh's stdin and stdout; `rudy bridge` on the host dials the host's default socket under the unix socket rules above, starts `rudy serve` detached when nothing answers (its own session, stdio on `log.file`) and joins it, and copies messages both ways; `--no-start` makes a socket nothing answers exit 1 rather than start one; `--stop` dials without starting, greets, requests `server.shutdown`, requires matching `server.stopped` followed by EOF after full cleanup, and succeeds when no daemon answers; the bridge holds the daemon it started as a child, so a daemon that exits instead of serving is exit 1 naming the exit status and the last 20 lines of `log.file` rather than a wait for the start budget or a stream that closed unexplained; `rudy serve` exits 3 for a socket another server already holds, which is the one exit that means another daemon won rather than that none is coming, so the bridge keeps dialing for the winner until the budget ends and never reports it to a client; once a connection is established no child exit ends it, and a daemon-side close consults the child's exit only to name a reason; exit 111 means rudy is not on the host's PATH and the client installs it once (ADR 0029 decision 4) before retrying; the hello is bounded at 30s since the bridge may be starting a daemon; `--host` with `--socket` or `--embed` is exit 2 and `--host` never falls back to a local server; `RUDY_SSH` names the ssh binary for tests (ADR 0029, ADR 0030, ADR 0031) |

### Caller classes

| class | transport | authentication | may assert | must never assert |
|---|---|---|---|---|
| TUI client | in-memory, unix socket | in-memory: trusted by construction; socket: directory `0700`, socket `0600`, peer uid equals server uid via `LOCAL_PEERCRED` or `SO_PEERCRED` | user messages, permission answers, model, mode, thinking, title, attach and detach, slash commands | entries of any other kind, tool results, registrations, registry contents |
| headless client | in-memory, unix socket | as TUI client | user messages, commands, attach without asker | permission answers; it declares `asker: false` in hello and the Gate treats it as absent |
| spawned plugin | stdio | spawned by the server from a manifest the user placed in config; identity is the manifest name | registrations under its own name, results for its own tools, hook returns, notes, status and widgets under its own name, child sessions it opens, runtime events and approval requests for a runtime it registered | user messages to sessions it did not open, permission answers, runtime assertions for another plugin's name, entries directly |
| linked plugin | in-memory Go interface | compiled in; trusted by build | as spawned plugin | as spawned plugin |
| ACP adapter | unix socket | as TUI client | as TUI client | as TUI client; deferred, not in v1 |

A port reached by two classes has one authentication story per class, listed above. The authz column below names the class-level rule.

Attachment is not a permission, deliberately. Apart from `session.submit` and
`session.interrupt` on a session whose log records a parent, a plain client may
name any live session by id and interrupt, compact, close, shell or change the
model on it without ever having attached to it; only the plugin class is checked
for ownership. The boundary is the socket, not the subscription: the directory is
`0700`, the socket `0600`, and the peer uid is proved on accept, so every caller
that reaches the port is already the user whose sessions these are, and
`session.shell` runs a command ungated on exactly that reasoning (ADR 0023). A
gate here would be a rail against a mistake rather than a boundary against an
attacker, and `session.resume` would have to be reconciled with it first, since
its authz is any session on this machine. Recorded rather than closed silently
(`rudy-jkz`); the README's trust model says the same thing to an operator.

### Requests, client to server

Authn column names the caller class table. Domain column names the aggregate method or says query.

| method | caller | authn | authz | request | response | errors | idempotency | domain |
|---|---|---|---|---|---|---|---|---|
| `client.hello` | TUI, headless, ACP, plugin | per class | first request on a connection; refused otherwise | `{client, version, asker: bool}` | `{server, version, home, instance_id}`; `home` is the server process's home directory, which a client over ssh uses to place the workspace (`<home>/<cwd relative to the local home>`) and a local client ignores; `instance_id` is the runtime-only Server ULID and lets a reconnect prove replacement; a client whose `version` differs from the server's carries on and shows a notice (ADR 0029, ADR 0030) | `invalid_argument` on protocol mismatch | idempotent per connection; a second hello is `refused_by_invariant` | registers the connection as an asker or not and identifies the Server. A plugin connection may send it (it needs no introduction, so it usually does not) and its `asker` is ignored: the plugin holding a child session is the one waiting on that child's tool call, so its own question must never route back to it |
| `server.shutdown` | TUI, headless, ACP | unix socket or ssh under the TUI class rule | greeted non-plugin connection carrying the same-user marker minted by the accepting unix listener; the `client.hello.client` string grants nothing | `{}` | `{instance_id, state: "shutting_down"}`; the response is physically written before shutdown is requested | `unauthorized` for a plugin, in-memory connection or connection without the marker; `refused_by_invariant` before hello or after shutdown was reserved | not idempotent on one Server; the first accepted request reserves shutdown and fences later work while public state remains `running`. A failed response releases the reservation. `rudy bridge --stop` treats no answering Server as an idempotent success at the CLI boundary | `Server.RequestShutdown`; the request's connection and writer remain open for terminal proof |
| `session.open` | TUI, headless, plugin | per class | any; `parent` only from a plugin, and only naming a `tool_use` pending in a live session whose tool that same plugin registered, in a session that is not itself a child, at most once per `tool_use`, and with `cwd` equal to that session's workspace root | `{cwd, model?: string, mode?: PermissionMode, thinking?: ThinkingLevel, agent?: string, tools?: [string], parent?: ParentRef}`; `model` is `provider:id` or a unique bare id; absent `model`, `mode`, `thinking` take the agent definition's values, then the parent's when `parent` is given, then config defaults; absent `agent` means the default agent. A session's tool set is the agent definition's list, intersected with `tools` when given, intersected with the parent's effective set when `parent` is given, minus `agent` for a child. Every term only removes: `tools` names what to keep and cannot name a tool the definition or the parent withheld, so delegation never widens what the caller holds (ADR 0028). Absent `tools` narrows nothing and an explicit empty list narrows to no tools at all, the same distinction the definition file carries. A name in `tools` that no plugin has registered is dropped before the set is persisted, so a session cannot acquire a tool later by having named one that did not exist when it opened | `SessionInfo`, same shape as `session.resume`; the `session_opened` entry is replayed first as `entry.appended`, this response returns once replay finishes | `invalid_argument` root not a directory, `parent` from a non-plugin, or `cwd` not the parent's workspace root; a name in `tools` that the definition or the parent did not hold is dropped rather than refused, since the set is an intersection and a caller asking for less than it is owed is not an error; `not_found` an explicitly named `model`, an unknown agent, parent session or parent tool_use; a model that came from config, an agent definition or the parent instead opens on the ModelRef as written and notices that it is not in the registry, since a provider retiring an id the operator configured must not stop the harness from starting; `unauthorized` the pending `tool_use` is not a tool of the calling plugin; `refused_by_invariant` the parent is itself a child session; `conflict` that `tool_use` has already opened a child; `unavailable` registry unreachable and no cached snapshot | not idempotent; every call opens a session | `Session.Open` factory; appends `session_opened` with `parent_session_id` and `parent_tool_use_id` |
| `session.resume` | TUI, headless, ACP | per class | any session on this machine | `{session_id}` | `SessionInfo`; native entries replay first as `entry.appended`, runtime history replays first as `runtime.entry`, then current turn and permission state, then the response | `not_found`; `unavailable` locked by another process, `data.socket`; `runtime_error` when canonical runtime history cannot be read | idempotent; re-attaches | `Session.Load` then attach; native Load runs Recovery, runtime Load resumes and reads canonical thread when linked |
| `session.fork` | TUI, headless, ACP | per class | any session | `{session_id, at_entry_id}`; `at_entry_id` empty means the newest entry | `SessionInfo` for the new session, same shape as `session.resume`; its entries are replayed first as `entry.appended`, this response returns once replay finishes | `not_found` session or entry | not idempotent | `Session.Fork(at)`; appends `fork_point` |
| `session.list` | TUI, headless, ACP | per class | any | `{}` no params | `{sessions: [SessionSummary]}` | none | idempotent | query over `session_opened` and last entries; no mutation |
| `session.close` | TUI, headless, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id}` | `{}` | `not_found` | idempotent | detach; fires `session_closed` hook when the last client detaches; appends nothing, except that a last subscriber leaving a turn parked in `TurnSteering` cancels it, which appends `turn_interrupted` |
| `session.submit` | TUI, headless, plugin | per class | plugin only to sessions it opened; a session whose log records a parent refuses any caller, plugin or not, that is not subscribed to it, whatever it may be subscribed to instead. The test is the recorded parent rather than the live one, so a child resumed cold is still a child (ADR 0028 decision 6: watching a session's parent is not being subscribed to it) | `{session_id, content: [ContentBlock text or image], source: typed or steer}`; `shell` is written by `session.shell`, never submitted; pass 3: not yet implemented, `queued` (refused as `invalid_argument`) and `idempotency_key` | `{turn_id}`; pass 3: not yet reported, `entry_id` and `queued_position`, neither of which exists without a queue | `refused_by_invariant` typed while a turn is active, steer while not steering; `invalid_argument` empty content, a content block the log refuses or a source that is neither typed nor steer; `unauthorized` a plugin submitting to a session it did not open, or any caller submitting to a child session it is not subscribed to | not idempotent; pass 3 ignores `idempotency_key` because it does not accept one | `Turn.Start` for typed on idle, `Turn.Resume` for steer; appends `user_message` |
| `session.shell` | TUI, headless | per class | any attached | `{session_id, command}` | `{entry_id, is_error}` | `invalid_argument` empty command; `not_found` unknown session or no `bash` tool in this session's view; `conflict` a turn is active | not idempotent | invokes the registered `bash` tool with the command, ungated, and appends one `user_message` with `source: shell` carrying `$ <command>` and its output. Starts no turn: the model reads it with the next message. The Gate does not run, since the operator typed the command themselves (ADR 0023) |
| `session.interrupt` | TUI, headless, ACP, plugin (own sessions) | per class | a plugin only a session it opened; a plain client is not checked for attachment at all, which is pre-existing and true of every session method but `submit` (`rudy-jkz`); a session whose log records a parent refuses any caller not subscribed to it, on the same rule and for the same reason as `session.submit`, because routing a child's notifications to its parent's client is what made a running child's id reachable | `{session_id, how: steer or cancel}` | `{turn_id, state: TurnState}` | `refused_by_invariant` no active turn; `unauthorized` a child session from a connection not subscribed to it | idempotent; repeating returns current state | `Turn.Steer` or `Turn.Cancel`; cancel appends `turn_interrupted` |
| `session.answer` | TUI, ACP | per class | connection declared `asker: true` | `{session_id, turn_id, tool_use_id, decision: allow or deny, scope: once or session, reason: string}`; `turn_id` is the turn the question was asked in, as `permission.requested` carried it | `{}` | `unauthorized` not an asker; `invalid_argument` no `turn_id`; `not_found` no pending request; `conflict` when another asker answered first, or when `turn_id` is not the active turn | keyed by the turn and the `tool_use_id` together, so consent given in one turn cannot decide a question in another that reuses the id (rudy-rn7); a second answer is `conflict` | `Turn.Answer` via the Gate; appends `permission_decision` |
| `runtime.approval.answer` | TUI, ACP | per class | connection declared `asker: true` | `{session_id, turn_id, request_id, decision: allow or deny, scope: once or session, reason: string, granted:[string]}` | `{}` after the runtime accepts the response | `unauthorized` not an asker; `not_found` no pending request; `conflict` another answer won, active binding changed, granted value was not requested or scope is session without explicit allow | keyed by session, turn and upstream request id; never replayed | AgentRuntime coordinator appends `runtime_permission_decision`, fsyncs allow, then answers RuntimeApproval |
| `session.set_model` | TUI, headless, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id, model: ModelRef}` | `{entry_id}` | `conflict` a turn is active; `not_found` model not in registry | same value appends nothing and returns the latest `model_change` id | `Session.SetModel`; appends `model_change` |
| `session.set_mode` | TUI, headless, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id, mode: PermissionMode}` | `{entry_id}` | `conflict` a turn is active; `invalid_argument` | same value appends nothing | `Session.SetMode`; appends `mode_change` |
| `session.set_thinking` | TUI, headless, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id, thinking: ThinkingLevel}` | `{entry_id}` | `conflict` a turn is active; `invalid_argument` | same value appends nothing | `Session.SetThinking`; appends `thinking_change` |
| `session.set_title` | TUI, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id, title: string}` | `{entry_id}` | `conflict` a turn is active; `invalid_argument` empty | same value appends nothing | `Session.SetTitle`; appends `title_change` |
| `session.compact` | TUI, headless, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id, instructions?: string}`; `instructions` steers the model's summary and skips the `before_compaction` hook when present | `{entry_id}` of the `compaction`; `{entry_id: ""}` when the request context holds fewer than two entries to cover | `conflict` a turn is active; `provider_error` the summary request failed; `unavailable` the session's provider is not loaded | not idempotent | Compactor; appends `compaction` |
| `registry.list` | TUI, headless, plugin, ACP | per class | any | `{provider?: string}` | `{fetched_at, models: [Model]}` | none | idempotent | query over the Registry snapshot |
| `registry.refresh` | TUI, headless, ACP | per class | any | `{provider?: string}` | `{fetched_at, models: [Model], failures: [{provider, error}]}` | none; a failing provider lands in `failures` and the rest succeed | idempotent | `Registry.Refresh` |
| `command.list` | TUI, headless, ACP | per class | any | `{}` | `{commands: [{name, description}]}` in registration order | none | idempotent | query over `PluginRegistry.Commands`; the set is fixed once every plugin has answered `plugin.init`, so a client asks once on connect and there is no notification for it. A plugin is refused: it knows its own registrations and has no use for another's. `/exit`, `/quit` and `/scoped-models` are not in it, being the client's own (ADR 0015, ADR 0020) |
| `command.run` | TUI, headless, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id, name, args: string}` | `{turn_id, notice, session_id, auth_challenge}`; `turn_id` set for a submitted prompt, `notice` for a report, `session_id` for a newly opened session and `auth_challenge` for caller-private login work; unused strings are empty and absent challenge means no challenge | `not_found` unknown command or unknown session; `conflict` a turn is active and the command's action needs a resting session (`SubmitPrompt`, `Compact`, `NewSession`) | not idempotent; the command decides | called whatever the turn is doing: a command is not a message and does not wait behind one. Runs the plugin Command, then maps its `Action` (`SubmitPrompt`, `Notice`, `Compact`, `SetModel`, `SetMode`, `SetTitle`, `Fork`, `NewSession`, `AuthChallenge`, `NoAction`) onto the matching domain call. `AuthChallenge` is returned only on this connection and never broadcast. Other action semantics remain as previously specified |
| `plugin.register_tool` | plugin | per class | own name only | `{name, description, input_schema, safety: Safety}`; `input_schema` raw JSON | `{}` | `conflict` name taken; the plugin stays loaded and a `notice` is emitted | not idempotent; a second registration of a name already held is refused with `conflict` whatever it carries, since a plugin registering one name twice is a mistake rather than a retry, and the first registration is the one sessions were opened against | `PluginRegistry.RegisterTool` |
| `plugin.register_command` | plugin | per class | own name only | `{name, description}`; not yet implemented, `args` and their completion sources: the slash menu completes a command name, never its arguments | `{}` | `conflict` | not idempotent; a second registration of a name already held is refused with `conflict` whatever it carries | `PluginRegistry.RegisterCommand` |
| `plugin.register_hook` | plugin | per class | own name only | `{point: HookPoint, priority: int}` | `{}` | `invalid_argument` unknown point | idempotent | `PluginRegistry.RegisterHook` |
| `plugin.register_widget` | plugin | per class | own name only | `{key, slot: header, above_editor or below_editor, content: [Span]}` | `{}` | `invalid_argument` unknown slot | idempotent; re-registering replaces content | `PluginRegistry.SetWidget` |
| `plugin.set_status` | plugin | per class | own name only | `{key, content: [Span]}`; empty content clears | `{}` | none | idempotent | `PluginRegistry.SetStatus` |
| `plugin.register_provider` | plugin | per class | own name only | `{name, wire: anthropic_messages, openai_chat or custom}`; `custom` means the server calls `provider.complete` on the plugin; a spawned plugin may register only `custom`, since a codec wire needs the endpoint and credential config a linked provider plugin reads | `{}` | `conflict` provider name taken; `invalid_argument` a codec wire from a spawned plugin, or an unknown wire | not idempotent; a second registration of a name already held is refused with `conflict` whatever it carries | `PluginRegistry.RegisterProvider` |
| `plugin.register_runtime` | plugin | per class | own name only, during `plugin.init` | `{name}` | `{}` | `conflict` Provider or AgentRuntime namespace taken | not idempotent | `PluginRegistry.RegisterRuntime` |
| `plugin.register_agent` | plugin | per class | own name only | `{name, description, prompt: string, tools?: [string], model: string, thinking: ThinkingLevel, max_turns: int}`; the same fields `agents/<name>.md` carries, since a definition is static data and needs no callback. Absent `tools` means every tool, an empty list means none, matching the file | `{}` | `conflict` a plugin already registered that name; `invalid_argument` empty description, or a `thinking` that is not off, low, medium or high | not idempotent; a second registration of a name already held is refused with `conflict` whatever it carries | `PluginRegistry.RegisterAgent`; read by `resolveAgent` after both disk roots, so a user or workspace definition of the same name wins and a plugin cannot take a name an operator is using (ADR 0028) |
| `plugin.append_note` | plugin | per class | any live session; pass 3 has no ownership check here, see the open list | `{session_id, text, role: info, muted, warn or error}` | `{entry_id}` | `not_found` for a session nobody holds live, which is what a plugin's background work gets once the session it belongs to has closed (ADR 0041); the caller handles it rather than dropping it, and a note that reports a failure goes to the notice sink instead | not idempotent | `Session.Append(note)` |
| `runtime.approval.request` | plugin | per class | ready plugin that registered `request.runtime`; no `session_id` is accepted | `RuntimeApprovalQuestion` | `RuntimeApprovalAnswer` | `unauthorized` wrong owner; `not_found` no verified thread link; `conflict` stale turn, duplicate request id or terminal item; `no_asker` returns a deny response and records the reason rather than leaving the request pending | keyed by runtime connection and request id; a repeat is `conflict` | AgentRuntime coordinator resolves Session from the verified thread link and creates RuntimeApproval |

### Requests, server to plugin

| method | callee | authn | authz | request | response | errors | idempotency | domain |
|---|---|---|---|---|---|---|---|---|
| `plugin.init` | spawned plugin | parent-child | first request the server sends | `{name, version, protocol_version, config: table, workspace_roots: [string]}`; `config` is the plugin's `[plugins.<name>]` table verbatim | `{name, version, protocol_version}`; registrations the plugin sends before it answers are committed with the plugin, so a plugin declares its surface while the server waits for this response | `plugin_error` on mismatch or timeout; plugin marked failed; a `plugin.register_tool`, `plugin.register_command`, `plugin.register_hook`, `plugin.register_provider`, `plugin.register_runtime` or `plugin.register_agent` after the response is `refused_by_invariant`, since the execution surface a session opens against never changes under it. Display registrations stay live | once per process | `Plugin.Ready` or `Plugin.Fail` |
| `tool.invoke` | owning plugin | parent-child | the plugin that registered the tool | `{session_id, tool_use_id, name, input, workspace: Workspace, timeout_ms: int}` | `{content: [ContentBlock], is_error: bool}` | `plugin_error` timeout or crash; `interrupted` after `tool.cancel` | keyed by `tool_use_id`; a repeat after a lost connection is a new invocation and the old result is discarded | `Turn.RunTool`; the result is appended as `tool_result` |
| `tool.cancel` | owning plugin | parent-child | as above | `{tool_use_id}` | `{}` | none | idempotent | `Turn.Steer` or `Turn.Cancel` reaching a running tool |
| `hook.fire` | registered plugins in priority order | parent-child | registered for that point | `{point, session_id, turn_id, payload}`; payloads in the events contract; `turn_id` is empty for `session_opened`, `before_compaction` and `session_closed` | `{result}` per point; timeout `hook_timeout_ms` from config | `plugin_error` timeout; the hook is skipped and a `notice` emitted | not idempotent | the domain event's consumer list |
| `command.invoke` | owning plugin | parent-child | the plugin that registered the command | `{session_id, name, args: string, mode, model, thinking}`; the session's own facts | `{prompt, notice, auth_challenge}`; unused strings are empty and absent challenge means none. A non-empty prompt is submitted, a notice is shown and a challenge is returned only to the invoking connection | `plugin_error` | not idempotent | plugin-defined |
| `provider.complete` | provider plugin with `wire: custom` | parent-child | registered provider | `{request_id, session_id, model: ModelRef, system: string, messages: [{role, content?: [ContentBlock], results?: [{tool_use_id, content: [ContentBlock], is_error: bool}]}], tools: [{name, description, input_schema}], thinking: ThinkingLevel, max_tokens: int, headers:table}`; `session_id` and hook-produced headers remain bound to the request across the spawned-plugin seam. A message carries `content` for every role but `tool_result`, and `results` for that one. The results of one assistant message arrive as one grouped message | `{stop_reason, stop_reason_raw, usage: Usage}` after the stream ends | `provider_error`, `interrupted` | keyed by `request_id` | `Provider.Complete` port |
| `provider.list_models` | provider plugin with `wire: custom` | parent-child | registered provider | `{}` | `{models: [Model]}` | `provider_error` | idempotent | `Registry.Refresh` for that provider |

### Notifications, server to clients

Per session, notifications are delivered in entry order. On `session.resume` the client receives every entry as `entry.appended` notifications first, then the `SessionInfo` response, then live notifications from the next entry on. Over in-memory and socket transports delivery is exactly once per connection; after a reconnect the client resumes and de-duplicates by entry id. `stream.delta` is never replayed.

A child session's `entry.appended`, `stream.delta`, `turn.state` and `tool.state` are also delivered to its parent's non-plugin subscribers, so a client watching a session sees the work it delegated (ADR 0028). They carry the child's `session_id`, which is what tells them apart from the parent's own; a client that keys on the session id was already correct and needs no change to stay correct. Depth is one, so the walk does not recurse. The plugin connection that opened the child is excluded, since it is the one waiting on the tool call and has no use for its own echo. A connection already subscribed to the child is excluded too, so a client watching the parent and reading the child receives each notification once rather than twice; `stream.delta` is never replayed and so could not be de-duplicated after the fact.

A client attaching to a parent is told about every child of it that is live at that moment, after the parent's own replay: one `entry.appended` carrying that child's `session_opened` entry, then, to an asker, every question standing on that child. The `session_opened` is what names the `agent` call the child's later notifications render under, and it is broadcast live exactly once, when the child opens, so a client that attaches after that moment would otherwise receive every one of the child's notifications and have nowhere to put any of them. The same exclusions the live routing applies apply here: nothing is sent to a plugin connection or to one already subscribed to the child itself. Depth is one.

Receiving a child's notifications is not subscribing to it, and a watcher may not drive what it watches: `session.submit` and `session.interrupt` refuse a child session from a connection that is not subscribed to it. This matters because the routing is what makes a running child's id reachable at all. Before it, that id reached a client only in the parent's `tool_result`, after the call had already returned.

Two things this does not claim. Answering is deliberately not gated, because a child with no asker of its own borrows its parent's, so a parent's asker answering a child's question is the mechanism working rather than a leak. And the remaining session methods are not subscription-gated for a plain client at all, which is pre-existing and applies to every session rather than to children (`rudy-jkz`); the two gated above are the ones this wave put within reach.

| notification | to | payload | delivery |
|---|---|---|---|
| `entry.appended` | every client attached to the session, and the parent's | `{session_id, entry: Entry}` | ordered by entry id; replayed on attach via resume. A child's are not replayed to the parent on attach, with one exception: a live child's own `session_opened`, which names the call its notifications render under and is otherwise broadcast only once, at open. The rest are not, because the parent's own log carries the `agent` tool's result and a finished child is read by resuming it |
| `stream.delta` | attached clients, and the parent's | `{session_id, turn_id, part: StreamPart}` | ordered; not replayed; superseded by the `assistant_message` entry |
| `turn.state` | attached clients, and the parent's | `{session_id, turn_id, state: TurnState}` | ordered; the current state is sent to a client attaching while a turn is active, after the replay and before the resume response. `RunningTool` means at least one tool call is running, not that exactly one is: which calls and where each has got to is `tool.state` |
| `tool.state` | attached clients, and the parent's | `{session_id, turn_id, tool_use_id, name, state: running, awaiting_permission or done}` | ordered per `tool_use_id`; every call in flight is sent to a client attaching mid-turn, after the replay and before the resume response. Not replayed afterwards: a finished call is its `tool_result` entry, and the log is what a client reads it from. Exists because tool calls run concurrently (ADR 0028), so a turn-level state cannot say which of several calls is waiting on the operator |
| `permission.requested` | attached asker clients | `{session_id, turn_id, tool_use_id, tool, input, matcher: {tool, prefix}}` | delivered to every attached asker, to an asker attaching while the question stands, and to an asker attaching to the parent of a live child while a question stands on that child, since the child has no asker of its own and the parent's is who answers for it; the first `session.answer` decides; with no asker attached the Gate denies immediately, and when the last asker detaches while a question stands the Gate denies it with `no_asker` and appends `permission_decision` (ADR 0014) |
| `status.updated` | every client | `{items: [{owner, key, content: [Span]}]}` full set | latest wins; sent on connect |
| `widget.updated` | every client | `{owner, key, slot, content: [Span]}` | latest wins per owner and key; all sent on connect |
| `notice` | every client | `{level: info, warn or error, owner, text}` | best effort; not replayed |
| `plugin.state` | every client | `{name, origin: linked or spawned, state: loading, ready, failed or stopped, reason: string}`; `reason` empty unless failed | latest wins; all sent on connect. It carries what the state now is, not that it changed, so a client renders it and keeps no history. A connection is in the broadcast set before its connect snapshot is sent, so it may be told the same state twice and must treat the repeat as the state it already holds. What it is never told is an older state after a newer one: the snapshot and the broadcasts are serialized against each other, so the last `plugin.state` a client receives for a plugin is that plugin's state (rudy-9jl) |
| `registry.updated` | every client | `{fetched_at, owners: [{name, owner_kind, count: int, error: string}]}`; `owner_kind` is provider or runtime and `error` is empty on success | latest wins |
| `server.stopped` | the one connection whose `server.shutdown` request was accepted | `{instance_id, state: stopped}` | sent only after successful runtime and listener cleanup, then followed by EOF. Cleanup failure, process death or transport loss produces no notification, so bare EOF is failure |

### Notifications, plugin to server

| notification | payload | delivery |
|---|---|---|
| `tool.progress` | `{tool_use_id, text}` | pass 3: received by the spawned plugin's adapter and dropped there; forwarding to clients as `stream.delta` needs a stream part type the TUI plan defines |
| `provider.delta` | `{request_id, part: StreamPart}` | ordered per request; the server assembles the `assistant_message` from them |
| `runtime.event` | `{runtime, event:RuntimeEvent}` | owning ready runtime only; ordered per connection, rejected on any unverified thread or turn binding, repeated final events de-duplicated |

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

### Server events

| name | aggregate | transition | payload | consumers | delivery | boundary | owner |
|---|---|---|---|---|---|---|---|
| `ServerShutdownRequested` | Server | running to shutting_down | `{instance_id}` | process owner (`ShutdownCoordinator`) | sync, after the successful protocol response is sent | internal | `Server.RequestShutdown` |
| `ServerStopped` | Server | shutting_down to stopped | `{instance_id}` | the held shutdown control connection, which is then closed | sync, after runtime cleanup completes | internal | `Server.CompleteShutdown` |

### AgentRuntime events

| name | aggregate | transition | payload | consumers | delivery | boundary | owner |
|---|---|---|---|---|---|---|---|
| `RuntimeStarted` | CodexRuntime | starting to ready | `{runtime, version}` | linked Sessions, clients as account state | sync | internal | AgentRuntime |
| `RuntimeFailed` | CodexRuntime | ready to failed with an active turn | `{thread_id, turn_id, status:"failed"}`; runtime identity is bound by the registered sink | linked Session, which marks the turn terminal and fences new work as ambiguous | ordered after preceding notifications for that process | internal | AgentRuntime |
| `LoginStarted` | LoginAttempt | new to starting | `{runtime, mode, client_id}` with no challenge secret | account coordinator | sync | internal | AgentRuntime |
| `LoginChallenged` | LoginAttempt | starting to challenged | `AuthChallenge` | invoking connection only | sync | internal | AgentRuntime |
| `LoginCompleted` | LoginAttempt | starting or challenged to terminal | `{runtime, login_id, success, error}` redacted | invoking connection, Registry refresh on success | sync | internal | AgentRuntime |
| `ThreadLinked` | Session | unlinked to linked | `{session_id, runtime, thread_id}` | runtime router, Session resume | after `runtime.toml` fsync | internal | Session |
| `RuntimeTurnStarted` | AgentRuntime | idle to active | `{session_id, thread_id, turn_id}` | clients, runtime router | sync | internal | AgentRuntime |
| `RuntimeItemProjected` | AgentRuntime | item completed or cold read | `{session_id, entry:ProjectedEntry}` | clients | ordered per thread | internal | RuntimeProjector |
| `RuntimeUsageUpdated` | AgentRuntime | usage notification or terminal turn usage | `{session_id, turn_id, usage}` | attached clients | latest wins per turn | internal | AgentRuntime |
| `RuntimeTurnCompleted` | AgentRuntime | active to terminal | `{session_id, thread_id, turn_id, status, usage}` | Session, clients, `turn_completed` hook | sync | published only as existing hook | AgentRuntime |
| `RuntimeApprovalRequested` | RuntimeApproval | new to pending | `RuntimeApprovalQuestion` plus resolved `session_id` | asker connections | sync | internal | AgentRuntime |
| `RuntimePermissionDecided` | Session | append runtime decision | the `runtime_permission_decision` entry | RuntimeApproval, audit clients through normal entry replay | sync; fsync before allow response | internal | Session |
| `RuntimeApprovalResolved` | RuntimeApproval | pending or answered to resolved | `{session_id, request_id}` | clients clear pending UI | sync | internal | AgentRuntime |

AgentRuntime is the domain service owning cross-aggregate rules between Session,
CodexRuntime, LoginAttempt, RuntimeApproval and Registry. An application handler
only transports its decisions.

### Plugin and Registry events

| name | aggregate | transition | payload | consumers | delivery | boundary | owner |
|---|---|---|---|---|---|---|---|
| `PluginLoading` | Plugin | new to loading | `{name, origin}` | clients (`plugin.state`) | sync | internal | `Plugin.Load` |
| `PluginReady` | Plugin | loading to ready | `{name}` | PluginRegistry (capabilities become visible), clients | sync | internal | `Plugin.Ready` |
| `PluginFailed` | Plugin | loading or ready to failed | `{name, reason}` | PluginRegistry (capabilities withdrawn), clients (`notice`, `plugin.state`) | sync | internal | `Plugin.Fail` |
| `PluginStopped` | Plugin | ready to stopped | `{name}` | PluginRegistry, clients | sync | internal | `Plugin.Stop` |
| `CapabilityRegistered` | Plugin | ready, on any `register_*` | `{plugin, kind: tool, command, hook, widget, status, provider, runtime or agent, name}` | RequestAssembler (tools), `resolveAgent` (agents), execution registry, clients | sync | internal | `PluginRegistry.Register*` |
| `CapabilityRejected` | Plugin | ready, on a `conflict` | `{plugin, kind, name, held_by}` | clients (`notice`) | sync | internal | `PluginRegistry.Register*` |
| `RegistryRefreshed` | Registry | `Refresh` | `{fetched_at, owners: [{name, owner_kind, count, error}]}` | clients (`registry.updated`), model picker, `session.open` validation | sync | internal | `Registry.Refresh` |

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
| `session_closed` | last client detaches, or the server exits | `{session_id}` | nothing; the server waits `hook_timeout_ms`, then closes the session log. A handler that hands work to the background has given up that log for it: nothing holds the session open for a plugin's in-flight work, so `plugin.append_note` from it answers `not_found` and the plugin reports through its own channels instead (ADR 0041) |

### Domain services

| service | rule it owns | aggregates it spans |
|---|---|---|
| Gate | given a `tool_use`, the tool's safety class, the session's mode, session allowances and whether an asker is attached, decide allow, deny or ask; classify the input into a matcher; hold the dangerous set for permissive mode. Two rules belong to this service and are enforced at the asker rather than in `Evaluate`, whose verdict is a pure function of its input and holds no session: concurrent calls with the same matcher **and the same input bytes** raise one question and share its answer, and an answer of `session` scope resolves every parked question its allowance covers. The input is part of the key because a matcher is coarse (every tool but `bash` reduces to its name), so matcher-alone coalescing would bind a call to consent given for another call's arguments (ADR 0028) | Turn, Plugin (tool safety), Session (mode, allowances), connection registry (askers) |
| RequestAssembler | build the provider request: `Session.RequestContext`, system prompt from the agent definition, skills index, `session_opened` context and `before_turn` additions, tool definitions from ready plugins, model and thinking | Session, Plugin registry, Registry |
| Compactor | after `AssistantMessageAppended`, when that message's prompt tokens (`usage.input + cache_read + cache_write`) reach `sessions.compact_at` of the model's context window and the window is known, or on `session.compact`: cover every request-context entry before the current turn's `user_message`, ask the `before_compaction` hook for a summary, else summarize with the session's model, append `compaction` | Session, Registry (context window), Provider, Plugin registry (hook) |
| Subagent runner | given an `agent` tool call, open a child session under the named agent definition with `parent` set and the call's `tools` narrowing, submit the prompt, wait for `turn.state` completed or failed, return the final assistant text or the failure as the tool result; a child session exposes no `agent` tool. Several `agent` calls in one assistant message run at once, so the runner holds no state shared between calls | Session (child), Plugin (tool), Registry (model) |
| Tool scheduler | given the tool calls of one assistant message, run them all concurrently, each with its own cancellation, and append each result as it lands; an interrupt is observed by every call in flight rather than consumed by one, so no call records success after the turn was cut. Reentrancy is the tool's own obligation, not a property the scheduler infers from safety (ADR 0028) | Turn, Plugin (tool invoke), Session (append) |
| ShutdownCoordinator | reserve admission for an authenticated `server.shutdown`, flush the success response, transition Server once, cancel other connection work, close sessions and plugins, and close the listener. On success, complete Server shutdown so the retained writer sends `server.stopped` before EOF. On failure, release it without terminal proof | Server, Session, Turn, Plugin, connection registry |
| AgentRuntime coordinator | select execution from the model owner, own runtime process readiness, bind threads to Sessions, route verified events, project canonical history, correlate login attempts, translate approval requests, persist consent before an allow and fail closed when the binding or asker is absent | Session, Registry, Plugin, CodexRuntime, LoginAttempt, RuntimeApproval |
| Recovery | on `Session.Load`, for every `permission_decision` allow without a `tool_result`, append `tool_result` with outcome `lost`; single aggregate, listed here because it runs outside a turn | Session |

The dangerous set is checked before session allowances in every mode but off: a dangerous command always asks even when a prior session-scope allow matches, so widening a matcher prefix can never silence a dangerous command. See ADR 0011.

Steering is `Turn.Steer` and `Turn.Resume`, one aggregate, no service.

## 3. Record layer

No database. Files under XDG roots, resolved as `$XDG_CONFIG_HOME` or `~/.config`, `$XDG_DATA_HOME` or `~/.local/share`, `$XDG_RUNTIME_DIR` or the OS temp dir, `$XDG_CACHE_HOME` or `~/.cache`, on every platform including macOS.

| path | owner | holds |
|---|---|---|
| `$XDG_DATA_HOME/rudy/sessions/<ulid>/entries.jsonl` | Session | the log; append-only |
| `$XDG_DATA_HOME/rudy/sessions/<ulid>/runtime.toml` | AgentRuntime | runtime name and canonical thread id; absent before a runtime session's first successful thread start and from every native session |
| `$XDG_DATA_HOME/rudy/sessions/<ulid>/blobs/<sha256>` | Session | image bytes referenced by `blob` |
| `$XDG_DATA_HOME/rudy/sessions/<ulid>/lock` | Session | `flock` held by the process serving the session; a second process gets `unavailable` and is told the socket path |
| `$XDG_CACHE_HOME/rudy/registry.json` | Registry | the last discovered snapshot; a snapshot is a cache |
| `$XDG_CACHE_HOME/rudy/rudy.log` | kernel | slog JSON lines, one record per line, appended for the life of the process, 0600, never rotated. Every record carries `pid`, because a `serve` daemon and a client attaching to it are two processes writing one file; `O_APPEND` keeps lines whole and the pid says whose. Written by `Build`, so every command that boots the kernel writes it. Records: process start with version and paths; every notice the operator was shown, at warn; plugin load, ready and fail with the reason; session open, resume and close; turn start, complete and fail with the error; connection accept and close; the socket bind; the exit path with its code and error, for a command that got as far as booting the kernel. A provider failure is the `turn: failed` record, which carries its class, message and retry count. At `debug`, each tool result with its duration and outcome. A file that cannot be opened is a notice and the records go to stderr instead |
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
| `$XDG_DATA_HOME/rudy/plugins.lock.toml` | `rudy plugin` | what is installed, from which kind of source, pinned to which ref, resolved to which commit or digest, enabled or not |

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
- in native execution, an `assistant_message` containing `tool_use` blocks is followed, for each `tool_use.id`, by exactly one `permission_decision` and then exactly one `tool_result` before the next `assistant_message`; `Load` appends `tool_result` outcome `lost` for any allow without a result
- for a tool whose safety is `unsafe`, the `permission_decision` line is fsynced before the tool runs
- a runtime approval allow is a `runtime_permission_decision` line fsynced before its response is sent to the AgentRuntime
- `user_message` with source `steer` appears only after an `assistant_message` with `stop_reason: interrupted` or a `tool_result` with outcome `killed`
- `compaction.first_entry_id` and `last_entry_id` name entries in this file or, for a fork, in the parent chain
- a session with children under `fork_point` is refused deletion

Kinds:

**`session_opened`**

| field | type | null | meaning |
|---|---|---|---|
| `schema_version` | int | no | 3. Version 1 lacks `tools`; versions 1 and 2 lack `execution` and infer native |
| `rudy_version` | string | no | the binary that opened it |
| `workspace` | Workspace | no | `git_root` empty means not a repo |
| `model` | ModelRef | no | initial selection |
| `execution` | SessionExecution | no from version 3 | fixed execution kind; absent in older logs means native |
| `thinking` | ThinkingLevel | no | initial |
| `mode` | PermissionMode | no | initial |
| `agent` | string | no | agent definition name; `default` when none |
| `parent_session_id` | ulid string | no | the session whose tool call opened this one; empty means a root session |
| `parent_tool_use_id` | string | no | the `tool_use` id in the parent that opened this one; empty exactly when `parent_session_id` is empty |
| `tools` | [string] | yes | the tool set resolved at open, before the `agent` deny. `null` means every tool the registry offers, an empty list means none. The one nullable field in the log, because the distinction it carries is the difference between an unrestricted session and a restricted one |

```json
{"id":"01K4M0A7Q8ZJ3N6R9T2V5X8B1D","at":"2026-09-07T20:30:00.123456789-06:00","kind":"session_opened","schema_version":3,"rudy_version":"0.1.0","workspace":{"root":"/Users/guy/projects/rudy","git_root":"/Users/guy/projects/rudy","project_id":"local/rudy"},"model":{"provider":"aperture","model":"cline-pass/kimi-k3"},"execution":{"kind":"native","provider":"aperture"},"thinking":"high","mode":"strict","agent":"default","parent_session_id":"","parent_tool_use_id":"","tools":null}
```

A pass 1 log lacks the two parent fields; `Load` reads their absence as empty.

`tools` is the session's tool set as resolved at open: the agent definition's list, intersected with the caller's `tools` and with the parent's effective set. It is recorded **before** the `agent` deny that caps subagent depth, because that deny is derived on every rebuild from `parent_session_id` rather than replayed, so recording it would store a rule rather than a fact. A child under a parent that holds `agent` therefore records `agent` here and is still refused it. `null` means every tool the registry offers and an empty list means none, the same distinction the definition file carries. It is written because a session is rebuilt on resume long after its parent is gone, and recomputing it from the definition alone would hand back exactly what the intersection removed (ADR 0028).

This field is why `schema_version` is 2. A version 1 log has no `tools` key, and an absent key is indistinguishable from an explicit `null` once decoded into a slice, so reading one as `null` would hand every pre-existing session the whole registry on its next resume: for a child that is the escalation this field exists to close. `Load` takes the definition's list for a version 1 log and the recorded value from version 2 on.

`execution` raises the schema to 3. Versions 1 and 2 are native by definition.
No migration writes an old log.

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
| `usage` | Usage | no | the provider's counts normalized into the common disjoint buckets; zeros when the provider sent none |
| `stop_reason` | `end_turn`, `tool_use`, `max_tokens`, `interrupted`, `refused`, `other` | no | domain classification |
| `stop_reason_raw` | string | no | the provider's own value; empty for `interrupted` |

`usage` is an external fact normalized by the provider codec, then stored because the provider is the only source and totals are summed from these rows.

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

**`runtime_permission_decision`**

| field | type | null | meaning |
|---|---|---|---|
| `runtime` | string | no | registered runtime name |
| `thread_id` | string | no | verified linked thread |
| `turn_id` | string | no | verified active runtime turn |
| `item_id` | string | no | requesting runtime item |
| `request_id` | string | no | canonical upstream request id |
| `approval_kind` | `command`, `file_change`, `permissions` | no | request family |
| `decision` | `allow`, `deny` | no | |
| `decided_by` | `asker`, `no_asker`, `disconnect`, `timeout`, `stale`, `runtime_failure`, `shutdown` | no | fail-closed source or asker |
| `scope` | `once`, `session` | no | session only from an explicit asker allow |
| `reason` | string | no | never empty |

The five binding fields are unique within one Session. An allow line is fsynced
before its response crosses to the AgentRuntime. It has no paired `tool_result`
because runtime history is canonical outside this log.

```json
{"id":"01K4M0AC...","at":"...","kind":"runtime_permission_decision","runtime":"codex","thread_id":"thr_1","turn_id":"turn_1","item_id":"item_1","request_id":"42","approval_kind":"command","decision":"allow","decided_by":"asker","scope":"once","reason":"allow once"}
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

### runtime.toml

Exactly two non-null string keys:

| key | meaning |
|---|---|
| `runtime` | registered AgentRuntime name; `codex` in this integration |
| `thread_id` | non-empty canonical thread id returned by that runtime |

Write a temporary file in the Session directory with mode `0600`, fsync it,
rename it to `runtime.toml` and fsync the directory before starting the first
turn. Refuse unknown keys, a runtime that differs from `session_opened.execution`
or a thread id already linked to another Session. A fork writes its own returned
thread id. The file is never copied or inherited.

```toml
runtime = "codex"
thread_id = "thr_1"
```

### config.toml

Every key, its type, default and meaning. A missing key takes the default. Unknown keys are
ignored by the decoder and warned about by the linter: `config.Lint` runs on every start and
names each key or table rudy does not read, with its line and, where there is one, the key or
nesting it meant. `rudy config lint` is the same pass on demand. Findings are warnings and
the config still loads; a file that will not parse is the one error (ADR 0044).

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
| `max_tokens` | int | 0 | output tokens one request may produce. 0 means the model's own `max_output`, which the registry already carries per model, so a model that serves 128000 is asked for 128000 and one that serves 64000 is asked for 64000. A value above the model's maximum is clamped to it rather than sent and refused, and 8192 is the floor for a model whose endpoint reports no maximum. Negative is refused at load |
| `permissions.mode` | PermissionMode | `strict` | |
| `permissions.dangerous` | [string] | see open list | matchers that always ask unless the mode is off, ahead of any session allowance. Three forms: a bare `prefix` is a bash command prefix, `tool:` is every call of that tool whatever its input, and `tool:prefix` is that tool with a bash command prefix. For bash, every simple command of the input is matched, and so is every command a wrapper runs: a shell's `-c` script, an `eval` argument and the remainder after `env`, `xargs`, `sudo`, `nohup`, `time`, `exec`, `command`, `timeout` and the rest, three wrappers deep. An entry a wrapper walks around is an entry that does nothing (rudy-k0.34, rudy-y3d) |
| `permissions.double_press_ms` | int | 500 | the Esc window; lives here because the client reads it; must be positive, since zero is a window no two presses fall inside and the double-Esc cancel would be unreachable |
| `prompt.file` | path | `` | a system prompt template of the operator's own; empty reads `system.md` under the config directory when it exists, and the built-in template when it does not. `${base}`, `${tools}`, `${agents}`, `${version}`, `${workspace}`, `${project}`, `${model}`, `${date}`, `${os}` are the values a template may name, `$${` writes a literal `${`, and a name outside the set is a warning notice and the built-in prompt (ADR 0024) |
| `sessions.dir` | path | `$XDG_DATA_HOME/rudy/sessions` | |
| `sessions.compact_at` | float | 0.8 | fraction of the context window that triggers the Compactor |
| `log.level` | `debug`, `info`, `warn`, `error` | `info` | the least severe record written; `debug` adds each tool invocation and provider stream error. Any other value is refused at load |
| `log.file` | path | `""` | empty means `$XDG_CACHE_HOME/rudy/rudy.log`, filled at load the way `sessions.dir` is; `~` expands. The record layer row above says what is written and what happens when the file cannot be opened |
| `secrets.file` | path | `""` | The env-format file a `cache:KEY` secret reference is read from: `KEY=value` or `export KEY="value"` lines, `#` comments ignored. Empty is filled at load the way `log.file` is, with `~/Library/Caches/op-secrets.env` on macOS, where a 1Password cache lands, and `$XDG_CACHE_HOME/rudy/secrets.env` everywhere else; `~` expands. A `cache:` reference whose file cannot be opened names this key |
| `remote.host` | string | `""` | The ssh destination, alias or `user@name`, of the machine that runs the kernel when `--host` is not given; empty means the kernel runs here. A value beginning with `-` is refused at load (ADR 0029) |
| `remote.source` | path | `~/projects/rudy` | Where the rudy checkout sits on the host, which `rudy hosts install` checks out at this binary's commit and runs `make install` in; `~` expands on the host (ADR 0029) |
| `ui.status.host` | bool | `true` | Show `host:` before the workspace path in the status bar when the session runs on a host reached by `--host` or `remote.host` (ADR 0029) |
| `ui.header.frame` | bool | true | false draws the header's lines with no box around them |
| `ui.header.greeting` | bool | true | the time of day and a name in the header |
| `ui.header.mark` | bool | true | the cat in the header |
| `ui.header.facts` | [string] | `["model", "thinking", "workspace"]` | the session's facts the header carries, in the order they draw; legal entries are `model`, `thinking`, `mode`, `workspace` |
| `ui.mouse` | `off`, `click`, `all` | `click` | what the altscreen client asks the terminal to report. `click` reports wheel and button gestures: the wheel scrolls the viewport, a drag highlights the selected cells and copies their plain text through OSC 52 on release, and a release without movement toggles a tool row. `off` leaves every mouse gesture to the terminal; `all` additionally reports movement without a button held. Inline always leaves the mouse to the terminal because its scrollback is terminal-owned (ADR 0043) |
| `ui.render` | `inline`, `altscreen` | `altscreen` | `altscreen` owns the screen and keeps every row expandable; `inline` draws in the terminal's own scrollback and commits a rested turn's rows out of the live region, where they can no longer be expanded (ADR 0015) |
| `ui.vim` | bool | true | |
| `ui.layout.slots` | [string] | `["transcript", "input", "status"]` | order top to bottom; `header` may be added |
| `ui.transcript.tool_collapsed` | bool | true | |
| `ui.transcript.tool_grouped` | bool | true | a run of two or more consecutive tool rows with no other row between them folds into one group line naming each tool and its count, opening with the same toggle a tool row does; a question standing in a row's place or a call still running keeps the run ungrouped until it settles |
| `ui.transcript.tool_preview_lines` | int | 2 | |
| `ui.transcript.thinking` | `hidden`, `shown` | `hidden` | |
| `ui.transcript.user_prefix` | string | `›` | |
| `ui.transcript.block_gap` | int | 1 | blank lines between assistant blocks; zero or positive |
| `ui.diff.style` | `text`, `background` | `text` | |
| `ui.status.above_editor` | [string] | `["turn"]` | the status line, drawn above the composer's upper line, same vocabulary as `ui.status.items`: what is worth seeing while typing rather than after. An item is drawn once, in the first list that names it, so a file that carried `turn` under the composer before this key existed does not draw it twice |
| `ui.status.items` | [string] | `["vim_mode", "model", "permission_mode", "cost", "cwd", "workspace", "cat"]` | built-in keys (`vim_mode`, `model`, `permission_mode`, `context`, `cost`, `cwd`, `workspace`, `turn`, `cat`) plus `plugin:key` for plugin items; `cwd` is the directory the session runs in, with the home directory written `~`, and it is the only item that says where you are when the directory is not a repository, since `workspace` has nothing to draw without git; `context` is still a legal item and is no longer a default, since the composer's lower rule carries it (ADR 0017); `turn` is a spinner and one word (`thinking`, `streaming`, `tool`, `steering`, `waiting`) while a turn runs and nothing at rest |
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
| `providers.<name>.auth` | string | `` | `env:NAME` reads the environment; `cache:KEY` reads a `KEY=value` line from the file `secrets.file` names; empty means no auth header |
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
| `web.brave_api_key` | string | `env:BRAVE_API_KEY` | where Brave's key comes from, in a provider key's form: `env:NAME` or `cache:NAME` for the file `secrets.file` names |
| `web.exa_api_key` | string | `env:EXA_API_KEY` | where Exa's key comes from, same form. Brave is asked first and Exa answers when Brave cannot; no backend with a key means neither web tool is registered (ADR 0039) |
| `web.max_results` | int | 5 | results one `web_search` returns, 1 through 20 |
| `web.fetch_max_bytes` | int | 2000000 | most one `web_fetch` reads from a response before it stops reading; at least 1024 |
| `web.allow_private_hosts` | bool | false | true lets `web_fetch` reach loopback, private, link-local and unique-local addresses, checked at resolution and at every redirect |

### themes/<name>.toml

Roles, every one required in a theme file or the built-in default applies: `accent`, `text`, `muted`, `user`, `assistant`, `tool`, `success`, `error`, `warning`, `diff_add`, `diff_del`, `shell`, `status`, `spinner`, `code`. A value is a hex color, a role name, or for `code` a `chroma:<style>` name. No role paints a background.

`rudy config sync` writes `themes/default.toml` when it is absent: every role at its built-in default, with the sentence above it that says what it paints, so a person has every option in front of them instead of a value that lives only in the binary. Once written it is a file like any other theme: sync never overwrites it, and loading still treats the name `default` as the built-in, so an edit to the file is a palette a person picks by name, not a silent change to the default.

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
| `plugins.<name>.source` | string | required | the source as typed to `rudy install`: `go:module/path@version`, `git:host/user/repo@ref`, an `https://` URL, or a local path. A bare git URL or scp form without the `git:` prefix reads as `git:` |
| `plugins.<name>.kind` | string | required | `git`, `path`, `go` or `https`: what `source` resolved as. A lock written before this key reads as `git` when `commit` is set and `path` otherwise. A binary from before this key drops it on its next write, so a `go` or `https` entry touched by an older `rudy plugins disable` comes back as `path` and its next `update` fails at clone; reinstalling restores it |
| `plugins.<name>.ref` | string | `""` | what the operator asked for, as typed: the part after `@`. Empty means the default branch for `git` and `latest` for `go`; `path` and `https` carry none. `rudy plugins update` re-resolves this, never the tip of whatever the checkout happens to track |
| `plugins.<name>.commit` | string | `""` | the checked-out commit for `git`; empty for every other kind |
| `plugins.<name>.digest` | string | `""` | what a non-git source resolved to: `<version> <h1:sum>` from the module proxy for `go`, `sha256:<hex>` of the downloaded bytes for `https`; empty for `git` and `path`. With `commit`, this is what makes an install reproducible and an update a diff rather than a surprise (ADR 0025) |
| `plugins.<name>.installed_at` | rfc3339 | required | |
| `plugins.<name>.enabled` | bool | true | `rudy plugins disable` sets false; a disabled plugin is not spawned |

The four kinds and what install does with each. `git`: clone at `ref`, or the default branch when empty, recording the commit. `path`: copy the directory, or clone it when it is itself a repository. `go`: `go mod download -json module@version`, copy the module directory it names, record the version and sum it reports. The store contacts nothing itself; the `go` tool resolves the module under the operator's own `GOPROXY`, `GONOPROXY` and `GONOSUMDB`, so a module the operator has routed direct does reach its git host, by the operator's choice. `https`: download a `.tar.gz`, unpack it, record the sha256 of the bytes as downloaded. `plugin.toml` is expected at the root, and an archive whose every entry sits under one top-level directory, the shape `git archive` and a GitHub release produce, has that directory stripped; any other nesting is refused naming what was found. In every kind the manifest's `build` command, when present, runs once in the staged checkout after the source lands and before the manifest is accepted, and its output and any failure are shown; `rudy plugins update` runs it again after re-resolving, except that an `https` source whose digest is unchanged is left exactly as it is, build and all, since nothing new arrived to build. `rudy install <source>` is the top-level spelling of `rudy plugins install <source>` and does the same thing; it is the one verb this CLI adds to its top-level exceptions beyond `serve`, with the reason recorded in the shape test (ADR 0025).

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
| Server.RequestShutdown, CompleteShutdown | `server.shutdown`; response precedes shutdown, then `server.stopped` precedes EOF only after successful cleanup | `ServerShutdownRequested`, `ServerStopped` | none; Server identity and lifecycle are process facts, not durable records |
| Session.Open | `session.open` | `SessionOpened` | `session_opened` |
| Runtime Session.Open | `session.open` resolved through model `owner_kind` | `SessionOpened` | schema 3 `session_opened.execution`; no thread yet |
| Session.Open, child | `session.open` with `parent` from a plugin | `SessionOpened`; the subagent runner consumes `TurnCompleted` and `TurnFailed` of the child | `session_opened` with `parent_session_id` and `parent_tool_use_id` |
| Session.Fork | `session.fork` | `ForkPointRecorded` | `fork_point` |
| Runtime Session.Fork | `session.fork` to `runtime.thread.fork` | `ThreadLinked`, `ForkPointRecorded` | child `session_opened`, `fork_point`, distinct `runtime.toml` |
| Session.Load and Recovery | `session.resume` | none; recovery appends through `Append` | `tool_result` outcome `lost` |
| Session.SetModel | `session.set_model` | `ModelChanged` | `model_change` |
| Session.SetThinking | `session.set_thinking` | `ThinkingChanged` | `thinking_change` |
| Session.SetMode | `session.set_mode` | `ModeChanged` | `mode_change` |
| Session.SetTitle | `session.set_title` | `TitleChanged` | `title_change` |
| Session.Enqueue | pass 3: not yet implemented; `session.submit` refuses a queued source with `invalid_argument` and there is no queue behind it | none until dequeued; queued messages would be in memory only, returned to the client on cancel | none; a queued message becomes a `user_message` only when it starts a turn |
| Session.Append(note) | `plugin.append_note` | `NoteAppended` | `note` |
| Turn.Start | `session.submit` source typed | `UserMessageAppended`, `TurnStarted` | `user_message` |
| Runtime thread start | first runtime `session.submit` to `runtime.thread.start` | `ThreadLinked` | `runtime.toml` durable before turn start; an orphan thread is possible only before the link rename |
| Runtime turn start or steer | `session.submit` to `runtime.turn.start` or `runtime.turn.steer` | `RuntimeTurnStarted`, `RuntimeItemProjected`, `RuntimeTurnCompleted` | canonical runtime thread; no copied conversation record |
| Turn.OnResponse | none; provider stream | `AssistantMessageAppended` | `assistant_message` |
| Gate decide | `session.answer` when asked | `ToolRequested`, `PermissionRequested`, `PermissionDecided` | `permission_decision` |
| Runtime approval decide | `runtime.approval.request`, `runtime.permission.requested`, `runtime.approval.answer` | `RuntimeApprovalRequested`, `RuntimePermissionDecided`, `RuntimeApprovalResolved` | `runtime_permission_decision`, allow fsynced before runtime response |
| Turn.RunTool | `tool.invoke` to plugin, `tool.state` to clients | `ToolStarted` | none until finished; the decision row precedes the run. Every call of one assistant message runs at once, so the `tool_result` rows of a turn may be in a different order from its `tool_use` blocks; both codecs pair them by id, and the invariant below is per `tool_use`, not positional. The results of one assistant message are assembled into a single message carrying every block, not one message per result: Anthropic's parallel tool use requires that shape, and splitting them is accepted on the wire but teaches the model to stop calling tools in parallel, which is the behaviour this wave exists to enable |
| Turn.OnToolResult | none | `ToolFinished`, `ToolResultAppended` | `tool_result` |
| Turn.Steer | `session.interrupt` how steer | `TurnSteering` | partial `assistant_message` stop_reason `interrupted`, or `tool_result` outcome `killed` |
| Turn.Resume | `session.submit` source steer | `UserMessageAppended`, `TurnResumed` | `user_message` source steer, then `turn_interrupted` how steer |
| Turn.Cancel | `session.interrupt` how cancel | `TurnCancelled` | `turn_interrupted` how cancel |
| Turn.Complete | none | `TurnCompleted` | none beyond the final `assistant_message`; completion is derived from `stop_reason` |
| Turn.Fail | none | `TurnFailedEvent`, `TurnFailed` | `turn_failed` |
| Compactor | `session.compact`, implicit on threshold | `CompactionRequested`, `CompactionRecorded` | `compaction` |
| MCP server connect | none; boot | none; a failure is a `notice` | `mcp.toml` read, nothing written |
| Plugin install, enable, disable, update | `rudy install` and the `rudy plugins` CLI, not protocol | none | `plugins.lock.toml`, the checkout, and the manifest's `build` run inside it |
| Plugin.Load, Ready, Fail, Stop | `plugin.init`; state via `plugin.state` | `PluginLoading`, `PluginReady`, `PluginFailed`, `PluginStopped` | none; plugin state is runtime, rebuilt at boot from config |
| PluginRegistry.Register* | `plugin.register_*`, `plugin.set_status` | `CapabilityRegistered`, `CapabilityRejected` | none; capabilities are runtime |
| Registry.Refresh | `registry.refresh`, implicit on open | `RegistryRefreshed` | `registry.json` |
| Runtime login | `/login` through `command.run`, runtime login methods and caller-private notifications | `LoginStarted`, `LoginChallenged`, `LoginCompleted` | none; Codex owns credentials and challenges are ephemeral |
| Runtime process start, fail and lazy restart | the version probe and App Server receive only the documented operational environment allowlist plus explicit process overrides; active turns emit `runtime_failed` and linked Sessions reject submit or fork as `ambiguous` until `session.resume` reads canonical history; the next account, model or thread operation starts a replacement process | `RuntimeFailed`, `RuntimeStarted` | existing `runtime.toml` read on resume; lost non-idempotent calls never replayed |
| session close | `session.close` | `session_closed` hook | none; closing is a connection fact, not a conversation fact |

Invariants and where they are enforced:

| invariant | aggregate | record layer |
|---|---|---|
| unsafe tool runs only after a durable allow | Gate then `Session.Append` with fsync | line order plus fsync; `Load` checks order |
| runtime authority crosses only after durable consent | AgentRuntime coordinator binds the question, then Session appends and fsyncs an allow before answering | `runtime_permission_decision` binding plus fsync; no runtime `tool_result` is synthesized |
| one runtime thread belongs to one Rudy session | AgentRuntime coordinator rejects duplicate or mismatched links and never trusts an upstream session id | atomic mode-0600 `runtime.toml`; load rejects duplicate live bindings |
| runtime projection is stable but not canonical | RuntimeProjector derives ids from the full runtime binding and replaces completed items | no projected content is stored in `entries.jsonl` |
| ambiguous runtime mutations are not replayed | CodexRuntime fails the operation and reconciles by read | no retry record or second link is written |
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
- [x] pass 10: AgentRuntime registration, login, model discovery, thread link,
  resume, fork, turn, projection, approval and restart trace through protocol,
  domain events and records or state why no record exists

## Open

- the contents of `permissions.dangerous`; the shape is settled (a bash prefix, `tool:`, or
  `tool:prefix`) and the shipped contents are still the pass 1 shell prefixes
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
