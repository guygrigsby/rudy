# ACP at the remote boundary

Status: design, approved in conversation 2026-09-14. Implementation is UNSHIPPED and tracked by
`rudy-4tf.1`. ADR 0032 records the decision. Contracts passes 10 through 12 carry the wire rows and
bounded implementation refinements. This spec
supersedes the remote session carrier in
`2026-09-13-remote-runtime-design.md`; workspace sync, placement and install remain in Hosts.

## Problem

The first remote runtime carries Rudy's private protocol through `rudy bridge` over ssh. It
works only when both ends are Rudy. The box is already an agent process boundary: it owns the
daemon, sessions, providers, plugins and workspace while a client on the Mac renders and
drives them. Agent Client Protocol v1 names that boundary and lets another ACP client drive
the same box-side runtime.

Replacing the kernel protocol with ACP would lose Rudy's append-only entry contract, raw byte
fidelity, plugin interface and local transport parity. Keeping the private wire as the remote
wire would keep the client coupled to one harness. ACP belongs at the edge, translating to the
same Unix socket every local client and bridge already use.

## Decision

The remote path becomes:

```text
Rudy TUI or generic ACP client
        |
        | ACP v1 over stdio
        v
ssh host "exec rudy acp"
        |
        | Rudy protocol over the host Unix socket
        v
persistent rudy serve daemon
        |
        +-- Session and Gate
        +-- Provider and registry
        +-- linked and spawned plugins
        +-- future sandboxed tool adapter
```

`rudy acp` is an ACP agent and an anti-corruption layer. It attaches to the host's daemon,
starting one through the existing daemon start path when nothing answers. It translates ACP
requests, responses and notifications to Rudy protocol messages. Disconnecting it detaches
one client. The daemon and active turns continue.

The Mac side gains an ACP client anti-corruption layer behind a `SessionClient` port expressed
only in Rudy types. Its `Prompt` call stays open until the Turn's terminal durable Entry and
matching terminal state while its event stream remains live; steer, cancel and permission
answers may run concurrently. A command that starts a Turn uses the same completion barrier.
Local socket, embedded and plugin paths keep the private protocol.
Remote TUI and headless paths
implement the port through ACP. ACP types do not cross either adapter.

`rudy bridge` stays during the parity rollout. It is removed as a session carrier after the
ACP path passes the same remote tests. `rudy hosts stop` and forced install then use the
negotiated `_rudy/server_shutdown` extension, which maps to `server.shutdown` over the exact
greeted Unix connection held by `rudy acp`. The ACP request returns `stopped` only after the
adapter receives the matching internal `server.stopped` terminal proof and internal EOF. This
supersedes `rudy bridge --stop` once bridge compatibility is removed.

Forced replacement treats a successful normal initialize without negotiated
`_rudy/server_shutdown` as incompatible. It installs first, then uses the new binary's bounded
shutdown-control adapter against the surviving daemon; it never enters the compatible negotiated
shutdown branch with a method the peer did not advertise.

## Bounded contexts

### Session

Session remains the core. It owns Server, Session, Entry, Turn, Gate and the append-only log.
No ACP type enters it. Its protocol remains the published language used by local clients,
plugins and the ACP agent adapter.

The Rudy session ULID is the ACP `SessionId` byte for byte. No mapping table or second durable
identity exists. The adapter holds only connection-scoped correlation, negotiated capabilities
and replay suppression.

### Client

Client remains a conformist renderer. `SessionClient` is its port for session operations and
events. The existing protocol client and the new ACP client implement it. The port includes
Rudy features the TUI already needs, but no transport, JSON-RPC or ACP shape.

### Hosts

Hosts still owns Host, Placement, ssh, install and sideband sync. It starts the remote process
and returns a bidirectional stdio stream plus Placement. The ACP client adapter wraps that
stream. Hosts never imports ACP. Git push, tar copy and pull do not move into ACP. ACP carries
agent interaction, not workspace transfer.

### ACP adapters

ACP is an external published language behind two anti-corruption layers:

- `acpagent` translates ACP v1 into the internal Session protocol on the box.
- `acpclient` translates ACP v1 into the Client `SessionClient` port on the Mac.

Both use `github.com/coder/acp-go-sdk` pinned exactly. It is Apache-2.0, implements both sides
of stable ACP v1 and supports extension methods and `_meta`. A private ACP codec would copy a
large evolving schema and add no Rudy behavior. ACP SDK imports are allowed only in the two
adapter packages, enforced beside `make vendor-types`.

## ACP initialization

`rudy acp` serves stable ACP v1 over newline-delimited JSON-RPC on stdin and stdout. stdout is
protocol only, stderr contains fixed diagnostics and detailed errors stay in the box log. Each
JSON payload is capped at 8 MiB excluding its one terminating line feed, below the selected
SDK's fixed 10 MiB scanner ceiling. Raw UTF-8 whitespace counts, a CR before LF counts as payload
and unterminated EOF is rejected. A larger payload closes the adapter with a fixed local cause and
never reaches the daemon or peer. Oversized ingress receives no response; outbound overflow is a
writer refusal followed by supervisor close.

