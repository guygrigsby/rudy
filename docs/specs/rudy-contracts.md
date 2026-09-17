# rudy contracts

Pass 12, 2026-09-15: bounded ACP implementation refinements (ADRs 0034 through 0038).
Tool scheduling, strict JSON profiles, producer-side structural admission, revision-bound Registry
operations, linearized Session quarantine, preserved shutdown proof and fail-closed ACP diagnostics
now have exact pre-code contracts.

Pass 11, 2026-09-14: internal daemon capabilities gate load-bearing edge guarantees (ADR 0033).
ACP readiness now requires explicit bounded-listing, bounded Session-event, bounded process-event
and terminal-durability capabilities from the live daemon, so a newly installed adapter cannot
trust an older surviving process by version alone. Written before the code.

Pass 10, 2026-09-14: stable ACP v1 replaces the private remote session carrier (ADR 0032).
`rudy acp` is an edge adapter over stdio; it translates through a same-user Unix connection
and leaves the internal protocol, kernel and records unchanged. The Mac client reaches it over
ssh through a Rudy-typed SessionClient port. Standard ACP methods carry standard behavior;
versioned `_rudy` methods carry only Rudy parity. Workspace sync remains ssh sideband and
provider identity remains on the box. Written before the code.

Pass 9, 2026-09-14: daemon shutdown completion gains explicit terminal proof (ADR 0031). After successful cleanup the retained control connection receives `server.stopped {instance_id, state: "stopped"}` before EOF. Bare EOF means crash, transport loss or failed cleanup and never authorizes replacement. A tentative shutdown claim fences work without moving public Server state. Recorded with the security hardening before final verification.

Pass 8, 2026-09-14: protocol-owned Server shutdown replaces pid signaling for daemon upgrades (ADR 0030). `server.shutdown` is confined to greeted non-plugin connections whose same-user identity the unix listener proved. Its response is flushed before cleanup begins, its connection closes only after cleanup completes, and `client.hello.instance_id` proves replacement without a process id. `rudy bridge --stop` exposes the same path. No pid file remains. Written before the code.

Pass 7, 2026-09-13: the remote runtime (ADR 0029). ssh joins the transports as a fourth row, carried by `rudy bridge` on the host; `client.hello` returns `home` so the client can place the workspace; `remote.host`, `remote.source` and `ui.status.host` join the config keys. The kernel does not change. Written before the code. Pass 6, 2026-09-11: the plugin install wave (ADR 0025). `plugins.lock.toml` gains `kind`, `ref` and `digest` so an install is reproducible per source kind; the source vocabulary is `go:`, `git:`, `https://` and a path; the manifest's `build` row, normative since pass 5 but never run, is implemented at install and update; `rudy install` is the top-level spelling. The trust model was already implemented and documented before this pass. Written before the code. Pass 5, 2026-09-10: the subagents wave (ADR 0028). Tool calls run concurrently, so the Gate coalesces asks, a Tool scheduler joins the domain services and `tool.state` joins the notifications; a session's tool set becomes one narrowing chain that a caller may only shrink, which closes a child's view escaping its parent's; `plugin.register_agent` lets a plugin contribute an agent definition; a child's notifications reach its parent's subscribers without conferring authority over the child. Written before the code, as the rules require. Pass 4, 2026-09-09: the socket transport, attach and the asker rules (ADR 0014). Pass 4 implemented 2026-09-09; its rows were walked against the code and one was corrected: `server.socket` was listed as a config key and is not one, since ADR 0014 makes the socket `Paths.Socket()` with `--socket` overriding it. Pass 3, 2026-09-08. Pass 3 implemented 2026-09-09; the rows below were walked against the code and corrected where they differed. Pass 2 aligned the protocol section with the kernel implementation; pass 3 adds the plugins wave (ADR 0012): child sessions for subagents, `session.compact` and the ninth hook point `before_compaction`, the clinepass dialect key, the memory summary model, skill migration sources, and the `mcp.toml` and `plugins.lock.toml` records. Companion to [rudy-domain-model.md](rudy-domain-model.md) and [rudy-context-map.md](rudy-context-map.md). Three contracts: the protocol, the domain events and the record layer. A transition that appears in one and not the others is listed in the cross-check with a reason.

## Error taxonomy

One closed set covers internal JSON-RPC request responses. Every request row picks from it.
JSON-RPC `error.code` is the numeric column, `error.data.kind` is the name. A
`transport_error` is instead a SessionClient connection failure with no received response and no
internal numeric code. The ACP safe discriminator may also name `transport_error` for a correlated
Turn failure without adding it to the internal response set.

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
| `internal` | -32603 | an unexpected server failure; public adapters replace detail with fixed text |

## Common types

| type | shape |
|---|---|
| `ModelRef` | `{provider: string, model: string}` both non-empty |
| `Workspace` | `{root: string, git_root: string, project_id: string}`; `git_root` empty means not a git repo |
| `ContentBlock` | one of `{type:"text", text}`, `{type:"image", media_type, sha256}` where `sha256` is the hex digest naming `blobs/<sha256>` in the session dir, stored byte-exact; inline image bytes never appear in the log, `{type:"thinking", text, signature}` where `signature` is the provider's opaque bytes verbatim, `{type:"tool_use", id, name, input}` where `input` is raw JSON bytes verbatim after strict tool-input admission: valid UTF-8, one JSON object, recursively unique object keys and the shared structural limits |
| `Span` | `{text: string, role: string}`; `role` is one of the closed Theme role names `accent`, `text`, `muted`, `user`, `assistant`, `tool`, `success`, `error`, `warning`, `diff_add`, `diff_del` or `code` |
| `Usage` | `{input: int, output: int, cache_read: int, cache_write: int}` |
| `Entry` | `{id: ulid, at: rfc3339nano, kind: EntryKind, ...payload}`; payloads in the record layer |
| `Model` | `{provider, id, display_name, upstream, context_window: int, max_output: int, pricing}`; `upstream` is who actually serves the model when the endpoint is a proxy: the route it takes, named as the endpoint names that route, comma separated when it serves the id over more than one and absent when it names none; `context_window` and `max_output` zero mean unknown; `pricing` is always `{input, output, cache_read, cache_write}` as strings in USD per token. Each value is empty when its source supplied no price or otherwise matches `(?:0\|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?` exactly. The provider's accepted number spelling is preserved rather than normalized |
| `SessionSummary` | `{id, opened_at, workspace, model, forked, parent_session_id, title?, last_entry_at?, entry_count?}`; `parent_session_id` empty means root and `forked` is true when the session began as a fork. Internal listing derives the latest title and includes it only when valid UTF-8 through 4096 bytes; absence also represents no title. ACP listing preserves that value unless omitting the individual title is required for its page bound. `last_entry_at` and `entry_count` remain pass 3 and are omitted |
| `PermissionMode` | `strict`, `permissive`, `off` |
| `ThinkingLevel` | `off`, `low`, `medium`, `high` |
| `TurnState` | `idle`, `streaming`, `running_tool`, `awaiting_permission`, `steering`, `completed`, `failed` |
| `ServerState` | `running`, `shutting_down`, `stopped` |
| `HookPoint` | `session_opened`, `before_turn`, `before_request`, `after_response`, `before_tool`, `after_tool`, `before_compaction`, `turn_completed`, `session_closed` |
| `StreamPart` | `{type: text_delta, thinking_delta, thinking_signature, tool_use_start, tool_use_delta, tool_use_end, usage or stop, text, id, name, signature, usage, stop_reason, stop_reason_raw}`; one streamed piece of a completion. `text` carries the fragment for `text_delta`, `thinking_delta` and `tool_use_delta`; `id` and `name` the tool use; `signature` the provider's verbatim bytes on `thinking_signature`; `usage`, `stop_reason` and `stop_reason_raw` are set on `usage` and `stop` |
| `ParentRef` | `{session_id: ulid, tool_use_id: string}`; the session and the `tool_use` that spawned a child session |
| `Safety` | `safe`, `unsafe` |

`turn_id` everywhere is the entry id of the `user_message` that started the turn. Turns are not stored; that id is enough to find one in the log.

## 1. Protocol

JSON-RPC 2.0. Requests carry `id`; notifications do not. Both peers may send requests. Three
internal transports:

| transport | who | framing |
|---|---|---|
| in-memory | embedded server inside the `rudy` process; the TUI and linked plugins | Go channels; JSON-RPC envelope kept so the same handlers serve every transport |
| unix socket | `rudy serve` and any client attaching to it | `$XDG_RUNTIME_DIR/rudy/rudy.sock`, or `$TMPDIR/rudy-<uid>/rudy.sock` when `XDG_RUNTIME_DIR` is unset, `--socket` overriding both; directory `0700`, socket `0600`; the server closes a connection whose peer uid is not its own before reading a byte; an accepted same-uid connection carries an unforgeable in-process same-user marker minted by the listener, never by `client.hello`; a socket file that refuses connections is stale and `rudy serve` replaces it; newline-delimited JSON. A client dials an explicit `--socket` or fails, else probes the default with a 50ms timeout and embeds when nothing answers; `--embed` skips the probe; before it connects, the client refuses a socket or a socket directory that is not owned by its uid, is reached through a symlink, or sits in a directory group or other can write, and refuses rather than embedding; its hello is bounded at 2s; `rudy serve` checks an existing socket directory against the same rule and never narrows one it did not create (ADR 0014, ADR 0030) |
| stdio | spawned plugins; the server is the parent | newline-delimited JSON on the child's stdin and stdout; stderr is captured into the log |

### Compatibility ssh carrier

During ACP parity rollout, the private remote carrier remains:

| transport | who | framing |
|---|---|---|
| ssh | a client on another machine reaching `rudy serve` on the host named by `--host` or `remote.host` | the client runs `ssh -- <host> '<PATH prefix>; command -v rudy >/dev/null 2>&1 \|\| exit 111; exec rudy bridge'` and speaks newline-delimited JSON on ssh's stdin and stdout; `rudy bridge` on the host dials the host's default socket under the unix socket rules above, starts `rudy serve` detached when nothing answers (its own session, stdio on `log.file`) and joins it, and copies messages both ways; `--no-start` makes a socket nothing answers exit 1 rather than start one; `--stop` dials without starting, greets, requests `server.shutdown`, requires matching `server.stopped` followed by EOF after full cleanup, and succeeds when no daemon answers; the bridge holds the daemon it started as a child, so a daemon that exits instead of serving is exit 1 naming the exit status and the last 20 lines of `log.file` rather than a wait for the start budget or a stream that closed unexplained; `rudy serve` exits 3 for a socket another server already holds, which is the one exit that means another daemon won rather than that none is coming, so the bridge keeps dialing for the winner until the budget ends and never reports it to a client; once a connection is established no child exit ends it, and a daemon-side close consults the child's exit only to name a reason; exit 111 means rudy is not on the host's PATH and the client installs it once (ADR 0029 decision 4) before retrying; the hello is bounded at 30s since the bridge may be starting a daemon; `--host` with `--socket` or `--embed` is exit 2 and `--host` never falls back to a local server; `RUDY_SSH` names the ssh binary for tests (ADR 0029, ADR 0030, ADR 0031) |

### SessionClient port

Client owns one transport-free Session port. Protocol retains its wire DTOs and adapters
translate them into Client-owned values; protocol, Server and plugins never depend on Client
types. The local protocol and remote ACP implementations pass one shared conformance suite.

`Initialize(ctx, asker)` succeeds only after the edge proves the live daemon advertises
`bounded_session_list_v1`, `bounded_session_events_v1`, `bounded_process_events_v1` and
`terminal_turn_durability_v1`. LocalProtocolAdapter checks those
names directly in `client.hello`. ACPClientAdapter relies on a completed ACP initialize response,
which the box-side ACPAgentAdapter gates on the same set. An absent capability fails as typed
unavailable before any Session operation. LocalProtocolAdapter closes its protocol client and
event channel, and `Wait` returns that same cause. Version mismatch alone remains a notice.

`ListSessions(ctx, filter, cursor, limit)` returns one bounded Client-owned page, never an
aggregate of the whole store. `filter.cwd` is optional and normalized by the implementation,
`limit` is 1 through 100 and the returned cursor is opaque, scoped to that implementation's
current connection generation and valid only with the same filter. An absent cursor starts the
listing. A page carries at most the requested limit and the implementation's byte bound.

`Prompt(ctx, sessionID, content)` submits one typed message and blocks until that Turn has both a
terminal durable Entry and its matching terminal state: a terminal `assistant_message` with
`completed`, `turn_interrupted` with `idle`, or `turn_failed` with `failed`. `Events()` remains
live while it blocks. `Steer`, `Cancel` and
`AnswerPermission` may run concurrently with it, and different Sessions remain concurrent. One
prompt generation may stand per Session. A second is conflict before submission. Admission for
the old generation retires before its terminal state enters `Events()`, so a queued prompt may
start from that event even while the old call is receiving its result. The result is
`{turnId, stopReason}` where `stopReason` is the normalized Rudy reason `end_turn`, `max_tokens`,
`interrupted`, `refused` or `other`; `tool_use` is never terminal. A `turn_failed` entry instead
returns a typed failure carrying class `provider`, `transport`, `plugin` or `internal`.

`RunCommand(ctx, sessionID, name, args)` returns immediately when the command starts no Turn. Its
result carries notice and replacement Session id when present. When `command.run` returns a
nonempty `turn_id`, `RunCommand` follows the same blocking contract as `Prompt`: it observes the
matching terminal durable Entry and state while `Events()` stays live, then returns that
`PromptResult` alongside the command result. It never treats the initial `command.run` response
as Turn completion. Caller cancellation records intent but retains ownership until the internal
command result establishes whether a Turn started. An empty `turn_id` or pre-start error returns
cancellation without a Turn barrier. A nonempty Turn id causes the adapter to interrupt only that
Turn and wait for its matching terminal Entry and state or connection failure.

The remote adapter mints a prompt ULID and learns `turnId` from the reordered initial typed
`user_message` carrier that echoes that `promptId`. Prompt correlation observes an internal copy
of reordered events and never drains the public event stream. It returns `refused` and `other` as
normalized SessionClient results only after the matching carrier and terminal state arrive, even
though the generic ACP prompt receives the fixed error required below. Missing correlation, a
mismatched ACP stop reason or loss between the terminal Entry and state is a protocol failure.
Caller context cancellation requests
cancellation but does not guess which Turn owns an in-flight submit. It records intent until
`session.submit` returns. A pre-start error returns typed interruption without a domain cancel; a
nonempty Turn id causes cancellation of only that Turn and retains terminal ownership until the
matching generation terminates or the connection fails. An explicit concurrent `Cancel` instead
yields the normalized `interrupted` result for the Session's active Turn.

A Client permission event includes `{permissionId, sessionId, turnId, toolUseId, tool, input,
matcher}`. `permissionId` is opaque and unique for the `SessionClient` lifetime, including across
reconnects. `AnswerPermission` accepts that id with decision, scope and reason, validates them
before claiming the pending request and resolves only its exact callback. Valid pairs are allow
once, allow Session and deny once; deny Session is invalid. One concurrent local claim wins;
another is conflict. Success means the selected outcome was accepted for delivery to the peer
callback, not that the box Gate granted it. ACP has no reverse result after that callback returns;
another asker may win at the box first, in which case normal permission retirement and later tool
state are authoritative. Prompt cancel, Session close, a decision from another asker and
disconnect retire it without authorization. An unknown or retired id is not found and cannot
answer a later request that reused a tool-use id. On negotiated ACP, the selected outcome carries
the caller's reason in its reserved metadata as specified below. Every SessionClient
implementation normalizes an empty reason to fixed `asker` before delivery; the receiving adapter
does the same before `session.answer`, and a generic ACP client also records that value.

The port preserves request error kinds `invalid_argument`, `not_found`, `no_asker`, `conflict`,
`unauthorized`, `refused_by_invariant`, `unavailable`, `provider_error`, `transport_error`,
`plugin_error`, `interrupted` and `internal`, plus the Turn failure class above. The local adapter
retains safe local detail. The remote adapter exposes only the public text allowed by the ACP
error mapping.
Both preserve representable raw Entry, Part and permission input bytes with defensive copies. The
remote event representation and local internal transport are bounded by the explicit carrier
limits below; both publish `OversizedEvent` beyond them. Events publish exactly once per connection generation after adapter
ordering. Across reconnect, durable entries deduplicate with one highest-published Entry ULID per
originating Session. Replay ids at or below that watermark are dropped and publishing a newer
durable Entry advances it. Closing a root attachment releases its watermark plus those of routed
children that have no other root or direct child attachment. Current Turn and tool snapshots replace idempotently and
stream deltas never replay. A standing permission recovered on a new connection is not omitted:
it replaces the same pending-request identity with a fresh lifetime-unique `permissionId` bound
to the new callback, while the old id remains retired. Cross-Session
sequence is one connection observation, not a domain ordering guarantee. A bounded event queue
and reorder buffer share limits of 64 undelivered items and 32 MiB. A remote nonfragment item
charges its validated original ACP frame length. Fragment frames transfer their charges into one
bounded assembly as each is consumed; the completed Entry or Part then charges `totalBytes`, not
the larger aggregate base64 frame length. Local items charge the encoded internal notification
length. The charge transfers from reorder to delivery without duplication and releases only when the
`Events()` consumer accepts the item or the implementation closes. ACP pre-SDK or client reorder
overflow closes the implementation and wakes every pending call. LocalProtocolAdapter and the ACP
agent's internal protocol client instead stop their dedicated reader before admitting a 65th item
or crossing 32 MiB. Lossless attach replay uses a dedicated blocking enqueue outside Session and
observer locks, so transport backpressure streams an arbitrarily long durable log through the fixed
window. Before subscribing live, replay compares its highest Entry id with the current log tail;
if it changed, it replays the new suffix and repeats, then attaches atomically when caught up. Live
observer fan-out remains nonblocking: a full connection queue disconnects only that client and
never stalls a Turn, tool callback or another subscriber. A single internal notification above the
byte cap closes. Event pumps start before any Session call and never wait on a Session response
while holding a charge. ACPAgent retains the internal event charge until all raw-derived display
frames have acquired their writer charges. For a negotiated raw event it releases the internal
charge after the authoritative carrier's physical write. For a generic event it releases after all
emitted display frames are writer-charged, or immediately when the projection table emits none; no
raw Entry or Part bytes become unaccounted between projection and output.
`Wait(ctx)` reports the cause after the event channel
closes. A reconnecting implementation advances its connection generation,
fails every old Prompt, turn-starting RunCommand and Close waiter with typed transport failure,
retires every old permission without authorization and stops the old event pump. Its public
`Events()` channel stays open across that recoverable transition so load can publish the recovered
view; `Wait` returns only after permanent close. Late callbacks from the replaced generation are
discarded.

