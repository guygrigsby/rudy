# ACP at the remote boundary

Status: design, approved in conversation 2026-09-14. Implementation is UNSHIPPED and tracked by
`rudy-4tf.1`. ADR 0032 records the decision. Contracts pass 10 carries the wire rows. This spec
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
only in Rudy types. Local socket, embedded and plugin paths keep the private protocol. Remote
TUI and headless paths implement the port through ACP. ACP types do not cross either adapter.

`rudy bridge` stays during the parity rollout. It is removed as a session carrier after the
ACP path passes the same remote tests. `rudy hosts stop` and forced install then use the
negotiated `_rudy/server_shutdown` extension, which maps to `server.shutdown` over the exact
greeted Unix connection held by `rudy acp`. The ACP request returns `stopped` only after the
adapter receives the matching internal `server.stopped` terminal proof and internal EOF. This
supersedes `rudy bridge --stop` once bridge compatibility is removed.

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
frame is capped at 8 MiB, below the selected SDK's fixed 10 MiB scanner ceiling. A larger
inbound or outbound frame closes the adapter with a bounded error and never reaches the daemon
or peer.

The SDK starts receiving during construction and its default logger can include a malformed raw
line. Rudy wraps its reader in a closed gate, installs a sanitized SDK logger, then opens the
gate. The sanitizer drops raw values, ids, request ids and unstructured errors. The reader and
writer enforce the frame cap independently of the SDK. The reader also admits no more than 64
active requests. It records each admitted JSON-RPC id, and the writer releases the slot when the
matching response is successfully written, including SDK validation failures. A duplicate id or
65th active request disconnects the adapter instead of creating another SDK goroutine. The reader emits
one fixed JSON-RPC parse or invalid-request error before closing a malformed envelope, without
logging its bytes. The ACP process writes only fixed diagnostics to stderr and never tails the
daemon's box log over ssh.

Frame, admission and writer refusals signal the adapter supervisor. It closes the ACP stream and
internal daemon connection so an SDK write failure cannot leave a half-open adapter.

The adapter reads ACP `initialize`, then dials the local Unix socket and sends internal
`client.hello`. A generic ACP client is an asker because permission requests are a standard ACP
client method. The Rudy client supplies `_meta._rudy.asker`; TUI sends true and headless sends
false. The adapter then answers initialize with:

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

The Mac client offers `_meta._rudy.version: 1`, `asker` and the capabilities it understands.
The agent returns the intersection plus its `home`, daemon `instanceId` and binary version. A
generic ACP client omits `_rudy` and receives a standard session surface. Unknown metadata is
ignored.

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
account. Listing uses bounded keyset pagination over descending Session ULIDs. Cursors are
connection-scoped authenticated tokens bound to the normalized filter, never persisted or
logged.

## Rudy extensions

Extension methods use the `_rudy/` prefix. Every request requires `_meta._rudy.version: 1` to
have been negotiated at initialize. The agent returns method-not-found to an unnegotiated
extension so a generic client cannot accidentally depend on it.

The extension catalogue and every request and result shape live once in
`rudy-contracts.md#rudy-extension-methods`. It covers fork, steer, compact, operator shell,
titles, slash commands, registry access, Rudy UI events and server-owned shutdown. A generated
Go catalogue and a contract guard will keep code and that table equal in both directions.

Rudy-specific fields on standard `session/update` use the closed `_meta._rudy.event` union in
the contracts. Exactly one standard update carries each internal event, using a metadata-only
session-info update when the event has no honest standard projection. The metadata names the
originating Session and carries complete Entry or `provider.Part` JSON as padded base64 when
byte fidelity matters. A `turn_failed` Entry is the exception: its failure message is replaced
with fixed public text and marked redacted before crossing ACP. Every assistant Entry and
streamed stop Part clears the provider's raw stop reason and marks the carrier redacted when a
raw value existed. Permission requests carry their exact input bytes the same way. The Rudy
client rejects missing or inconsistent negotiated metadata instead of reconstructing state from
ACP's display-safe form. Status items, widgets, plugin state, registry state and notices use the
exact `_rudy/session/update` union because they have no honest standard update shape. Notices
and plugin failure reasons cross only through explicit public text with fixed fallbacks.
Registry errors become fixed provider failure text. The adapter never forwards a nested error
through these successful update channels. A standard client may ignore all of them.

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

An ACP adapter connection declared as an asker routes permission requests. The Gate remains the
durable authority: an unsafe tool runs only after the permission decision is appended and
fsynced. If that ACP connection is the last asker and disappears, the standing request is
denied as `no_asker`. Headless declares no asker and cannot answer.

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

The framed reader orders cancellation intent before the SDK sees the frame. It records only
prompt and cancel routing fields, marking `session/cancel` as semantic Turn cancellation and
`$/cancel_request` as request-scoped cancellation. The SDK collapses both into the same cancelled
prompt context and calls its cancel hook after cancellation, so callback arrival order cannot be
the discriminator. The reader rejects duplicate JSON object keys and gives classification and
SDK dispatch the same validated routing values, preventing an ambiguous frame from selecting
different Sessions or request ids at the two layers.

| Failure | Behavior |
|---|---|
| `rudy` absent on the box | Remote shell exits 111. Hosts installs and retries once. |
| `rudy acp` absent or stable v1 unsupported | Fail with the remote version and `rudy hosts install`; never fall back to the private bridge. |
| required `_rudy` capability absent | Rudy client fails before opening a session and names the missing capability. Generic clients continue on the standard surface. |
| ACP adapter and daemon versions differ | Continue when internal hello succeeds, report both versions and do not restart a live daemon. |
| daemon start fails | Exit nonzero with a fixed diagnostic and correlation id. Keep the cause in the box log; never tail it over ssh. |
| ssh drops | Adapter exits. Daemon and turn continue. TUI reconnects and loads the same ULID. Headless exits nonzero and names `--continue`. |
| malformed or oversized ACP frame | Return a sanitized protocol error when possible, close on framing loss and never log raw secret-bearing payloads. |
| request admission or outbound queue fills | Close the adapter connection; recovery is `session/load`. |
| known Rudy error | Map to the closest ACP or JSON-RPC error with safe structured data. |
| unknown internal error | Return `Internal error`, log the correlated detail on the box and expose no path, credential or provider body. |

## Daemon lifecycle

`rudy acp` shares one daemon dialer with the compatibility bridge. It connects to the host's
default socket, starts `rudy serve` detached when absent and joins the winner when concurrent
adapters race to start it.
Startup readiness, fixed startup errors and same-user socket checks stay one implementation.

Forced install opens `rudy acp --no-start`, initializes with `_rudy`, remembers the returned
daemon `instanceId`, installs, calls `_rudy/server_shutdown`, waits for ACP stdio EOF after
the terminal `stopped` result, then opens the normal ACP path. The result, not EOF, proves
cleanup. The replacement must return another `instanceId`. A daemon too old for the extension
fails closed. No process id or signal fallback returns.

`rudy hosts check` also uses `rudy acp --no-start`; it initializes, inspects versions and exits
without starting a missing daemon. `rudy hosts stop` treats no daemon as success and otherwise
uses the same shutdown exchange.

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