The SDK starts receiving during construction and its default logger can include a malformed raw
line. Rudy wraps its reader in a closed gate, installs a sanitized SDK logger, then opens the
gate. The sanitizer drops raw values, ids, request ids and unstructured errors. The reader and
writer enforce the frame cap independently of the SDK. Before SDK input, the reader admits at
most 64 active requests, 64 known notifications and 64 responses matched to active outbound
requests. All three sets share a 32 MiB original-frame budget; request ids in the two peer
directions are independent. Inbound ACP request ids accept strings, mathematical signed 64-bit
integers and null as the pinned schema requires, with null occupying its own directional
active-request key. Rudy-generated
outbound request ids remain monotonic positive integers. Request bytes remain charged until the response is physically written,
notification bytes until the wrapped sequential handler returns and matched response bytes until
its wrapped callback returns. A frame that crosses a count or byte limit closes before SDK
admission, preventing either request goroutines or SDK queues from retaining hundreds of megabytes
of near-limit frames. A duplicate active inbound id or 65th item in any set
disconnects the adapter instead of creating another SDK goroutine. The reader emits one fixed
JSON-RPC parse or invalid-request error through the same serialized writer before closing a
malformed envelope, without logging its bytes. The ACP process writes only fixed diagnostics to
stderr and never tails the daemon's box log over ssh.

That guarantee survives logging failure. `rudy acp` selects a fail-closed log sink before it
reads any ACP frame: failure to open the 0600 box log returns only a fixed correlated stderr line
and exits, while a later write failure signals the supervisor and closes the adapter with the same
fixed form. It never falls back detailed records or captured daemon stderr to the SSH stderr
stream.
This fail-closed diagnostic boundary is ADR 0038.

Each direction separately caps active outbound requests at 64 and their complete encoded frames at
32 MiB until response callback, cancellation or close. Monotonic safe-integer outbound ids and one
issued high-water mark let legitimate late responses after cancellation be dropped while future or
invalid ids close the connection. Each writer also caps queued frames at 64 and 32 MiB through physical write.
Outbound request admission happens before sequence allocation; overflow closes the adapter and
retires callbacks rather than blocking an unseen permission. Response construction has a separate
32 MiB weighted admission pool: large handlers reserve 8 MiB upfront, small handlers reserve 1 MiB
upfront and no encoded result may exceed its class reservation or 7 MiB. These are concurrency
weights based on maximum encoded output, not allocator-byte accounting. Aggregate cardinality,
scalar and encoded-input limits bound decoded graphs, while retained encoded buffers remain charged
to their request, response or writer budgets. Before SDK decode in either adapter direction, one
streaming configurable strict-JSON scanner rejects invalid UTF-8, duplicate keys after escape decoding, depth
above 64, more than 65,536 combined values and member names, an array above 16,384 elements or an
object above 4,096 members. Every complete outbound frame passes the same profile before enqueue.
Predictable mutation responses and process updates pass it before commit; an ordinary query that
cannot produce a conforming result returns fixed internal failure. Reassembled public Entry and
Part JSON uses a separate event profile with depth 72 and 131,072 structural units plus the same
container and key limits, leaving headroom around a nested input that reaches its legal limit
(ADR 0036).

Before admission, a strict classifier accepts only JSON-RPC 2.0 request, notification or response
objects with the required id, method and exclusive result or error fields. It rejects known
request methods shaped as notifications, known notifications carrying ids, hybrid envelopes and
batches before SDK dispatch. Unknown requests reach the SDK for method-not-found; unknown
notifications are dropped.

Frame, admission and writer refusals signal the adapter supervisor. It closes the ACP stream and
internal daemon connection so an SDK write failure cannot leave a half-open adapter.

The adapter accepts ACP `initialize` exactly once, then dials the local Unix socket and sends
internal `client.hello`. It requires the daemon capabilities for bounded Session listing, bounded
Session event transport, bounded process-event replay and terminal Turn durability before
committing readiness. A missing capability is an incompatible
daemon failure even when hello otherwise succeeds. Initialize params are schema-validated before
the adapter consults its initialize state. Malformed params return invalid-params and leave the
waiting, in-progress or ready state unchanged. Only a schema-valid initialize is eligible to
start the transition. Pre-ready Session work is unavailable, a schema-valid concurrent initialize
is conflict and a schema-valid repeated initialize is invariant refusal, all with the fixed codes
and messages in the contract and without a domain call. An unnegotiated extension is
method-not-found. A malformed initialize may be retried only while the adapter is still waiting.
Verified `protocol.ErrNoServer` under
`--no-start` returns to the CLI before any ACP frame and becomes exit 112; every other daemon dial,
hello or capability failure is written safely before close. A
generic ACP client whose metadata omits `_rudy` is an asker because permission requests are a
standard ACP client method. Present malformed, unsupported or incomplete `_rudy` metadata is
rejected and never falls back to generic asker authority. The Rudy client supplies
`_meta._rudy.asker`; TUI sends true and headless sends false. The adapter publishes the daemon
connection, asker bit and negotiated capabilities atomically only after the initialize response
is physically written. Until then, it stages the hello result and rejects other requests. The
initialize response contains:

- stable protocol version 1
- agent name `rudy` and the running binary version
- no ACP authentication methods
- `loadSession: true`
- session capabilities for list, resume and close; delete stays unadvertised because Rudy has
  no delete operation
- text prompt support plus mandatory resource-link blocks mapped to non-dereferenced Rudy text;
  image, audio and embedded context stay false until their real Rudy path is verified
- no ACP client filesystem or terminal dependency
- `_meta._rudy` when the client offered the same extension version

The daemon physically writes internal `client.hello` before enqueueing its bounded process-state
snapshot. The adapter's single internal pump stages that snapshot in its fixed ledger while the
ACP initialize response is in flight, then begins projection only after the ACP response is
physically written. No process event can precede ACP readiness or fill the internal outbox ahead
of hello.

The process derives this response from one explicit supported-capability set. It advertises a
capability only after its handler and every event projection required for correct use are
installed. In particular, `session.update` remains absent until the full safe process-event
union is wired, even if its encoder exists earlier.

The Mac client normally offers `_meta._rudy.version: 1`, `asker` and the capabilities it
understands. The agent returns the intersection plus its `home`, daemon `instanceId` and binary
version. A generic ACP client omits `_rudy` and receives a standard session surface. Unknown
metadata is ignored outside the reserved `_rudy` key; a present malformed or unsupported `_rudy`
object is rejected.

Host lifecycle commands may instead offer the exact shutdown-control initialize variant. That
connection is never an asker and advertises only the ACP mandatory text and resource-link baseline
plus `_rudy/server_shutdown`. Mandatory Session requests receive fixed control-connection errors
or no-op cancellation and reach no domain Session; optional Session and other negotiated methods
stay unadvertised. It does not require bounded Session listing, bounded Session events, bounded
process events or terminal Turn durability because it cannot observe or drive them. It greets the
daemon with `process_events: false`. A new daemon suppresses the process snapshot and later process
UI broadcasts only; a bounded discard pump safely drains those broadcasts from an older daemon
that ignores the field. Point-to-point `server.stopped` always bypasses suppression and reaches the
control waiter. Oversized
input, unbounded admission or response-order failure closes without shutdown success, so capability
bypass does not promise liveness against every old daemon. Internal same-user socket identity and
the existing shutdown terminal proof remain mandatory.

The Mac client requires the `_rudy` capabilities used by the invoked Rudy command. Missing
ones fail with the remote version and `rudy hosts install` instruction. There is no private
wire fallback.

## Standard method mapping

The exact stable method, constraint, error and update mapping lives once in
`rudy-contracts.md#standard-methods`. ACP new, load, resume, list, close, prompt, cancel, mode,
configuration, permission and update methods carry every behavior they can express honestly.
New, load and resume reject nonempty client-supplied MCP servers and additional directories;
Rudy plugins and the stored Workspace remain the only authority for those roots and processes.
Rudy does not advertise ACP session deletion because the kernel has no delete operation.
Prompt and steer share one translator: text stays text, resource links become clearly marked
text without dereferencing the URI and every unadvertised content kind is rejected.

Session new performs its event-driven Registry refresh before mutation. The Registry returns one
immutable model copy plus a runtime revision. The agent derives and preflights the complete response
from that copy, then asks `session.open` to resolve under the same revision. A concurrent Registry
commit makes open refuse before creating the Session; the agent never refreshes again after
preflight. The same revision binds ACP set-model resolution to the option set already preflighted.

`session/load` and `session/resume` are intentionally different. Load rebuilds a transcript.
Resume drops only replayed entries while still forwarding current turn, tool and permission
state. The internal protocol orders newly appended entries after its resume response, so none is
lost. One attach transition per Session may run on an ACP connection at a time. Rudy's TUI uses
load after an ssh reconnect because its local view may have missed entries. An ACP client that
retained its view may use resume.

The adapter never rebuilds a raw tool input or thinking signature and sends it back into the
kernel. Stored bytes remain authoritative. Standard ACP updates carry display-safe fields.
Negotiated Rudy metadata may carry byte-exact values encoded as base64 when the Rudy client
needs them. `session/list` applies ACP's optional absolute `cwd` filter before returning any
Session metadata. An omitted filter intentionally exposes every Session owned by the same Unix
account. The internal `session.list` query already filters and pages over descending Session
ULIDs, returning at most 100 records and less than 4 MiB per response. Store traversal reads
directory entries in fixed batches and retains the newest requested limit plus one candidate in a
bounded top-K selection, so arbitrary directory order cannot corrupt descending pages and
filtering a large store does not rebuild an unbounded slice. The ACP adapter consumes those pages
incrementally while retaining no more than one bounded ACP page, applies its
independent 7 MiB response bound and replaces the internal cursor with a connection-scoped
authenticated token bound to the normalized filter. It never exposes, persists or logs either
cursor or key material.