### ACP v1 edge

ACP is not a fourth internal transport. `rudy acp` is an agent-side anti-corruption layer. It
serves stable ACP v1 as newline-delimited JSON-RPC on stdin and stdout, writes only fixed
diagnostics to stderr, keeps detailed errors in the box log and drives the internal protocol
through the host's same-user Unix socket. `acpclient` is the client-side anti-corruption layer
behind SessionClient. Both use
`github.com/coder/acp-go-sdk@v0.13.5`. ACP SDK types stay inside those two packages. `make
vendor-types` refuses that module anywhere else and an ACP contract guard keeps this recorded
version equal to `go.mod`.

The remote client runs:

```text
ssh -- <host> 'PATH="$HOME/.local/bin:$HOME/bin:$HOME/go/bin:$PATH"; command -v rudy >/dev/null 2>&1 || exit 111; exec rudy acp'
```

`rudy acp --no-start` exits 112 before writing an ACP frame only when safe socket checks and the
daemon dialer return the dedicated `protocol.ErrNoServer`. No other startup, ownership,
permission, handshake or transport failure may use 112. Hosts maps that exact process status to
its typed no-server result. Exit 111 remains only “rudy absent.” Every other nonzero or premature
EOF is ambiguous failure, never proof that the daemon is absent.

ssh owns host authentication, encryption and host keys. `rudy acp` starts `rudy serve` through
the shared daemon dialer when no socket answers, unless passed `--no-start`. It never exposes a
TCP listener. Each JSON payload is at most 8 MiB excluding its one terminating line feed, below
the selected SDK's fixed 10 MiB scanner ceiling. Size is the raw UTF-8 frame bytes including JSON
whitespace; a carriage return in CRLF counts as payload. Unterminated EOF is a framing failure
and is never dispatched. Inbound overflow closes the adapter; each outbound direction has a
queue of at most 64 complete encoded frames and 32 MiB excluding line feeds. A frame is charged
before enqueue and released only after physical write acknowledgment or connection close.
Overflow detaches that ACP client without stopping the daemon or Turn.

The SDK starts its receive goroutine in its constructor and otherwise uses the process default
logger, which can emit a malformed raw line. Both adapters therefore construct it over a gated,
size-limited reader, install a sanitizer with `SetLogger` and release the reader only after that
happens. The sanitizer allows only fixed diagnostic messages and bounded numeric fields; it
drops raw values, ids, request ids and unstructured errors. The reader admits at most 64 active
inbound requests, because the SDK starts a goroutine for each one, plus 64 known queued
notifications and 64 responses matched to active outbound request ids. One 32 MiB aggregate byte
budget covers the original frame bytes for all three sets before SDK input. An inbound request
stays charged until its complete response is physically written. A known notification stays
charged until the adapter's wrapped sequential SDK handler returns. A matched response stays
charged until its wrapped response callback returns, including while the SDK holds it behind an
earlier notification barrier. The SDK processes notifications serially, so their order is exact.
A 65th item in any set or a frame that would exceed the shared byte budget closes before SDK
retention. Unknown notifications are dropped before accounting. Rudy's outbound request ids are
monotonic positive JSON integers from 1 through 9007199254740991 and never wrap. Each direction
retains only the numeric issued high-water mark. A response for an active outbound id is matched,
an inactive id at or below that mark is a late response and is dropped and an invalid or future id
closes as a protocol failure. The adapter closes before allocating a request past the safe-integer
maximum, before SDK creation or event-sequence allocation. Cancellation or supersession deletes
the SDK callback only after advancing that fixed-size classification state. `$/cancel_request` is validated and classified synchronously on the SDK reader
path rather than queued; its bounded routing shape is released after dispatch.

`rudy acp` selects a fail-closed kernel log policy before building or connecting. Failure to open
its 0600 box log returns a fixed local diagnostic with a correlation id and exits before reading an
ACP frame; it never installs stderr as the record sink. A write failure after open signals the ACP
supervisor, which closes the adapter and emits only the same fixed diagnostic. Detailed record
fields, partial errors and daemon stderr are never copied to ACP stderr. When daemon startup fails,
the connector retains or discards its captured stderr locally and reports only a fixed correlated
cause. Other Rudy commands retain the ordinary operator-visible stderr fallback below.

Each adapter also permits at most 64 active outbound requests under a separate 32 MiB budget.
The complete encoded request frame is charged before the SDK call and remains charged until the
matching response callback returns, the request is cancelled or the connection closes. This set
includes agent-to-client permission requests and client-to-agent Session calls. Blocked
notification callbacks followed by near-limit responses, or many pending permission requests,
therefore remain within explicit count and byte budgets.
Admission occurs before event-sequence allocation or SDK request creation. A 65th request or byte
overflow closes the adapter, retires pending permission callbacks without authorization and fails
client Session calls with the connection cause; it never blocks a sole asker's Turn behind an
unobservable question and never creates a sequence gap.

Response construction uses a separate 32 MiB weighted admission pool. Initialize, new, load, resume, fork, list,
set-config, command-list and Registry handlers acquire their complete 8 MiB class reservation
before work begins; every other request acquires its complete 1 MiB reservation. Acquisition waits
without blocking the reader or cancellation classifier. A response whose allocation would cross
its class reservation or whose encoded result would cross 7 MiB aborts construction and reserves
fixed `-32603`, `Internal error`, without retaining the rejected bytes. The handler holds its whole
class reservation until physical response write or connection close. Registry and command
handlers acquire the full 8 MiB before issuing their bounded 4 MiB internal calls. No handler or SDK goroutine may fully
decode or marshal a response before its reservation, so at most four large or 32 small constructors
are active and reservation growth cannot deadlock. The weights classify maximum encoded output;
they are not an allocator-byte accounting claim. The pre-SDK structural limits below bound decoded
object graphs, while every retained encoded buffer is charged separately to its owning request,
response or writer budget.
New, load, resume, fork and configuration operations preflight their predictable complete response
shape under that reservation before the internal attach or domain mutation. Every complete outbound ACP
frame also passes the same strict structural scanner defined below before enqueue. Structural refusal during
ordinary query response construction reserves the same fixed `Internal error`. A mutation whose result or
process update is predictable preflights the complete worst-case ACP envelope before commit, so refusal leaves
no new Session, attachment, changed configuration or changed process state behind.

Each active request has one atomic response reservation. Before any response is reserved, a
matching `$/cancel_request` may reserve only `-32800`. After a result or error is reserved or
enqueued, later cancellation is a no-op even though the request id and bytes remain charged until
physical write acknowledgment. Prompt and turn-starting RunCommand satisfy their stronger
terminal-proof rules before reserving a response.

Before any SDK decode, one shared configurable streaming strict-JSON scanner validates every incoming ACP frame
in either adapter direction. Root depth is one and maximum nesting depth is 64. One frame contains
at most 65,536 structural units, counting one for every JSON value and one for every object member
name, at most 16,384 elements in any array and at most 4,096 members in any object. Every string is
valid UTF-8 and object keys are unique within their containing object after JSON escape decoding.
The scanner retains only its bounded nesting and per-object key state and runs while the original
frame remains charged. Crossing any inbound limit closes before SDK decode, schema validation or callback
allocation. The outbound writer runs the same profile over every complete frame before enqueue; failure is an
adapter invariant error and closes before any invalid frame reaches the peer.

Reassembled public Entry and Part JSON uses a distinct event profile: maximum depth 72, at most
131,072 structural units, at most 16,384 elements in any array and at most 4,096 members in any
object, with the same UTF-8 and recursive unique-key rules. This leaves envelope headroom around
one tool input that legitimately reaches its nested 65,536-unit limit while bounding a complete
event that contains several nested inputs. The public projector tests this exact profile while it
streams. An Entry or Part that crosses it uses `event.oversized` even when both byte forms fit
through 7 MiB. The ACP client applies this event profile before typed decode after reassembly.
Decoded `inputJsonBase64` remains a standalone tool input and uses the nested tool-input profile:
depth 64, 65,536 structural units, 16,384 elements per array and 4,096 members per object.

Request-id namespaces are directional, so an inbound prompt id `1` may coexist with an outbound
permission request id `1`. An inbound request id is a JSON string, a numeric spelling that denotes
a mathematical integer in the signed 64-bit range or null, matching ACP's `RequestId`; null has its
own canonical key and at most one null-id request may be active in a direction. A decoded string id
and the canonical key for any id are each at most 4096 bytes. Numeric source text is at most 4096
bytes, has at most 4096 coefficient digits and an exponent whose absolute value is at most 4096.
Exact integer and rational arithmetic may be used only to prove that the decoded value is integral
and within the signed 64-bit range; its canonical key is the decimal signed integer. Canonical
equality follows the JSON value: `"a"` equals `"\u0061"`, `1` equals `1.0` and `1e0`, and `-0`
equals `0`. Fractional or out-of-range numeric ids are invalid. No `float64` participates. An id
outside these bounds makes the envelope invalid before admission. The minimal envelope parser
records inbound request ids and methods.
It decodes and retains only the routing fields needed to order cancellation: `sessionId` from
`session/prompt` and `session/cancel`, and `requestId` from `$/cancel_request`. It rejects any
duplicate JSON object key in the full frame before the SDK
sees it, then gives the classifier and SDK the same validated routing value; adversarial
duplicate `sessionId` or `requestId` fields cannot select different targets at the two layers.
Accepted `session/cancel` and `$/cancel_request` frames are re-encoded from that validated
structure before SDK dispatch; a valid prompt frame is passed unchanged after its routing value
is recorded. The reader never retains prompt content or other params. Before releasing a
`session/cancel` frame to the SDK it marks that Session's standing prompt as semantic Turn
cancellation. Before releasing `$/cancel_request` it marks the named request as request-scoped
cancellation. This ordering is required because the selected SDK cancels the same prompt
context for both paths and invokes the agent's `Cancel` callback only afterwards; a timer or the
order in which callbacks happen cannot distinguish them safely.

Before SDK parameter decoding, the pre-dispatch classifier consults the same atomic initialize
state and installed-method table used by Agent dispatch. A structurally valid known Session or
extension request before readiness receives `-32014` even when its params would fail the ACP
schema. Once normal or shutdown-control readiness is committed, a known but uninstalled or
unadvertised method receives method-not-found regardless of params. Only a
request that passes this gate reaches SDK schema decoding and its Agent method. The rejection uses
the request's atomic response reservation and serialized writer.

Before admission or cancellation classification, the reader requires one JSON object with
`jsonrpc: "2.0"` and one closed envelope shape. A request has a method, an explicitly present
string, signed-64-bit mathematical-integer number or null id and neither result nor error. A
notification has a method, no id and neither result nor error. A response has an explicitly
present string, signed-64-bit mathematical-integer number or null id, no
method and exactly one of result or error. A null response id cannot match Rudy's integer-only
outbound ids. It is admitted only on an error with code `-32700` or `-32600`, consumes no slot and
is dropped after validation. A known request-only method without an id, a known notification-only
method with an id, a hybrid envelope, a batch or any other shape is invalid request and reaches
no SDK or domain handler. Unknown requests are
admitted and reach the SDK for method-not-found. Unknown notifications are dropped without a
response or SDK dispatch, as JSON-RPC requires. The stable ACP method set plus the generated
`_rudy` catalogue determines request-only and notification-only methods in both directions.

A known notification whose params fail its stable or generated extension schema closes before SDK
admission and receives no response because it has no id. This includes malformed pre-ready
`session/cancel` and `$/cancel_request`. No cancellation intent or domain call occurs.

The reader sends one fixed JSON-RPC parse or invalid-request error through the same serialized
outbound writer and closes before passing a malformed envelope to the SDK; an oversized frame
closes without a response. The writer releases an inbound slot only after successfully writing
the matching JSON-RPC response, including an SDK-generated invalid-params response; a duplicate
active inbound id or 65th inbound request closes that adapter as overloaded. The same writer
rejects an outbound payload above 8 MiB. Either wrapper signals the adapter supervisor on
refusal; the supervisor closes the peer stream and internal connection instead of relying on the
SDK to notice a writer error. Tests send malformed secret-bearing input before and after gate
release and assert that neither stderr nor captured logs contain the payload.

#### Initialize and capabilities

The adapter accepts ACP `initialize` exactly once, dials the daemon and sends internal
`client.hello {client: "acp", version, asker}` before answering. Its configured required internal
capability set for a normal or generic Session initialize is `bounded_session_list_v1`,
`bounded_session_events_v1`, `bounded_process_events_v1` and `terminal_turn_durability_v1` when the public `rudy acp` command is registered. Until internal
hello succeeds and advertises all four, the daemon connection, asker bit and negotiated capabilities
remain uncommitted. Its successful result is
staged until the complete initialize response and line feed are physically written;
that write atomically publishes readiness and opens the Session-request and outbound-event gates.
A Session or extension request before that commit returns `-32014`, `Agent not initialized` and
keeps the connection open. Initialize params are schema-validated before initialize-state
selection. Malformed params always return `-32602`, reach no daemon or domain method and leave the
waiting, in-progress or ready state unchanged. Only a schema-valid initialize is evaluated against
that state. A schema-valid concurrent initialize while the first is dialing, staging or being
written returns `-32011`, `Initialize already in progress` and keeps the connection open. A
schema-valid repeated initialize after readiness returns `-32013`, `Agent already initialized`
and keeps the connection open. None reaches a domain Session method. An extension after readiness
but without its negotiated capability is method-not-found. A malformed initialize may retry only
while the adapter remains in the waiting state. The one exception is verified
`protocol.ErrNoServer` under `--no-start`, which returns to the CLI before any ACP frame and becomes
exit 112. Every other daemon dial or hello failure returns the mapped fixed public error, writes it
and then closes the adapter. For normal or generic
Session initialize, a hello response missing any required internal capability returns `-32014`,
`Incompatible daemon; restart Rudy`, writes that response and closes. A version mismatch alone is
allowed only when the mode's required capabilities are present.

Pre-ready `session/cancel` is validated and dropped without a domain call; the connection remains
open. A pre-ready `$/cancel_request` naming no active request is likewise dropped. When it names
the in-progress initialize request before the success response is reserved, it atomically reserves
`-32800`, cancels daemon dial and hello, writes `Request cancelled` and closes. Once the success
response is reserved or enqueued, cancellation is a no-op and its physical write acknowledgment
commits readiness. The cancelled valid attempt may not retry on the same connection. Neither
notification can publish readiness, asker state or negotiated capabilities.

A generic ACP client whose metadata omits `_rudy` defaults to `asker: true` because
`session/request_permission` is a standard client method. If `_rudy` is present, its version,
asker and capability set must all be valid; malformed or unsupported Rudy metadata is invalid
params and never falls back to generic asker authority. The Rudy client sends
`_meta._rudy.asker`: TUI is true and headless is false. The standard
initialize response advertises protocol version 1, agent name and version, `loadSession: true`, text
prompts and session capabilities `list`, `resume` and `close`. It accepts ACP's mandatory
resource-link prompt blocks without fetching them. It does not advertise `delete`, additional
directories, image, audio, embedded context, MCP transports or an authentication method. It
never calls ACP filesystem or terminal methods even when a generic client advertises them. SDK
types and methods marked `Unstable` are unused; Rudy fork stays a negotiated `_rudy` method.

The Rudy client offers this exact initialize metadata shape:

```json
{"_rudy":{"version":1,"asker":true,"capabilities":["session.update"]}}
```

`asker` is false for a headless client. `capabilities` is a set of names from the extension
table below; `session.update` also enables the versioned event metadata defined below. The
agent returns this exact shape, with the requested and supported capability intersection:

```json
{"_rudy":{"version":1,"capabilities":["session.update"],"home":"/home/guy","instanceId":"01K...","rudyVersion":"v0.1.0"}}
```

All extension fields use lower camel case. Capability arrays are sorted and contain no
duplicates. The agent's `home`, `instanceId` and `rudyVersion` come unchanged from internal
hello. Negotiated `_meta._rudy.open` has the exact shape below; absent fields take the daemon's
normal defaults. Absent or null `tools` adds no caller narrowing while `tools: []` narrows to no
tools.

```json
{"_rudy":{"open":{"model":"aperture:model","mode":"strict","thinking":"medium","agent":"default","tools":null}}}
```

Host lifecycle code may instead offer this exact shutdown-control initialize metadata:

```json
{"_rudy":{"version":1,"control":"shutdown","asker":false,"capabilities":["server.shutdown"]}}
```

The agent accepts this variant only after the shutdown handler is installed and returns:

```json
{"_rudy":{"version":1,"control":"shutdown","capabilities":["server.shutdown"],"home":"/home/guy","instanceId":"01K...","rudyVersion":"v0.1.0"}}
```

It sends internal hello as a non-asker with `process_events: false` and does not require the four
edge guarantees. A new daemon therefore sends no process snapshot or later process events. When an
older daemon ignores the field, the control adapter starts a dedicated bounded notification drain
before hello and discards process events without projecting or retaining them. Any oversized frame,
unbounded admission or hello ordering failure closes with a fixed local cause and makes replacement
fail closed; bypassing the capability gate is a safety claim, not a liveness promise against every
old daemon. Its
standard initialize response advertises only ACP's mandatory text and resource-link prompt
baseline. Valid mandatory `session/new` and `session/prompt` requests return fixed `-32014`,
`Shutdown control connection`, without a domain call; `session/cancel` is validated and dropped,
and the agent sends no `session/update`. Optional Session methods and every other extension are
unadvertised and method-not-found. `_rudy/server_shutdown` is the only operational method. The
Unix listener's same-user marker, internal shutdown acceptance, matching `server.stopped`, EOF and
physical ACP response write remain required. A generic client cannot select this mode by omission,
and any other `control`, asker value or capability set is invalid params.

`client.hello.process_events` governs only the broadcast process UI set: `status.updated`,
`widget.updated`, `plugin.state`, `registry.updated` and `notice`. It never suppresses or discards
the point-to-point `server.stopped` terminal proof. The shutdown-control discard pump applies only
to that broadcast set and routes `server.stopped` unchanged to its control waiter.

After `session.update` negotiation, every `session/prompt` request also carries this exact
metadata. `promptId` is a canonical 26-character ULID minted by the Rudy client, unique for its
`SessionClient` lifetime and used only for correlation:

```json
{"_rudy":{"version":1,"promptId":"01K..."}}
```

A missing, malformed or repeated active `promptId` is invalid params and starts no Turn. It
grants no identity or authority. The agent binds it to one per-Session prompt generation.

Both objects live under ACP's `_meta`. A generic ACP client omits `_rudy` and receives only the
standard surface. An `_rudy/` request without negotiated version and capability is
method-not-found. The Mac client requires the capabilities used by its command before it opens
a Session. It never falls back to `rudy bridge` or a local daemon.

ACP advertises no auth method because the stdio process boundary already inherits its client
identity from the operating system. On a remote connection, ssh authenticates a principal and
maps it to the Unix account that launches `rudy acp`; the daemon's same-user socket check then
preserves that identity. Every principal or key mapped to that account has the account's full
plain-client authority, including session history, `mode: off`, operator shell and negotiated
server shutdown. Provider identity is separate: the daemon resolves provider access on the box
from an explicit credential, ambient host identity or no authentication. Aperture uses the
box's tailnet identity and has no provider token. No provider credential crosses ACP.

#### Standard methods

Wire shapes are the stable ACP v1 schema. These rows add Rudy constraints and mappings.

| ACP method | Internal mapping | Rudy rule |
|---|---|---|
| `authenticate` | none | No method id is advertised. Every call is invalid params and creates no identity or credential state. |
| `logout` | none | Logout capability is not advertised. A call is method-not-found and changes no operating-system or provider identity. |
| `session/new` | `session.open` | `cwd` is an absolute path on the box. `mcpServers` and `additionalDirectories` must be empty. Negotiated `_meta._rudy.open` may carry `model`, `mode`, `thinking`, `agent` and `tools`, never `parent`. Session id in the response is the Rudy ULID unchanged. |
| `session/load` | `session.resume` | Look up the target through internal `session.list` and validate request cwd against its primary Workspace root before attach. Require empty `mcpServers` and `additionalDirectories`; a client may not spawn a process or widen stored roots on the box. Translate replayed entries and current live state into `session/update` before answering. |
| `session/resume` | `session.resume` | Perform the same pre-attach lookup and cwd, MCP and additional-directory validation as load. Suppress only replayed `entry.appended` notifications before the internal response. Forward current turn, tool and permission state while the request is open, then forward every live notification; the internal ordering contract puts newly appended entries after the response. |
| `session/list` | `session.list` | Validate an optional `cwd` as an absolute box path, normalize it and filter before pagination. With no filter, expose every Session visible to the same Unix account. A negotiated Rudy request may carry exactly `_meta._rudy: {version:1, limit}` with integer `limit` from 1 through 100; absence, including every generic request, means 100. Sort Rudy ULIDs descending and take the largest ordered prefix of at most `limit` Sessions whose encoded response stays below 7 MiB. Preserve each valid bounded internal title unless including that individual title would cross the ACP page bound; then omit only that title and retry sizing. If that one title-less Session still cannot fit, return fixed `Internal error` rather than skip it or emit an oversized page. An absent cursor starts the listing. Mint `nextCursor` only when another match exists. A cursor is an unpadded base64url token authenticated by HMAC-SHA-256 under a random per-connection key and carries version, last ULID and a SHA-256 digest of the normalized cwd filter. Continue strictly below that ULID. Reject an empty, malformed, modified, cross-connection or filter-mismatched cursor as invalid params. Never persist or log cursor or key material. |
| `session/close` | exact `session.interrupt cancel`, then `session.close` | Every other Session-targeting request or notification takes a per-connection operation lease before its domain call. Every permission response callback takes the operation lease for its outer attachment before claiming its callback token or making an answer, decline or cancel domain call; a pre-fence claim joins the close-owned permission identity and physical-write set, while a post-fence callback retires without a domain call. Close atomically installs the fence instead of taking an ordinary lease. A second Close while that fence is active is a separate controller rejected with fixed `-32011`, `Session close already in progress`; it joins no lease or waiter set. Under the same event-state lock, fence installation stops new leases, snapshots the pre-fence set without including the winning close controller, snapshots the current Turn, tool and permission identities and installs proof waiters seeded from every already-retained terminal proof before releasing the lock. This makes terminal publication concurrent with Close either seed or satisfy its waiter without a lost-wakeup window. A Turn identity committed later by a pre-fence Prompt or turn-starting RunCommand lease joins that close-owned set, installs its proof waiters under the same lock and issues exact-id cancel before the lease can complete; other late identities join their matching waiter sets without a cancel. A `$/cancel_request` that reserves `-32800` first leaves no fence or domain mutation, while cancellation after the fence is installed is a no-op and close proceeds. Send every cancel with its exact close-owned Turn id. `refused_by_invariant` means that Turn already ended and `conflict` means a successor is active; both mean no cancellation occurred, never fall through to the successor and continue waiting for the close-owned Turn's durable terminal proof. A terminal-durability `unavailable` error yields no Close success and tears down this ACP connection and fence with the quarantine cause; write its fixed unavailable response first only when the writer remains usable. Any other recoverable interrupt error reserves its mapped Close error, releases the fence after that error is physically written and leaves the Session attached; transport loss instead tears down the connection and yields no success. Retire only callbacks whose closed outer attachment Session is the Session being closed, including callbacks for child events routed through that attachment; callbacks for another attached root on the connection remain live. Then wait every pre-fence lease on this connection through domain completion and every request lease through physical response write, including attach, steer, compact, shell, configuration, title and extension work; do not wait for another connection's leases. Observe durable `permission_decision` and `tool_result` entries plus terminal Entry and state only for the close-owned ids. A `session.event_oversized` Entry reference counts as that durable proof only when its entry id, Entry kind, Turn id and conditional projected tool-call id match the close-owned identity exactly after recomputing it from the close-owned Turn id, a zero byte and raw tool-use id. Require matching ACP response writes for this connection's close-owned Prompt and turn-starting RunCommand generations. A successor Turn or call started by another connection after the snapshot is outside the set and remains that connection's work. Then detach and write the close response. Tolerate an empty close-owned set. Release the fence only after detach and response write. No close-owned operation lease, Prompt, RunCommand waiter or permission id remains live on this connection when close returns. The append-only log remains. |
| `session/prompt` | `session.submit` | Translate text and resource-link blocks through the shared content translator as `source: typed`, keep the request open through the Turn and map the terminal Turn reason to ACP `stopReason`. A negotiated Rudy client supplies its required `promptId`; a generic client supplies no Rudy metadata. Unsupported content is invalid params. Before submission, require both the internal content value at most 4 MiB and its complete public typed `user_message` Entry projection at most 7 MiB, so every accepted live prompt has an exact initial carrier for correlation. |
| `session/cancel` | exact `session.interrupt cancel` | Best effort and idempotent at the ACP edge. Before SDK callback dispatch, the framed admission layer binds this exact notification to the active Prompt or turn-starting RunCommand generation, or snapshots the exact current Turn id under the same event-state lock when no owned generation exists. The handler uses only that recorded id. No active Turn at admission, `refused_by_invariant` or `conflict` is a no-op; none may fall through to a successor. Complete standing ACP permission requests for that exact Turn as cancelled. |
| `session/set_mode` | `session.set_mode` | Advertise and accept `strict`, `permissive` and `off`. |
| `session/set_config_option` | `session.set_model` or `session.set_thinking` | Model values come from the daemon Registry and thinking values are `off`, `low`, `medium`, `high`. Return the complete current option set. |
| `session/request_permission` | `permission.requested`, then `session.answer` | Agent-to-client request. Offer `allow_once`, `allow_always` and `reject_once`, mapping to allow once, allow session and deny once. Rudy has no durable deny allowance, so it never offers `reject_always`. First answer wins. A valid JSON-RPC error response performs exact-question `session.decline_permission` and then closes that ACP connection; it never authorizes or cancels the Turn. Disconnect of the last asker denies `no_asker`. |
| `session/update` | `entry.appended`, `stream.delta`, `turn.state`, `tool.state` | Standard updates carry assistant text, thought chunks, tool calls, tool updates, mode, config options and Session title metadata. Stored entries remain authoritative. Rudy does not send `available_commands_update`: its command invocation path is `_rudy/command/run`, so advertising commands to a generic client would be dishonest. |

ACP `session/new` completes the Session-open Registry trigger before mutation: call internal
`registry.refresh`, retain its immutable models plus `revision`, derive and preflight the complete
ACP response from that exact copy, then call internal `session.open` with optional
`registry_revision`. The optional field only narrows an operation, so any plain client may use it
and existing callers may omit it. Server checks
the revision and resolves the selected model under one Registry read lock before `Session.Open`
appends anything. A stale revision maps to fixed ACP conflict and creates
no Session. The ACP path performs no second refresh after preflight. Other internal callers omit
the field and retain their current open behavior. `session.set_model` uses the same conflict code
for a stale revision or active Turn and changes nothing in either case.

The `session/close` terminal barrier always requires internal durable proof and the close-owned
Prompt or RunCommand response. On a negotiated Rudy connection it also requires physical-write
acknowledgment for every close-owned terminal Entry carrier or oversized reference and its matching
terminal state carrier. On a generic connection it waits only for display projections the closed
projection table actually emits for those records; carrier-only Entry and `turn.state` events emit
no generic frame and create no phantom waiter. Fence installation creates or seeds the applicable
write waiters under the same event-state lock used for proof waiters. An identity admitted later by
a pre-fence lease joins both sets before that lease completes. Routing for an admitted close-owned
frame remains installed through its acknowledgment, so detach and the Close response cannot
overtake or suppress it.

For `session/close`, the conditional identity in a permission-decision or tool-result
`session.event_oversized` reference is `projected_tool_call_id`, never the raw provider id. Close
recomputes the universal projection from the close-owned Turn id, a zero byte and its raw tool-use
id and requires an exact match with the reference.

`session/delete` is not advertised or implemented. ACP `session/load` replays history;
`session/resume` does not. Rudy TUI reconnect uses load because ssh loss may have hidden durable
entries. The adapter keeps only replay suppression and request correlation for the connection.
Only one load or resume for a Session may be active on an ACP connection. A concurrent second
attach for the same ULID is a conflict; different Sessions still attach concurrently.

The adapter never parses raw tool input or a thinking signature and feeds re-marshaled bytes
back into Session. Standard updates carry display-safe fields. Negotiating `session.update`
activates one sequenced Rudy event stream for the lifetime of that ACP connection. Every
internal event admitted to that stream produces one logical carrier set. Non-byte events use one
carrier; an Entry or Part uses one through seven fragment carriers. A standard
`session/update` carrier uses this exact outer metadata shape, never metadata on the nested
update variant:

```json
{"_rudy":{"version":1,"sequence":1,"event":{"kind":"entry.fragment","sessionId":"01K...","eventId":"01K...","entryId":"01K...","fragmentIndex":0,"fragmentCount":1,"totalBytes":417,"sha256":"6d...","dataBase64":"eyJ...=","redacted":false}}}
```

`event.kind` selects one of these closed variants:

| kind | required fields after `kind` and `sessionId` | meaning |
|---|---|---|
| `entry.fragment` | `eventId`, `entryId`, `fragmentIndex`, `fragmentCount`, `totalBytes`, `sha256`, `dataBase64`, `redacted`; fragment zero of the live initial negotiated typed-prompt set on its admitting connection also requires `promptId`, `turnId` | One fragment of the complete UTF-8 public Rudy Entry JSON. `eventId` is a connection-local ULID shared by the set. `fragmentIndex` is zero-based, `fragmentCount` is 1 through 7, every decoded non-final fragment is exactly 1 MiB and the final is 1 byte through 1 MiB. `totalBytes` is 1 through 7 MiB, `sha256` is the lowercase hex digest of the complete public JSON and `dataBase64` is padded RFC 4648 base64. For the live typed `user_message`, buffer fragment zero until `session.submit` returns, require decoded `id == entryId == turnId` and echo that generation's `promptId`. Historical replay and other entries omit both correlation fields. For `turn_failed`, encode the redacted public Entry projection with fixed `message: "Turn failed; see box log"`. For every `assistant_message`, clear `stop_reason_raw`. `redacted` is true exactly when either replacement occurred. |
| `part.fragment` | `eventId`, `turnId`, `fragmentIndex`, `fragmentCount`, `totalBytes`, `sha256`, `dataBase64`, `redacted` | One fragment under the same indexing, sizing, digest and base64 rules of the complete public Rudy `provider.Part` JSON. For every stop Part, clear `stop_reason_raw`; `redacted` is true exactly when that occurred. |
| `event.oversized` | `eventId`, `originalKind`, `totalBytes`, `sha256`, `redacted` and the category fields defined below | A bounded, non-actionable reference when an Entry or Part's exact private internal JSON or public projection exceeds 7 MiB, when its public projection exceeds the decoded-event structural profile or when a permission input exceeds its 1 MiB carrier limit. `originalKind` is exactly `entry.appended`, `stream.delta` or `permission.requested`. For Entry and Part, `totalBytes` is the public JSON projection size as a canonical positive decimal string of at most 20 digits with no leading zero and fitting uint64; it may be at most 7 MiB when only the private form or public structural profile crossed. Their `sha256` is over that complete public JSON. For permission, `totalBytes` is the exact valid JSON input length and its canonical public projection is a same-length stream of zero bytes, generated incrementally without allocation; `sha256` is over that redaction. Thus the permission digest reveals nothing beyond the already exposed length and never names secret input. `redacted` is always true because the referenced content is omitted. It contains no raw content and does not close the connection. |
| `turn.state` | `turnId`, `state` | exact Rudy Turn state: `idle`, `streaming`, `running_tool`, `awaiting_permission`, `steering`, `completed` or `failed` |
| `tool.state` | `turnId`, `toolUseId`, `name`, `state` | exact Rudy tool state: `gating`, `queued`, `running`, `awaiting_permission` or `done` |

`sessionId` is always the originating Rudy Session, including a live child whose update is
routed through its parent. The outer ACP `sessionId` remains the attached Session the ACP
client is driving. The Rudy client treats missing metadata, an unknown variant, invalid base64,
invalid decoded JSON, a fragment inconsistency, a digest mismatch or an id mismatch as a protocol
failure. The dispatcher never interleaves fragment sets. The client permits one active assembly,
bounds it at 7 MiB, charges each original frame until its bytes transfer into that assembly and
charges the completed event's `totalBytes` against the shared event queue until public acceptance.
Before typed decode or publication, it runs the reassembled Entry or Part JSON through the
decoded-event UTF-8, depth, structural-unit, container-size and recursive unique-key profile. It
then decodes through Rudy types and never reconstructs a raw value from the
display-safe ACP update.

An `event.oversized` with `originalKind: "entry.appended"` additionally requires `entryId` and
`entryKind`. `entryKind: "permission_decision"` or `tool_result` requires both `turnId` and
`projectedToolCallId`, using the universal Turn-scoped projection even for an unbounded legacy
raw id; the dispatcher derives the owning Turn from live execution context or replay timeline even
though those Entry payloads do not store it. Every terminal Entry also requires its owning
`turnId`. Every other Entry kind forbids `projectedToolCallId`; `turnId` is required wherever the replaced
Entry contract owns it plus the derived permission, tool-result and terminal cases. This variant
permits no `promptId`, Part or permission-only field. A
live typed-prompt initial Entry cannot take this form because prompt admission preflights its public
Entry projection through 7 MiB before `session.submit`; historical and every other reference have
no prompt generation to correlate.
When it stands for the terminal Entry of a Turn, it also carries either normalized
`terminalStopReason` or `failureClass`; a `turn_failed` reference with `failureClass` also requires
`retries` as a JSON integer from 0 through 2147483647. Prompt and turn-starting RunCommand accept
that bounded reference plus the matching terminal state as terminal proof and return no omitted content. An
oversized reference with `originalKind: "stream.delta"` requires only `turnId` and `partKind` after
the common fields. One with `originalKind: "permission.requested"` requires only `turnId` after the
common fields; its `totalBytes` and `sha256` name the same-length zero-byte public redaction defined
above. It carries no tool identity or matcher and cannot be answered. `OversizedEvent` is a
sealed SessionClient event containing exactly these safe reference fields. LocalProtocolAdapter
produces it from the bounded internal reference.