## Rudy extensions

Extension methods use the `_rudy/` prefix. Every request requires `_meta._rudy.version: 1` to
have been negotiated at initialize. The agent returns method-not-found to an unnegotiated
extension so a generic client cannot accidentally depend on it.

The extension catalogue and every request and result shape live once in
`rudy-contracts.md#rudy-extension-methods`. It covers fork, steer, compact, operator shell,
titles, slash commands, registry access, Rudy UI events and server-owned shutdown. A generated
Go catalogue and a contract guard will keep code and that table equal in both directions.

Rudy-specific fields on standard `session/update` use the closed `_meta._rudy.event` union in
the contracts. Each internal event has one logical carrier set. Non-byte events use one carrier;
Entry and Part values whose private and public forms both fit through 7 MiB and whose public form
passes the decoded-event structural profile use 1 through 7 non-interleaved 1 MiB fragments. If
either form crosses 7 MiB or the public form crosses that profile, one bounded oversized reference
carries the public size, digest and terminal retry count when applicable, including when only the
private form crossed. Metadata-only session-info updates carry values
with no honest standard projection. The single agent dispatcher assigns every fragment or other
negotiated carrier, permission request and `_rudy/session/update` a
connection-scoped safe-integer sequence in internal notification order. SDK callbacks may arrive
concurrently, so the Rudy client publishes them only in sequence order and closes on malformed,
duplicate or bounded-buffer overflow. Display-only projections carry no event and allocate no
sequence. Allocation and enqueue are atomic, and the dispatcher waits for the carrier's physical
write acknowledgment before it allocates a successor, including between fragments. A cancellation
or failure before write closes the connection, so a live sequence cannot acquire a gap. The
metadata names the originating Session and carries Entry or `provider.Part` public JSON as padded
base64 fragments when byte fidelity matters. The client validates the set and digest under a
7 MiB assembly bound, then runs the decoded-event profile before typed decode. The daemon emits a
bounded internal oversized reference on that private, public byte or public structural
public boundary, keeping
the internal 16 MiB frame path replay-safe. A `turn_failed` Entry is the
exception: its failure message is replaced with fixed public text and marked redacted before
crossing ACP. Every assistant Entry and streamed stop Part clears the provider's raw stop reason
and marks the carrier redacted when a raw value existed. Permission requests carry exact input
through 1 MiB in one complete request under 7 MiB; larger input produces a non-actionable oversized
reference whose size is the exact input length and whose digest covers a same-length zero-byte
redaction rather than secret input, then retires only that asker's callback. The Rudy client rejects missing or inconsistent negotiated metadata
instead of reconstructing state from ACP's display-safe form. Status items, widgets, plugin
state, registry state and notices use the exact `_rudy/session/update` union because they have
no honest standard update shape. Notices and plugin failure reasons cross only through explicit
public text with fixed fallbacks.

Standard ACP tool-call identity is a deterministic projection of Rudy Turn id plus raw provider
tool-use id: `rudy-call-sha256-<digest>`. Starts, updates, results and permission requests all use
that value, so a provider may reuse its raw id in a later Turn without aliasing the earlier ACP
call. Negotiated metadata retains the raw id and echoes the projected id for cross-checking;
generic clients see only the bounded projection.

A superseded historical `model_change` whose provider or model field is invalid or overlong remains
available only through its negotiated exact Entry carrier. It emits no standard config update, so a
generic client never receives a display alias that looks selectable. If that value is current,
internal resume fails unavailable before replay or attachment.

The Server owns one ProcessSnapshotCoordinator admission lock shared by PluginRegistry and
RegistrySnapshot. Every Plugin, status, widget and Registry mutation constructs and sizes its
prospective combined replay state with `sequence: 9007199254740991`, verifies each complete ACP
carrier under the outer structural profile, then commits while holding
that lock. Status owner/key, widget owner/key, plugin name and Registry provider name remain exact
identities and must be valid UTF-8 through 4096 bytes before commit. Status and widget Span text
has the same admission bound and Span roles use the closed Theme role set. A violation refuses the
whole candidate; no process-event field uses a digest alias. Provider fetches stay
outside the lock after per-provider generations are assigned. Under the lock, superseded results
are dropped and non-overlapping provider deltas rebase onto current state. Registry cache
replacement and in-memory publication are ordered inside the commit, so concurrent individually
valid mutations cannot jointly cross the bound, lose another provider's update or overwrite a
newer cache with an older candidate.