`sequence` is a positive JSON integer from 1 through 9007199254740991, the largest integer ACP
implementations using IEEE 754 can represent exactly. The first carrier is written only after
the successful initialize response and has sequence 1. Every later carrier increments it by
exactly one; the adapter closes before overflow. One connection-wide counter spans every root
Session, routed child Session and process-wide event. It is not an event identity, is never
persisted and does not reset on Session attach, detach or close.

The ACP agent's single outbound dispatcher filters routing and replay suppression before
allocation, then commits sequence allocation and carrier enqueue as one serialized action before
the next carrier can allocate. It writes every fragment of an authoritative set before any
display projection for that event. Every enqueued carrier is tagged at the framed writer and must
receive physical-write acknowledgment before the dispatcher allocates its successor. Enqueue failure, SDK cancellation
before write or writer failure closes the connection before a successor becomes live, so a live
connection never creates a numeric gap. Calls that wait for an ACP response may continue
concurrently after their request carrier is acknowledged.
An internal event suppressed by routing, attach mode or resume replay suppression consumes no
sequence. Load gives replayed entries fresh
connection sequences in replay order, followed by current Turn, tool and standing permission
state, then live events. A standing permission delivered after reconnect receives a new
connection sequence; durable duplicate suppression remains keyed by Entry id.

One Rudy event may have several honest ACP projections. The closed table below selects the
authoritative carrier and every later display-only projection. `blocks in order` means each stored
block becomes one update in original order: text becomes the named message chunk, thinking becomes
an agent-thought chunk, image becomes a fixed text placeholder containing only media type and
SHA-256, and tool use becomes a tool-call start with id and name but no raw input. Tool-result
content uses fixed text or image placeholders and never raw nested input or output. Marshal-aware
UTF-8 splitting keeps each fragmentable text display update at or below 256 KiB of encoded JSON.
For a negotiated connection, the table's Entry and Part carrier is a metadata-only
`session_info_update` per fragment, or one `event.oversized` carrier, written before its display
projections. Other events carry metadata on the table's single authoritative projection. Later
display projections carry only `_meta._rudy.version: 1`. For a generic connection the fragment or
oversized carrier is omitted and every display projection omits `_rudy` entirely. If a row produces
no visible projection, the negotiated carrier itself is `session_info_update` with both `title` and
`updatedAt` absent, which ACP v1 permits.

| admitted internal event and context | negotiated carrier, then ordered display projections |
|---|---|
| replayed `entry.appended` `user_message` | Entry fragment set; `user_message_chunk` for its blocks in order |
| live `entry.appended` `user_message` | Entry fragment set, with prompt correlation on fragment zero for the admitting connection; `user_message_chunk` for its blocks in order |
| replayed `entry.appended` `assistant_message` | Entry fragment set; `agent_message_chunk`, `agent_thought_chunk` and tool-call starts for its blocks in stored order |
| live `entry.appended` `assistant_message` | Entry fragment set only; live Parts already own incremental display |
| replayed or live `entry.appended` `tool_result` | Entry fragment set; one or more `tool_call_update` values for the projected tool id, with `completed` for `ok` and `failed` for `error`, `killed` or `lost` |
| replayed or live `entry.appended` `mode_change` | Entry fragment set; `current_mode_update` |
| replayed or live `entry.appended` `thinking_change`, or bounded valid `model_change` | Entry fragment set; `config_option_update` for the changed option |
| replayed non-current `model_change` with an invalid or overlong ModelRef field | Entry fragment set only for negotiated clients; no standard config update, so a generic client receives no forged selectable value. A current value in this form makes internal resume fail before replay |
| replayed or live `entry.appended` `title_change` | Entry fragment set; `session_info_update` carrying only the bounded title projection |
| every other `entry.appended` kind | Entry fragment set only |
| `stream.delta` `text_delta` | Part fragment set; `agent_message_chunk` |
| `stream.delta` `thinking_delta` | Part fragment set; `agent_thought_chunk`, without thinking signatures |
| `stream.delta` `tool_use_start` | Part fragment set; `tool_call` with the Turn-scoped projected id, projected name and `pending`, without raw input |
| `stream.delta` `tool_use_delta`, `tool_use_end`, `thinking_signature`, `usage` or `stop` | Part fragment set only; later tool state or the durable Entry owns display |
| attach-snapshot `tool.state` `gating`, `queued`, `running` or `awaiting_permission` | `tool_call` with id and name, and status `pending`, `pending`, `in_progress` or `pending` respectively |
| live `tool.state` `gating`, `queued`, `running` or `awaiting_permission` | `tool_call_update` with status `pending`, `pending`, `in_progress` or `pending` respectively |
| `tool.state` `done` or any `turn.state` | metadata-only `session_info_update`; durable terminal Entries and tool results own display |
| `permission.requested` | the outbound `session/request_permission` request itself |
| negotiated process event | its `_rudy/session/update` notification |

Every standard ACP tool-call id is the stable string
`rudy-call-sha256-<lowercase hex SHA-256>` over the exact UTF-8 Turn id, one zero byte and the raw
provider tool-use id bytes. The same projection is used for starts, updates, results and the
standard side of permission requests. It is unique across Turns even when a provider reuses a raw
id, always fits the ACP scalar bound and reveals no historical invalid bytes. Negotiated raw
carriers retain the exact provider id; generic clients see only the projected id.

Every required non-fragmentable ACP string is valid UTF-8 and at most 4096 bytes. Inbound title,
model, command, tool and option values over that limit are invalid params before a domain call.
Registry refresh admits only Models whose individual string fields are valid UTF-8 and fit and whose complete encoded
internal `registry.list` result remains at most 4 MiB; an overflowing refresh fails and retains the
previous snapshot. Plugin command registration similarly refuses a command if its name or
description exceeds the scalar limit or the complete encoded `command.list` result would exceed
4 MiB. New provider tool-use ids or names with invalid UTF-8 or over the scalar limit fail the Turn as a provider error
before an assistant Entry or tool goroutine is committed. The Turn-scoped projected call id above
covers every current or historical tool-use id. For a historical durable tool name that predates
this rule, every standard projection uses the stable alias
`rudy-name-sha256-<lowercase hex SHA-256>`; the raw fragment remains exact. A historical title over the limit is omitted from internal list and resume
state and its title-change projection carries fixed `Title omitted because it exceeds the ACP scalar
limit`. Internal list summaries replace invalid or overlong historical ModelRef fields with the
stable display-only aliases `rudy-provider-sha256-<lowercase hex SHA-256>` and
`rudy-model-sha256-<lowercase hex SHA-256>`. Internal resume instead fails unavailable before replay
or attach when the current operational ModelRef is invalid or overlong, because substituting an
alias there would change provider semantics. A superseded historical `model_change` with such a
field retains its exact negotiated Entry fragment but emits no standard config-option projection;
generic clients receive nothing for that row.
Process-event identity is never projected or aliased. A candidate `status.items[].owner`,
`status.items[].key`, `widget.owner`, `widget.key`, `plugin_state.name` or
`registry_state.providers[].name` must be valid UTF-8 through 4096 bytes before the
ProcessSnapshotCoordinator may commit it. An invalid or overlong candidate is refused atomically
and retains the prior aggregate; plugin discovery and provider registration therefore fail before
spawn or refresh publication. Existing nonempty and uniqueness rules still apply.

Every `status` or `widget` Span text must also be valid UTF-8 through 4096 bytes. Its role must
be exactly one of `accent`, `text`, `muted`, `user`, `assistant`, `tool`, `success`,
`error`, `warning`, `diff_add`, `diff_del` or `code`. An invalid Span is refused with its
containing mutation. No process-event field uses a digest alias. Failed plugin reasons and
nonempty Registry errors use the fixed public strings defined below. `notice`
uses only valid `public_text` through 4096 bytes; absent, invalid or overlong `public_text`
becomes fixed `Remote notice; see box log`. The private `text` is never hashed or forwarded.
All remaining process-event strings are closed enum values or daemon-generated RFC 3339
timestamps. Thus no identity, title, config value or extension scalar can bypass the frame bound
merely because it cannot be split as display text.

One Server-owned ProcessSnapshotCoordinator serializes preflight and commit for every Plugin,
status, widget and Registry mutation that contributes to replay. While holding its admission lock,
it rebuilds the prospective complete replayable process snapshot from the current other aggregate
plus the candidate mutation: at most 64 notifications and 32 MiB in both exact internal and safe
public encoded forms. Every safe public event is sized with the widest valid
`sequence: 9007199254740991`, independent of the current connection. No contributing aggregate
commits outside that coordinator. A Registry
refresh assigns per-provider generations before upstream I/O, then rebases its non-superseded
provider deltas onto the current snapshot under admission. Durable cache replacement precedes
in-memory publication inside the same commit, so concurrent non-overlapping refreshes do not lose
one another and an older overlapping result cannot overwrite a newer one. A cache-write failure
retains the prior cache and in-memory Registry snapshot.

The prospective full `status.updated` snapshot and each prospective `widget.updated` value also fit
both individual forms through 4 MiB and every complete worst-case ACP carrier passes the outer
structural profile. Status or widget byte or structural overflow is `invalid_argument` and atomically
retains prior state; Registry byte or structural overflow retains its prior snapshot. Plugin discovery reserves one `plugin.state` event
with a worst-case encoded 4096-byte failure reason before adding or spawning that plugin. The
reservation encodes the complete failed `plugin.state` candidate with 4096 ASCII NUL bytes through
the exact internal JSON encoder, covering six-byte escaping of every allowed input byte. Overflow rejects that plugin
before it enters the Registry, emits no replay state and records a fixed box-log diagnostic. A
later plugin failure always commits `failed`: keep a valid reason through 4096 bytes, otherwise log
the full reason and store fixed `Plugin failed; see box log`; the admission reserve makes either
form fit without rollback. A live notice whose exact internal or safe public form would exceed
4 MiB or fail the outer structural profile logs its detail and emits fixed `Notice omitted; see box log`. This aggregate invariant, not
only the per-string rule, keeps connect replay and the nonfragmentable `_rudy/session/update`
carrier below both internal and ACP frame limits.

Load replay owns transcript reconstruction from durable Entries. Live Parts own incremental agent
text, thought and tool-start display. A live final `assistant_message` Entry therefore emits no
second standard transcript content. Resume suppresses replayed Entries, so it never combines a
historical assistant projection with retained live chunks. A negotiated Rudy client consumes only
the event metadata from authoritative carriers and ignores display-only successors, so fan-out
neither duplicates nor drops its event stream.

An attachment made while a Turn is already streaming has not observed that Turn's prefix. The
dispatcher marks that attachment and Turn display-incomplete, suppresses later Part carriers and
projections for that pair and projects the complete final assistant Entry as though it were replayed.
It clears the mark at terminal state or detach. Attachments that observed the Turn from its start
keep live Parts and omit the final duplicate. This rule applies to generic and negotiated clients;
the negotiated Client receives either the post-attach Part suffix never, then the complete Entry,
or the complete live Part sequence plus the ordinary final Entry event. It never receives a
misleading suffix-only transcript.

The adapter rejects translated prompt content whose marshaled internal `UserMessage.Content`
exceeds 4 MiB or whose complete public typed `user_message` Entry projection exceeds 7 MiB before
`session.submit`. Public Entry and Part JSON through 7 MiB is fragmented
byte-exactly. A larger valid replayed or live value emits one `event.oversized` reference and no
content projection; it never poisons later load or reconnect. A negotiated client receives the
metadata-only `event.oversized` carrier. A generic client receives no update for that value
because no honest standard projection exists, but the connection may continue. The daemon itself
emits a bounded internal `event.oversized`
notification instead of an Entry or Part notification when either its exact private internal JSON
or public projection exceeds 7 MiB, so
the 16 MiB internal frame cap cannot poison replay. LocalProtocolAdapter publishes the same
`OversizedEvent` for that reference and remains byte-exact through 7 MiB.

Standard carriers, negotiated permission requests and `_rudy/session/update` notifications use
the same sequence. Display-only projections, ACP responses and other non-event messages allocate
none. The Rudy client feeds all three carrier paths into one reorderer for its current connection
generation. It publishes only the next expected sequence, then drains contiguous buffered
successors, stripping connection sequence before the Client event. A missing, zero, fractional,
unsafe, duplicate or decreasing sequence is a protocol failure. A later sequence waits in the
reorder buffer without a timer because a delayed SDK
request callback is indistinguishable from a missing carrier. The reorder buffer shares the
client event queue's count and byte limits; overflow closes the connection and wakes pending
operations. EOF with a gap is a protocol failure. Reconnect discards the buffer, releases every
pending permission without authorization and expects sequence 1; callbacks retained from the
old connection generation cannot publish or resolve work on the new one. A generic client that
did not negotiate `session.update` receives no Rudy sequence metadata.

An ACP `session/request_permission` for a negotiated Rudy client carries this exact metadata;
the standard tool call remains the generic-client view:

```json
{"_rudy":{"version":1,"sequence":7,"event":{"kind":"permission.requested","sessionId":"01K...","turnId":"01K...","toolUseId":"toolu_01","projectedToolCallId":"rudy-call-sha256-7f...","tool":"bash","inputJsonBase64":"eyJ...=","matcher":{"tool":"bash","prefix":"go test"}}}}
```

`inputJsonBase64` uses padded RFC 4648 base64 over the exact valid JSON input bytes, which must be
at most 1 MiB before base64, and the complete encoded permission request must be at most 7 MiB.
Its `projectedToolCallId` must equal the standard ACP tool call id and the deterministic projection
of `turnId` plus raw `toolUseId`. The raw id remains only in Rudy metadata. Also,
`matcher.tool` must equal `tool`. Before typed decode or publication, the client runs decoded
input through the shared strict-JSON scanner and requires one object. Invalid base64, invalid UTF-8,
duplicate keys, structural-limit failure, a non-object value or inconsistent metadata is a
protocol failure at the Rudy client edge. It never compares re-marshaled ACP `rawInput` bytes.
When the exact input or complete request cannot fit, the adapter allocates no permission request,
emits one negotiated `event.oversized` reference, retires this asker's callback without
authorization and calls internal `session.decline_permission` for this exact question. Another
asker remains free to decide. If every attached asker has disconnected or declined, the Gate
records the ordinary `no_asker` denial. A generic client receives no update and no question
because it did not negotiate the only honest metadata carrier; this asker's callback is declined
by the same internal path. Raw input stays out of logs and errors.

The Rudy client registers the inbound permission callback by connection generation, closed outer
attachment Session, originating Session, Turn, tool-use id and an adapter-minted request token before publishing its
`PermissionRequested` event. It mints an opaque canonical ULID `permissionId` unique for the
`SessionClient` lifetime; ACP sequence remains private to event ordering and never enters the
Client event. Duplicate delivery of the same standing question is suppressed before sequence
allocation. Every resolution or cleanup compares the request token before deleting the map entry
or completing the callback. `AnswerPermission` completes only that exact pending callback.
Prompt cancellation and Session close retire it without authorization; when its sequenced request
was already enqueued, they wait for physical-write acknowledgment before cancelling the callback
so the live connection keeps a gap-free sequence. If no carrier was enqueued, they retire it
immediately. Disconnect retires immediately because no connection remains to preserve. A decision
from another internal asker makes the ACP agent cancel its outbound permission request and retire
the Client id, but does not cancel the Turn. A late or duplicate answer never reaches
`session.answer` or resolves a later request that reused a tool-use id.

Permission option ids are exactly `allow_once`, `allow_always` and `reject_once`, with matching
ACP kinds. Only an option offered on that exact pending request is accepted. `allow_once` maps
to allow once, `allow_always` to allow Session and `reject_once` to deny once. A `cancelled`
outcome sends no `session.answer` and idempotently cancels the Turn; it is never authorization.
The agent uses internal `session.cancel_permission`, not general `session.interrupt`, so an eligible
parent attachment may cancel the exact routed child question without gaining authority over any
other child work or successor Turn. If `not_found` or `conflict` proves another asker already
decided, mark this request superseded and leave that Turn alone; the valid cancelled outcome is then
an idempotent no-op.
For a negotiated Rudy client, a selected outcome carries exactly
`_meta._rudy {version: 1, reason?}` on the selected outcome object. The metadata object is required;
`reason` is optional and, when present, is a UTF-8 string of
at most 4096 bytes and may be empty. The agent passes a nonempty value unchanged to
`session.answer`; empty or omitted reason becomes the fixed nonempty value `asker`. A generic
client omits this metadata and therefore also records `asker`. Present malformed, oversized or
unnegotiated Rudy reason metadata sends no answer, invokes `session.cancel_permission` for the
recorded exact question and closes the adapter. It waits durable terminal proof only when that
cancel succeeds; `not_found` or `conflict` means another decision won and closes without claiming a
cancellation. An empty outcome or unknown, unoffered or stale option id follows the same
exact-question cancel-and-close path. A late response after that exact request already resolved is an
idempotent no-op. No malformed outcome defaults to allow.

A syntactically valid JSON-RPC error response to the outbound permission request is different from
a malformed outcome or explicit cancellation. The agent claims only that callback token, calls
`session.decline_permission` with its recorded exact Session, Turn and canonical tool-use
identity, then closes the ACP connection regardless of whether decline returns success,
`not_found`, `conflict` or a transport error. It sends no `session.answer` and no
`session.cancel_permission`. Another asker remains eligible; if none remains, ordinary
disconnect/decline handling durably records `no_asker` for the group.

Cancellation of the agent-to-client JSON-RPC request because another internal asker already
decided is not a client `cancelled` outcome. The agent marks that request as superseded, waits for
physical-write acknowledgment when its sequenced request was already enqueued, then cancels its
context. It sends no `session.answer` and leaves the already-decided Turn alone. The Rudy client
retires the matching `permissionId` when that callback context ends.

Prompt and steer share one content translator. ACP text becomes one Rudy text block with the
same string. An ACP resource link becomes one Rudy text block prefixed `ACP resource link (not
fetched): ` followed by compact JSON containing `name`, `uri` and any present `title`,
`description`, `mimeType` and `size` in that order. The adapter ignores annotations and `_meta`
and never resolves, opens or fetches the URI. It rejects image, audio and embedded-resource
blocks as invalid params because their capabilities are not advertised. Empty translated
content is invalid.

#### Rudy extension methods

`InitialSessionState` is the exact lower-camel object `{sessionId, modes, configOptions}`. `modes`
is required and is exactly `{currentModeId, availableModes:[{id,name,description?}]}`.
`configOptions` is required and contains only select variants exactly
`{type:"select",id,name,description?,category?,currentValue,options:[{value,name,description?}]}`.
Absent optional descriptions and categories are omitted, never null. `RegistryResult` is exactly
`{fetchedAt,models:[{ref:{provider,model},displayName,contextWindow,maxOutput,pricing:{input,output,cacheRead,cacheWrite},capabilities:{tools,vision,reasoning},upstream?}]}`;
`fetchedAt` is RFC 3339 with nanoseconds, absent `upstream` is omitted and each missing source price
maps to its exact empty string in the required `pricing` object. The internal Registry catalogue
projection is capped at depth 56 and 60,000 structural units in addition to 4 MiB. The fixed conversion and ACP
envelope add fewer than 5,536 units and fewer than eight depth levels; cross-package fixtures prove
that budget against both exact encoders, leaving the kernel free of ACP types. The Registry and command
catalogue byte and outer-structural bounds above apply before the complete internal response is encoded, so ACP handlers
acquire their large construction reservation before issuing the internal request and never first
decode an unbounded array.

| Capability | Method | Side | Params | Result | Internal mapping |
|---|---|---|---|---|---|
| `session.fork` | `_rudy/session/fork` | agent | `{sessionId, atEntryId}` | `InitialSessionState` | `session.fork` |
| `session.steer` | `_rudy/session/steer` | agent | `{sessionId, prompt: [ACP prompt ContentBlock]}` | `{turnId}` | Translate content through the exact `session/prompt` translator, then call `session.submit` with `source: steer` |
| `session.compact` | `_rudy/session/compact` | agent | `{sessionId, instructions?}` | `{entryId}` | `session.compact` |
| `session.shell` | `_rudy/session/shell` | agent | `{sessionId, command}` | `{entryId, isError}` | `session.shell` |
| `session.title` | `_rudy/session/set_title` | agent | `{sessionId, title}` | `{entryId}` | `session.set_title` |
| `command.list` | `_rudy/command/list` | agent | `{}` | `{commands: [{name, description}]}` | `command.list` |
| `command.run` | `_rudy/command/run` | agent | `{sessionId, name, args}` | `{turnId, notice, sessionId, stopReason?}`; returns immediately without `stopReason` when `turnId` is empty. When `turnId` is nonempty, keeps the request open through that Turn's terminal Entry and state, then requires normalized `stopReason` or the same fixed failure used by Prompt | `command.run`, then the Prompt completion barrier when it starts a Turn |
| `registry.list` | `_rudy/registry/list` | agent | `{provider?}` | `RegistryResult` | `registry.list` |
| `registry.refresh` | `_rudy/registry/refresh` | agent | `{provider?}` | `RegistryResult` plus required `failures: [{provider, error: "Refresh failed; see box log"}]` | Call `registry.refresh`; replace every nonempty internal failure error with the fixed public text |
| `server.shutdown` | `_rudy/server_shutdown` | agent | `{}` | `{instanceId, state: "stopped"}` only after terminal proof | Before forwarding internal `server.shutdown`, request cancellation may reserve `-32800`. After forwarding, record cancellation intent but await the internal response: rejection may still return cancellation, while accepted `shutting_down` proves the one-way shutdown committed and makes the ACP request non-cancellable. Keep it open, require matching internal `server.stopped` followed by internal EOF, then return and exit. Bare internal or ACP EOF is never proof. |
| `session.update` | `_rudy/session/update` notification | client | exact closed union below | none | status, widget, plugin, registry and sanitized notice events; also enables versioned metadata on standard updates and permission requests |

Every `_rudy/session/update` has params from this closed union. External field names use lower
camel case. These v1 events mirror the existing process-wide client shapes, carry no
`sessionId` and require the next connection sequence in `_meta._rudy.sequence`.

| kind | exact params shape |
|---|---|
| `status` | `{_meta: {_rudy: {version: 1, sequence}}, kind: "status", payload: {items: [{owner, key, content: [{text, role}]}]}}` |
| `widget` | `{_meta: {_rudy: {version: 1, sequence}}, kind: "widget", payload: {owner, key, slot, content: [{text, role}]}}` |
| `plugin_state` | `{_meta: {_rudy: {version: 1, sequence}}, kind: "plugin_state", payload: {name, origin, state, reason}}` |
| `registry_state` | `{_meta: {_rudy: {version: 1, sequence}}, kind: "registry_state", payload: {fetchedAt, providers: [{name, count, error}]}}` |
| `notice` | `{_meta: {_rudy: {version: 1, sequence}}, kind: "notice", payload: {level, text}}` |

Status and widget payloads are already explicit user-facing plugin output and their identity and
Span validation occurs before mutation as specified above. A failed plugin
state sets `reason` to fixed `Plugin failed; see box log`, never raw `reason`; other states set it
to the empty string. A registry state replaces every nonempty
provider error with `Refresh failed; see box log`. A notice sets `text` only from valid bounded
`public_text`, substituting `Remote notice; see box log` when absent, invalid or overlong and
never falling back to internal `text`. Receiving a child Session id grants no authority to create
a child or forge
`ParentRef`; child creation stays on the internal plugin caller path.

#### Prompt completion

`session/prompt` does not finish at an assistant `tool_use`: tool execution and subsequent
model calls remain inside the standing request. The terminal mapping is exact:

| Rudy terminal condition | ACP result |
|---|---|
| `end_turn` | `PromptResponse{stopReason: "end_turn"}` |
| `max_tokens` | `PromptResponse{stopReason: "max_tokens"}` |
| `refused` | JSON-RPC `-32603`, `Internal error`; the redacted terminal Entry still reports normalized Rudy reason `refused` to a negotiated client |
| `interrupted` caused by `session/cancel` | `PromptResponse{stopReason: "cancelled"}` |
| `interrupted` caused by Rudy steer | not terminal; the standing prompt continues through the steered model call |
| `other` | JSON-RPC `-32603`, `Internal error`; negotiated metadata still carries the terminal Entry before the error |
| `tool_use` while the Turn remains active | not terminal; no ACP response yet |
| completed Turn whose final reason is `tool_use`, or completed Turn with no terminal assistant Entry | JSON-RPC `-32603`, `Internal error`; this is a Rudy invariant failure |
| `turn_failed` class `provider`, `transport`, `plugin` or `internal` | JSON-RPC `-32603`, `Internal error`; the raw message and provider detail remain in the box log and durable Rudy entry |

A turn-starting `_rudy/command/run` uses the same terminal table and does not answer at Turn
admission. Its success requires `_meta._rudy {version: 1, turnId}` and `stopReason`; its
post-start error data requires the same correlation plus the safe `kind` when applicable. The
Client requires both the correlated response and reordered terminal Entry and state before
returning `RunCommand`. The response's top-level `turnId`, metadata `turnId`, internal
`command.run.turn_id` and terminal carrier Turn id must all be equal and nonempty; any mismatch is
a protocol failure. A command that starts no Turn returns immediately with an empty top-level
`turnId` and carries no terminal metadata.

Every negotiated prompt success carries
`_meta._rudy {version: 1, promptId, turnId}` in its `PromptResponse`. Every error after the Turn
starts carries the same object in `error.data`; when the underlying closed Rudy error kind is
`provider_error`, `transport_error`, `plugin_error` or `internal`, that object also requires
`kind` with exactly that value. Fixed public error text remains unchanged and no raw detail
accompanies the kind. An error after the agent accepts `promptId` but before the initial typed
Entry carries `version` and `promptId` with no `turnId`, plus the safe `kind` when applicable,
retires admission immediately and waits for no terminal proof. The Rudy client correlates only
after its reorderer publishes the initial carrier, then requires its `promptId` and `turnId` to
match every post-start success or error. Generic responses and errors carry no Rudy correlation
metadata.

For a correlated terminal `turn_failed`, the safe kind is mandatory and maps its carrier class
exactly: `provider` to `provider_error`, `transport` to `transport_error`, `plugin` to
`plugin_error` and `internal` to `internal`. The Rudy client requires the response kind to match
the reordered carrier class before returning a typed Turn failure; absence or mismatch is a
protocol failure. The same rule applies to a turn-starting RunCommand.

ACP `refusal` is deliberately not emitted. Stable ACP says a refusal causes the user's prompt
and everything after it to be excluded from the next model request, while Rudy currently keeps
a provider refusal in Session context. Returning `refusal` would make the generic client's view
disagree with the daemon; returning `end_turn` would report a safety refusal as successful.
ACP `max_turn_requests` is also not emitted: Rudy's current
`max_turns` path is an internal `turn_failed`, not a typed terminal condition, and the adapter
must not infer one by matching failure text. `other` is an error because built-in codecs use it
for unknown terminal reasons and context-window exhaustion; reporting success could make a
generic client act on incomplete output.

ACP `session/cancel` is the semantic Turn cancellation and yields the `cancelled` prompt
result above. JSON-RPC `$/cancel_request` aborts only the named request before that request's
response reservation. When it names a standing prompt, it cancels the internal Turn so work is not
orphaned, waits for its matching terminal Entry and state, then reserves `-32800`,
`Request cancelled`. While `session.submit` is in flight, it records intent without issuing
Session cancel; a pre-start error may then reserve `-32800`, while a returned Turn id cancels only
that exact Turn and waits for terminal proof. A turn-starting `_rudy/command/run` follows that same rule after its Turn id
exists. Before the internal command result arrives, cancellation records intent without reserving
a response; an empty `turn_id` or pre-start error may then reserve `-32800`, while a nonempty id
cancels that exact Turn and waits for terminal proof before reservation. A close request becomes
non-cancellable when its Session fence wins the atomic close commit point. Connection loss or
cancellation of another non-prompt request uses the same request-cancel result when a response
channel still exists and no response is reserved. `_rudy/server_shutdown` records cancellation
after internal dispatch but cannot act on it until the internal response proves whether shutdown
committed; an accepted shutdown then retains terminal ownership and ignores cancellation. Ingress
order decides if both
cancellation forms arrive: a prior semantic cancel converts a racing lower-layer failure into a
successful `cancelled`, while a prior request cancel keeps `-32800`; the later cancellation only
performs idempotent cleanup. A cancellation after any terminal prompt result is a no-op.

Prompt correlation is one generation per Session. The generation binds the inbound prompt
request id, the first exact typed `user_message` Turn id, cancellation classification and one
terminal result. A concurrent second prompt for that Session is conflict before SDK dispatch;
different Sessions remain concurrent. Cancellation of the SDK callback context never completes
the prompt by itself. The agent first sends the internal cancel, waits for the matching durable
terminal Entry and state, then maps the result above. Terminal completion and cancellation
compete in one
atomic generation state, so neither can complete a later prompt. The agent retires per-Session
prompt admission before enqueueing the terminal state carrier; writing the old prompt response
may finish later without blocking a new Turn.

#### Error mapping

For every negotiated request outside Prompt whose underlying Rudy code is `provider_error`,
`plugin_error` or `internal`, `error.data._rudy` carries exactly
`{version: 1, kind}` with that closed kind. Prompt errors add the correlation fields above.
Clients reject an unknown kind or a kind attached to another public error code. The discriminator
is safe classification only: paths, provider bodies, nested errors and raw messages remain absent.
`transport_error` is reserved for a correlated Prompt or turn-starting RunCommand whose terminal
`turn_failed.class` is `transport`; ordinary request transport loss is a SessionClient connection
failure because no ACP response exists to carry error metadata.
Every adapter-generated negotiated `-32603` carries `_rudy.kind: "internal"`; Prompt and
turn-starting RunCommand also carry their required correlation fields.

| Rudy code | ACP or JSON-RPC code | Public message |
|---|---|---|
| `invalid_argument` | `-32602` | validated argument reason |
| `not_found` | ACP resource not found, `-32002` | sanitized resource and id |
| `no_asker` | `-32010` | `No permission asker attached` |
| `conflict` | `-32011` | sanitized conflict reason |
| `unauthorized` | `-32012` | `Unauthorized` |
| `refused_by_invariant` | `-32013` | sanitized invariant reason |
| `unavailable` | `-32014` | sanitized availability reason; internal socket paths are removed |
| JSON-RPC request cancellation | `-32800` | `Request cancelled` |
| `provider_error`, `transport_error`, `plugin_error`, `internal` | `-32603` | `Internal error`; correlated detail only in the box log |

Unknown paths, credentials, provider bodies and raw prompt or tool payloads never enter an ACP
error or `_rudy` notice. A malformed JSON-RPC envelope uses the standard parse, invalid request
or method-not-found code.

### Caller classes

| class | transport | authentication | may assert | must never assert |
|---|---|---|---|---|
| TUI client | in-memory, unix socket | in-memory: trusted by construction; socket: directory `0700`, socket `0600`, peer uid equals server uid via `LOCAL_PEERCRED` or `SO_PEERCRED` | user messages, permission answers, model, mode, thinking, title, attach and detach, slash commands | entries of any other kind, tool results, registrations, registry contents |
| headless client | in-memory, unix socket | as TUI client | user messages, commands, attach without asker | permission answers; it declares `asker: false` in hello and the Gate treats it as absent |
| spawned plugin | stdio | spawned by the server from a manifest the user placed in config; identity is the manifest name | registrations under its own name, results for its own tools, hook returns, notes, status and widgets under its own name, child sessions it opens and messages to those | user messages to sessions it did not open, permission answers, items under another plugin's name, entries directly |
| linked plugin | in-memory Go interface | compiled in; trusted by build | as spawned plugin | as spawned plugin |
| ACP adapter | unix socket | same-user marker minted by the listener; the outside ACP client is local to the box or authenticated by ssh, but no ACP claim replaces the marker | user messages, permission answers, model, mode, thinking, title, attach and detach, slash commands and Server shutdown | entries of any other kind, tool results, registrations, registry contents or `ParentRef` |

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

### Internal daemon capabilities

`client.hello.capabilities` is a sorted, duplicate-free set from this closed table. A Server
advertises a name only when the complete invariant is implemented. Unknown names are ignored by
clients; a client fails closed when one of its required names is absent.

| name | guarantee |
|---|---|
| `bounded_session_list_v1` | `session.list` applies bounded store traversal, bounded response sizing and keyset pagination exactly as its request row specifies. Historical overlong titles are omitted and invalid or overlong ModelRef fields use stable digest aliases in summaries before response sizing, so one legacy record cannot poison the internal frame. |
| `bounded_session_events_v1` | Replay and live Entry or Part notifications use the ordinary exact internal notification only when both private JSON and public projection fit through 7 MiB and the public projection passes the decoded-event structural profile; otherwise they use the bounded internal `event.oversized` reference. Every provider or hook tool input is a valid UTF-8 strict JSON object with recursively unique keys and the shared nested-input depth, structural-unit, per-array and per-object limits. Inputs are rejected before commit above 7 MiB individually, 16 MiB per provider response or 32 MiB cumulatively per Turn counting accepted hook replacements. Tool-use ids remain unique throughout the active Turn, matcher values and decision reasons are bounded and every complete `permission.requested` notification is preflighted through 8 MiB, so no valid persisted or permission event can exceed the 16 MiB internal frame path or let a stale answer select a later question. An unexpected permission-notification preflight overflow emits no question, appends a bounded invariant denial plus error result, terminalizes peer calls and fails the Turn with a bounded internal `turn_failed` Entry. A call remains non-runnable in `gating` until its Gate decision is complete; an unsafe call reaches `queued` only after its exact allow decision is appended and fsynced. Coalesced calls have one offered canonical question identity and a bounded member set; only that offered identity accepts an answer. Gate freezes the complete resolved set, appends every member decision as one scheduling batch, syncs the batch when it allows and only then releases any member to queue. Batch failure queues none and quarantines the Session. Interrupt and failure terminalize every admitted call with a decision before its result. `session.interrupt` with a Turn id atomically targets only that active Turn and never falls through to a successor. Every permission answer atomically matches the exact standing Session, Turn and canonical tool-use identity. The authenticated per-question `session.decline_permission` operation removes an ACP asker that cannot represent an otherwise valid input without granting or blocking another asker. The authenticated exact-question `session.cancel_permission` operation cancels only that question's Turn and waits for durable terminal proof. `session.resume` omits a legacy overlong title and fails unavailable before response construction when the current operational ModelRef is invalid UTF-8 or exceeds the scalar bound. `session.fork` applies the same projection and validates the ModelRef derived at its exact fork point before mutation. |
| `bounded_process_events_v1` | Every replayable process event fits individually through 4 MiB in both its complete internal notification and safe public projection and its complete worst-case ACP carrier passes the outer structural profile, sized with `sequence: 9007199254740991`. Process identity fields remain exact and, with status/widget Span text, are admitted only when valid UTF-8 through 4096 bytes; Span roles come from the closed Theme role set and no process field uses a digest alias. The full connect snapshot contains at most 64 events and fits through 32 MiB in both exact encoded forms. The complete internal Registry catalogue fits through 4 MiB, depth 56 and 60,000 structural units, its fixed ACP conversion fits the outer profile and its immutable result carries the committed runtime revision used by exact-revision Session open and set-model. The complete `client.hello` response is physically written first. Then, under the Server-owned coordinator lock, the connection captures current state, reserves and enqueues the ordered snapshot and becomes a live process subscriber before the lock releases. Every later mutation enqueues after that snapshot under the same ordering boundary, so connect cannot miss or regress state. The coordinator also serializes byte and structural preflight plus commit across Plugin admission, status, widget and Registry mutations, including ordered Registry cache replacement, and atomically retains prior state on overflow or cache-write failure. Plugin admission reserves its bounded failure form so a later failure always commits; live notices use a fixed bounded replacement when needed. |
| `terminal_turn_durability_v1` | Every terminal Turn state follows successful sync of its matching terminal Entry; sync failure returns an error, enters the Server's shared Session-durability quarantine and closes its subscribed connections without publishing terminal success. |