Registry admission also streams the complete internal catalogue through 4 MiB, depth 56 and
60,000 structural units. Cross-package fixtures prove that the fixed ACP conversion and envelope
fit inside the outer profile with the reserved 5,536-unit and eight-level headroom. A committed
state change advances one runtime revision and internal list or refresh returns that revision with
an immutable model copy. Stale-revision Session open and set-model refuse before mutation.

PluginRegistry preflights the prospective full status snapshot and each widget before mutation.
Both the complete internal notification and safe public process-event projection must fit through
4 MiB and each complete worst-case ACP carrier must pass the outer structural profile. An overflowing
update is rejected atomically, leaving prior state intact, so connect replay
cannot repeatedly poison the nonfragmentable process-event carrier. Plugin, status, widget and
Registry mutations also preflight the complete replayable process snapshot: at most 64 events and
32 MiB in both exact internal and safe public encoded forms. Plugin discovery reserves its replay
slot using the complete internal failed-state encoding with 4096 ASCII NUL bytes as the worst-case
escaped reason before spawn; excess plugins are rejected before Registry
admission. A later failure always commits, logging an invalid or overlong reason and storing fixed bounded
text. An oversized live notice is similarly logged and replaced with fixed text. The daemon
advertises `bounded_process_events_v1` only after the shared coordinator and the whole invariant
are active.
After a hello response is physically written, the same coordinator lock covers current-state
capture, reservation and ordered enqueue of the connection's complete snapshot plus activation of
live delivery. Mutations enqueue under that ordering boundary, so each appears either in the
captured state or after the whole snapshot. A connect can neither miss a mutation nor receive an
older snapshot after its live update.

Negotiated permission selections carry the caller's bounded reason in reserved Rudy metadata;
empty and generic selections normalize to fixed `asker`. Negotiated internal errors carry only a
closed safe kind discriminator so SessionClient preserves provider, transport, plugin and internal
classes without exposing their messages.
Registry errors become fixed provider failure text. The adapter never forwards a nested error
through these successful update channels. A standard client may ignore all of them.

`SessionClient.AnswerPermission` claims only its local opaque permission callback. Success means
the selected outcome was accepted for delivery, not that the box Gate authorized the call. ACP
provides no reverse result after the callback returns. If another asker wins at the box first, the
agent treats its later `session.answer` conflict as supersession and authoritative permission and
tool events converge the client view.

Thinking and model selection are not extensions. Stable ACP v1 session config options cover
them. Child sessions are still opened by the daemon's subagent plugin through the internal
protocol. ACP reports their exact Rudy session ids in metadata but grants no new ability to
forge a `ParentRef`.

## Workspace placement and sync

The Mac computes the remote placement before `session/new`, using the box home from
`_meta._rudy.home`. A local cwd below the Mac home maps to the same relative path below that
home. `--cwd` still supplies an absolute box path when no mapping exists.

A generic ACP client supplies a cwd meaningful on the machine where `rudy acp` runs. The
adapter never rewrites it and the daemon validates it through `session.open`.

Sync remains an ssh sideband operation before a new remote session. Neither ACP filesystem
methods nor terminal methods move the workspace. Rudy does not ask the Mac ACP client to read,
write or execute anything on behalf of the box.

## Identity, authentication and secrets

ssh authenticates a principal to the box, maps it to a Unix account and encrypts the ACP stdio
stream. That account launches `rudy acp`, which reaches the daemon through its same-user Unix
socket. ACP advertises no authentication method because stdio already inherits this parent and
operating-system identity. There is no TCP listener, ACP bearer token or second identity
system.

Every SSH principal or key mapped to the account receives full plain-client authority for that
account: unfiltered Session history, `mode: off`, operator shell and negotiated server shutdown.
Operators who need different authority boundaries must use different Unix accounts. Provider
identity is separate from ACP client identity and resolves where the daemon runs. A provider may
read an explicit credential on the box, use ambient host identity or require no authentication.
Aperture is the ambient case: the box's tailnet identity determines access and no provider token
exists.

ACP never carries provider credentials. Hosts install and sync never copy Mac secrets to the
box. A future Firecracker guest receives tool calls only. Provider calls remain in the box
daemon, so no provider token or tailnet identity enters the guest.

An ACP adapter connection declared as an asker routes permission requests. Every admitted tool call
starts in non-runnable `gating`. A hook-modified input must still be a JSON object through 7 MiB,
and an invalid replacement becomes a fixed hook denial without retaining its bytes. The Gate uses
a bounded matcher: a bash prefix over 4096 bytes becomes `sha256:` plus its lowercase digest. The
complete internal permission notification must fit through 8 MiB before a call may enter
`awaiting_permission`; unexpected overflow produces a fixed invariant denial, an error tool result
and bounded Turn failure after peer calls are terminalized. The Gate remains the durable authority:
an unsafe tool reaches `queued` only after its own permission decision is appended and fsynced. If
that ACP connection is the last asker and disappears, the standing request is denied as
`no_asker`. Headless declares no asker and cannot answer.

Every answer names the exact standing Session, Turn and canonical tool-use question offered to the
asker. Calls with the same matcher and exact input bytes form one bounded group whose first member
is that immutable identity. The Gate freezes the current member set plus any non-dangerous groups
settled by a Session allowance, appends every decision as one scheduling batch and syncs the whole
batch when it allows. No member queues or finishes until the batch succeeds; failure queues none
and quarantines the Session. An unoffered member id cannot answer the group. The Gate validates identity atomically,
so a delayed ACP callback cannot authorize a successor Turn. Provider tool-use ids must remain
unique throughout an active Turn, so a delayed callback also cannot select a later question in the
same Turn.

Provider and hook tool input passes the nested-input strict JSON profile before append or Gate evaluation.
One input is at most 7 MiB, one provider response carries at most 16 MiB of raw tool input and one
active Turn retains at most 32 MiB across provider inputs plus accepted hook replacements. A
provider violation rejects the whole response before any prefix executes; a hook violation becomes
a fixed denial without retaining the replacement.

An exact permission input over the ACP request limit does not disconnect an otherwise healthy
Session. The agent emits a non-actionable oversized reference, then calls authenticated internal
`session.decline_permission` for that exact Session, Turn and tool-use question. The Gate removes
only that connection from the question's eligible askers. Another asker may still decide; when none
remain, the ordinary durable `no_asker` denial applies. Decline never authorizes a tool.

An ACP `cancelled` answer, malformed answer or unoffered outcome calls authenticated internal
`session.cancel_permission` for the exact standing Session, Turn and tool-use question before the
adapter closes when the input is invalid. Eligibility may route through the attached parent but
does not grant general child Session authority. A successful cancel waits for the exact Turn's
durable terminal proof and never falls through to a successor. If another asker decided first,
`conflict` or `not_found` is a superseded no-op rather than a second cancellation.
A valid JSON-RPC error response instead declines that exact question and closes the ACP connection.
It neither answers nor cancels the Turn. Other askers remain eligible; the last-asker path records
the ordinary durable `no_asker` denial.

`_rudy/server_shutdown` adds no authority beyond the internal connection. The Unix listener
minted its same-user marker, and `server.shutdown` rechecks it. The ACP client cannot assert
that marker through metadata. The adapter keeps the ACP request open through daemon cleanup. A
matching internal `server.stopped` followed by internal EOF yields the ACP `stopped` result;
bare EOF, transport loss or mismatched instance identity fails closed and yields no success.

## Backpressure and failure

Each direction has a bounded queue and each adapter admits at most 64 concurrent inbound
requests. A client that exceeds either bound is disconnected. The daemon continues, its log
remains authoritative and `session/load` recovers the view. No unbounded goroutine or byte queue
follows a slow ssh connection.

The ACP agent's internal protocol client and LocalProtocolAdapter use lossless bounded handoff for
daemon replay. Their dedicated pumps start before Session calls and carry one shared 64-item,
32 MiB charge through delivery. Replay uses a blocking per-connection enqueue outside Session and
observer locks, catches up by Entry-id suffix and subscribes atomically only when it reaches the
current tail. ACPAgent transfers raw-derived display frames into separately charged writer frames
before releasing the internal event. Live observer fan-out stays nonblocking; a full queue
disconnects only that slow client and never stalls the Turn. A single internal notification above
the byte bound still closes, while `bounded_session_events_v1` ensures valid Entry and Part events
use a safe reference first.

The framed reader orders cancellation intent before the SDK sees the frame. It records only
prompt and cancel routing fields, marking `session/cancel` as semantic Turn cancellation and
`$/cancel_request` as request-scoped cancellation. The SDK collapses both into the same cancelled
prompt context and calls its cancel hook after cancellation, so callback arrival order cannot be
the discriminator. The reader rejects duplicate JSON object keys and gives classification and
SDK dispatch the same validated routing values, preventing an ambiguous frame from selecting
different Sessions or request ids at the two layers. At admission, each `session/cancel` frame binds
to the connection's active Prompt or turn-starting command generation, or snapshots the exact
active Turn under the event-state lock. Its callback uses only that recorded id. No active Turn at
admission and a later successor conflict are both no-ops.