### Requests, client to server

Authn column names the caller class table. Domain column names the aggregate method or says query.

Every row below that targets an existing Session also returns `unavailable` when that Session is
quarantined after terminal Entry sync or permission decision-batch append/sync failure.
`session.list` may still report its durable
summary, but no attach, mutation, answer, command or close operation may use its in-memory
aggregate until a new Server process performs ordinary log recovery.
Every protocol operation, Turn or Gate append and plugin Host call enters one Server-owned
per-Session admission fence for its immediate Session read, mutation or attachment commit.
Transport waits and provider I/O occur outside and must reenter for each later commit. Quarantine
takes the same fence; if the fenced commit returns a durability cause, that cause is installed
before release. Thus an attach or `Host.Note` either commits before quarantine and is included in
its subscriber or cancellation snapshot, or observes quarantine and changes nothing. An earlier
availability check alone never authorizes a commit.

The `session.open` request row below additionally accepts `registry_revision?: uint64`. When
present, Server verifies and resolves the effective ModelRef under that exact Registry revision
before `Session.Open`; stale revision is `conflict` and no Session entry is appended. Omission keeps
the existing caller behavior.

| method | caller | authn | authz | request | response | errors | idempotency | domain |
|---|---|---|---|---|---|---|---|---|
| `client.hello` | TUI, headless, ACP, plugin | per class | first request on a connection; refused otherwise | `{client, version, asker: bool, process_events?: bool}`; client and version are valid UTF-8 through 4096 bytes and absent `process_events` means true | `{server, version, home, instance_id, capabilities: [string]}`; `home` is the server process's home directory, which the ACP adapter returns in negotiated `_meta._rudy` so a Mac client can place the workspace (`<home>/<cwd relative to the local home>`); `instance_id` is the runtime-only Server ULID and lets a reconnect prove replacement; `capabilities` follows the closed internal capability table; a client whose `version` differs from the server's carries on and shows a notice only after checking its required capabilities (ADR 0029, ADR 0030, ADR 0031, ADR 0032, ADR 0033) | `invalid_argument` for malformed fields, invalid UTF-8 or an overlong client/version; version inequality alone is never an error | idempotent per connection; a second hello is `refused_by_invariant` | registers the connection as an asker or not and identifies the Server. Its complete response is physically written before process snapshot admission. After that acknowledgment, the ProcessSnapshotCoordinator lock covers current-state capture, reservation and ordered enqueue of the bounded snapshot plus activation of live delivery; a mutation either precedes the captured state or enqueues after the whole snapshot. `process_events: false` suppresses both that snapshot and later process-wide notifications on this connection. A plugin connection may send hello (it needs no introduction, so it usually does not); its `asker` is ignored and `process_events` is forced false because the plugin holding a child session is the one waiting on that child's tool call, so its own question must never route back to it |
| `server.shutdown` | TUI, headless, ACP | Unix socket under the class rule | greeted non-plugin connection carrying the same-user marker minted by the accepting Unix listener; the `client.hello.client` string and ACP metadata grant nothing | `{}` | `{instance_id, state: "shutting_down"}`; the response is physically written before shutdown is requested | `unauthorized` for a plugin, in-memory connection or connection without the marker; `refused_by_invariant` before hello or after shutdown was reserved | not idempotent on one Server; the first accepted request reserves shutdown and fences later work while public state remains `running`. A failed response releases the reservation. `rudy hosts stop` treats no answering Server as an idempotent success at the CLI boundary after ADR 0031 | `Server.RequestShutdown`; the request's connection and writer remain open for terminal proof |
| `session.open` | TUI, headless, ACP, plugin | per class | any; `parent` only from a plugin, and only naming a `tool_use` pending in a live session whose tool that same plugin registered, in a session that is not itself a child, at most once per `tool_use`, and with `cwd` equal to that session's workspace root | `{cwd, model?: string, mode?: PermissionMode, thinking?: ThinkingLevel, agent?: string, tools?: [string], parent?: ParentRef}`; `model` is `provider:id` or a unique bare id; absent `model`, `mode`, `thinking` take the agent definition's values, then the parent's when `parent` is given, then config defaults; absent `agent` means the default agent. A session's tool set is the agent definition's list, intersected with `tools` when given, intersected with the parent's effective set when `parent` is given, minus `agent` for a child. Every term only removes: `tools` names what to keep and cannot name a tool the definition or the parent withheld, so delegation never widens what the caller holds (ADR 0028). Absent `tools` narrows nothing and an explicit empty list narrows to no tools at all, the same distinction the definition file carries. A name in `tools` that no plugin has registered is dropped before the set is persisted, so a session cannot acquire a tool later by having named one that did not exist when it opened | `SessionInfo`, same shape as `session.resume`; the `session_opened` entry is replayed first as `entry.appended`, this response returns once replay finishes | `invalid_argument` root not a directory, `parent` from a non-plugin, or `cwd` not the parent's workspace root; a name in `tools` that the definition or the parent did not hold is dropped rather than refused, since the set is an intersection and a caller asking for less than it is owed is not an error; `not_found` model, agent, parent session or parent tool_use; `unauthorized` the pending `tool_use` is not a tool of the calling plugin; `refused_by_invariant` the parent is itself a child session; `conflict` that `tool_use` has already opened a child; `unavailable` registry unreachable and no cached snapshot | not idempotent; every call opens a session | `Session.Open` factory; appends `session_opened` with `parent_session_id` and `parent_tool_use_id` |
| `session.resume` | TUI, headless, ACP | per class | any session on this machine | `{session_id}` | `SessionInfo`: `{session_id, workspace: Workspace, model: ModelRef, mode: PermissionMode, thinking: ThinkingLevel, title: string}`; before replay or attach, require both current ModelRef fields valid UTF-8 through 4096 bytes and project a legacy invalid or overlong title as empty. Every entry is replayed first as `entry.appended` notifications, then, when a turn is active, the current `turn.state`, a `tool.state` per call in flight and any standing `permission.requested` to an asker connection, then, for each live child of the session, that child's `session_opened` as `entry.appended` and any question standing on it, then this response | `not_found`; `unavailable` locked by another process, `data.socket` or current operational ModelRef cannot fit the bounded response | idempotent; re-attaches | `Session.Load` then bounded-state validation, recovery and attach. An unrepresentable current ModelRef fails before notifications or attachment because an alias would change provider semantics |
| `session.fork` | TUI, headless, ACP | per class | any session | `{session_id, at_entry_id}`; `at_entry_id` empty means the newest entry | `SessionInfo` for the new session, same bounded shape as `session.resume`. Before mutation, derive the exact ModelRef and title at the fork point, require both ModelRef fields valid UTF-8 through 4096 bytes and project a legacy invalid or overlong title as empty. Its entries are replayed first as `entry.appended`, this response returns once replay finishes | `not_found` session or entry; `unavailable` the resulting operational ModelRef cannot fit the bounded response | not idempotent; validation or response preflight failure creates no child | `Session.Fork(at)` after bounded-state and complete-response preflight; appends `fork_point` |
| `session.list` | TUI, headless, ACP | per class | any | `{cwd?: string, before_id?: ULID, limit?: int}`; `cwd` must be absolute and is normalized before comparison with the primary Workspace root, `before_id` means strictly older than that ULID, and `limit` is 1 through 100 with default 100 | `{sessions: [SessionSummary], next_before_id?: ULID}`; sorted by descending Session ULID after filtering. Return the largest ordered prefix within `limit` whose encoded internal response stays below 4 MiB. Omit an invalid or overlong title and replace an invalid or overlong historical ModelRef field with `rudy-provider-sha256-<lowercase hex SHA-256>` or `rudy-model-sha256-<lowercase hex SHA-256>` before sizing. These are display-only summary aliases and are never accepted by `session.set_model`. If that sanitized Session still cannot fit, return `internal` rather than skip it. `next_before_id` is the last returned ULID only when another match exists | `invalid_argument` for an invalid cwd, cursor or limit; `internal` when one sanitized record cannot fit | idempotent; no mutation. A later newly opened ULID sorts above an existing continuation and does not shift it | bounded query over `session_opened` and last entries. Store traversal uses `ReadDir(128)` batches and a bounded min-heap retaining the newest `limit+1` matching summaries by ULID before sorting; filtered-out and older Sessions never accumulate. Clients page explicitly and never aggregate an unbounded result |
| `session.close` | TUI, headless, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id}` | `{}` | `not_found` | idempotent | detach; fires `session_closed` hook when the last client detaches; appends nothing, except that a last subscriber leaving a turn parked in `TurnSteering` cancels it, which appends `turn_interrupted` |
| `session.submit` | TUI, headless, ACP, plugin | per class | plugin only to sessions it opened; a session whose log records a parent refuses any caller, plugin or not, that is not subscribed to it, whatever it may be subscribed to instead. The test is the recorded parent rather than the live one, so a child resumed cold is still a child (ADR 0028 decision 6: watching a session's parent is not being subscribed to it) | `{session_id, content: [ContentBlock text or image], source: typed or steer}`; `shell` is written by `session.shell`, never submitted; pass 3: not yet implemented, `queued` (refused as `invalid_argument`) and `idempotency_key` | `{turn_id}`; pass 3: not yet reported, `entry_id` and `queued_position`, neither of which exists without a queue | `refused_by_invariant` typed while a turn is active, steer while not steering; `invalid_argument` empty content, a content block the log refuses or a source that is neither typed nor steer; `unauthorized` a plugin submitting to a session it did not open, or any caller submitting to a child session it is not subscribed to | not idempotent; pass 3 ignores `idempotency_key` because it does not accept one | `Turn.Start` for typed on idle, `Turn.Resume` for steer; appends `user_message` |
| `session.shell` | TUI, headless, ACP | per class | any attached | `{session_id, command}` | `{entry_id, is_error}` | `invalid_argument` empty command; `not_found` unknown session or no `bash` tool in this session's view; `conflict` a turn is active | not idempotent | invokes the registered `bash` tool with the command, ungated, and appends one `user_message` with `source: shell` carrying `$ <command>` and its output. Starts no turn: the model reads it with the next message. The Gate does not run, since the operator typed the command themselves (ADR 0023) |
| `session.interrupt` | TUI, headless, ACP, plugin (own sessions) | per class | a plugin only a session it opened; a plain client is not checked for attachment at all, which is pre-existing and true of every session method but `submit` (`rudy-jkz`); a session whose log records a parent refuses any caller not subscribed to it, on the same rule and for the same reason as `session.submit`, because routing a child's notifications to its parent's client is what made a running child's id reachable | `{session_id, how: steer or cancel, turn_id?: ULID}`; when `turn_id` is present, act only if it is the exact active Turn and never fall through to a successor | `{turn_id, state: TurnState}` | `refused_by_invariant` no active turn; `conflict` an explicit `turn_id` differs from the active Turn; `unauthorized` a child session from a connection not subscribed to it; `unavailable` when terminal Entry synchronization fails or the Session is quarantined | idempotent for the same active Turn; an explicit id can never affect a later Turn | `Turn.Steer` or `Turn.Cancel`; cancel appends and fsyncs `turn_interrupted` before publishing terminal state |
| `session.answer` | TUI, ACP | per class | connection declared `asker: true` and eligible for the exact standing question, including through a parent attachment | `{session_id, turn_id, tool_use_id, decision: allow or deny, scope: once or session, reason: string}`; reason is valid UTF-8 through 4096 bytes and empty normalizes to `asker` | `{}` | `unavailable` the decision batch cannot append or sync; `not_found` no exact pending question; `conflict` when another asker answered first or the active Turn differs; `unauthorized` not an eligible asker | keyed by the exact offered Session, Turn and canonical tool-use identity; a second answer is `conflict` | Atomically claim the same standing group and any Session-allowance-settled groups in the Gate. Never accepts an unoffered coalesced member id, falls through to a successor Turn or selects a reused tool id. Freeze their complete member sets, append every decision in stable Turn admission order as one scheduling batch and sync once when any decision allows. Only after the whole batch succeeds may allowed members enter `queued` or denied members finish. Failure queues none, quarantines the Session and returns unavailable |
| `session.decline_permission` | ACP | Unix socket under the class rule | greeted connection declared `asker: true`; non-authorizing edge cleanup only | `{session_id, turn_id, tool_use_id, reason: "unrepresentable"}` where `tool_use_id` is the canonical identity offered to that connection | `{}` | `unauthorized` not an ACP asker; `not_found` no exact pending question; `conflict` already decided | a repeated decline by the same connection succeeds while the question remains pending; after resolution it is `not_found` | Remove only this connection from the canonical question's eligible-asker set without recording a decision. Other askers remain eligible; when none remain, the Gate appends one ordinary `permission_decision{no_asker}` denial for every current member. Never grants authority or cancels another asker's answer |
| `session.cancel_permission` | ACP | Unix socket under the class rule | greeted connection declared `asker: true` and eligible for the exact standing question, including through a parent attachment | `{session_id, turn_id, tool_use_id}` where `tool_use_id` is the canonical identity offered to that connection | `{turn_id, state}` after durable terminal proof | `unauthorized` not an eligible ACP asker; `not_found` no exact pending question; `conflict` already decided; `unavailable` terminal sync failure | keyed by the exact offered canonical identity; after resolution it is `not_found` | Atomically verify the canonical question and cancel only its exact Turn, including every member in the bounded group. Never falls through to a successor, grants no child Session authority and appends the normal durable `turn_interrupted` before terminal state |
| `session.set_model` | TUI, headless, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id, model: ModelRef, registry_revision?: uint64}`; `registry_revision` binds resolution to the immutable internal Registry result used for response preflight and omission retains current behavior | `{entry_id}` | `conflict` a turn is active or the supplied Registry revision is stale; `not_found` model not in that Registry revision | same value appends nothing and returns the latest `model_change` id | resolve the model under the Registry revision read lock, then `Session.SetModel`; appends `model_change` |
| `session.set_mode` | TUI, headless, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id, mode: PermissionMode}` | `{entry_id}` | `conflict` a turn is active; `invalid_argument` | same value appends nothing | `Session.SetMode`; appends `mode_change` |
| `session.set_thinking` | TUI, headless, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id, thinking: ThinkingLevel}` | `{entry_id}` | `conflict` a turn is active; `invalid_argument` | same value appends nothing | `Session.SetThinking`; appends `thinking_change` |
| `session.set_title` | TUI, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id, title: string}` valid UTF-8 from 1 through 4096 bytes | `{entry_id}` | `conflict` a turn is active; `invalid_argument` empty, invalid UTF-8 or overlong | same value appends nothing | `Session.SetTitle`; appends `title_change` |
| `session.compact` | TUI, headless, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id, instructions?: string}`; `instructions` steers the model's summary and skips the `before_compaction` hook when present | `{entry_id}` of the `compaction`; `{entry_id: ""}` when the request context holds fewer than two entries to cover | `conflict` a turn is active; `provider_error` the summary request failed; `unavailable` the session's provider is not loaded | not idempotent | Compactor; appends `compaction` |
| `registry.list` | TUI, headless, plugin, ACP | per class | any | `{provider?: string}` | `{revision, fetched_at, models: [Model]}` where `revision` is the runtime-only snapshot revision and `fetched_at` is the latest successful provider refresh and does not advance on a failure-only commit; the complete encoded response is at most 4 MiB, the catalogue is at most 60,000 structural units and its worst-case ACP result envelope passes the outer structural profile by the RegistrySnapshot invariant | none | idempotent | query returns an immutable copy of the bounded Registry snapshot |
| `registry.refresh` | TUI, headless, ACP | per class | any | `{provider?: string}` | `{revision, fetched_at, models: [Model], failures: [{provider, error}]}` where `revision` identifies the immutable committed result and `fetched_at` has the same latest-success meaning as `registry.list`; the complete encoded response is at most 4 MiB and its worst-case ACP result envelope passes the outer structural profile | an upstream, scalar, duplicate-conflict, price-invalid, structural-limit or superseded provider result lands in `failures` with all of that provider's candidate models omitted while other provider results form the candidate; `internal` when the combined candidate or prospective process replay snapshot exceeds its bound or its cache replacement fails | idempotent | `Registry.Refresh`; assigns each requested provider a monotonic generation before fetching outside the process-snapshot admission lock. Under the commit lock, a result older than that provider's last committed generation becomes superseded, while successful remaining provider deltas rebase onto the latest Registry state. The coordinator streams the combined candidate through byte and structural admission, then orders cache replacement before in-memory publication. Admission or cache-write failure commits none of the request's deltas and retains the prior whole snapshot and cache. A non-superseded failure may commit bounded provider error state while retaining its models, but does not advance `fetched_at`; a committed state change advances `revision` exactly once |
| `command.list` | TUI, headless, ACP | per class | any | `{}` | `{commands: [{name, description}]}` in registration order; the complete encoded response is at most 4 MiB and its worst-case ACP result envelope passes the outer structural profile | none | idempotent | query over `PluginRegistry.Commands`; registration streams the prospective complete result through byte and structural admission before commit. The set is fixed once every plugin has answered `plugin.init`, so a client asks once on connect and there is no notification for it. A plugin is refused: it knows its own registrations and has no use for another's. `/exit`, `/quit` and `/scoped-models` are not in it, being the client's own (ADR 0015, ADR 0020) |
| `command.run` | TUI, headless, ACP, plugin (own sessions) | per class | any attached; a plugin only a session it opened | `{session_id, name, args: string}` | `{turn_id, notice, session_id}`; `turn_id` set for a command that submitted a prompt, `notice` for one that only reports, `session_id` set when the command opened another session, a fork | `not_found` unknown command or unknown session; `conflict` a turn is active and the command's action needs a resting session (`SubmitPrompt`, `Compact`) | not idempotent; the command decides | called whatever the turn is doing: a command is not a message and does not wait behind one, and the refusals above are what stop the two that cannot run mid-turn (ADR 0026). Runs the plugin.Command's `Run`, then maps its `Action` (`SubmitPrompt`, `Notice`, `Compact`, `SetModel`, `SetMode`, `SetTitle`, `Fork`, `NoAction`) onto the matching domain call. `SetTitle` names the session through the same path `session.set_title` takes, refusing an empty title the same way (ADR 0019); `SetMode` sets the permission mode through the same path `session.set_mode` takes, refusing an invalid one the same way |
| `plugin.register_tool` | plugin | per class | own name only | `{name, description, input_schema, safety: Safety}`; `input_schema` raw JSON | `{}` | `conflict` name taken; the plugin stays loaded and a `notice` is emitted | not idempotent; a second registration of a name already held is refused with `conflict` whatever it carries, since a plugin registering one name twice is a mistake rather than a retry, and the first registration is the one sessions were opened against | `PluginRegistry.RegisterTool` |
| `plugin.register_command` | plugin | per class | own name only | `{name, description}`; not yet implemented, `args` and their completion sources: the slash menu completes a command name, never its arguments | `{}` | `conflict` | not idempotent; a second registration of a name already held is refused with `conflict` whatever it carries | `PluginRegistry.RegisterCommand` |
| `plugin.register_hook` | plugin | per class | own name only | `{point: HookPoint, priority: int}` | `{}` | `invalid_argument` unknown point | idempotent | `PluginRegistry.RegisterHook` |
| `plugin.register_widget` | plugin | per class | own name only | `{key, slot: header, above_editor or below_editor, content: [Span]}`; key and every Span text are valid UTF-8 through 4096 bytes and every role is in the closed process Span role set | `{}` | `invalid_argument` invalid or overlong key or Span text, unknown Span role or slot, prospective individual encoding above 4 MiB or prospective full process replay snapshot above 64 events or 32 MiB | idempotent; re-registering replaces content | `PluginRegistry.SetWidget`; preflight scalar rules, both complete individual encodings and the complete replay snapshot before mutation and retain the prior widget on refusal |
| `plugin.set_status` | plugin | per class | own name only | `{key, content: [Span]}`; key and every Span text are valid UTF-8 through 4096 bytes, every role is in the closed process Span role set and empty content clears | `{}` | `invalid_argument` for invalid or overlong key or Span text, unknown Span role, prospective full status value above 4 MiB or prospective full process replay snapshot above 64 events or 32 MiB | idempotent | `PluginRegistry.SetStatus`; preflight scalar rules and both exact encoded forms before mutation and retain the prior set on refusal |
| `plugin.register_provider` | plugin | per class | own name only | `{name, wire: anthropic_messages, openai_chat or custom}`; name is valid UTF-8 through 4096 bytes; `custom` means the server calls `provider.complete` on the plugin; a spawned plugin may register only `custom`, since a codec wire needs the endpoint and credential config a linked provider plugin reads | `{}` | `conflict` provider name taken; `invalid_argument` an invalid or overlong name, a codec wire from a spawned plugin or an unknown wire | not idempotent; a second registration of a name already held is refused with `conflict` whatever it carries | `PluginRegistry.RegisterProvider` |
| `plugin.register_agent` | plugin | per class | own name only | `{name, description, prompt: string, tools?: [string], model: string, thinking: ThinkingLevel, max_turns: int}`; the same fields `agents/<name>.md` carries, since a definition is static data and needs no callback. Absent `tools` means every tool, an empty list means none, matching the file | `{}` | `conflict` a plugin already registered that name; `invalid_argument` empty description, or a `thinking` that is not off, low, medium or high | not idempotent; a second registration of a name already held is refused with `conflict` whatever it carries | `PluginRegistry.RegisterAgent`; read by `resolveAgent` after both disk roots, so a user or workspace definition of the same name wins and a plugin cannot take a name an operator is using (ADR 0028) |
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