Prompt correlation is generation-scoped per Session. A second concurrent prompt is rejected
before SDK dispatch. Cancellation intent binds to the active generation, and the ACP request
does not complete from its cancelled SDK context: the agent requests the internal cancel, waits
for the matching durable terminal Entry and state, then applies the semantic or request-scoped
result. It retires per-Session prompt admission before publishing terminal state so a queued
prompt may start while the old response finishes. Session close uses the same barrier, resolving
standing permission callbacks and waiting for durable tool, permission and Turn completion,
response writes for its own standing Prompt or turn-starting command and physical writes for every
frame its projection mode actually emits before it detaches and answers. Negotiated Rudy mode
requires each close-owned terminal Entry or oversized carrier plus terminal state carrier. Generic
mode requires only actual display projections; carrier-only Entry and state events create no
waiter. Routing for an already-admitted close-owned frame remains installed through its acknowledgment. It
never waits for another connection's response write. A per-connection closing fence
rejects racing Session operations until detach and the close response finish; it grants nothing
over other connections. Permission response callbacks take the outer attachment's operation lease
before token claim or domain action. Fence installation snapshots close-owned identities and
installs proof waiters under the event-state lock, seeded from retained terminal proof, so a
concurrent terminal publication cannot be missed. A post-fence callback retires without acting.
A Turn identity committed after the initial snapshot by a pre-fence Prompt or turn-starting command
lease joins the close-owned set under the same lock, installs its proof waiters and issues an
exact-id cancel before that lease completes. No-active-Turn or successor conflict records that no
cancellation occurred and leaves Close waiting only for proof of the close-owned generation.

Prompt admission preflights the complete public typed `user_message` Entry through 7 MiB before
`session.submit`. Every accepted negotiated prompt therefore has an exact fragmented initial
carrier carrying its prompt and Turn correlation; an oversized Entry reference never substitutes
for that initial carrier.

Before steering, cancellation or failure becomes visible, the daemon terminalizes every admitted
non-done call. A call without a prior decision gets a fixed interrupt or invariant denial, then a
killed result for interrupt, an error result for the failure's causal call or a killed result for a
failure peer. This leaves no unmatched tool-use block in later request context. The daemon emits a terminal Turn state only after its matching terminal Entry is fsynced. If
terminal synchronization or a permission decision-batch append/sync fails, it emits no false
success, enters the same process-lifetime Session durability quarantine, makes the affected Session
unavailable for the rest of that Server process and closes subscribed connections with fixed
failure text. Every Session read, mutation, append and attachment commit enters one Server-owned
per-Session admission fence also used by quarantine. A racing operation therefore commits before
the cause and joins its cancellation or subscriber snapshot, installs the cause before releasing
its own failed commit, or observes the cause and changes nothing. A prior check grants no later
authority. A later daemon may run ordinary log recovery. Neither ACP adapter nor SessionClient
may infer success from an Entry notification without that state proof.

| Failure | Behavior |
|---|---|
| `rudy` absent on the box | Remote shell exits 111. Hosts installs and retries once. |
| no daemon answers `rudy acp --no-start` after safe socket checks | The remote process exits 112 before any ACP frame. Hosts alone maps that status to typed no-server. No other failure uses it. |
| `rudy acp` absent or stable v1 unsupported | A normal command fails with the remote version and `rudy hosts install`; never falls back to the private bridge. Forced install installs first, then uses the new binary's shutdown-control mode against any surviving old daemon. |
| required `_rudy` capability absent | Rudy client fails before opening a session and names the missing capability. Generic clients continue on the standard surface. |
| ACP adapter and daemon versions differ | Continue only when internal hello advertises every required daemon capability, report both versions and do not restart a compatible live daemon. |
| required internal daemon capability absent | Normal initialize returns `-32014`, `Incompatible daemon; restart Rudy`, writes the response and closes before exposing a Session surface. Hosts installs the new binary, opens its shutdown-control ACP mode against the old daemon and replaces it through terminal shutdown proof. |
| daemon start fails | Exit nonzero with a fixed diagnostic and correlation id. Keep the cause in the box log; never tail it over ssh. |
| ssh drops | Adapter exits. Daemon and turn continue. TUI reconnects and loads the same ULID. Headless exits nonzero and names `--continue`. |
| malformed or oversized ACP frame | Return a sanitized protocol error when possible, close on framing loss and never log raw secret-bearing payloads. |
| request, pre-SDK notification or outbound queue admission fills | Close the adapter connection; recovery is `session/load`. |
| known Rudy error | Map to the closest ACP or JSON-RPC error with safe structured data. |
| unknown internal error | Return `Internal error`, log the correlated detail on the box and expose no path, credential or provider body. |

Reconnect advances a client connection generation before load. Event pumps, permission
callbacks, prompt results and close signals carry that generation. The Client fails old Prompt,
turn-starting command and close waiters with transport failure, retires old permissions, stops the
old pump and discards later callbacks. Its public event channel stays open for recovered load
events; only a permanent close completes `Wait`.

## Daemon lifecycle

`rudy acp` shares one daemon dialer with the compatibility bridge. It connects to the host's
default socket, starts `rudy serve` detached when absent and joins the winner when concurrent
adapters race to start it.
Startup readiness, fixed startup errors and same-user socket checks stay one implementation.

Forced install first runs the missing-binary probe. When old `rudy acp --no-start` exists, Hosts
attempts normal `_rudy` initialization. Exit 112 proves no daemon: install, open normal ACP and do
not attempt shutdown. A compatible daemon returns its `instanceId`; Hosts installs, shuts it down
on that existing connection and verifies a different instance afterward. Missing `rudy`, stable
ACP unsupported or required edge capabilities absent causes install first. Hosts then runs the
new binary with `rudy acp --no-start` in exact shutdown-control mode: exit 112 again means no daemon
and proceeds directly to normal startup; otherwise the returned `instanceId` identifies the
surviving old daemon to shut down. Both shutdown paths wait for ACP stdio EOF after the terminal
`stopped` result, then open normal ACP and require another `instanceId`. The result, not EOF, proves
cleanup. A daemon too old for internal authenticated shutdown fails closed. No process id or signal
fallback returns.

`rudy hosts check` also uses `rudy acp --no-start`; it attempts normal initialize, reports missing
daemon capabilities without starting or replacing anything and exits. `rudy hosts stop` treats no
daemon as success and otherwise uses shutdown-control initialize followed by the same terminal
exchange.

## Rollout

1. Add the pinned ACP dependency, import guard and box-side `rudy acp` adapter. Keep bridge and
   every existing remote command.
2. Add the Client `SessionClient` port and ACP client adapter. Run local socket and embedded
   paths through the existing implementation of that port.
3. Reach ACP parity for new, load, resume, list, prompt, cancel, permission, close, model,
   thinking, mode and negotiated Rudy extensions.
4. Switch `--host` and `remote.host` to `exec rudy acp`. Keep bridge only as compatibility for
   installed older binaries during this phase.
5. Move check, stop and forced install to ACP, remove bridge and its private ssh transport once
   one release boundary has passed.
6. Specify Firecracker against the unchanged Tool port. ACP does not enter the guest boundary.

Every phase is a discrete green commit. The compatibility bridge is removed only after the
ACP remote real-path test covers reconnect, replay and permission.

## Testing

- Pin official ACP v1 schema fixtures and validate initialize, session lifecycle, permission,
  content, tool and extension envelopes against them.
- Unit-test every mapping above, capability negotiation, error redaction, 8 MiB framing,
  64-request admission, replay suppression and queue overflow.
- Fill request admission with handler calls and SDK validation failures; prove response writes
  release every slot and duplicate or excess ids disconnect only the adapter.
- Feed malformed secret-bearing frames before and after the SDK reader gate opens; assert no
  payload bytes reach stderr or captured logs.
- Make registry refresh return a provider error containing a sentinel secret; assert ACP notice,
  response and stderr omit it while the box log retains correlated detail.
- Make plugin and registry state carry sentinel error detail; assert their `_rudy` updates use
  only public or fixed text.
- Run agent adapter integration tests over pipes against the real internal protocol client.
- Run client adapter integration tests against a fake ACP agent built with the same SDK.
- Run a generic SDK client against `rudy acp`, with no `_rudy` metadata, through new, prompt,
  load, resume, list, cancel, permission and close. Verify delete is not advertised.
- Run the existing server suite over in-memory and Unix socket transports unchanged.
- Point `RUDY_SSH` at the test shim and drive the actual command
  `rudy -p --host shim <prompt>`. Cover daemon start, mapped cwd, sync, provider stream,
  permission, ssh loss, reconnect by load, missing binary exit 111 and forced replacement.
- Run `make check`, including the bridge shutdown regression that proves no unrelated desktop
  process is signaled.
- Drive one real box from the Mac before claiming the remote path works. Record the command,
  session ULID and observed reconnect or permission result.

## Not in this wave

- ACP v2. It is draft and changes session semantics. The adapters pin stable v1.
- SDK methods marked unstable, including standard fork and delete. Rudy fork remains a
  negotiated extension.
- ACP over HTTP or WebSocket. Stable v1 defines stdio; ssh carries it remotely.
- Provider authentication through ACP. Provider identity belongs to the box daemon.
- Workspace sync through ACP client filesystem or terminal methods.
- Firecracker implementation. Its tool boundary gets a separate spec and ADR.
- Replacing the internal protocol, append-only log or plugin interface with ACP.

## References

- [ACP architecture](https://agentclientprotocol.com/get-started/architecture)
- [ACP v1 schema](https://github.com/agentclientprotocol/agent-client-protocol/blob/main/schema/v1/schema.json)
- [ACP v1 transport](https://agentclientprotocol.com/protocol/v1/transports)
- [ACP session setup](https://agentclientprotocol.com/protocol/v1/session-setup)
- [ACP prompt turns](https://agentclientprotocol.com/protocol/v1/prompt-turn)
- [ACP tool calls and permission](https://agentclientprotocol.com/protocol/v1/tool-calls)
- [coder/acp-go-sdk](https://github.com/coder/acp-go-sdk)