A terminal `turn.state` is durability proof. The Server emits `completed` only after the matching
terminal `assistant_message` Entry is fsynced, `idle` after cancellation only after the matching
`turn_interrupted` Entry is fsynced and `failed` only after the matching `turn_failed` Entry is
fsynced. If terminal synchronization fails, the Turn runner emits no terminal state. It returns a
terminal durability error, quarantines the affected Session for the rest of that Server process
and closes every connection subscribed to that Session with fixed internal or connection failure
text. Later operations on that in-memory Session return `unavailable`; a daemon restart may run
ordinary log recovery. A synchronous `session.interrupt` cancel propagates the durability error.
The append and quarantine transition share the Session admission fence, so no racing attach or
mutation can land between the failed sync and the retained cause. No adapter may report terminal
proof or normalized success from an Entry whose sync failed.

A child session's `entry.appended`, `stream.delta`, `session.event_oversized`, `turn.state` and `tool.state` are also delivered to its parent's non-plugin subscribers, so a client watching a session sees the work it delegated (ADR 0028). They carry the child's `session_id`, which is what tells them apart from the parent's own; a client that keys on the session id was already correct and needs no change to stay correct. Depth is one, so the walk does not recurse. The plugin connection that opened the child is excluded, since it is the one waiting on the tool call and has no use for its own echo. A connection already subscribed to the child is excluded too, so a client watching the parent and reading the child receives each notification once rather than twice; `stream.delta` is never replayed and so could not be de-duplicated after the fact.

A client attaching to a parent is told about every child of it that is live at that moment, after the parent's own replay: one `entry.appended` carrying that child's `session_opened` entry, then, to an asker, every question standing on that child. The `session_opened` is what names the `agent` call the child's later notifications render under, and it is broadcast live exactly once, when the child opens, so a client that attaches after that moment would otherwise receive every one of the child's notifications and have nowhere to put any of them. The same exclusions the live routing applies apply here: nothing is sent to a plugin connection or to one already subscribed to the child itself. Depth is one.

Receiving a child's notifications is not subscribing to it, and a watcher may not drive what it watches: `session.submit` and `session.interrupt` refuse a child session from a connection that is not subscribed to it. This matters because the routing is what makes a running child's id reachable at all. Before it, that id reached a client only in the parent's `tool_result`, after the call had already returned.

Two things this does not claim. Answering is deliberately not gated, because a child with no asker of its own borrows its parent's, so a parent's asker answering a child's question is the mechanism working rather than a leak. And the remaining session methods are not subscription-gated for a plain client at all, which is pre-existing and applies to every session rather than to children (`rudy-jkz`); the two gated above are the ones this wave put within reach.

| notification | to | payload | delivery |
|---|---|---|---|
| `entry.appended` | every client attached to the session, and the parent's | `{session_id, entry: Entry}` | ordered by entry id; replayed on attach via resume. A child's are not replayed to the parent on attach, with one exception: a live child's own `session_opened`, which names the call its notifications render under and is otherwise broadcast only once, at open. The rest are not, because the parent's own log carries the `agent` tool's result and a finished child is read by resuming it |
| `stream.delta` | attached clients, and the parent's | `{session_id, turn_id, part: StreamPart}` | ordered; not replayed; superseded by the `assistant_message` entry |
| `session.event_oversized` | the recipients of the Entry or Part it replaces | `{session_id, original_kind: entry.appended or stream.delta, total_bytes, sha256, redacted, entry_id?, entry_kind?, turn_id?, projected_tool_call_id?, part_kind?, terminal_stop_reason?, failure_class?, retries?}` | The Server uses one shared boundary-safe projector to stream both the exact private internal value and canonical public projection through separate 7 MiB capture buffers while counting both, checking the public decoded-event structural profile and hashing only the complete public projection. It clears raw stop reasons and replaces failure text in the public stream before hashing, so `sha256` never names secret-bearing private JSON. Only when both captures fit and the public profile passes does it send the ordinary exact private notification. If either value crosses 7 MiB or the public projection crosses that profile, it discards both captures and sends this public reference without allocating either complete value. Entry references replay wherever that Entry would; Part references are live only. Child routing matches the replaced event. `turn_id` and the universal Turn-scoped `projected_tool_call_id` are both required for `entry_kind: permission_decision` or `tool_result`; the live emitter supplies the owning Turn and replay derives it while walking that Turn's timeline. This projection bounds even a legacy raw id that predates scalar admission. Every terminal Entry also requires `turn_id`. Every other Entry kind forbids `projected_tool_call_id`; other identity fields appear only where the replaced event contract requires them. A terminal Entry carries exactly one of `terminal_stop_reason` or `failure_class`. A `turn_failed` Entry with `failure_class` also requires `retries` from 0 through 2147483647; every other reference forbids it. `total_bytes` is the public projection size as the canonical decimal string defined above and `redacted` is true. |
| `turn.state` | attached clients, and the parent's | `{session_id, turn_id, state: TurnState}` | ordered; the current state is sent to a client attaching while a turn is active, after the replay and before the resume response. `RunningTool` means at least one tool call is in gating, queued or running, not that exactly one is: which calls and where each has got to is `tool.state` |
| `tool.state` | attached clients, and the parent's | `{session_id, turn_id, tool_use_id, name, state: gating, queued, running, awaiting_permission or done}` | ordered per `tool_use_id`; every call in flight is sent to a client attaching mid-turn, after the replay and before the resume response. `gating` is non-runnable. Not replayed afterwards: a finished call is its `tool_result` entry, and the log is what a client reads it from. Exists because tool calls run concurrently (ADR 0028), so a turn-level state cannot say which of several calls is gated, queued, running or waiting on the operator |
| `permission.requested` | attached asker clients | `{session_id, turn_id, tool_use_id, tool, input, matcher: {tool, prefix}}`; the complete encoded notification is at most 8 MiB | preflighted before the call leaves `gating`; an unexpected overflow publishes no question, appends a fixed invariant denial and error `tool_result` for that call, terminalizes every other admitted call by the rule below, then fails the Turn with a bounded internal `turn_failed` Entry. Otherwise delivered to every attached asker, to an asker attaching while the question stands, and to an asker attaching to the parent of a live child while a question stands on that child, since the child has no asker of its own and the parent's is who answers for it. Calls with the same matcher and exact input bytes form one group of at most 64 members. Its first member is the immutable canonical question identity carried by this notification; later members are not separately offered and their ids cannot answer it. The first exact `session.answer` for the canonical identity atomically freezes the current member set and any groups settled by Session allowance. Gate appends the complete decision batch and syncs it when allowing before it queues or finishes any member. A member arriving after the freeze re-evaluates only after that batch commits, so it observes the new allowance or forms a new question. With no asker attached the Gate denies immediately, and when the last asker detaches or declines while a question stands the Gate batches one `no_asker` decision per member before finishing any member (ADR 0014) |
| `status.updated` | every client | `{items: [{owner, key, content: [Span]}]}` full set | latest wins; sent on connect. Owner and key are exact validated identities, every Span has bounded text and a closed role, both complete encoded forms are at most 4 MiB and the complete worst-case ACP carrier passes the outer structural profile by the PluginRegistry invariant |
| `widget.updated` | every client | `{owner, key, slot, content: [Span]}` | latest wins per exact validated owner and key; all sent on connect. Every Span has bounded text and a closed role, each complete encoded form is at most 4 MiB and the complete worst-case ACP carrier passes the outer structural profile by the PluginRegistry invariant |
| `notice` | every client | `{level: info, warn or error, owner, text, public_text?}`; `text` is for same-process and same-host clients, while `public_text` is a trust-boundary-safe rendering that contains no raw nested error. ACP forwards only valid UTF-8 `public_text` through 4096 bytes, substituting `Remote notice; see box log` when absent, invalid or overlong. Registry refresh failure uses `Registry refresh failed; see box log` and keeps provider detail in the box log. | best effort; not replayed |
| `plugin.state` | every client | `{name, origin: linked or spawned, state: loading, ready, failed or stopped, reason: string}`; `reason` is empty unless failed and remains same-host detail when valid UTF-8 through 4096 bytes. An invalid or overlong failure is logged in full and stored as fixed `Plugin failed; see box log`. Safe public projection always replaces a failed reason with fixed `Plugin failed; see box log`; there is no second public reason field to reserve or spoof. | latest wins; all sent on connect. Plugin admission reserves its one snapshot event using the complete exact encoding with a worst-case 4096-byte escaped reason, so every later valid failure commits without crossing the process snapshot bound |
| `registry.updated` | every client | `{fetched_at, providers: [{name, count: int, error: string}]}`; `fetched_at` is the latest successful provider refresh and is unchanged by a failure-only commit; `count` is the retained last-good model count and `error` is empty after success | latest wins |
| `server.stopped` | the one connection whose `server.shutdown` request was accepted | `{instance_id, state: stopped}` | sent only after successful runtime and listener cleanup, then followed by EOF. Cleanup failure, process death or transport loss produces no notification, so bare EOF is failure |

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
| `ToolRequested` | Turn | add this call as non-runnable `gating`, then recompute coarse state from every call | `{session_id, turn_id, tool_use_id, tool, input}` | clients (`tool.state gating`, then derived `turn.state`), Gate, `before_tool` hook | sync | published as `before_tool` | Gate |
| `PermissionRequested` | Turn | after the complete notification passes its 8 MiB preflight, this call `gating` to `awaiting_permission`, then recompute coarse state from every call; preflight failure instead invokes `Turn.Fail` while the call remains non-runnable | `{session_id, turn_id, tool_use_id, matcher, mode}` | clients (`tool.state awaiting_permission`, derived `turn.state`, `permission.requested`) | sync; auto-deny when no asker | internal | Gate |
| `ToolQueued` | Turn | this call from `gating` or `awaiting_permission` to `queued` only after an allow decision is appended and, for unsafe calls, fsynced; then recompute coarse state | `{session_id, turn_id, tool_use_id}` | clients (`tool.state queued`, derived `turn.state`), Tool scheduler | sync | internal | Gate |
| `ToolStarted` | Turn | this call `queued` to `running`, then recompute coarse state; coarse state may remain `running_tool` | `{session_id, turn_id, tool_use_id}` | clients (`tool.state running`, derived `turn.state`), owning plugin (`tool.invoke`) | sync | internal | `Turn.RunTool` |
| `ToolFinished` | Turn | this call from `gating`, `queued`, `running` or `awaiting_permission` to `done`, then recompute coarse state as `running_tool`, `awaiting_permission` or `streaming` | `{session_id, turn_id, tool_use_id, outcome}` | Session (`Append tool_result`), clients (`tool.state done`, derived `turn.state`) | sync | internal | `Turn.OnToolResult` |
| `TurnSteering` | Turn | streaming, running_tool or awaiting_permission to steering after every admitted non-done call is terminalized | `{session_id, turn_id, partial: [ContentBlock]}` | clients, running tool (`tool.cancel`), Session (partial `assistant_message` with `stop_reason: interrupted`, fixed interrupt denial for a call with no decision, then one `tool_result` with outcome `killed` per call) | sync | internal | `Turn.Steer` |
| `TurnResumed` | Turn | steering to streaming | `{session_id, turn_id}` | RequestAssembler, `before_request` hook | sync | published as `before_request` | `Turn.Resume` |
| `TurnCompleted` | Turn | streaming to completed | `{session_id, turn_id, usage: Usage}` | clients, `turn_completed` hook, Session (`Dequeue` next queued message) | sync | published as `turn_completed` | `Turn.Complete` |
| `TurnCancelled` | Turn | streaming, running_tool, awaiting_permission or steering to idle after every admitted non-done call is terminalized | `{session_id, turn_id}` | Session (`Append turn_interrupted` after tool cleanup), clients | sync | internal | `Turn.Cancel` |
| `TurnFailedEvent` | Turn | any active to failed after every admitted non-done call is terminalized | `{session_id, turn_id, class, message, retries}` | Session (fixed invariant denial for each call with no decision, one error result for the failing call, killed results for the others, then `Append turn_failed`), clients | sync | internal | `Turn.Fail` |
| `CompactionRequested` | Turn | streaming, after an `assistant_message` whose prompt tokens cross `sessions.compact_at` of the context window, or `session.compact` | `{session_id, first_entry_id, last_entry_id, prompt_tokens, context_window}` | `before_compaction` hook (a summary), else Compactor (model summary) | sync | published as `before_compaction` | Compactor |

### Server events

| name | aggregate | transition | payload | consumers | delivery | boundary | owner |
|---|---|---|---|---|---|---|---|
| `ServerShutdownRequested` | Server | running to shutting_down | `{instance_id}` | process owner (`ShutdownCoordinator`) | sync, after the successful protocol response is sent | internal | `Server.RequestShutdown` |
| `ServerStopped` | Server | shutting_down to stopped | `{instance_id}` | the held shutdown control connection, which is then closed | sync, after runtime cleanup completes | internal | `Server.CompleteShutdown` |

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
| `before_tool` | `ToolRequested`, before the Gate | `{session_id, turn_id, tool_use_id, tool, input, safety}` | `{decision: pass, allow, deny or modify, input, reason}`; `allow` and `deny` short-circuit the Gate and are recorded with `decided_by: hook`; a `modify` candidate replaces `input` only when it is valid UTF-8, one JSON object with recursively unique keys, within the shared structural limits, through 7 MiB individually and within the Turn's 32 MiB cumulative retained-input budget. Any violation becomes a fixed hook denial without retaining, logging or publishing the rejected bytes. A hook reason is valid UTF-8 through 4096 bytes; empty or invalid becomes fixed `hook` |
| `after_tool` | `ToolResultAppended`, before the result reaches the next request | `{session_id, turn_id, tool_use_id, result: Entry}` | `{content: [ContentBlock]}` replacing what the model sees; the stored entry is unchanged. The replacement is held for as long as the session is live, so it applies to every later request in the session and not only the turn that produced it, and it is not persisted: a session loaded from disk sends the stored entry again until a handler replaces it again |
| `before_compaction` | `CompactionRequested`, when the Compactor decides to compact and `session.compact` carried no `instructions` | `{session_id, first_entry_id, last_entry_id, prompt_tokens: int, context_window: int}` | `{summary: string}`; the first non-empty summary in handler order is used and the model is not asked; empty means pass |
| `turn_completed` | `TurnCompleted` | `{session_id, turn_id, usage}` | nothing |
| `session_closed` | last client detaches, or the server exits | `{session_id}` | nothing; the server waits `hook_timeout_ms` |

### Domain services

| service | rule it owns | aggregates it spans |
|---|---|---|
| Gate | given a `tool_use`, the tool's safety class, the session's mode, session allowances and whether an asker is attached, decide allow, deny or ask; classify the input into a bounded matcher; hold the dangerous set for permissive mode. Two rules belong to this service and are enforced at the asker rather than in `Evaluate`, whose verdict is a pure function of its input and holds no session: concurrent calls with the same matcher **and the same input bytes** form a bounded permission group whose first member is its immutable canonical offered identity, and an answer of `session` scope resolves every eligible non-dangerous parked group its allowance covers. Only the canonical identity accepts an answer, decline or cancellation. Resolution holds group admission, freezes all affected bounded member sets and appends their decisions in stable Turn admission order through one Session batch. If any decision allows, the whole batch syncs before Gate releases any member to queue or finish. Failure releases none and quarantines the Session. The group retains its member set for permission cleanup and Close barriers through batch completion. The input is part of the key because a matcher is coarse (every tool but `bash` reduces to its name), so matcher-alone coalescing would bind a call to consent given for another call's arguments (ADR 0028) | Turn, Plugin (tool safety), Session (mode, allowances), connection registry (askers) |
| RequestAssembler | build the provider request: `Session.RequestContext`, system prompt from the agent definition, skills index, `session_opened` context and `before_turn` additions, tool definitions from ready plugins, model and thinking | Session, Plugin registry, Registry |
| Compactor | after `AssistantMessageAppended`, when that message's prompt tokens (`usage.input + cache_read + cache_write`) reach `sessions.compact_at` of the model's context window and the window is known, or on `session.compact`: cover every request-context entry before the current turn's `user_message`, ask the `before_compaction` hook for a summary, else summarize with the session's model, append `compaction` | Session, Registry (context window), Provider, Plugin registry (hook) |
| Subagent runner | given an `agent` tool call, open a child session under the named agent definition with `parent` set and the call's `tools` narrowing, submit the prompt, wait for `turn.state` completed or failed, return the final assistant text or the failure as the tool result; a child session exposes no `agent` tool. Several `agent` calls in one assistant message remain independently runnable through the shared Tool scheduler, so the runner holds no state shared between calls | Session (child), Plugin (tool), Registry (model) |
| Tool scheduler | Refuse a provider assistant message that would raise the active Turn above 64 total tool uses, carries a tool id or name over 4096 UTF-8 bytes, carries a raw tool input that violates the strict JSON object or structural rules, exceeds 7 MiB individually, raises that response above 16 MiB aggregate input, raises the Turn above 32 MiB cumulative retained tool input or reuses an id from that Turn before appending it or submitting work. Count every accepted hook replacement in the Turn budget too; an over-budget replacement completes only that call with a fixed hook denial and retains no replacement bytes. Otherwise keep each call non-runnable in `gating` until the Gate emits `ToolQueued`, then submit it to one Server-owned pool of 64 workers through root and child admission lanes totaling 64 queued jobs. Forty-eight general workers serve either lane; sixteen workers serve child-Session jobs only. Root queue capacity is 48 and child queue capacity is 16. Queue admission waits on completion or cancellation and starts no per-call goroutine. Thus 48 root `agent` calls may wait for children while at least 16 child jobs continue; a child cannot open another child. Append results as they land; an interrupt is observed by gating, running, queued and not-yet-submitted calls, producing deterministic killed results without allowing success after the cut. Reentrancy is the tool's own obligation, not a property the scheduler infers from safety (ADR 0034) | Turn, Plugin (tool invoke), Session (append) |
| ProcessSnapshotCoordinator | serialize prospective process snapshot sizing and mutation commit under one Server-owned admission lock. It spans PluginRegistry and RegistrySnapshot, orders Registry cache replacement before in-memory publication and emits replay state only after the bounded commit succeeds | Server, Plugin registry, Registry |
| ShutdownCoordinator | reserve admission for an authenticated `server.shutdown`, flush the success response, transition Server once, cancel other connection work, close sessions and plugins, and close the listener. On success, complete Server shutdown so the retained writer sends `server.stopped` before EOF. On failure, release it without terminal proof | Server, Session, Turn, Plugin, connection registry |
| Recovery | on `Session.Load`, walk every `tool_use` without a result. If it has no decision, append a fixed `invariant` deny with reason `recovery: process ended before a decision`; then append `tool_result` with outcome `lost` for every call still lacking one. Sync all synthesized rows before exposing the Session. This reconciles an incomplete permission batch or process crash without leaving an unmatched provider call; single aggregate, listed here because it runs outside a turn | Session |

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
| `$XDG_CACHE_HOME/rudy/rudy.log` | kernel | slog JSON lines, one record per line, appended for the life of the process, 0600, never rotated. Every record carries `pid`, because a `serve` daemon and a client attaching to it are two processes writing one file; `O_APPEND` keeps lines whole and the pid says whose. Written by `Build`, so every command that boots the kernel writes it. Records: process start with version and paths; every notice the operator was shown, at warn; plugin load, ready and fail with the reason; session open, resume and close; turn start, complete and fail with the error; connection accept and close; the socket bind; the exit path with its code and error, for a command that got as far as booting the kernel. A provider failure is the `turn: failed` record, which carries its class, message and retry count. At `debug`, each tool result with its duration and outcome. For ordinary commands, a file that cannot be opened is a notice and records go to stderr. `rudy acp` uses the fail-closed ACP policy above: open or later write failure never redirects records or daemon detail to stderr and terminates the adapter with only a fixed correlated diagnostic |
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
- an `assistant_message` containing `tool_use` blocks is followed, for each `tool_use.id`, by exactly one `permission_decision` and then exactly one `tool_result` before the next `assistant_message`; `Load` appends a fixed `invariant` deny where a decision is missing and `tool_result` outcome `lost` wherever a result is missing
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
| `matcher` | `{tool, prefix}` | no | the Gate's bounded classification of the input. For bash, `prefix` is the first simple command's first two words joined with one space when that value is at most 4096 UTF-8 bytes; a longer value is `sha256:` plus its lowercase SHA-256 digest. It is empty for other tools |
| `decision` | `allow`, `deny` | no | |
| `decided_by` | `class`, `mode`, `allowance`, `hook`, `asker`, `no_asker`, `interrupt`, `invariant` | no | `class` means the tool is safe; `mode` means off or permissive let it through; `allowance` means a prior session-scope allow matched; `no_asker`, `interrupt` and `invariant` are always denies. `interrupt` closes an admitted call before Gate decision; `invariant` closes it while failing the Turn |
| `scope` | `once`, `session` | no | `session` only ever comes from an asker allow; every other row says `once` |
| `reason` | string | no | the asker's text, the hook's reason or the rule name; valid UTF-8 through 4096 bytes and never empty |
| `input` | object | yes | the input the tool ran with when a valid `before_tool` hook modification replaced it; absent otherwise. At most 7 MiB, written and read byte for byte like a `tool_use` input, and the last key on the line |

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
| `retries` | int | no | attempts made before giving up, from 0 through 2147483647; 0 when not applicable. Creation and Load refuse any other value before replay |

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
| `fetched_at` | rfc3339 | no | latest successful provider refresh; unchanged when a commit contains only provider failures |
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
| `log.level` | `debug`, `info`, `warn`, `error` | `info` | the least severe record written; `debug` adds each tool invocation and provider stream error. Any other value is refused at load |
| `log.file` | path | `""` | empty means `$XDG_CACHE_HOME/rudy/rudy.log`, filled at load the way `sessions.dir` is; `~` expands. The record layer row above says what is written and what happens when the file cannot be opened |
| `remote.host` | string | `""` | The ssh destination, alias or `user@name`, where `rudy acp` reaches the daemon when `--host` is not given; empty means the kernel runs here. A value beginning with `-` is refused at load (ADR 0029, ADR 0032) |
| `remote.source` | path | `~/projects/rudy` | Where the rudy checkout sits on the host, which `rudy hosts install` checks out at this binary's commit and runs `make install` in; `~` expands on the host (ADR 0029) |
| `ui.status.host` | bool | `true` | Show `host:` before the workspace path in the status bar when the session runs on a host reached by `--host` or `remote.host` (ADR 0029) |
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
| Session.Open, child | `session.open` with `parent` from a plugin | `SessionOpened`; the subagent runner consumes `TurnCompleted` and `TurnFailed` of the child | `session_opened` with `parent_session_id` and `parent_tool_use_id` |
| Session.Fork | `session.fork` | `ForkPointRecorded` | `fork_point` |
| Session.Load and Recovery | `session.resume` | none; recovery appends through `Append` | fixed `invariant` deny for an unmatched tool use without a decision, then `tool_result` outcome `lost` for every unmatched tool use |
| Session.SetModel | `session.set_model` | `ModelChanged` | `model_change` |
| Session.SetThinking | `session.set_thinking` | `ThinkingChanged` | `thinking_change` |
| Session.SetMode | `session.set_mode` | `ModeChanged` | `mode_change` |
| Session.SetTitle | `session.set_title` | `TitleChanged` | `title_change` |
| Session.Enqueue | pass 3: not yet implemented; `session.submit` refuses a queued source with `invalid_argument` and there is no queue behind it | none until dequeued; queued messages would be in memory only, returned to the client on cancel | none; a queued message becomes a `user_message` only when it starts a turn |
| Session.Append(note) | `plugin.append_note` | `NoteAppended` | `note` |
| Turn.Start | `session.submit` source typed | `UserMessageAppended`, `TurnStarted` | `user_message` |
| Turn.OnResponse | none; provider stream | `AssistantMessageAppended` | `assistant_message` |
| Gate decide | `session.answer` when asked | `ToolRequested`, `PermissionRequested`, `PermissionDecided`, `ToolQueued` | one `permission_decision` per call before `ToolQueued` |
| Turn.RunTool | `tool.invoke` to plugin, `tool.state` to clients | `ToolStarted` | none until finished; the decision row precedes `ToolQueued`, and `ToolStarted` can follow only from queued. Every call of one assistant message is independently runnable through the shared bounded scheduler, so the `tool_result` rows of a turn may be in a different order from its `tool_use` blocks; both codecs pair them by id, and the invariant below is per `tool_use`, not positional. The results of one assistant message are assembled into a single message carrying every block, not one message per result: Anthropic's parallel tool use requires that shape, and splitting them is accepted on the wire but teaches the model to stop calling tools in parallel, which is the behaviour this wave exists to enable |
| Turn.OnToolResult | none | `ToolFinished`, `ToolResultAppended` | `tool_result` |
| Turn.Steer | `session.interrupt` how steer | `TurnSteering` | partial `assistant_message` stop_reason `interrupted`; each admitted non-done call gets an `interrupt` deny first when it has no decision, then `tool_result` outcome `killed` |
| Turn.Resume | `session.submit` source steer | `UserMessageAppended`, `TurnResumed` | `user_message` source steer, then `turn_interrupted` how steer |
| Turn.Cancel | `session.interrupt` how cancel | `TurnCancelled` | each admitted non-done call gets an `interrupt` deny first when it has no decision, then `tool_result` outcome `killed`; `turn_interrupted` how cancel follows all call cleanup |
| Turn.Complete | none | `TurnCompleted` | none beyond the final `assistant_message`; completion is derived from `stop_reason` |
| Turn.Fail | none | `TurnFailedEvent`, `TurnFailed` | each admitted non-done call gets an `invariant` deny first when it has no decision, the failing call gets an error result, the others get killed results, then `turn_failed` |
| Compactor | `session.compact`, implicit on threshold | `CompactionRequested`, `CompactionRecorded` | `compaction` |
| MCP server connect | none; boot | none; a failure is a `notice` | `mcp.toml` read, nothing written |
| Plugin install, enable, disable, update | `rudy install` and the `rudy plugins` CLI, not protocol | none | `plugins.lock.toml`, the checkout, and the manifest's `build` run inside it |
| Plugin.Load, Ready, Fail, Stop | `plugin.init`; state via `plugin.state` | `PluginLoading`, `PluginReady`, `PluginFailed`, `PluginStopped` | none; plugin state is runtime, rebuilt at boot from config |
| PluginRegistry.Register* | `plugin.register_*`, `plugin.set_status` | `CapabilityRegistered`, `CapabilityRejected` | none; capabilities are runtime |
| Registry.Refresh | `registry.refresh`, implicit on open | `RegistryRefreshed` | `registry.json` |
| session close | `session.close` | `session_closed` hook | none; closing is a connection fact, not a conversation fact |
| ACP edge | stable ACP v1 and negotiated `_rudy` methods | none; translates existing Client events | none; capabilities, request correlation, replay suppression and cursors are connection facts |

Invariants and where they are enforced:

| invariant | aggregate | record layer |
|---|---|---|
| unsafe tool runs only after a durable allow | Gate then `Session.Append` with fsync | line order plus fsync; `Load` checks order |
| one decision and one `tool_result` per `tool_use` | Gate then `Turn.OnToolResult` | Recovery writes a fixed `invariant` deny if needed and `lost` for every missing result |
| `fork_point` only first | `Session.Fork` | `Load` refuses otherwise |
| ids monotonic | `Session.Append` | `Load` refuses otherwise |
| steer only in steering state | `Turn.Resume` | `Load` checks the preceding entry |
| one process writes a session | lock file | `flock` on `lock` |
| tool names unique | `PluginRegistry.RegisterTool` | none; runtime |
| a session's tool set never exceeds its parent's | `Session.Open` intersects definition, `tools` and parent before stamping the view | the resolved set is written on `session_opened` at `schema_version` 2 and replayed on resume, with a version 1 log falling back to the definition's list. It is recorded rather than recomputed because a child is cold by the time anyone resumes it, its parent may be gone, and recomputing from the definition alone hands back exactly what the intersection removed. What a session ran under is a fact about that session, so the log is where it belongs |
| a call never runs on consent given for another call's arguments | the asker coalesces only calls whose matcher and input bytes are both equal. The first member is the only offered and answerable identity; the exact answer applies to the group's bounded current member set. A `session` answer resolves other parked groups its allowance covers, never one the Gate marked dangerous, which keeps ADR 0011's ordering that puts the dangerous set ahead of allowances. All affected groups freeze while the complete decision batch appends and syncs | one `permission_decision` per `tool_use` in stable Turn admission order; no allowed member becomes queued until the whole batch is durable |
| an interrupted or failed turn leaves no dangling tool call | every admitted non-done call observes terminalization rather than one consuming it; a call without an earlier decision gets a fixed `interrupt` or `invariant` deny before its result | `tool_result` outcome `killed` for every interrupted call, error for the call that caused failure and killed for its peers; every result follows its call's decision and all precede `turn_interrupted` or `turn_failed` |

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
- live verification of `anthropic_messages`: no configured endpoint serves the route; the codec is proven against recorded fixtures until one does
- the subagent model ladder from registry prices; pass 3 takes the model from the agent definition or inherits
- a `tools` narrowing that names a tool the caller does not hold is dropped silently, so an orchestrator learns what its child actually got only from what the child says. Whether `SessionInfo` should report the resolved set is undecided
- whether a tool should be able to declare that it must not run beside another call of itself, so the scheduler serializes it rather than the tool locking internally. Pass 5 puts the obligation on the tool, which is the smaller change and keeps the scheduler free of a second classification
- the socket path on macOS when neither `XDG_RUNTIME_DIR` nor `TMPDIR` is set
- how a spawned provider plugin authenticates to its upstream; pass 1 leaves it to the plugin's own config table
- image content from an MCP tool result is rendered as a text placeholder, `[image <media_type>, <n> bytes]`; the bytes should go to the session's blob store and come back as an `image` content block
- MCP servers are per process: the plugin loads `mcp.toml` once at boot with the project scope of the workspace rudy was started in. Per-session project servers wait for `rudy serve` holding many workspaces at once
- a spawned plugin may register only `wire: custom` providers; `openai_chat` and `anthropic_messages` from a spawned plugin are refused, so a spawned plugin cannot yet stand up an endpoint of its own with config
- `tool.progress` from a spawned plugin is received by the adapter and dropped; forwarding it to clients needs the stream part type the TUI plan defines
- `plugin.append_note` is not in the own-session set, so a plugin may append a note to any live session, not only the ones it opened. Whether that is the contract or an omission is undecided; the note carries the plugin's name either way
