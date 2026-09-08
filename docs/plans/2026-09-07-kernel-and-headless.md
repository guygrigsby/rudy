# rudy kernel and headless Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `rudy` binary whose `rudy -p "prompt"` runs a full agent turn against the aperture proxy with the six built-in tools, the permission gate and a durable session log, through the same JSON-RPC protocol the TUI will use.

**Architecture:** One binary, one protocol. The kernel (session log, turn loop, gate, provider port, plugin registry, protocol server) is a set of Go packages with no framework underneath. Every tool, slash command and provider adapter is a linked-in plugin registered through the same interface an external plugin will use. The printer client embeds the server over an in-memory transport; nothing in this plan opens a socket.

**Tech Stack:** Go 1.26, cobra, viper, `oklog/ulid/v2`, `mvdan.cc/sh/v3/syntax`, `bmatcuk/doublestar/v4`, `net/http` with hand-rolled SSE. No Bubble Tea, no anthropic SDK, no MCP in this plan.

**Spec:** `docs/specs/2026-09-07-rudy-design.md`, with `docs/specs/rudy-domain-model.md` and `docs/specs/rudy-contracts.md` normative on names, fields, enum values and JSON shapes. Read the contracts' `entries.jsonl` section before Task 2 and its protocol section before Task 12.

## Global Constraints

- Module `github.com/guygrigsby/rudy`, `go 1.26`. `CGO_ENABLED=0` must build.
- `for range n`, never a three-clause count loop.
- No vendor or wire types outside `internal/provider/openaichat`. `grep -r '"net/http"' internal | grep -v provider` returns nothing after this plan except `internal/provider/httpx`.
- Tool inputs and thinking signatures are `json.RawMessage` or `string` carried byte-exact. Never unmarshal a tool input into a map and re-marshal it.
- An unsafe tool runs only after its `permission_decision` allow entry is appended and fsynced. No asker means deny.
- `config.toml` is read, never written. XDG paths: `XDG_CONFIG_HOME` else `~/.config`, `XDG_DATA_HOME` else `~/.local/share`, `XDG_RUNTIME_DIR` else `os.TempDir()`, each plus `/rudy`.
- No model lists in config beyond `[default]`. The registry comes from `/v1/models`. No timers.
- Every provider request carries `User-Agent: rudy/<version> (<GOOS>/<GOARCH>)` and `X-Rudy-Session: <session ulid>`. Retries honor `Retry-After` in seconds and HTTP-date forms, else exponential backoff 1s, 2s, 4s, 8s, 16s, five attempts.
- Errors are handled where recovery is possible. A plugin that fails to load is recorded with a notice and skipped. A provider failure after retries becomes a `turn_failed` entry.
- Tests: `go test -race ./...` green at every commit. Golden files under `testdata/`. No network in tests; recorded fixtures under `internal/provider/openaichat/testdata/` are the only provider bytes tests see.
- Commits: terse, verb-first, area prefix (`session:`, `turn:`, `gate:`, `provider:`, `plugin:`, `protocol:`, `server:`, `tools:`, `cli:`, `repo:`, `docs:`), no em or en dashes, no Oxford commas, no attribution trailers.
- Work is tracked in beads. Before starting a task run `bd create --title "<task title>" --type task` if no issue exists, claim it and close it in the task's final commit step.

---

## File structure

```
cmd/rudy/main.go                         cobra root and subcommand wiring
internal/cli/root.go                     root command, global flags, version
internal/cli/print.go                    rudy -p / --print: the headless printer client
internal/cli/models.go                   rudy models
internal/cli/sessions.go                 rudy sessions list
internal/cli/wire.go                     builds the server from config: store, registry, plugins, gate
internal/config/paths.go                 XDG paths
internal/config/config.go                Config struct, defaults, viper load, validation
internal/config/secret.go                auth reference resolution: env:, cache:
internal/workspace/workspace.go          Detect, git root, project id
internal/session/entry.go                Entry, Kind, payload types, enums, Block
internal/session/entry_json.go           flattened envelope MarshalJSON / UnmarshalJSON
internal/session/log.go                  append-only JSONL writer and reader, fsync
internal/session/blobs.go                content-addressed blob store
internal/session/session.go              Session aggregate: Open, Load, Append, Fork, derived getters
internal/session/recovery.go             lost results on Load
internal/session/store.go                directory layout, List, Lock
internal/provider/provider.go            Provider interface, Request, Message, ToolDef, Part
internal/provider/model.go               Model, Pricing, Capabilities, cost
internal/provider/registry.go            Registry: Refresh, snapshot, Resolve
internal/provider/httpx/client.go        http.Client wrapper: headers, retry, Retry-After
internal/provider/openaichat/client.go   openai_chat codec: Complete, ListModels
internal/provider/openaichat/request.go  domain Request to wire JSON
internal/provider/openaichat/stream.go   SSE reader and chunk to Part
internal/provider/openaichat/models.go   /v1/models with extension fields
internal/tool/tool.go                    Tool, Safety, Call, Result
internal/gate/gate.go                    Evaluate, Verdict
internal/gate/matcher.go                 MatcherFor, dangerous set, shell word parsing
internal/plugin/plugin.go                Plugin, Host, Command, Action
internal/plugin/registry.go              Registry: Load, states, capability lookup
internal/protocol/jsonrpc.go             JSON-RPC 2.0 envelope, error codes
internal/protocol/methods.go             method names, param and result structs, notification structs
internal/protocol/conn.go                Conn, Pipe (in-memory transport)
internal/protocol/client.go              Client: Call, Notifications
internal/turn/assemble.go                entries to provider.Request
internal/turn/runner.go                  Turn state machine and loop
internal/turn/system.go                  base system prompt plus AGENTS.md
internal/server/server.go                Server, connection loop, dispatch
internal/server/session_live.go          live session: runner, asker routing, observers
internal/plugins/tools/read/read.go      read tool plugin
internal/plugins/tools/write/write.go    write tool plugin
internal/plugins/tools/edit/edit.go      edit tool plugin
internal/plugins/tools/bash/bash.go      bash tool plugin
internal/plugins/tools/grep/grep.go      grep tool plugin
internal/plugins/tools/glob/glob.go      glob tool plugin
internal/plugins/initcmd/init.go         /init slash command plugin
internal/plugins/openaichat/plugin.go    provider plugin: one Provider per configured openai_chat entry
Makefile, .golangci.yml, .github/workflows/ci.yml
```

Package dependency order, top to bottom, no cycles: `session` (ulid only) → `tool` → `provider` → `gate` (session, tool) → `plugin` (tool, provider) → `protocol` (session, provider) → `turn` (session, provider, tool, gate, plugin) → `server` (all of the above) → `plugins/*` (plugin, tool, provider, session) → `cli` (everything).

## Interfaces

These are the exact names every task uses. A task that needs a type not listed here defines it in its own package and lists it under its own **Produces**.

### `internal/session`

```go
package session

type Kind string

const (
	KindSessionOpened      Kind = "session_opened"
	KindForkPoint          Kind = "fork_point"
	KindUserMessage        Kind = "user_message"
	KindAssistantMessage   Kind = "assistant_message"
	KindPermissionDecision Kind = "permission_decision"
	KindToolResult         Kind = "tool_result"
	KindModelChange        Kind = "model_change"
	KindModeChange         Kind = "mode_change"
	KindThinkingChange     Kind = "thinking_change"
	KindTitleChange        Kind = "title_change"
	KindCompaction         Kind = "compaction"
	KindTurnInterrupted    Kind = "turn_interrupted"
	KindTurnFailed         Kind = "turn_failed"
	KindNote               Kind = "note"
)

type Mode string          // "strict", "permissive", "off"
type ThinkingLevel string // "off", "low", "medium", "high"
type Source string        // "typed", "queued", "steer"
type StopReason string    // "end_turn", "tool_use", "max_tokens", "interrupted", "refused", "other"
type Outcome string       // "ok", "error", "killed", "lost"
type Decision string      // "allow", "deny"
type DecidedBy string     // "class", "mode", "allowance", "hook", "asker", "no_asker"
type Scope string         // "once", "session"
type Interrupt string     // "steer", "cancel"
type ErrorClass string    // "provider", "transport", "plugin", "internal"
type NoteRole string      // "info", "muted", "warn", "error"
type BlockType string     // "text", "image", "thinking", "tool_use"

// Each enum has func (v T) Valid() bool. The constants are the enum value with the type name
// prefixed: ModeStrict, ModePermissive, ModeOff, ThinkingOff .. ThinkingHigh, SourceTyped,
// SourceQueued, SourceSteer, StopEndTurn, StopToolUse, StopMaxTokens, StopInterrupted,
// StopRefused, StopOther, OutcomeOK, OutcomeError, OutcomeKilled, OutcomeLost, Allow, Deny,
// ByClass, ByMode, ByAllowance, ByHook, ByAsker, ByNoAsker, ScopeOnce, ScopeSession,
// InterruptSteer, InterruptCancel, ErrProvider, ErrTransport, ErrPlugin, ErrInternal,
// NoteInfo, NoteMuted, NoteWarn, NoteError, BlockText, BlockImage, BlockThinking, BlockToolUse.

type Workspace struct {
	Root      string `json:"root"`
	GitRoot   string `json:"git_root"` // empty means not a git repository
	ProjectID string `json:"project_id"`
}

type ModelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

func (m ModelRef) String() string // "provider:model"
func ParseModelRef(s string) (ModelRef, bool) // "provider:model"; false when no colon

type Usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

func (u Usage) Add(o Usage) Usage

// Block is a sum. Type selects which fields are meaningful; MarshalJSON emits only those.
type Block struct {
	Type      BlockType       `json:"type"`
	Text      string          `json:"text,omitempty"`       // text, thinking
	MediaType string          `json:"media_type,omitempty"` // image
	SHA256    string          `json:"sha256,omitempty"`     // image, names blobs/<sha256>
	Signature string          `json:"signature,omitempty"`  // thinking, provider bytes verbatim
	ID        string          `json:"id,omitempty"`         // tool_use
	Name      string          `json:"name,omitempty"`       // tool_use
	Input     json.RawMessage `json:"input,omitempty"`      // tool_use, provider bytes verbatim
}

func TextBlock(s string) Block
func ToolUseBlock(id, name string, input json.RawMessage) Block

type Matcher struct {
	Tool   string `json:"tool"`
	Prefix string `json:"prefix"`
}

type Payload interface{ Kind() Kind }

type SessionOpened struct {
	SchemaVersion int           `json:"schema_version"`
	RudyVersion   string        `json:"rudy_version"`
	Workspace     Workspace     `json:"workspace"`
	Model         ModelRef      `json:"model"`
	Thinking      ThinkingLevel `json:"thinking"`
	Mode          Mode          `json:"mode"`
	Agent         string        `json:"agent"` // "default" when none
}
type ForkPoint struct {
	ParentSessionID ulid.ULID `json:"parent_session_id"`
	ParentEntryID   ulid.ULID `json:"parent_entry_id"`
}
type UserMessage struct {
	Source  Source  `json:"source"`
	Content []Block `json:"content"`
}
type AssistantMessage struct {
	Model         ModelRef      `json:"model"`
	Thinking      ThinkingLevel `json:"thinking"`
	Content       []Block       `json:"content"`
	Usage         Usage         `json:"usage"`
	StopReason    StopReason    `json:"stop_reason"`
	StopReasonRaw string        `json:"stop_reason_raw"`
}
type PermissionDecision struct {
	ToolUseID string    `json:"tool_use_id"`
	Tool      string    `json:"tool"`
	Mode      Mode      `json:"mode"`
	Matcher   Matcher   `json:"matcher"`
	Decision  Decision  `json:"decision"`
	DecidedBy DecidedBy `json:"decided_by"`
	Scope     Scope     `json:"scope"`
	Reason    string    `json:"reason"`
}
type ToolResult struct {
	ToolUseID  string  `json:"tool_use_id"`
	Outcome    Outcome `json:"outcome"`
	Content    []Block `json:"content"`
	DurationMS int64   `json:"duration_ms"`
}
type ModelChange struct{ Model ModelRef `json:"model"` }
type ModeChange struct{ Mode Mode `json:"mode"` }
type ThinkingChange struct{ Thinking ThinkingLevel `json:"thinking"` }
type TitleChange struct{ Title string `json:"title"` }
type Compaction struct {
	Summary      string    `json:"summary"`
	FirstEntryID ulid.ULID `json:"first_entry_id"`
	LastEntryID  ulid.ULID `json:"last_entry_id"` // not before FirstEntryID
	Model        ModelRef  `json:"model"`         // the model that wrote the summary
	Usage        Usage     `json:"usage"`         // what writing it cost
}
// A turn id is the ULID of the user_message entry that started the turn. Turns are not
// stored. Runner.TurnID() returns its string form and every protocol turn_id is that string.
type TurnInterrupted struct {
	TurnID ulid.ULID `json:"turn_id"`
	How    Interrupt `json:"how"`
}
type TurnFailed struct {
	TurnID  ulid.ULID  `json:"turn_id"`
	Class   ErrorClass `json:"class"`
	Message string     `json:"message"`
	Retries int        `json:"retries"` // attempts made before giving up; 0 when not applicable
}
type Note struct {
	Plugin string   `json:"plugin"`
	Text   string   `json:"text"` // display only, never sent to a model
	Role   NoteRole `json:"role"`
}

// Each payload type implements Kind().

type Entry struct {
	ID      ulid.ULID
	At      time.Time
	Kind    Kind
	Payload Payload
}

// JSON is the flattened envelope from the contracts:
// {"id":"01K…","at":"2026-09-07T20:30:00.123456789-06:00","kind":"user_message","source":"typed","content":[…]}
func (e Entry) MarshalJSON() ([]byte, error)
func (e *Entry) UnmarshalJSON(b []byte) error

func NewID() ulid.ULID // monotonic within the process

// Log is the append-only entries.jsonl of one session directory.
type Log struct{ /* file, bufio.Writer, mutex */ }
func OpenLog(dir string) (*Log, error)
func (l *Log) Append(e Entry) error
func (l *Log) Sync() error   // flush and fsync
func (l *Log) Close() error

// ReadLog returns every complete entry. A truncated final line is dropped and reported
// through Truncated; any other malformed line is an error.
type Truncated struct{ Line int }
func (t Truncated) Error() string
func ReadLog(dir string) ([]Entry, error)

type Blobs struct{ dir string }
func OpenBlobs(dir string) (*Blobs, error)
func (b *Blobs) Put(data []byte) (sha256hex string, err error)
func (b *Blobs) Get(sha256hex string) ([]byte, error)

type Store struct{ root string }
func OpenStore(root string) (*Store, error)
func (st *Store) Root() string
func (st *Store) Dir(id ulid.ULID) string
type Summary struct {
	ID        ulid.ULID
	OpenedAt  time.Time
	Workspace Workspace
	Model     ModelRef
	Forked    bool
}
func (st *Store) List() ([]Summary, error) // newest first, reads only each first line
func (st *Store) Lock(id ulid.ULID) (unlock func(), err error) // flock on <dir>/lock; ErrLocked when held

var ErrLocked = errors.New("session: locked by another process")

type Session struct{ /* id, dir, log, blobs, own []Entry, inherited []Entry, unlock */ }
func Open(st *Store, opened SessionOpened) (*Session, error)                // new session, appends session_opened
func Load(st *Store, id ulid.ULID) (*Session, error)                       // replay, resolve fork parent, run Recovery
func (s *Session) ID() ulid.ULID
func (s *Session) Dir() string
func (s *Session) Blobs() *Blobs
func (s *Session) Append(p Payload) (Entry, error)                          // assigns ID and At; fsyncs when p is an allow PermissionDecision
func (s *Session) Fork(st *Store, at ulid.ULID) (*Session, error)           // new session whose first entry is fork_point
func (s *Session) Entries() []Entry                                         // inherited then own
func (s *Session) Workspace() Workspace
func (s *Session) Model() ModelRef
func (s *Session) Mode() Mode
func (s *Session) Thinking() ThinkingLevel
func (s *Session) Title() string                                            // "" when no title_change
func (s *Session) Agent() string
func (s *Session) Usage() Usage                                             // sum over assistant_message and compaction entries
func (s *Session) Allowances() []Matcher                                    // from allow decisions with scope session
func (s *Session) RequestContext() []Entry                                  // the last compaction entry then everything after it; all entries when none
func (s *Session) PendingToolUses() []Block                                 // tool_use blocks with no tool_result yet
func (s *Session) Close() error

// Invariants Append refuses, each with a sentinel error wrapping ErrInvariant:
var ErrInvariant = errors.New("session: invariant")
// session_opened or fork_point after the first entry; any entry before the first;
// permission_decision or tool_result whose tool_use id is not pending;
// a second permission_decision or tool_result for the same tool_use id;
// an invalid enum value; user_message or assistant_message with an invalid block;
// compaction whose ids are not both present and ordered.
```

### `internal/tool`

```go
package tool

type Safety string

const (
	Safe   Safety = "safe"
	Unsafe Safety = "unsafe"
)

type Call struct {
	ID        string          // tool_use id
	Name      string
	Input     json.RawMessage // verbatim
	Workspace session.Workspace
	SessionID ulid.ULID
}

type Result struct {
	Content []session.Block
	IsError bool
}

type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage // JSON Schema for Input
	Safety      Safety
	Invoke      func(ctx context.Context, call Call) (Result, error) // err means the tool could not run; IsError means it ran and failed
}
```

### `internal/provider`

```go
package provider

type Role string // "user", "assistant", "tool_result"

type Message struct {
	Role      Role
	Content   []session.Block
	ToolUseID string // tool_result only
}

type ToolDef struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

type Request struct {
	Model     session.ModelRef
	System    string
	Messages  []Message
	Tools     []ToolDef
	Thinking  session.ThinkingLevel
	MaxTokens int
	SessionID ulid.ULID // for the X-Rudy-Session header
}

type PartType string

const (
	PartTextDelta     PartType = "text_delta"
	PartThinkingDelta PartType = "thinking_delta"
	PartToolUseStart  PartType = "tool_use_start" // ID, Name
	PartToolUseDelta  PartType = "tool_use_delta" // ID, Input fragment in Text
	PartToolUseEnd    PartType = "tool_use_end"   // ID
	PartUsage         PartType = "usage"
	PartStop          PartType = "stop"
)

type Part struct {
	Type          PartType
	Text          string
	ID            string
	Name          string
	Usage         session.Usage
	StopReason    session.StopReason
	StopReasonRaw string
}

// Complete streams parts by calling emit in order until the stream ends. Returning a non-nil
// error from emit stops the stream and is returned. ctx cancellation returns ctx.Err().
type Provider interface {
	Name() string
	Complete(ctx context.Context, req Request, emit func(Part) error) error
	ListModels(ctx context.Context) ([]Model, error)
}

// Error classifies a provider failure after retries. Turn maps it to turn_failed.
type Error struct {
	Class    session.ErrorClass
	Status   int
	Message  string
	Body     []byte
	Attempts int // attempts httpx made before giving up
}
func (e *Error) Error() string

type Pricing struct {
	Input      string // decimal USD per token, verbatim; "" unknown
	Output     string
	CacheRead  string
	CacheWrite string
}
func (p Pricing) Cost(u session.Usage) (usd string, known bool) // big.Rat, 6 decimals

type Capabilities struct {
	Tools     bool
	Vision    bool
	Reasoning bool
}

type Model struct {
	Ref           session.ModelRef
	DisplayName   string
	ContextWindow int64 // 0 unknown
	MaxOutput     int64 // 0 unknown
	Pricing       Pricing
	Capabilities  Capabilities
}

type Registry struct{ /* providers, models, snapshot path, mutex */ }
func NewRegistry(snapshotPath string, providers ...Provider) *Registry
func (r *Registry) Refresh(ctx context.Context) error // ListModels on every provider concurrently; a failing provider keeps its previous models and is reported in the returned joined error
func (r *Registry) LoadSnapshot() error              // fills models from the snapshot file; missing file is not an error
func (r *Registry) Models() []Model                   // sorted by provider then id
func (r *Registry) Resolve(spec string) (Model, error) // "provider:id" or a bare id unique across providers; ErrAmbiguous lists candidates
func (r *Registry) Provider(name string) (Provider, bool)
var ErrUnknownModel = errors.New("provider: unknown model")
```

### `internal/provider/httpx`

```go
package httpx

type Client struct{ /* *http.Client, version string */ }
func New(version string) *Client
// Do sets User-Agent and X-Rudy-Session (when sessionID is non-zero), retries 429, 502, 503,
// 504 and transport errors five times honoring Retry-After, and returns the final response.
func (c *Client) Do(ctx context.Context, req *http.Request, sessionID ulid.ULID) (*http.Response, error)
func RetryAfter(h http.Header, now time.Time) (time.Duration, bool)
```

### `internal/provider/openaichat`

```go
package openaichat

type Options struct {
	Name    string
	BaseURL string            // ends with /v1
	Token   string            // "" sends no Authorization
	Headers map[string]string // extra, verbatim
	HTTP    *httpx.Client
}
func New(o Options) *Client
func (c *Client) Name() string
func (c *Client) Complete(ctx context.Context, req provider.Request, emit func(provider.Part) error) error
func (c *Client) ListModels(ctx context.Context) ([]provider.Model, error)
```

### `internal/gate`

```go
package gate

type Input struct {
	Tool         string
	Safety       tool.Safety
	Mode         session.Mode
	Args         json.RawMessage
	Allowances   []session.Matcher
	AskerPresent bool
}

type Verdict struct {
	Decision  session.Decision
	DecidedBy session.DecidedBy
	Ask       bool // true means Decision is not final and the asker must answer
	Matcher   session.Matcher
	Reason    string
}

type Gate struct{ dangerous []string }
func New(dangerous []string) *Gate
func (g *Gate) Evaluate(in Input) Verdict
func (g *Gate) MatcherFor(tool string, args json.RawMessage) session.Matcher
func (g *Gate) Dangerous(tool string, args json.RawMessage) (bool, string) // matched prefix
```

### `internal/plugin`

```go
package plugin

type Action interface{ isAction() }
type SubmitPrompt struct{ Text string } // the server appends a user_message with this text and starts a turn
type Notice struct{ Text string }
type NoAction struct{}

type CommandCall struct {
	SessionID ulid.ULID
	Workspace session.Workspace
	Args      string
}

type Command struct {
	Name        string // without the slash
	Description string
	Run         func(ctx context.Context, call CommandCall) (Action, error)
}

type Host interface {
	RegisterTool(t tool.Tool) error
	RegisterCommand(c Command) error
	RegisterProvider(p provider.Provider) error
	Config() map[string]any // the plugin's [plugins.<name>] table, may be empty
	Notice(text string)
}

type Plugin interface {
	Name() string
	Init(ctx context.Context, h Host) error
}

type State string // "loading", "ready", "failed", "stopped"

type Status struct {
	Name   string
	State  State
	Reason string // failed only
}

type Registry struct{ /* tools, commands, providers, statuses, notices */ }
func NewRegistry(config map[string]map[string]any, notice func(string)) *Registry
func (r *Registry) Load(ctx context.Context, plugins ...Plugin) // never returns an error; failures become statuses and notices
func (r *Registry) Tools() []tool.Tool
func (r *Registry) Tool(name string) (tool.Tool, bool)
func (r *Registry) Commands() []Command
func (r *Registry) Command(name string) (Command, bool)
func (r *Registry) Providers() []provider.Provider
func (r *Registry) Statuses() []Status
```

### `internal/protocol`

```go
package protocol

// JSON-RPC 2.0
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent on notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}
func (e *Error) Error() string

const (
	CodeInvalidArgument    = -32602
	CodeNotFound           = -32001
	CodeRefusedByInvariant = -32002
	CodeNoAsker            = -32003
	CodeConflict           = -32004
	CodeUnauthorized       = -32005
	CodeUnavailable        = -32006
	CodeProviderError      = -32007
	CodePluginError        = -32008
	CodeInterrupted        = -32009
)

// Methods, client to server
const (
	MethodClientHello        = "client.hello"
	MethodSessionOpen        = "session.open"
	MethodSessionResume      = "session.resume"
	MethodSessionFork        = "session.fork"
	MethodSessionList        = "session.list"
	MethodSessionClose       = "session.close"
	MethodSessionSubmit      = "session.submit"
	MethodSessionInterrupt   = "session.interrupt"
	MethodSessionAnswer      = "session.answer"
	MethodSessionSetModel    = "session.set_model"
	MethodSessionSetMode     = "session.set_mode"
	MethodSessionSetThinking = "session.set_thinking"
	MethodSessionSetTitle    = "session.set_title"
	MethodRegistryList       = "registry.list"
	MethodRegistryRefresh    = "registry.refresh"
	MethodCommandRun         = "command.run"
)

// Notifications, server to client
const (
	NotifyEntryAppended       = "entry.appended"
	NotifyStreamDelta         = "stream.delta"
	NotifyTurnState           = "turn.state"
	NotifyPermissionRequested = "permission.requested"
	NotifyNotice              = "notice"
)

type ClientHelloParams struct {
	Client  string `json:"client"`
	Version string `json:"version"`
	Asker   bool   `json:"asker"`
}
type ClientHelloResult struct {
	Server  string `json:"server"`
	Version string `json:"version"`
}
type SessionOpenParams struct {
	Cwd      string `json:"cwd"`
	Model    string `json:"model,omitempty"`    // "provider:id" or bare id; default from config
	Mode     string `json:"mode,omitempty"`     // default from config
	Thinking string `json:"thinking,omitempty"` // default from config
	Agent    string `json:"agent,omitempty"`    // default "default"
}
type SessionInfo struct {
	SessionID string            `json:"session_id"`
	Workspace session.Workspace `json:"workspace"`
	Model     session.ModelRef  `json:"model"`
	Mode      session.Mode      `json:"mode"`
	Thinking  session.ThinkingLevel `json:"thinking"`
	Title     string            `json:"title"`
}
type SessionResumeParams struct{ SessionID string `json:"session_id"` }
type SessionForkParams struct {
	SessionID string `json:"session_id"`
	AtEntryID string `json:"at_entry_id"`
}
type SessionListResult struct{ Sessions []session.Summary `json:"sessions"` }
type SessionCloseParams struct{ SessionID string `json:"session_id"` }
type SessionSubmitParams struct {
	SessionID string          `json:"session_id"`
	Content   []session.Block `json:"content"`
	Source    session.Source  `json:"source"` // typed or steer; queued is reserved for the TUI plan
}
type SessionSubmitResult struct{ TurnID string `json:"turn_id"` }
type SessionInterruptParams struct {
	SessionID string            `json:"session_id"`
	How       session.Interrupt `json:"how"`
}
type SessionAnswerParams struct {
	SessionID string           `json:"session_id"`
	ToolUseID string           `json:"tool_use_id"`
	Decision  session.Decision `json:"decision"`
	Scope     session.Scope    `json:"scope"`
	Reason    string           `json:"reason"`
}
type SessionSetModelParams struct {
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
}
type SessionSetModeParams struct {
	SessionID string       `json:"session_id"`
	Mode      session.Mode `json:"mode"`
}
type SessionSetThinkingParams struct {
	SessionID string                `json:"session_id"`
	Thinking  session.ThinkingLevel `json:"thinking"`
}
type SessionSetTitleParams struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
}
type RegistryListResult struct{ Models []provider.Model `json:"models"` }
type CommandRunParams struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	Args      string `json:"args"`
}
type CommandRunResult struct {
	TurnID string `json:"turn_id,omitempty"` // set when the command submitted a prompt
	Notice string `json:"notice,omitempty"`
}

type EntryAppended struct {
	SessionID string        `json:"session_id"`
	Entry     session.Entry `json:"entry"`
}
type StreamDelta struct {
	SessionID string        `json:"session_id"`
	TurnID    string        `json:"turn_id"`
	Part      provider.Part `json:"part"`
}
type TurnStateChanged struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	State     string `json:"state"`
}
type PermissionRequested struct {
	SessionID string          `json:"session_id"`
	TurnID    string          `json:"turn_id"`
	ToolUseID string          `json:"tool_use_id"`
	Tool      string          `json:"tool"`
	Input     json.RawMessage `json:"input"`
	Matcher   session.Matcher `json:"matcher"`
}
type NoticeParams struct {
	Level string `json:"level"` // "info", "warn", "error"
	Text  string `json:"text"`
}

// Conn carries one JSON message at a time in either direction.
type Conn interface {
	Send(ctx context.Context, msg any) error
	Recv(ctx context.Context) (json.RawMessage, error) // io.EOF when the peer closed
	Close() error
}
func Pipe() (client Conn, server Conn) // in-memory, unbuffered

type Notification struct {
	Method string
	Params json.RawMessage
}

type Client struct{ /* conn, pending map, notifications chan */ }
func NewClient(conn Conn) *Client                                   // starts a reader goroutine
func (c *Client) Call(ctx context.Context, method string, params, result any) error // *Error on a JSON-RPC error
func (c *Client) Notifications() <-chan Notification
func (c *Client) Close() error
```

### `internal/turn`

```go
package turn

type State string

const (
	Idle               State = "idle"
	Streaming          State = "streaming"
	RunningTool        State = "running_tool"
	AwaitingPermission State = "awaiting_permission"
	Steering           State = "steering"
	Completed          State = "completed"
	Failed             State = "failed"
)

type Question struct {
	ToolUseID string
	Tool      string
	Input     json.RawMessage
	Matcher   session.Matcher
}
type Answer struct {
	Decision session.Decision
	Scope    session.Scope
	Reason   string
}

// Asker answers permission questions. A nil Asker means nobody can answer.
type Asker interface {
	Ask(ctx context.Context, q Question) (Answer, error)
}

type Observer interface {
	EntryAppended(e session.Entry)
	Delta(turnID string, p provider.Part)
	StateChanged(turnID string, s State)
}

type Tools interface {
	Tool(name string) (tool.Tool, bool)
	Tools() []tool.Tool
}

type Runner struct{ /* session, provider, model, tools, gate, asker, observer, state, interrupt chan */ }

type Config struct {
	Session   *session.Session
	Provider  provider.Provider
	Model     provider.Model
	Tools     Tools
	Gate      *gate.Gate
	Asker     Asker // nil allowed
	Observer  Observer
	System    string // full system prompt
	MaxTokens int
}
func NewRunner(c Config) *Runner
func (r *Runner) TurnID() string
func (r *Runner) State() State
// Run appends msg as a user_message and drives the loop until Completed, Failed or Steering.
// It returns nil for Completed and Steering, and the turn_failed error for Failed.
func (r *Runner) Run(ctx context.Context, msg session.UserMessage) error
// Interrupt is safe from any goroutine. Steer stops the current step and leaves the runner in
// Steering; Cancel appends turn_interrupted and returns to Idle.
func (r *Runner) Interrupt(how session.Interrupt)

func Assemble(s *session.Session, tools []tool.Tool, system string, maxTokens int) provider.Request
func SystemPrompt(ws session.Workspace, version string) string // base prompt plus AGENTS.md from the workspace root and ~/.agents/AGENTS.md when present
```

### `internal/server`

```go
package server

type Deps struct {
	Version  string
	Config   *config.Config
	Store    *session.Store
	Registry *provider.Registry
	Plugins  *plugin.Registry
	Gate     *gate.Gate
}
type Server struct{ /* deps, live sessions, connections, mutex */ }
func New(d Deps) *Server
// Serve runs one connection until it closes. Notifications for a session go to every
// connection that opened, resumed or forked it. Permission questions go to the first
// connection on that session whose hello declared asker; none means no asker.
func (s *Server) Serve(ctx context.Context, conn protocol.Conn) error
func (s *Server) Shutdown(ctx context.Context) error // cancels turns, closes sessions
```

### `internal/config`

```go
package config

type Paths struct {
	Config  string // $XDG_CONFIG_HOME/rudy
	Data    string // $XDG_DATA_HOME/rudy
	Runtime string // $XDG_RUNTIME_DIR/rudy
	Cache   string // $XDG_CACHE_HOME/rudy
}
func XDG(env func(string) string, home string) Paths

type ProviderConfig struct {
	Wire    string            `mapstructure:"wire"`     // "openai_chat"
	BaseURL string            `mapstructure:"base_url"`
	Auth    string            `mapstructure:"auth"`     // "", "env:NAME", "cache:KEY"
	Headers map[string]string `mapstructure:"headers"`
}
type Config struct {
	Default struct {
		Provider string `mapstructure:"provider"`
		Model    string `mapstructure:"model"`
		Thinking string `mapstructure:"thinking"`
	} `mapstructure:"default"`
	Permissions struct {
		Mode      string   `mapstructure:"mode"`
		Dangerous []string `mapstructure:"dangerous"`
	} `mapstructure:"permissions"`
	Providers map[string]ProviderConfig    `mapstructure:"providers"`
	Plugins   map[string]map[string]any    `mapstructure:"plugins"`
	MaxTokens int                          `mapstructure:"max_tokens"`
}
// Load reads <paths.Config>/config.toml when present, applies RUDY_* env overrides and the
// defaults below, validates, and never writes. overrides win over everything.
func Load(paths Paths, overrides map[string]any) (*Config, error)
func Defaults() map[string]any
// Defaults: default.thinking "high", permissions.mode "strict",
// permissions.dangerous ["rm -rf", "rm -r", "git push --force", "git push -f", "git reset --hard",
// "git clean", "sudo", "chmod -R", "chown -R", "mkfs", "dd"], max_tokens 8192.
func ResolveSecret(ref string, env func(string) string, cachePath string) (string, error) // "" → "", env:NAME, cache:KEY reads KEY=value lines
```

---


### Task 1: Repo scaffold

**Files:**
- Create: `go.mod`
- Create: `cmd/rudy/main.go`
- Create: `internal/cli/root.go`
- Create: `Makefile`
- Create: `.golangci.yml`
- Create: `.github/workflows/ci.yml`
- Test: `internal/cli/root_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `cli.NewRoot() *cobra.Command`, `cli.Execute() error`, `cli.Version() string`, the unexported `cli.version` string set by ldflags. Every later `cli` task attaches a subcommand to the command returned by `NewRoot`.

- [ ] **Step 1: Create the beads issue and claim it**

```bash
bd create --title "Task 1: repo scaffold" --type task
bd update rudy-1 --claim
```

Use the id `bd create` prints in place of `rudy-1` below.

- [ ] **Step 2: Initialize the module and fetch cobra**

```bash
cd /Users/guygrigsby/projects/rudy
go mod init github.com/guygrigsby/rudy
go get github.com/spf13/cobra@v1.10.2
```

Expected: `go.mod` begins with `module github.com/guygrigsby/rudy` and a `go 1.26` line. If the toolchain wrote a patch version such as `go 1.26.2`, leave it.

- [ ] **Step 3: Write the failing test**

`internal/cli/root_test.go`:

```go
package cli

import (
	"bytes"
	"testing"
)

func TestRootVersion(t *testing.T) {
	root := NewRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got, want := out.String(), "rudy dev\n"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}
```

- [ ] **Step 4: Run the test to see it fail**

Run: `go test ./internal/cli/`
Expected: FAIL to compile with `undefined: NewRoot`.

- [ ] **Step 5: Write the root command and main**

`internal/cli/root.go`:

```go
// Package cli holds the cobra commands. The root carries global flags; every
// subcommand lives in its own file and attaches in NewRoot.
package cli

import (
	"github.com/spf13/cobra"
)

// version is set by the linker:
// -X github.com/guygrigsby/rudy/internal/cli.version=<v>
var version = "dev"

// Version reports the build version.
func Version() string { return version }

// NewRoot builds the root command. Subcommands attach here in later tasks.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "rudy",
		Short:         "A coding agent harness",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("rudy {{.Version}}\n")
	return root
}

// Execute runs the root command against os.Args and reports the error once.
func Execute() error {
	root := NewRoot()
	if err := root.Execute(); err != nil {
		root.PrintErrln("rudy:", err)
		return err
	}
	return nil
}
```

`cmd/rudy/main.go`:

```go
package main

import (
	"os"

	"github.com/guygrigsby/rudy/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		os.Exit(1)
	}
}
```

- [ ] **Step 6: Run the test to see it pass**

Run: `go test ./internal/cli/`
Expected: `ok  	github.com/guygrigsby/rudy/internal/cli`

- [ ] **Step 7: Write the Makefile, lint config and CI**

`Makefile`:

```make
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/guygrigsby/rudy/internal/cli.version=$(VERSION)

.PHONY: build test lint check fmt-check install redeploy

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/rudy ./cmd/rudy

test:
	CGO_ENABLED=0 go test -race ./...

lint:
	golangci-lint run ./...

fmt-check:
	@out=$$(gofmt -l . 2>/dev/null); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

check: fmt-check
	go vet ./...
	$(MAKE) lint
	$(MAKE) test

install:
	CGO_ENABLED=0 go install -ldflags "$(LDFLAGS)" ./cmd/rudy

redeploy: install
```

The recipe lines are indented with a tab, not spaces.

`.golangci.yml` (golangci-lint v2 format):

```yaml
version: "2"
linters:
  default: none
  enable:
    - errcheck
    - govet
    - ineffassign
    - staticcheck
    - unused
```

`.github/workflows/ci.yml`:

```yaml
name: ci
on:
  push:
  pull_request:
jobs:
  check:
    runs-on: ubuntu-latest
    env:
      CGO_ENABLED: "0"
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.26"
      - uses: golangci/golangci-lint-action@v8
        with:
          version: latest
          install-only: true
      - run: make check
```

- [ ] **Step 8: Run the full check**

Run: `make check`
Expected: gofmt prints nothing, `go vet` is silent, golangci-lint reports `0 issues`, tests pass. Then `make build` and `./bin/rudy --version` prints `rudy <git describe output>`.

- [ ] **Step 9: Commit and close the issue**

```bash
git add go.mod go.sum cmd internal Makefile .golangci.yml .github
git commit -m "repo: module, cobra root, Makefile and CI"
bd close rudy-1
```

---

### Task 2: Session entry kinds, payloads and the JSON envelope

**Files:**
- Create: `internal/session/entry.go`
- Create: `internal/session/entry_json.go`
- Test: `internal/session/entry_test.go`

**Interfaces:**
- Consumes: `github.com/oklog/ulid/v2`.
- Produces: everything under `internal/session` in the plan's Interfaces section except `Log`, `Blobs`, `Store` and `Session`: the enums with `Valid()`, `Workspace`, `ModelRef` with `String` and `ParseModelRef`, `Usage.Add`, `Block`, `TextBlock`, `ToolUseBlock`, `Matcher`, every payload type with `Kind()`, `Entry` with `MarshalJSON` and `UnmarshalJSON`, `NewID`. Also `Block.Validate() error` and `Validate(p Payload) error`, used by Task 4.

A note on bytes. `encoding/json` compacts and HTML-escapes any `json.RawMessage` it marshals, so `json.Marshal(entry)` cannot keep a tool input byte-exact. `Entry.MarshalJSON` therefore assembles the line itself: envelope and scalar fields through an encoder with HTML escaping off, block arrays written by hand with the `input` bytes copied verbatim. `Log.Append` in Task 3 calls `Entry.MarshalJSON` directly. Anything that runs an `Entry` through `json.Marshal` at a higher level (a protocol notification) gets compacted bytes, which is fine on the wire; the log line is the record.

- [ ] **Step 1: Create the beads issue and claim it**

```bash
bd create --title "Task 2: session entry kinds and JSON envelope" --type task
bd update rudy-2 --claim
go get github.com/oklog/ulid/v2@v2.1.2
```

- [ ] **Step 2: Write the failing tests**

`internal/session/entry_test.go`:

```go
package session

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func mustULID(t *testing.T, s string) ulid.ULID {
	t.Helper()
	id, err := ulid.ParseStrict(s)
	if err != nil {
		t.Fatalf("parse ulid %q: %v", s, err)
	}
	return id
}

func TestEnumsValid(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
		v    interface{ Valid() bool }
	}{
		{"mode strict", true, ModeStrict},
		{"mode bogus", false, Mode("loose")},
		{"thinking high", true, ThinkingHigh},
		{"thinking bogus", false, ThinkingLevel("max")},
		{"source steer", true, SourceSteer},
		{"stop interrupted", true, StopInterrupted},
		{"stop bogus", false, StopReason("done")},
		{"outcome lost", true, OutcomeLost},
		{"decision allow", true, Allow},
		{"decided by hook", true, ByHook},
		{"decided by bogus", false, DecidedBy("user")},
		{"scope session", true, ScopeSession},
		{"interrupt cancel", true, InterruptCancel},
		{"error class transport", true, ErrTransport},
		{"error class plugin", true, ErrPlugin},
		{"note role muted", true, NoteMuted},
		{"note role bogus", false, NoteRole("loud")},
		{"block tool_use", true, BlockToolUse},
		{"block bogus", false, BlockType("audio")},
		{"kind note", true, KindNote},
		{"kind bogus", false, Kind("event")},
	}
	for _, c := range cases {
		if got := c.v.Valid(); got != c.ok {
			t.Errorf("%s: Valid() = %v, want %v", c.name, got, c.ok)
		}
	}
}

func TestModelRefString(t *testing.T) {
	ref := ModelRef{Provider: "aperture", Model: "cline-pass/kimi-k3"}
	if got := ref.String(); got != "aperture:cline-pass/kimi-k3" {
		t.Fatalf("String() = %q", got)
	}
	back, ok := ParseModelRef("aperture:cline-pass/kimi-k3")
	if !ok || back != ref {
		t.Fatalf("ParseModelRef = %+v, %v", back, ok)
	}
	if _, ok := ParseModelRef("kimi-k3"); ok {
		t.Fatal("bare id must not parse as a ModelRef")
	}
}

func TestUsageAdd(t *testing.T) {
	a := Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4}
	b := Usage{Input: 10, Output: 20, CacheRead: 30, CacheWrite: 40}
	if got := a.Add(b); got != (Usage{11, 22, 33, 44}) {
		t.Fatalf("Add = %+v", got)
	}
}

func TestBlockMarshalEmitsOnlyVariantFields(t *testing.T) {
	img := Block{Type: BlockImage, MediaType: "image/png", SHA256: "abc", Text: "leak", ID: "leak"}
	b, err := json.Marshal(img)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"type":"image","media_type":"image/png","sha256":"abc"}`; got != want {
		t.Fatalf("image block = %s, want %s", got, want)
	}
	var bad Block
	if err := json.Unmarshal([]byte(`{"type":"audio"}`), &bad); err == nil {
		t.Fatal("unknown block type must fail to unmarshal")
	}
}

func TestNewIDMonotonic(t *testing.T) {
	prev := NewID()
	for range 1000 {
		next := NewID()
		if next.Compare(prev) <= 0 {
			t.Fatalf("ids not increasing: %s then %s", prev, next)
		}
		prev = next
	}
}

func TestEntryRoundTripAllKinds(t *testing.T) {
	id1 := mustULID(t, "01K4M0A7Q8ZJ3N6R9T2V5X8B1D")
	id2 := mustULID(t, "01K4M0A8Q8ZJ3N6R9T2V5X8B1D")
	at := time.Date(2026, 9, 7, 20, 30, 0, 123456789, time.FixedZone("MDT", -6*3600))
	payloads := []Payload{
		SessionOpened{SchemaVersion: 1, RudyVersion: "0.1.0", Workspace: Workspace{Root: "/w", GitRoot: "/w", ProjectID: "local/w"}, Model: ModelRef{"aperture", "cline-pass/kimi-k3"}, Thinking: ThinkingHigh, Mode: ModeStrict, Agent: "default"},
		ForkPoint{ParentSessionID: id1, ParentEntryID: id2},
		UserMessage{Source: SourceTyped, Content: []Block{TextBlock("fix the flaky fork test")}},
		AssistantMessage{Model: ModelRef{"aperture", "cline-pass/kimi-k3"}, Thinking: ThinkingHigh, Content: []Block{TextBlock("Looking."), ToolUseBlock("toolu_01", "bash", json.RawMessage(`{"command":"go test ./..."}`))}, Usage: Usage{Input: 1200, Output: 80}, StopReason: StopToolUse, StopReasonRaw: "tool_calls"},
		PermissionDecision{ToolUseID: "toolu_01", Tool: "bash", Mode: ModeStrict, Matcher: Matcher{Tool: "bash", Prefix: "go test"}, Decision: Allow, DecidedBy: ByAsker, Scope: ScopeSession, Reason: "allow for session"},
		ToolResult{ToolUseID: "toolu_01", Outcome: OutcomeOK, Content: []Block{TextBlock("ok\n")}, DurationMS: 412},
		ModelChange{Model: ModelRef{"aperture", "gpt-5.6-sol"}},
		ModeChange{Mode: ModePermissive},
		ThinkingChange{Thinking: ThinkingMedium},
		TitleChange{Title: "fork off-by-one"},
		Compaction{Summary: "earlier work", FirstEntryID: id1, LastEntryID: id2, Model: ModelRef{"aperture", "cline-pass/kimi-k3"}, Usage: Usage{Input: 40000, Output: 900}},
		TurnInterrupted{TurnID: id1, How: InterruptCancel},
		TurnFailed{TurnID: id1, Class: ErrProvider, Message: "502 after 5 attempts", Retries: 5},
		Note{Plugin: "memory", Text: "3 concepts folded", Role: NoteMuted},
	}
	for _, p := range payloads {
		e := Entry{ID: id1, At: at, Kind: p.Kind(), Payload: p}
		line, err := e.MarshalJSON()
		if err != nil {
			t.Fatalf("%s: marshal: %v", p.Kind(), err)
		}
		if !strings.HasPrefix(string(line), `{"id":"01K4M0A7Q8ZJ3N6R9T2V5X8B1D","at":"2026-09-07T20:30:00.123456789-06:00","kind":"`+string(p.Kind())+`"`) {
			t.Fatalf("%s: envelope prefix wrong: %s", p.Kind(), line)
		}
		var back Entry
		if err := back.UnmarshalJSON(line); err != nil {
			t.Fatalf("%s: unmarshal: %v\n%s", p.Kind(), err, line)
		}
		again, err := back.MarshalJSON()
		if err != nil {
			t.Fatalf("%s: re-marshal: %v", p.Kind(), err)
		}
		if !bytes.Equal(line, again) {
			t.Fatalf("%s: not stable:\n%s\n%s", p.Kind(), line, again)
		}
	}
}

func TestEntryToolUseInputByteExact(t *testing.T) {
	raw := json.RawMessage(`{"path": "go.mod",  "html":"<b>&</b>", "z":1, "a":2}`)
	e := Entry{ID: NewID(), At: time.Now(), Kind: KindAssistantMessage, Payload: AssistantMessage{
		Model: ModelRef{"aperture", "m"}, Thinking: ThinkingOff,
		Content:    []Block{ToolUseBlock("t1", "read", raw)},
		StopReason: StopToolUse, StopReasonRaw: "tool_calls",
	}}
	line, err := e.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(line, []byte(`"input":`+string(raw))) {
		t.Fatalf("input bytes changed on the line:\n%s", line)
	}
	var back Entry
	if err := back.UnmarshalJSON(line); err != nil {
		t.Fatal(err)
	}
	got := back.Payload.(AssistantMessage).Content[0].Input
	if !bytes.Equal(got, raw) {
		t.Fatalf("input after round trip = %s, want %s", got, raw)
	}
}

func TestEntryTextAndSignatureVerbatim(t *testing.T) {
	e := Entry{ID: NewID(), At: time.Now(), Kind: KindAssistantMessage, Payload: AssistantMessage{
		Model: ModelRef{"aperture", "m"}, Thinking: ThinkingHigh,
		Content: []Block{
			{Type: BlockThinking, Text: "plan <x>", Signature: "EqQBCkYIBRgCIkD+/abc=="},
			TextBlock("see <b>bold</b> & done"),
		},
		StopReason: StopEndTurn, StopReasonRaw: "stop",
	}}
	line, err := e.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"signature":"EqQBCkYIBRgCIkD+/abc=="`, `"text":"see <b>bold</b> & done"`, `"text":"plan <x>"`} {
		if !bytes.Contains(line, []byte(want)) {
			t.Fatalf("missing %s in\n%s", want, line)
		}
	}
}

func TestEntryUnmarshalRejects(t *testing.T) {
	cases := map[string]string{
		"unknown kind":  `{"id":"01K4M0A7Q8ZJ3N6R9T2V5X8B1D","at":"2026-09-07T20:30:00Z","kind":"event"}`,
		"invalid enum":  `{"id":"01K4M0A7Q8ZJ3N6R9T2V5X8B1D","at":"2026-09-07T20:30:00Z","kind":"mode_change","mode":"loose"}`,
		"invalid block": `{"id":"01K4M0A7Q8ZJ3N6R9T2V5X8B1D","at":"2026-09-07T20:30:00Z","kind":"user_message","source":"typed","content":[{"type":"audio"}]}`,
		"bad id":        `{"id":"nope","at":"2026-09-07T20:30:00Z","kind":"mode_change","mode":"off"}`,
	}
	for name, line := range cases {
		var e Entry
		if err := e.UnmarshalJSON([]byte(line)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
```

- [ ] **Step 3: Run the tests to see them fail**

Run: `go test ./internal/session/`
Expected: FAIL to compile with `undefined: ModeStrict` and many more undefined names.

- [ ] **Step 4: Write the types**

`internal/session/entry.go`:

```go
// Package session is the core: the append-only log, its entry kinds, the
// aggregate that derives everything from the log, and the store that holds
// session directories. It imports nothing from the rest of rudy.
package session

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// Kind is the closed set of entry payloads.
type Kind string

const (
	KindSessionOpened      Kind = "session_opened"
	KindForkPoint          Kind = "fork_point"
	KindUserMessage        Kind = "user_message"
	KindAssistantMessage   Kind = "assistant_message"
	KindPermissionDecision Kind = "permission_decision"
	KindToolResult         Kind = "tool_result"
	KindModelChange        Kind = "model_change"
	KindModeChange         Kind = "mode_change"
	KindThinkingChange     Kind = "thinking_change"
	KindTitleChange        Kind = "title_change"
	KindCompaction         Kind = "compaction"
	KindTurnInterrupted    Kind = "turn_interrupted"
	KindTurnFailed         Kind = "turn_failed"
	KindNote               Kind = "note"
)

func (k Kind) Valid() bool {
	switch k {
	case KindSessionOpened, KindForkPoint, KindUserMessage, KindAssistantMessage,
		KindPermissionDecision, KindToolResult, KindModelChange, KindModeChange,
		KindThinkingChange, KindTitleChange, KindCompaction, KindTurnInterrupted,
		KindTurnFailed, KindNote:
		return true
	}
	return false
}

// Mode is the permission mode.
type Mode string

const (
	ModeStrict     Mode = "strict"
	ModePermissive Mode = "permissive"
	ModeOff        Mode = "off"
)

func (m Mode) Valid() bool { return m == ModeStrict || m == ModePermissive || m == ModeOff }

// ThinkingLevel is the extended thinking budget requested from a model.
type ThinkingLevel string

const (
	ThinkingOff    ThinkingLevel = "off"
	ThinkingLow    ThinkingLevel = "low"
	ThinkingMedium ThinkingLevel = "medium"
	ThinkingHigh   ThinkingLevel = "high"
)

func (l ThinkingLevel) Valid() bool {
	return l == ThinkingOff || l == ThinkingLow || l == ThinkingMedium || l == ThinkingHigh
}

// Source says how a user message arrived.
type Source string

const (
	SourceTyped  Source = "typed"
	SourceQueued Source = "queued"
	SourceSteer  Source = "steer"
)

func (s Source) Valid() bool { return s == SourceTyped || s == SourceQueued || s == SourceSteer }

// StopReason is the domain classification of why a completion ended.
type StopReason string

const (
	StopEndTurn     StopReason = "end_turn"
	StopToolUse     StopReason = "tool_use"
	StopMaxTokens   StopReason = "max_tokens"
	StopInterrupted StopReason = "interrupted"
	StopRefused     StopReason = "refused"
	StopOther       StopReason = "other"
)

func (r StopReason) Valid() bool {
	switch r {
	case StopEndTurn, StopToolUse, StopMaxTokens, StopInterrupted, StopRefused, StopOther:
		return true
	}
	return false
}

// Outcome is how a tool invocation ended.
type Outcome string

const (
	OutcomeOK     Outcome = "ok"
	OutcomeError  Outcome = "error"
	OutcomeKilled Outcome = "killed"
	OutcomeLost   Outcome = "lost"
)

func (o Outcome) Valid() bool {
	return o == OutcomeOK || o == OutcomeError || o == OutcomeKilled || o == OutcomeLost
}

// Decision is the gate's answer.
type Decision string

const (
	Allow Decision = "allow"
	Deny  Decision = "deny"
)

func (d Decision) Valid() bool { return d == Allow || d == Deny }

// DecidedBy says who or what produced a decision.
type DecidedBy string

const (
	ByClass     DecidedBy = "class"
	ByMode      DecidedBy = "mode"
	ByAllowance DecidedBy = "allowance"
	ByHook      DecidedBy = "hook"
	ByAsker     DecidedBy = "asker"
	ByNoAsker   DecidedBy = "no_asker"
)

func (d DecidedBy) Valid() bool {
	switch d {
	case ByClass, ByMode, ByAllowance, ByHook, ByAsker, ByNoAsker:
		return true
	}
	return false
}

// Scope is how long an allow lasts.
type Scope string

const (
	ScopeOnce    Scope = "once"
	ScopeSession Scope = "session"
)

func (s Scope) Valid() bool { return s == ScopeOnce || s == ScopeSession }

// Interrupt is how a turn was interrupted.
type Interrupt string

const (
	InterruptSteer  Interrupt = "steer"
	InterruptCancel Interrupt = "cancel"
)

func (i Interrupt) Valid() bool { return i == InterruptSteer || i == InterruptCancel }

// ErrorClass classifies a turn failure.
type ErrorClass string

const (
	ErrProvider  ErrorClass = "provider"  // the provider answered with an error after retries
	ErrTransport ErrorClass = "transport" // the provider never answered
	ErrPlugin    ErrorClass = "plugin"    // a plugin fault: a panic or a programming error, not a tool that ran and reported an error
	ErrInternal  ErrorClass = "internal"  // a rudy fault
)

func (c ErrorClass) Valid() bool {
	return c == ErrProvider || c == ErrTransport || c == ErrPlugin || c == ErrInternal
}

// NoteRole is the theme role a client renders a note with.
type NoteRole string

const (
	NoteInfo  NoteRole = "info"
	NoteMuted NoteRole = "muted"
	NoteWarn  NoteRole = "warn"
	NoteError NoteRole = "error"
)

func (r NoteRole) Valid() bool {
	return r == NoteInfo || r == NoteMuted || r == NoteWarn || r == NoteError
}

// BlockType selects the variant of a Block.
type BlockType string

const (
	BlockText     BlockType = "text"
	BlockImage    BlockType = "image"
	BlockThinking BlockType = "thinking"
	BlockToolUse  BlockType = "tool_use"
)

func (b BlockType) Valid() bool {
	return b == BlockText || b == BlockImage || b == BlockThinking || b == BlockToolUse
}

// Workspace is the directory a session acts on.
type Workspace struct {
	Root      string `json:"root"`
	GitRoot   string `json:"git_root"` // empty means not a git repository
	ProjectID string `json:"project_id"`
}

// ModelRef names a model on a provider.
type ModelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// String renders "provider:model".
func (m ModelRef) String() string { return m.Provider + ":" + m.Model }

// ParseModelRef splits "provider:model". A string with no colon is not a ref.
func ParseModelRef(s string) (ModelRef, bool) {
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return ModelRef{}, false
	}
	return ModelRef{Provider: s[:i], Model: s[i+1:]}, true
}

// Usage is a provider's token accounting for one completion.
type Usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// Add sums two usages.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		Input:      u.Input + o.Input,
		Output:     u.Output + o.Output,
		CacheRead:  u.CacheRead + o.CacheRead,
		CacheWrite: u.CacheWrite + o.CacheWrite,
	}
}

// Block is a sum. Type selects which fields are meaningful.
type Block struct {
	Type      BlockType       `json:"type"`
	Text      string          `json:"text,omitempty"`       // text, thinking
	MediaType string          `json:"media_type,omitempty"` // image
	SHA256    string          `json:"sha256,omitempty"`     // image, names blobs/<sha256>
	Signature string          `json:"signature,omitempty"`  // thinking, provider bytes verbatim
	ID        string          `json:"id,omitempty"`         // tool_use
	Name      string          `json:"name,omitempty"`       // tool_use
	Input     json.RawMessage `json:"input,omitempty"`      // tool_use, provider bytes verbatim
}

// TextBlock builds a text block.
func TextBlock(s string) Block { return Block{Type: BlockText, Text: s} }

// ToolUseBlock builds a tool_use block; input is kept verbatim.
func ToolUseBlock(id, name string, input json.RawMessage) Block {
	return Block{Type: BlockToolUse, ID: id, Name: name, Input: input}
}

// Validate checks the variant's required fields.
func (b Block) Validate() error {
	switch b.Type {
	case BlockText:
		if b.Text == "" {
			return errors.New("text block: empty text")
		}
	case BlockImage:
		if b.MediaType == "" || b.SHA256 == "" {
			return errors.New("image block: media_type and sha256 required")
		}
	case BlockThinking:
		// text may be empty when a provider sends only a signature
	case BlockToolUse:
		if b.ID == "" || b.Name == "" {
			return errors.New("tool_use block: id and name required")
		}
		if len(b.Input) > 0 && !json.Valid(b.Input) {
			return errors.New("tool_use block: input is not valid JSON")
		}
	default:
		return fmt.Errorf("block: invalid type %q", b.Type)
	}
	return nil
}

// Matcher is the gate's classification of a tool input.
type Matcher struct {
	Tool   string `json:"tool"`
	Prefix string `json:"prefix"`
}

// Payload is one entry kind's data.
type Payload interface{ Kind() Kind }

type SessionOpened struct {
	SchemaVersion int           `json:"schema_version"`
	RudyVersion   string        `json:"rudy_version"`
	Workspace     Workspace     `json:"workspace"`
	Model         ModelRef      `json:"model"`
	Thinking      ThinkingLevel `json:"thinking"`
	Mode          Mode          `json:"mode"`
	Agent         string        `json:"agent"` // "default" when none
}

type ForkPoint struct {
	ParentSessionID ulid.ULID `json:"parent_session_id"`
	ParentEntryID   ulid.ULID `json:"parent_entry_id"`
}

type UserMessage struct {
	Source  Source  `json:"source"`
	Content []Block `json:"content"`
}

type AssistantMessage struct {
	Model         ModelRef      `json:"model"`
	Thinking      ThinkingLevel `json:"thinking"`
	Content       []Block       `json:"content"`
	Usage         Usage         `json:"usage"`
	StopReason    StopReason    `json:"stop_reason"`
	StopReasonRaw string        `json:"stop_reason_raw"`
}

type PermissionDecision struct {
	ToolUseID string    `json:"tool_use_id"`
	Tool      string    `json:"tool"`
	Mode      Mode      `json:"mode"`
	Matcher   Matcher   `json:"matcher"`
	Decision  Decision  `json:"decision"`
	DecidedBy DecidedBy `json:"decided_by"`
	Scope     Scope     `json:"scope"`
	Reason    string    `json:"reason"`
}

type ToolResult struct {
	ToolUseID  string  `json:"tool_use_id"`
	Outcome    Outcome `json:"outcome"`
	Content    []Block `json:"content"`
	DurationMS int64   `json:"duration_ms"`
}

type ModelChange struct {
	Model ModelRef `json:"model"`
}

type ModeChange struct {
	Mode Mode `json:"mode"`
}

type ThinkingChange struct {
	Thinking ThinkingLevel `json:"thinking"`
}

type TitleChange struct {
	Title string `json:"title"`
}

type Compaction struct {
	Summary      string    `json:"summary"`
	FirstEntryID ulid.ULID `json:"first_entry_id"`
	LastEntryID  ulid.ULID `json:"last_entry_id"` // not before FirstEntryID
	Model        ModelRef  `json:"model"`         // the model that wrote the summary
	Usage        Usage     `json:"usage"`         // what writing it cost, verbatim
}

// A turn id is the ULID of the user_message entry that started the turn. Turns are not
// stored; that id is enough to find one in the log, and it is the turn_id every protocol
// message carries.
type TurnInterrupted struct {
	TurnID ulid.ULID `json:"turn_id"`
	How    Interrupt `json:"how"`
}

type TurnFailed struct {
	TurnID  ulid.ULID  `json:"turn_id"`
	Class   ErrorClass `json:"class"`
	Message string     `json:"message"`
	Retries int        `json:"retries"` // attempts made before giving up; 0 when not applicable
}

// Note is display only and never sent to a model.
type Note struct {
	Plugin string   `json:"plugin"`
	Text   string   `json:"text"`
	Role   NoteRole `json:"role"`
}

func (SessionOpened) Kind() Kind      { return KindSessionOpened }
func (ForkPoint) Kind() Kind          { return KindForkPoint }
func (UserMessage) Kind() Kind        { return KindUserMessage }
func (AssistantMessage) Kind() Kind   { return KindAssistantMessage }
func (PermissionDecision) Kind() Kind { return KindPermissionDecision }
func (ToolResult) Kind() Kind         { return KindToolResult }
func (ModelChange) Kind() Kind        { return KindModelChange }
func (ModeChange) Kind() Kind         { return KindModeChange }
func (ThinkingChange) Kind() Kind     { return KindThinkingChange }
func (TitleChange) Kind() Kind        { return KindTitleChange }
func (Compaction) Kind() Kind         { return KindCompaction }
func (TurnInterrupted) Kind() Kind    { return KindTurnInterrupted }
func (TurnFailed) Kind() Kind         { return KindTurnFailed }
func (Note) Kind() Kind               { return KindNote }

// Validate checks a payload's enums and blocks. Cross-entry rules live in
// Session.Append; this is what can be checked on the payload alone.
func Validate(p Payload) error {
	blocks := func(bs []Block, allowed ...BlockType) error {
		for _, b := range bs {
			if err := b.Validate(); err != nil {
				return err
			}
			ok := false
			for _, a := range allowed {
				if b.Type == a {
					ok = true
				}
			}
			if !ok {
				return fmt.Errorf("block type %q not allowed in %s", b.Type, p.Kind())
			}
		}
		return nil
	}
	switch v := p.(type) {
	case SessionOpened:
		if v.SchemaVersion < 1 || v.RudyVersion == "" || v.Workspace.Root == "" || v.Model == (ModelRef{}) || !v.Thinking.Valid() || !v.Mode.Valid() || v.Agent == "" {
			return errors.New("session_opened: incomplete")
		}
	case ForkPoint:
		if v.ParentSessionID.IsZero() || v.ParentEntryID.IsZero() {
			return errors.New("fork_point: parent ids required")
		}
	case UserMessage:
		if !v.Source.Valid() {
			return fmt.Errorf("user_message: invalid source %q", v.Source)
		}
		if len(v.Content) == 0 {
			return errors.New("user_message: at least one block")
		}
		return blocks(v.Content, BlockText, BlockImage)
	case AssistantMessage:
		if v.Model == (ModelRef{}) || !v.Thinking.Valid() || !v.StopReason.Valid() {
			return errors.New("assistant_message: incomplete")
		}
		return blocks(v.Content, BlockText, BlockThinking, BlockToolUse)
	case PermissionDecision:
		if v.ToolUseID == "" || v.Tool == "" || !v.Mode.Valid() || !v.Decision.Valid() || !v.DecidedBy.Valid() || !v.Scope.Valid() || v.Reason == "" {
			return errors.New("permission_decision: incomplete")
		}
	case ToolResult:
		if v.ToolUseID == "" || !v.Outcome.Valid() {
			return errors.New("tool_result: incomplete")
		}
		return blocks(v.Content, BlockText, BlockImage)
	case ModelChange:
		if v.Model == (ModelRef{}) {
			return errors.New("model_change: model required")
		}
	case ModeChange:
		if !v.Mode.Valid() {
			return fmt.Errorf("mode_change: invalid mode %q", v.Mode)
		}
	case ThinkingChange:
		if !v.Thinking.Valid() {
			return fmt.Errorf("thinking_change: invalid level %q", v.Thinking)
		}
	case TitleChange:
		if v.Title == "" {
			return errors.New("title_change: empty title")
		}
	case Compaction:
		if v.FirstEntryID.IsZero() || v.LastEntryID.IsZero() {
			return errors.New("compaction: entry ids required")
		}
		if v.FirstEntryID.Compare(v.LastEntryID) > 0 {
			return errors.New("compaction: last_entry_id before first_entry_id")
		}
		if v.Model.Provider == "" || v.Model.Model == "" {
			return errors.New("compaction: model required")
		}
	case TurnInterrupted:
		if v.TurnID.IsZero() {
			return errors.New("turn_interrupted: turn_id required")
		}
		if !v.How.Valid() {
			return fmt.Errorf("turn_interrupted: invalid how %q", v.How)
		}
	case TurnFailed:
		if v.TurnID.IsZero() || !v.Class.Valid() || v.Message == "" || v.Retries < 0 {
			return errors.New("turn_failed: incomplete")
		}
	case Note:
		if v.Plugin == "" || v.Text == "" {
			return errors.New("note: plugin and text required")
		}
		if !v.Role.Valid() {
			return fmt.Errorf("note: invalid role %q", v.Role)
		}
	default:
		return fmt.Errorf("unknown payload %T", p)
	}
	return nil
}

// Entry is one immutable item in a session log.
type Entry struct {
	ID      ulid.ULID
	At      time.Time
	Kind    Kind
	Payload Payload
}

var (
	idMu      sync.Mutex
	idEntropy = ulid.Monotonic(rand.Reader, 0)
)

// NewID returns a ULID strictly greater than every earlier NewID in this process.
func NewID() ulid.ULID {
	idMu.Lock()
	defer idMu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now()), idEntropy)
}
```

`internal/session/entry_json.go`:

```go
package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// encodeNoEscape marshals v without HTML escaping and without the trailing
// newline json.Encoder adds.
func encodeNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// appendString appends s as a JSON string with minimal escaping.
func appendString(dst []byte, s string) ([]byte, error) {
	b, err := encodeNoEscape(s)
	if err != nil {
		return nil, err
	}
	return append(dst, b...), nil
}

// appendBlocks writes a block array by hand so tool_use input bytes are
// copied verbatim rather than compacted by encoding/json.
func appendBlocks(dst []byte, blocks []Block) ([]byte, error) {
	dst = append(dst, '[')
	for i, b := range blocks {
		if i > 0 {
			dst = append(dst, ',')
		}
		var err error
		switch b.Type {
		case BlockText:
			dst = append(dst, `{"type":"text","text":`...)
			if dst, err = appendString(dst, b.Text); err != nil {
				return nil, err
			}
		case BlockImage:
			dst = append(dst, `{"type":"image","media_type":`...)
			if dst, err = appendString(dst, b.MediaType); err != nil {
				return nil, err
			}
			dst = append(dst, `,"sha256":`...)
			if dst, err = appendString(dst, b.SHA256); err != nil {
				return nil, err
			}
		case BlockThinking:
			dst = append(dst, `{"type":"thinking","text":`...)
			if dst, err = appendString(dst, b.Text); err != nil {
				return nil, err
			}
			dst = append(dst, `,"signature":`...)
			if dst, err = appendString(dst, b.Signature); err != nil {
				return nil, err
			}
		case BlockToolUse:
			dst = append(dst, `{"type":"tool_use","id":`...)
			if dst, err = appendString(dst, b.ID); err != nil {
				return nil, err
			}
			dst = append(dst, `,"name":`...)
			if dst, err = appendString(dst, b.Name); err != nil {
				return nil, err
			}
			dst = append(dst, `,"input":`...)
			if len(b.Input) == 0 {
				dst = append(dst, '{', '}')
			} else {
				if !json.Valid(b.Input) {
					return nil, fmt.Errorf("tool_use %q: input is not valid JSON", b.ID)
				}
				dst = append(dst, b.Input...)
			}
		default:
			return nil, fmt.Errorf("block: invalid type %q", b.Type)
		}
		dst = append(dst, '}')
	}
	return append(dst, ']'), nil
}

// MarshalJSON emits only the variant's fields. Used when a Block travels
// inside a larger json.Marshal, such as a protocol notification; the log
// line path uses appendBlocks instead.
func (b Block) MarshalJSON() ([]byte, error) {
	type alias Block
	out := alias{Type: b.Type}
	switch b.Type {
	case BlockText:
		out.Text = b.Text
	case BlockImage:
		out.MediaType, out.SHA256 = b.MediaType, b.SHA256
	case BlockThinking:
		out.Text, out.Signature = b.Text, b.Signature
	case BlockToolUse:
		out.ID, out.Name, out.Input = b.ID, b.Name, b.Input
	default:
		return nil, fmt.Errorf("block: invalid type %q", b.Type)
	}
	return json.Marshal(out)
}

// UnmarshalJSON validates the variant tag; Input keeps the source bytes.
func (b *Block) UnmarshalJSON(data []byte) error {
	type alias Block
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	if !a.Type.Valid() {
		return fmt.Errorf("block: invalid type %q", a.Type)
	}
	*b = Block(a)
	return nil
}

// blocksOf returns the block slice of a payload that carries one, and the
// payload with that slice cleared so the scalar part can be marshaled alone.
func blocksOf(p Payload) (blocks []Block, rest Payload, has bool) {
	switch v := p.(type) {
	case UserMessage:
		v.Content = nil
		return p.(UserMessage).Content, v, true
	case AssistantMessage:
		v.Content = nil
		return p.(AssistantMessage).Content, v, true
	case ToolResult:
		v.Content = nil
		return p.(ToolResult).Content, v, true
	}
	return nil, p, false
}

// MarshalJSON writes the flattened envelope:
// {"id":…,"at":…,"kind":…,<payload fields>} with block arrays copied verbatim.
// For payloads with content the content array is written last.
func (e Entry) MarshalJSON() ([]byte, error) {
	if e.Payload == nil {
		return nil, errors.New("entry: nil payload")
	}
	if e.Kind != e.Payload.Kind() {
		return nil, fmt.Errorf("entry: kind %q does not match payload %q", e.Kind, e.Payload.Kind())
	}
	if err := Validate(e.Payload); err != nil {
		return nil, fmt.Errorf("entry %s: %w", e.Kind, err)
	}
	blocks, rest, has := blocksOf(e.Payload)
	scalars, err := encodeNoEscape(rest)
	if err != nil {
		return nil, err
	}
	// scalars is "{...}" or "{}"; splice its interior after the envelope.
	interior := bytes.TrimSuffix(bytes.TrimPrefix(scalars, []byte("{")), []byte("}"))

	out := make([]byte, 0, 64+len(scalars))
	out = append(out, `{"id":"`...)
	out = append(out, e.ID.String()...)
	out = append(out, `","at":"`...)
	out = append(out, e.At.Format(time.RFC3339Nano)...)
	out = append(out, `","kind":"`...)
	out = append(out, string(e.Kind)...)
	out = append(out, '"')
	if len(interior) > 0 {
		out = append(out, ',')
		out = append(out, interior...)
	}
	if has {
		out = append(out, `,"content":`...)
		if out, err = appendBlocks(out, blocks); err != nil {
			return nil, err
		}
	}
	return append(out, '}'), nil
}

type envelope struct {
	ID   string `json:"id"`
	At   string `json:"at"`
	Kind Kind   `json:"kind"`
}

// UnmarshalJSON reads the envelope, allocates the payload for its kind and
// decodes the same bytes into it. RawMessage fields keep the source bytes.
func (e *Entry) UnmarshalJSON(data []byte) error {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return err
	}
	id, err := ulid.ParseStrict(env.ID)
	if err != nil {
		return fmt.Errorf("entry: id: %w", err)
	}
	at, err := time.Parse(time.RFC3339Nano, env.At)
	if err != nil {
		return fmt.Errorf("entry: at: %w", err)
	}
	var p Payload
	switch env.Kind {
	case KindSessionOpened:
		p, err = decodePayload[SessionOpened](data)
	case KindForkPoint:
		p, err = decodePayload[ForkPoint](data)
	case KindUserMessage:
		p, err = decodePayload[UserMessage](data)
	case KindAssistantMessage:
		p, err = decodePayload[AssistantMessage](data)
	case KindPermissionDecision:
		p, err = decodePayload[PermissionDecision](data)
	case KindToolResult:
		p, err = decodePayload[ToolResult](data)
	case KindModelChange:
		p, err = decodePayload[ModelChange](data)
	case KindModeChange:
		p, err = decodePayload[ModeChange](data)
	case KindThinkingChange:
		p, err = decodePayload[ThinkingChange](data)
	case KindTitleChange:
		p, err = decodePayload[TitleChange](data)
	case KindCompaction:
		p, err = decodePayload[Compaction](data)
	case KindTurnInterrupted:
		p, err = decodePayload[TurnInterrupted](data)
	case KindTurnFailed:
		p, err = decodePayload[TurnFailed](data)
	case KindNote:
		p, err = decodePayload[Note](data)
	default:
		return fmt.Errorf("entry: unknown kind %q", env.Kind)
	}
	if err != nil {
		return fmt.Errorf("entry %s: %w", env.Kind, err)
	}
	if err := Validate(p); err != nil {
		return fmt.Errorf("entry %s: %w", env.Kind, err)
	}
	*e = Entry{ID: id, At: at, Kind: env.Kind, Payload: p}
	return nil
}

func decodePayload[T Payload](data []byte) (Payload, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return v, nil
}
```

- [ ] **Step 5: Run the tests to see them pass**

Run: `go test ./internal/session/ -run 'TestEnums|TestModelRef|TestUsage|TestBlock|TestNewID|TestEntry' -v`
Expected: every test PASS. If `TestEntryRoundTripAllKinds` fails on the `at` prefix, check the zone: `time.FixedZone("MDT", -6*3600)` formats as `-06:00`.

- [ ] **Step 6: Lint and commit**

```bash
make check
git add go.mod go.sum internal/session
git commit -m "session: entry kinds, payloads and the flattened JSON envelope"
bd close rudy-2
```

---

### Task 3: Append-only log and blob store

**Files:**
- Create: `internal/session/log.go`
- Create: `internal/session/blobs.go`
- Test: `internal/session/log_test.go`
- Test: `internal/session/blobs_test.go`

**Interfaces:**
- Consumes: `Entry.MarshalJSON`, `Entry.UnmarshalJSON` from Task 2.
- Produces: `OpenLog(dir) (*Log, error)`, `(*Log).Append(Entry) error`, `(*Log).Sync() error`, `(*Log).Close() error`, `ReadLog(dir) ([]Entry, error)`, `Truncated{Line int}`, `OpenBlobs(dir) (*Blobs, error)`, `(*Blobs).Put([]byte) (string, error)`, `(*Blobs).Get(string) ([]byte, error)`. The log file name is the constant `LogFile = "entries.jsonl"`.

- [ ] **Step 1: Create the beads issue and claim it**

```bash
bd create --title "Task 3: session log and blob store" --type task
bd update rudy-3 --claim
```

- [ ] **Step 2: Write the failing tests**

`internal/session/log_test.go`:

```go
package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testEntry(t *testing.T, p Payload) Entry {
	t.Helper()
	return Entry{ID: NewID(), At: time.Now().UTC(), Kind: p.Kind(), Payload: p}
}

func TestLogAppendThenRead(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		testEntry(t, ModeChange{Mode: ModeOff}),
		testEntry(t, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("hi")}}),
		testEntry(t, TitleChange{Title: "t"}),
	}
	for _, e := range want {
		if err := l.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("read %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Kind != want[i].Kind {
			t.Fatalf("entry %d: got %s %s, want %s %s", i, got[i].ID, got[i].Kind, want[i].ID, want[i].Kind)
		}
		if got[i].ID.Compare(want[i].ID) != 0 || (i > 0 && got[i].ID.Compare(got[i-1].ID) <= 0) {
			t.Fatalf("ids not increasing at %d", i)
		}
	}
}

func TestLogSyncMakesEntriesVisible(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if err := l.Append(testEntry(t, ModeChange{Mode: ModeOff})); err != nil {
		t.Fatal(err)
	}
	if err := l.Sync(); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("after Sync read %d entries, want 1", len(got))
	}
}

func TestReadLogTruncatedFinalLine(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append(testEntry(t, ModeChange{Mode: ModeOff})); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, LogFile), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"01K4M0A7Q8ZJ3N6R9T2V5X8B1D","at":"2026-09-07T2`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	got, err := ReadLog(dir)
	var tr Truncated
	if !errors.As(err, &tr) {
		t.Fatalf("want Truncated, got %v", err)
	}
	if tr.Line != 2 {
		t.Fatalf("Truncated.Line = %d, want 2", tr.Line)
	}
	if len(got) != 1 {
		t.Fatalf("read %d entries, want 1", len(got))
	}
}

func TestReadLogMalformedMiddleLine(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, LogFile), []byte("not json\n"+
		`{"id":"01K4M0A7Q8ZJ3N6R9T2V5X8B1D","at":"2026-09-07T20:30:00Z","kind":"mode_change","mode":"off"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadLog(dir)
	var tr Truncated
	if err == nil || errors.As(err, &tr) {
		t.Fatalf("want a hard error, got %v", err)
	}
}

func TestReadLogMissingFile(t *testing.T) {
	got, err := ReadLog(t.TempDir())
	if err != nil || len(got) != 0 {
		t.Fatalf("missing file: got %d entries, err %v; want 0, nil", len(got), err)
	}
}
```

`internal/session/blobs_test.go`:

```go
package session

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestBlobsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	b, err := OpenBlobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("\x89PNG\r\n\x1a\nfake")
	sha, err := b.Put(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(sha) != 64 {
		t.Fatalf("sha = %q", sha)
	}
	again, err := b.Put(data)
	if err != nil || again != sha {
		t.Fatalf("second Put = %q, %v; want %q", again, err, sha)
	}
	got, err := b.Get(sha)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs", sha)); err != nil {
		t.Fatalf("blob file missing: %v", err)
	}
	if _, err := b.Get("deadbeef"); err == nil {
		t.Fatal("Get of a missing blob must fail")
	}
}
```

- [ ] **Step 3: Run the tests to see them fail**

Run: `go test ./internal/session/ -run 'TestLog|TestReadLog|TestBlobs'`
Expected: FAIL to compile with `undefined: OpenLog`.

- [ ] **Step 4: Write the log and the blob store**

`internal/session/log.go`:

```go
package session

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// LogFile is the name of the entries file inside a session directory.
const LogFile = "entries.jsonl"

// Log is the append-only entries.jsonl of one session directory.
type Log struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
}

// OpenLog creates dir when missing and opens its entries file for append.
func OpenLog(dir string) (*Log, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("session: open log: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, LogFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("session: open log: %w", err)
	}
	return &Log{f: f, w: bufio.NewWriterSize(f, 64*1024)}, nil
}

// Append writes one entry as one line. The write is buffered; Sync makes it durable.
func (l *Log) Append(e Entry) error {
	line, err := e.MarshalJSON()
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.w.Write(line); err != nil {
		return fmt.Errorf("session: append: %w", err)
	}
	if err := l.w.WriteByte('\n'); err != nil {
		return fmt.Errorf("session: append: %w", err)
	}
	return nil
}

// Sync flushes the buffer and fsyncs the file.
func (l *Log) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.syncLocked()
}

func (l *Log) syncLocked() error {
	if err := l.w.Flush(); err != nil {
		return fmt.Errorf("session: sync: %w", err)
	}
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("session: sync: %w", err)
	}
	return nil
}

// Close syncs and closes the file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.syncLocked()
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}

// Truncated reports that the final line of a log was incomplete and was
// dropped. Line is 1-based. Callers that resume a session accept it; a
// process died mid-append.
type Truncated struct{ Line int }

func (t Truncated) Error() string {
	return fmt.Sprintf("session: entries.jsonl line %d is truncated", t.Line)
}

// ReadLog returns every complete entry in dir's log. A missing file yields no
// entries and no error. A truncated final line is dropped and reported as a
// Truncated error alongside the entries read before it. Any other malformed
// line is an error with no entries.
func ReadLog(dir string) ([]Entry, error) {
	f, err := os.Open(filepath.Join(dir, LogFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("session: read log: %w", err)
	}
	defer func() { _ = f.Close() }()

	r := bufio.NewReaderSize(f, 64*1024)
	var entries []Entry
	line := 0
	for {
		line++
		raw, rerr := r.ReadBytes('\n')
		if rerr != nil && rerr != io.EOF {
			return nil, fmt.Errorf("session: read log: %w", rerr)
		}
		complete := bytes.HasSuffix(raw, []byte("\n"))
		raw = bytes.TrimRight(raw, "\n")
		if len(raw) == 0 {
			if rerr == io.EOF {
				return entries, nil
			}
			continue
		}
		var e Entry
		if uerr := e.UnmarshalJSON(raw); uerr != nil || !complete {
			if rerr == io.EOF {
				return entries, Truncated{Line: line}
			}
			return nil, fmt.Errorf("session: read log line %d: %w", line, uerr)
		}
		entries = append(entries, e)
		if rerr == io.EOF {
			return entries, nil
		}
	}
}
```

`internal/session/blobs.go`:

```go
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Blobs is the content-addressed store under <session dir>/blobs.
type Blobs struct{ dir string }

// OpenBlobs creates the blobs directory when missing.
func OpenBlobs(dir string) (*Blobs, error) {
	d := filepath.Join(dir, "blobs")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return nil, fmt.Errorf("session: open blobs: %w", err)
	}
	return &Blobs{dir: d}, nil
}

// Put stores data under its sha256 hex and returns the hex. An existing
// blob is left alone; the write is a temp file renamed into place.
func (b *Blobs) Put(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:])
	dst := filepath.Join(b.dir, name)
	if _, err := os.Stat(dst); err == nil {
		return name, nil
	}
	tmp, err := os.CreateTemp(b.dir, "put-*")
	if err != nil {
		return "", fmt.Errorf("session: put blob: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("session: put blob: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("session: put blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("session: put blob: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("session: put blob: %w", err)
	}
	return name, nil
}

// Get reads a blob by its sha256 hex.
func (b *Blobs) Get(sha256hex string) ([]byte, error) {
	if len(sha256hex) != 64 {
		return nil, errors.New("session: get blob: not a sha256 hex")
	}
	data, err := os.ReadFile(filepath.Join(b.dir, sha256hex))
	if err != nil {
		return nil, fmt.Errorf("session: get blob: %w", err)
	}
	return data, nil
}
```

- [ ] **Step 5: Run the tests to see them pass**

Run: `go test ./internal/session/ -run 'TestLog|TestReadLog|TestBlobs' -v`
Expected: all PASS. `TestReadLogTruncatedFinalLine` proves the partial line is dropped and reported as line 2.

- [ ] **Step 6: Lint and commit**

```bash
make check
git add internal/session
git commit -m "session: append-only log, fsync and blob store"
bd close rudy-3
```

---

### Task 4: Session aggregate, invariants, fork by reference and recovery

**Files:**
- Create: `internal/session/store.go` (the minimal part: `Store`, `OpenStore`, `Root`, `Dir`; Task 5 adds `List` and `Lock`)
- Create: `internal/session/session.go`
- Create: `internal/session/recovery.go`
- Test: `internal/session/session_test.go`

**Interfaces:**
- Consumes: `Log`, `ReadLog`, `Truncated`, `Blobs` from Task 3; `Validate` from Task 2.
- Produces: `Store` with `OpenStore`, `Root`, `Dir`; `Session` with `Open`, `Load`, `ID`, `Dir`, `Blobs`, `Append`, `Fork`, `Entries`, `Workspace`, `Model`, `Mode`, `Thinking`, `Title`, `Agent`, `Usage`, `Allowances`, `RequestContext`, `PendingToolUses`, `Close`; `ErrInvariant`; `Recover(s *Session) (int, error)` as the exported recovery pass for tests and for Task 14's resume path.

Recovery rule, made precise. On `Load`, for every pending `tool_use` (a tool_use block in an assistant message with no `tool_result` yet):
- with an allow decision and no result: append `tool_result` outcome `lost`
- with a deny decision and no result: append `tool_result` outcome `lost`
- with no decision: append `permission_decision` deny, decided_by `no_asker`, scope `once`, reason `recovery: process ended before a decision`, then `tool_result` outcome `lost`

so the log's rule of exactly one decision then exactly one result per tool_use holds after every load.

- [ ] **Step 1: Create the beads issue and claim it**

```bash
bd create --title "Task 4: session aggregate, invariants, fork and recovery" --type task
bd update rudy-4 --claim
```

- [ ] **Step 2: Write the failing tests**

`internal/session/session_test.go`:

```go
package session

import (
	"encoding/json"
	"errors"
	"testing"
)

func opened() SessionOpened {
	return SessionOpened{
		SchemaVersion: 1, RudyVersion: "test",
		Workspace: Workspace{Root: "/w", ProjectID: "local/w"},
		Model:     ModelRef{Provider: "aperture", Model: "cline-pass/kimi-k3"},
		Thinking:  ThinkingHigh, Mode: ModeStrict, Agent: "default",
	}
}

func newSession(t *testing.T) (*Store, *Session) {
	t.Helper()
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(st, opened())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return st, s
}

func mustAppend(t *testing.T, s *Session, p Payload) Entry {
	t.Helper()
	e, err := s.Append(p)
	if err != nil {
		t.Fatalf("append %s: %v", p.Kind(), err)
	}
	return e
}

func assistantWithTool(id string) AssistantMessage {
	return AssistantMessage{
		Model: ModelRef{"aperture", "cline-pass/kimi-k3"}, Thinking: ThinkingHigh,
		Content:    []Block{ToolUseBlock(id, "bash", json.RawMessage(`{"command":"ls"}`))},
		StopReason: StopToolUse, StopReasonRaw: "tool_calls",
	}
}

func decision(id string, d Decision, scope Scope) PermissionDecision {
	return PermissionDecision{ToolUseID: id, Tool: "bash", Mode: ModeStrict,
		Matcher: Matcher{Tool: "bash", Prefix: "ls"}, Decision: d, DecidedBy: ByAsker, Scope: scope, Reason: "test"}
}

func TestOpenWritesSessionOpened(t *testing.T) {
	_, s := newSession(t)
	es := s.Entries()
	if len(es) != 1 || es[0].Kind != KindSessionOpened {
		t.Fatalf("entries after Open = %+v", es)
	}
	if s.Model().Model != "cline-pass/kimi-k3" || s.Mode() != ModeStrict || s.Thinking() != ThinkingHigh || s.Agent() != "default" || s.Workspace().Root != "/w" {
		t.Fatal("derived getters do not reflect session_opened")
	}
}

func TestAppendInvariants(t *testing.T) {
	_, s := newSession(t)
	cases := []struct {
		name string
		p    Payload
	}{
		{"second session_opened", opened()},
		{"fork_point after first", ForkPoint{ParentSessionID: NewID(), ParentEntryID: NewID()}},
		{"decision for unknown tool_use", decision("nope", Allow, ScopeOnce)},
		{"result for unknown tool_use", ToolResult{ToolUseID: "nope", Outcome: OutcomeOK}},
		{"invalid enum", ModeChange{Mode: Mode("loose")}},
		{"invalid block", UserMessage{Source: SourceTyped, Content: []Block{{Type: BlockType("audio")}}}},
		{"compaction with unknown ids", Compaction{Summary: "s", FirstEntryID: NewID(), LastEntryID: NewID(), Model: ModelRef{"aperture", "m"}}},
	}
	for _, c := range cases {
		if _, err := s.Append(c.p); !errors.Is(err, ErrInvariant) {
			t.Errorf("%s: err = %v, want ErrInvariant", c.name, err)
		}
	}
}

func TestDecisionThenResultExactlyOnce(t *testing.T) {
	_, s := newSession(t)
	mustAppend(t, s, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("ls")}})
	mustAppend(t, s, assistantWithTool("t1"))
	if got := s.PendingToolUses(); len(got) != 1 || got[0].ID != "t1" {
		t.Fatalf("pending = %+v", got)
	}
	mustAppend(t, s, decision("t1", Allow, ScopeOnce))
	if _, err := s.Append(decision("t1", Allow, ScopeOnce)); !errors.Is(err, ErrInvariant) {
		t.Fatalf("second decision: %v", err)
	}
	mustAppend(t, s, ToolResult{ToolUseID: "t1", Outcome: OutcomeOK, Content: []Block{TextBlock("ok")}, DurationMS: 1})
	if _, err := s.Append(ToolResult{ToolUseID: "t1", Outcome: OutcomeOK}); !errors.Is(err, ErrInvariant) {
		t.Fatalf("second result: %v", err)
	}
	if got := s.PendingToolUses(); len(got) != 0 {
		t.Fatalf("pending after result = %+v", got)
	}
}

func TestAllowDecisionIsOnDiskBeforeAppendReturns(t *testing.T) {
	_, s := newSession(t)
	mustAppend(t, s, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("ls")}})
	mustAppend(t, s, assistantWithTool("t1"))
	mustAppend(t, s, decision("t1", Allow, ScopeSession))
	onDisk, err := ReadLog(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	last := onDisk[len(onDisk)-1]
	if last.Kind != KindPermissionDecision {
		t.Fatalf("last entry on disk = %s, want permission_decision", last.Kind)
	}
	if got := s.Allowances(); len(got) != 1 || got[0] != (Matcher{Tool: "bash", Prefix: "ls"}) {
		t.Fatalf("allowances = %+v", got)
	}
}

func TestDerivedGettersFollowChanges(t *testing.T) {
	_, s := newSession(t)
	mustAppend(t, s, ModelChange{Model: ModelRef{"aperture", "gpt-5.6-sol"}})
	mustAppend(t, s, ModeChange{Mode: ModeOff})
	mustAppend(t, s, ThinkingChange{Thinking: ThinkingLow})
	mustAppend(t, s, TitleChange{Title: "renamed"})
	mustAppend(t, s, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("hi")}})
	mustAppend(t, s, AssistantMessage{Model: s.Model(), Thinking: s.Thinking(), Content: []Block{TextBlock("yo")}, Usage: Usage{Input: 10, Output: 5}, StopReason: StopEndTurn, StopReasonRaw: "stop"})
	mustAppend(t, s, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("more")}})
	mustAppend(t, s, AssistantMessage{Model: s.Model(), Thinking: s.Thinking(), Content: []Block{TextBlock("ok")}, Usage: Usage{Input: 20, Output: 7}, StopReason: StopEndTurn, StopReasonRaw: "stop"})
	if s.Model().Model != "gpt-5.6-sol" || s.Mode() != ModeOff || s.Thinking() != ThinkingLow || s.Title() != "renamed" {
		t.Fatalf("derived: model %s mode %s thinking %s title %q", s.Model(), s.Mode(), s.Thinking(), s.Title())
	}
	if got := s.Usage(); got != (Usage{Input: 30, Output: 12}) {
		t.Fatalf("usage = %+v", got)
	}
	es := s.Entries()
	mustAppend(t, s, Compaction{Summary: "summary", FirstEntryID: es[1].ID, LastEntryID: es[len(es)-1].ID, Model: s.Model(), Usage: Usage{Input: 100, Output: 20}})
	mustAppend(t, s, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("after")}})
	rc := s.RequestContext()
	if len(rc) != 2 || rc[0].Kind != KindCompaction || rc[1].Kind != KindUserMessage {
		t.Fatalf("request context = %+v", rc)
	}
}

func TestForkInheritsByReference(t *testing.T) {
	st, parent := newSession(t)
	mustAppend(t, parent, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("one")}})
	at := mustAppend(t, parent, AssistantMessage{Model: parent.Model(), Thinking: parent.Thinking(), Content: []Block{TextBlock("two")}, StopReason: StopEndTurn, StopReasonRaw: "stop"})
	mustAppend(t, parent, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("three, not inherited")}})

	child, err := parent.Fork(st, at.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Close() }()
	es := child.Entries()
	if len(es) != 4 || es[0].Kind != KindSessionOpened || es[2].ID != at.ID || es[3].Kind != KindForkPoint {
		t.Fatalf("child entries = %+v", es)
	}
	if child.Model() != parent.Model() || child.Workspace() != parent.Workspace() {
		t.Fatal("child must derive workspace and model through the parent")
	}
	onDisk, err := ReadLog(child.Dir())
	if err != nil || len(onDisk) != 1 || onDisk[0].Kind != KindForkPoint {
		t.Fatalf("child log on disk = %+v, %v; want only fork_point", onDisk, err)
	}
	if _, err := parent.Fork(st, NewID()); !errors.Is(err, ErrInvariant) {
		t.Fatalf("fork at unknown entry: %v", err)
	}

	reloaded, err := Load(st, child.ID())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reloaded.Close() }()
	if got := reloaded.Entries(); len(got) != 4 || got[2].ID != at.ID {
		t.Fatalf("reloaded child entries = %+v", got)
	}
}

func TestLoadRecoversLostResults(t *testing.T) {
	st, s := newSession(t)
	mustAppend(t, s, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("ls")}})
	mustAppend(t, s, AssistantMessage{
		Model: s.Model(), Thinking: s.Thinking(),
		Content: []Block{
			ToolUseBlock("allowed", "bash", json.RawMessage(`{"command":"ls"}`)),
			ToolUseBlock("undecided", "bash", json.RawMessage(`{"command":"pwd"}`)),
		},
		StopReason: StopToolUse, StopReasonRaw: "tool_calls",
	})
	mustAppend(t, s, decision("allowed", Allow, ScopeOnce))
	id := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	re, err := Load(st, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = re.Close() }()
	if got := re.PendingToolUses(); len(got) != 0 {
		t.Fatalf("pending after recovery = %+v", got)
	}
	es := re.Entries()
	tail := es[len(es)-3:]
	if tail[0].Kind != KindToolResult || tail[0].Payload.(ToolResult).Outcome != OutcomeLost || tail[0].Payload.(ToolResult).ToolUseID != "allowed" {
		t.Fatalf("expected lost result for allowed, got %+v", tail[0])
	}
	if tail[1].Kind != KindPermissionDecision || tail[1].Payload.(PermissionDecision).DecidedBy != ByNoAsker {
		t.Fatalf("expected recovery deny for undecided, got %+v", tail[1])
	}
	if tail[2].Kind != KindToolResult || tail[2].Payload.(ToolResult).Outcome != OutcomeLost {
		t.Fatalf("expected lost result for undecided, got %+v", tail[2])
	}
}

func TestLoadRefusesNonMonotonicIDs(t *testing.T) {
	st, s := newSession(t)
	mustAppend(t, s, ModeChange{Mode: ModeOff})
	id := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	dir := st.Dir(id)
	entries, err := ReadLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite the file with the two entries swapped.
	swapped := []Entry{entries[1], entries[0]}
	if err := rewriteLog(dir, swapped); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(st, id); err == nil {
		t.Fatal("Load must refuse non-monotonic ids")
	}
}

// rewriteLog is a test helper: it replaces a log with the given entries in order.
func rewriteLog(dir string, entries []Entry) error {
	l, err := OpenLog(dir)
	if err != nil {
		return err
	}
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	for _, e := range entries {
		if err := l.Append(e); err != nil {
			return err
		}
	}
	return l.Close()
}
```

- [ ] **Step 3: Run the tests to see them fail**

Run: `go test ./internal/session/ -run 'TestOpen|TestAppend|TestDecision|TestAllow|TestDerived|TestFork|TestLoad'`
Expected: FAIL to compile with `undefined: OpenStore`.

- [ ] **Step 4: Write the minimal store, the aggregate and recovery**

`internal/session/store.go` (Task 5 extends this file):

```go
package session

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/oklog/ulid/v2"
)

// Store is the directory that holds one subdirectory per session.
type Store struct{ root string }

// OpenStore creates root when missing.
func OpenStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("session: open store: %w", err)
	}
	return &Store{root: root}, nil
}

// Root is the store directory.
func (st *Store) Root() string { return st.root }

// Dir is the directory of one session.
func (st *Store) Dir(id ulid.ULID) string { return filepath.Join(st.root, id.String()) }
```

`internal/session/session.go`:

```go
package session

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/oklog/ulid/v2"
)

// ErrInvariant wraps every refusal by Append and Fork.
var ErrInvariant = errors.New("session: invariant")

func invariant(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvariant, fmt.Sprintf(format, args...))
}

// Session is the aggregate over one log. Everything it reports is derived by
// scanning entries; nothing is cached on disk.
type Session struct {
	id        ulid.ULID
	dir       string
	log       *Log
	blobs     *Blobs
	inherited []Entry // from the fork parent chain, read-only
	own       []Entry // this log's entries
	unlock    func()
}

// Open creates a new root session and appends session_opened.
func Open(st *Store, opened SessionOpened) (*Session, error) {
	s, err := create(st, NewID())
	if err != nil {
		return nil, err
	}
	if _, err := s.Append(opened); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func create(st *Store, id ulid.ULID) (*Session, error) {
	dir := st.Dir(id)
	unlock, err := st.lock(id)
	if err != nil {
		return nil, err
	}
	log, err := OpenLog(dir)
	if err != nil {
		unlock()
		return nil, err
	}
	blobs, err := OpenBlobs(dir)
	if err != nil {
		_ = log.Close()
		unlock()
		return nil, err
	}
	return &Session{id: id, dir: dir, log: log, blobs: blobs, unlock: unlock}, nil
}

// Load replays a session, resolves its fork parent chain and runs recovery.
func Load(st *Store, id ulid.ULID) (*Session, error) {
	own, err := ReadLog(st.Dir(id))
	var tr Truncated
	if err != nil && !errors.As(err, &tr) {
		return nil, err
	}
	if len(own) == 0 {
		return nil, fmt.Errorf("session %s: %w", id, errors.New("session: empty log"))
	}
	for i := range own[1:] {
		if own[i+1].ID.Compare(own[i].ID) <= 0 {
			return nil, invariant("ids not increasing at line %d", i+2)
		}
	}
	var inherited []Entry
	switch first := own[0].Payload.(type) {
	case SessionOpened:
	case ForkPoint:
		inherited, err = inheritedEntries(st, first, 0)
		if err != nil {
			return nil, err
		}
	default:
		return nil, invariant("first entry is %s", own[0].Kind)
	}
	s, err := create(st, id)
	if err != nil {
		return nil, err
	}
	s.inherited, s.own = inherited, own
	if _, err := Recover(s); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// inheritedEntries reads the parent chain named by a fork_point, up to and
// including the fork entry, recursing through grandparents.
func inheritedEntries(st *Store, fp ForkPoint, depth int) ([]Entry, error) {
	if depth > 64 {
		return nil, invariant("fork chain deeper than 64")
	}
	parent, err := ReadLog(st.Dir(fp.ParentSessionID))
	var tr Truncated
	if err != nil && !errors.As(err, &tr) {
		return nil, err
	}
	if len(parent) == 0 {
		return nil, invariant("fork parent %s has no log", fp.ParentSessionID)
	}
	var chain []Entry
	if gp, ok := parent[0].Payload.(ForkPoint); ok {
		chain, err = inheritedEntries(st, gp, depth+1)
		if err != nil {
			return nil, err
		}
	}
	all := append(chain, parent...)
	for i, e := range all {
		if e.ID == fp.ParentEntryID {
			return all[:i+1], nil
		}
	}
	return nil, invariant("fork entry %s not in parent %s", fp.ParentEntryID, fp.ParentSessionID)
}

func (s *Session) ID() ulid.ULID { return s.id }
func (s *Session) Dir() string   { return s.dir }
func (s *Session) Blobs() *Blobs { return s.blobs }

// Entries is the inherited chain followed by this log's entries.
func (s *Session) Entries() []Entry {
	out := make([]Entry, 0, len(s.inherited)+len(s.own))
	out = append(out, s.inherited...)
	return append(out, s.own...)
}

func (s *Session) has(id ulid.ULID) bool {
	for _, e := range s.Entries() {
		if e.ID == id {
			return true
		}
	}
	return false
}

// Append validates p against the log's rules, writes it and returns the entry.
// An allow permission_decision is fsynced before Append returns.
func (s *Session) Append(p Payload) (Entry, error) {
	if err := Validate(p); err != nil {
		return Entry{}, invariant("%v", err)
	}
	switch v := p.(type) {
	case SessionOpened:
		if len(s.own) > 0 || len(s.inherited) > 0 {
			return Entry{}, invariant("session_opened after the first entry")
		}
	case ForkPoint:
		if len(s.own) > 0 {
			return Entry{}, invariant("fork_point after the first entry")
		}
	case PermissionDecision:
		if !s.isPending(v.ToolUseID) {
			return Entry{}, invariant("permission_decision for tool_use %q that is not pending", v.ToolUseID)
		}
		if _, ok := s.decisionFor(v.ToolUseID); ok {
			return Entry{}, invariant("second permission_decision for tool_use %q", v.ToolUseID)
		}
	case ToolResult:
		if !s.isPending(v.ToolUseID) {
			return Entry{}, invariant("tool_result for tool_use %q that is not pending", v.ToolUseID)
		}
		if _, ok := s.decisionFor(v.ToolUseID); !ok {
			return Entry{}, invariant("tool_result for tool_use %q before its permission_decision", v.ToolUseID)
		}
	case Compaction:
		if !s.has(v.FirstEntryID) || !s.has(v.LastEntryID) || v.FirstEntryID.Compare(v.LastEntryID) > 0 {
			return Entry{}, invariant("compaction ids must name ordered entries of this session")
		}
	default:
		if len(s.own) == 0 && len(s.inherited) == 0 {
			return Entry{}, invariant("first entry must be session_opened or fork_point, not %s", p.Kind())
		}
	}
	e := Entry{ID: NewID(), At: time.Now(), Kind: p.Kind(), Payload: p}
	if err := s.log.Append(e); err != nil {
		return Entry{}, err
	}
	if pd, ok := p.(PermissionDecision); ok && pd.Decision == Allow {
		if err := s.log.Sync(); err != nil {
			return Entry{}, err
		}
	}
	s.own = append(s.own, e)
	return e, nil
}

// Fork syncs this log, then creates a new session whose first entry is a
// fork_point at `at`. Entries up to and including `at` are inherited by
// reference, so the parent's bytes must be on disk before the child exists.
func (s *Session) Fork(st *Store, at ulid.ULID) (*Session, error) {
	if err := s.log.Sync(); err != nil {
		return nil, err
	}
	all := s.Entries()
	cut := -1
	for i, e := range all {
		if e.ID == at {
			cut = i
		}
	}
	if cut < 0 {
		return nil, invariant("fork at %s: not an entry of this session", at)
	}
	child, err := create(st, NewID())
	if err != nil {
		return nil, err
	}
	child.inherited = append([]Entry(nil), all[:cut+1]...)
	if _, err := child.Append(ForkPoint{ParentSessionID: s.id, ParentEntryID: at}); err != nil {
		_ = child.Close()
		return nil, err
	}
	return child, nil
}

func (s *Session) opened() SessionOpened {
	for _, e := range s.Entries() {
		if o, ok := e.Payload.(SessionOpened); ok {
			return o
		}
	}
	return SessionOpened{}
}

func (s *Session) Workspace() Workspace { return s.opened().Workspace }
func (s *Session) Agent() string        { return s.opened().Agent }

func (s *Session) Model() ModelRef {
	m := s.opened().Model
	for _, e := range s.Entries() {
		if c, ok := e.Payload.(ModelChange); ok {
			m = c.Model
		}
	}
	return m
}

func (s *Session) Mode() Mode {
	m := s.opened().Mode
	for _, e := range s.Entries() {
		if c, ok := e.Payload.(ModeChange); ok {
			m = c.Mode
		}
	}
	return m
}

func (s *Session) Thinking() ThinkingLevel {
	l := s.opened().Thinking
	for _, e := range s.Entries() {
		if c, ok := e.Payload.(ThinkingChange); ok {
			l = c.Thinking
		}
	}
	return l
}

// Title is the last title_change, or "" when none.
func (s *Session) Title() string {
	t := ""
	for _, e := range s.Entries() {
		if c, ok := e.Payload.(TitleChange); ok {
			t = c.Title
		}
	}
	return t
}

// Usage sums every assistant_message usage.
// Usage sums every assistant_message and compaction usage, since both are provider calls
// the session paid for.
func (s *Session) Usage() Usage {
	var u Usage
	for _, e := range s.Entries() {
		switch p := e.Payload.(type) {
		case AssistantMessage:
			u = u.Add(p.Usage)
		case Compaction:
			u = u.Add(p.Usage)
		}
	}
	return u
}

// Allowances are the matchers of allow decisions with session scope.
func (s *Session) Allowances() []Matcher {
	var out []Matcher
	for _, e := range s.Entries() {
		if d, ok := e.Payload.(PermissionDecision); ok && d.Decision == Allow && d.Scope == ScopeSession {
			out = append(out, d.Matcher)
		}
	}
	return out
}

// RequestContext is the last compaction entry followed by everything after
// it, or every entry when there is no compaction.
func (s *Session) RequestContext() []Entry {
	all := s.Entries()
	for i, e := range slices.Backward(all) {
		if e.Kind == KindCompaction {
			return all[i:]
		}
	}
	return all
}

// PendingToolUses returns tool_use blocks that have no tool_result yet, in order.
func (s *Session) PendingToolUses() []Block {
	done := map[string]bool{}
	var uses []Block
	for _, e := range s.Entries() {
		switch v := e.Payload.(type) {
		case AssistantMessage:
			for _, b := range v.Content {
				if b.Type == BlockToolUse {
					uses = append(uses, b)
				}
			}
		case ToolResult:
			done[v.ToolUseID] = true
		}
	}
	var out []Block
	for _, b := range uses {
		if !done[b.ID] {
			out = append(out, b)
		}
	}
	return out
}

func (s *Session) isPending(toolUseID string) bool {
	for _, b := range s.PendingToolUses() {
		if b.ID == toolUseID {
			return true
		}
	}
	return false
}

func (s *Session) decisionFor(toolUseID string) (PermissionDecision, bool) {
	for _, e := range s.Entries() {
		if d, ok := e.Payload.(PermissionDecision); ok && d.ToolUseID == toolUseID {
			return d, true
		}
	}
	return PermissionDecision{}, false
}

// Close syncs and closes the log and releases the lock.
func (s *Session) Close() error {
	var err error
	if s.log != nil {
		err = s.log.Close()
		s.log = nil
	}
	if s.unlock != nil {
		s.unlock()
		s.unlock = nil
	}
	return err
}
```

`internal/session/recovery.go`:

```go
package session

// Recover appends the entries that make a log consistent after a crash and
// returns how many it appended. See the recovery rule in the plan.
func Recover(s *Session) (int, error) {
	n := 0
	for _, b := range s.PendingToolUses() {
		if _, decided := s.decisionFor(b.ID); !decided {
			if _, err := s.Append(PermissionDecision{
				ToolUseID: b.ID, Tool: b.Name, Mode: s.Mode(),
				Matcher:   Matcher{Tool: b.Name},
				Decision:  Deny, DecidedBy: ByNoAsker, Scope: ScopeOnce,
				Reason:    "recovery: process ended before a decision",
			}); err != nil {
				return n, err
			}
			n++
		}
		if _, err := s.Append(ToolResult{ToolUseID: b.ID, Outcome: OutcomeLost}); err != nil {
			return n, err
		}
		n++
	}
	if n > 0 {
		if err := s.log.Sync(); err != nil {
			return n, err
		}
	}
	return n, nil
}
```

Until Task 5 lands, `create` needs a lock function. Add this placeholder to `store.go` now; Task 5 replaces it with `flock`:

```go
// lock is replaced by a flock in Task 5. Until then no lock is taken.
func (st *Store) lock(id ulid.ULID) (func(), error) { return func() {}, nil }
```

- [ ] **Step 5: Run the tests to see them pass**

Run: `go test ./internal/session/ -v`
Expected: every test PASS, including `TestLoadRecoversLostResults` whose tail is lost result, recovery deny, lost result, in that order.

- [ ] **Step 6: Lint and commit**

```bash
make check
git add internal/session
git commit -m "session: aggregate, invariants, fork by reference and recovery"
bd close rudy-4
```

---

### Task 5: Store listing and the per-session lock

**Files:**
- Modify: `internal/session/store.go`
- Test: `internal/session/store_test.go`

**Interfaces:**
- Consumes: `Store`, `Session` from Task 4.
- Produces: `Summary`, `(*Store).List() ([]Summary, error)`, `(*Store).Lock(id) (func(), error)`, `ErrLocked`. `Session.Open` and `Load` hold the lock until `Close`.

- [ ] **Step 1: Create the beads issue and claim it**

```bash
bd create --title "Task 5: store listing and session lock" --type task
bd update rudy-5 --claim
```

- [ ] **Step 2: Write the failing tests**

`internal/session/store_test.go`:

```go
package session

import (
	"errors"
	"testing"
)

func TestStoreListNewestFirstWithForks(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := Open(st, opened())
	if err != nil {
		t.Fatal(err)
	}
	at := mustAppend(t, a, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("x")}})
	child, err := a.Fork(st, at.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("List = %d sessions, want 2", len(got))
	}
	if got[0].ID != child.ID() || !got[0].Forked {
		t.Fatalf("newest first should be the fork: %+v", got[0])
	}
	if got[0].Workspace.Root != "/w" || got[0].Model.Model != "cline-pass/kimi-k3" {
		t.Fatalf("fork summary must resolve workspace and model through its parent: %+v", got[0])
	}
	if got[1].ID != a.ID() || got[1].Forked {
		t.Fatalf("second should be the root: %+v", got[1])
	}
}

func TestStoreLock(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := NewID()
	unlock, err := st.Lock(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Lock(id); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Lock = %v, want ErrLocked", err)
	}
	unlock()
	unlock2, err := st.Lock(id)
	if err != nil {
		t.Fatalf("Lock after unlock: %v", err)
	}
	unlock2()
}

func TestSessionHoldsLockUntilClose(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(st, opened())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(st, s.ID()); !errors.Is(err, ErrLocked) {
		t.Fatalf("Load while open = %v, want ErrLocked", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	re, err := Load(st, s.ID())
	if err != nil {
		t.Fatalf("Load after Close: %v", err)
	}
	_ = re.Close()
}
```

- [ ] **Step 3: Run the tests to see them fail**

Run: `go test ./internal/session/ -run 'TestStore|TestSessionHoldsLock'`
Expected: FAIL to compile with `undefined: ErrLocked` and `st.List undefined`.

- [ ] **Step 4: Add List and Lock**

Replace `internal/session/store.go` with:

```go
package session

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"
)

// Store is the directory that holds one subdirectory per session.
type Store struct{ root string }

// OpenStore creates root when missing.
func OpenStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("session: open store: %w", err)
	}
	return &Store{root: root}, nil
}

// Root is the store directory.
func (st *Store) Root() string { return st.root }

// Dir is the directory of one session.
func (st *Store) Dir(id ulid.ULID) string { return filepath.Join(st.root, id.String()) }

// Summary is what List reports without replaying a log.
type Summary struct {
	ID        ulid.ULID
	OpenedAt  time.Time
	Workspace Workspace
	Model     ModelRef
	Forked    bool
}

// List reads the first line of every session log, newest first. A fork's
// workspace and model come from the first session_opened up its parent chain.
func (st *Store) List() ([]Summary, error) {
	dirs, err := os.ReadDir(st.root)
	if err != nil {
		return nil, fmt.Errorf("session: list: %w", err)
	}
	var out []Summary
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		id, err := ulid.ParseStrict(d.Name())
		if err != nil {
			continue
		}
		first, err := st.firstEntry(id)
		if err != nil {
			continue
		}
		sum := Summary{ID: id, OpenedAt: first.At}
		switch p := first.Payload.(type) {
		case SessionOpened:
			sum.Workspace, sum.Model = p.Workspace, p.Model
		case ForkPoint:
			sum.Forked = true
			root, err := st.rootOpened(p, 0)
			if err != nil {
				continue
			}
			sum.Workspace, sum.Model = root.Workspace, root.Model
		default:
			continue
		}
		out = append(out, sum)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.Compare(out[j].ID) > 0 })
	return out, nil
}

func (st *Store) firstEntry(id ulid.ULID) (Entry, error) {
	f, err := os.Open(filepath.Join(st.Dir(id), LogFile))
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = f.Close() }()
	line, err := bufio.NewReaderSize(f, 64*1024).ReadBytes('\n')
	if err != nil {
		return Entry{}, err
	}
	var e Entry
	if err := e.UnmarshalJSON(line[:len(line)-1]); err != nil {
		return Entry{}, err
	}
	return e, nil
}

func (st *Store) rootOpened(fp ForkPoint, depth int) (SessionOpened, error) {
	if depth > 64 {
		return SessionOpened{}, errors.New("session: fork chain deeper than 64")
	}
	first, err := st.firstEntry(fp.ParentSessionID)
	if err != nil {
		return SessionOpened{}, err
	}
	switch p := first.Payload.(type) {
	case SessionOpened:
		return p, nil
	case ForkPoint:
		return st.rootOpened(p, depth+1)
	}
	return SessionOpened{}, errors.New("session: parent log does not start with session_opened or fork_point")
}

// ErrLocked reports that another process holds the session.
var ErrLocked = errors.New("session: locked by another process")

// Lock takes an exclusive flock on <dir>/lock without blocking. The returned
// function releases it.
func (st *Store) Lock(id ulid.ULID) (unlock func(), err error) {
	dir := st.Dir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("session: lock: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("session: lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("session: lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func (st *Store) lock(id ulid.ULID) (func(), error) { return st.Lock(id) }
```

`Load` in Task 4 reads the log before taking the lock; that is fine because the read is of a complete file and the lock guards the append side, but the `ErrLocked` must surface before recovery writes anything. `create` is called after the read and before `Recover`, so it does.

- [ ] **Step 5: Run the tests to see them pass**

Run: `go test -race ./internal/session/ -v`
Expected: every test PASS. `TestSessionHoldsLockUntilClose` proves two handles on one session are refused in the same process, because `flock` locks belong to the open file description, not the process.

- [ ] **Step 6: Lint and commit**

```bash
make check
git add internal/session
git commit -m "session: store, listing and per-session lock"
bd close rudy-5
```

---

### Task 6: Workspace detection and the memory project id

**Files:**
- Create: `internal/workspace/workspace.go`
- Test: `internal/workspace/workspace_test.go`

**Interfaces:**
- Consumes: `session.Workspace` from Task 2.
- Produces: `workspace.Detect(cwd string) (session.Workspace, error)`, `workspace.ProjectIDFromOrigin(url string) (string, error)`, `workspace.LocalProjectID(dir string) string`, `workspace.AssertProjectID(id string) (string, error)`, `workspace.ErrRefused`.

The project id must match what the memory CLI derives for the same directory, byte for byte, because the memory plugin keys the bundle's `projects/<id>/` directory on it. The algorithm is ported from `~/projects/memory/src/project-id.mjs` and the test table from `~/projects/memory/test/project-id.test.mjs`.

- [ ] **Step 1: Write the failing tests**

```go
package workspace_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/guygrigsby/rudy/internal/workspace"
)

func TestProjectIDFromOriginNormalizes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"git@github.com:guygrigsby/x.git", "github.com/guygrigsby/x"},
		{"https://github.com/guygrigsby/x", "github.com/guygrigsby/x"},
		{"ssh://git@GitHub.com/guygrigsby/x.git", "github.com/guygrigsby/x"},
		{"https://user:tok@github.com/guygrigsby/x.git/", "github.com/guygrigsby/x"},
		{"ssh://git@github.com:2222/guygrigsby/x.git", "github.com/guygrigsby/x"},
		{"https://gitlab.example.com:8443/team/repo.git", "gitlab.example.com/team/repo"},
	}
	for _, c := range cases {
		got, err := workspace.ProjectIDFromOrigin(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.in, got, c.want)
		}
	}
}

func TestProjectIDFromOriginRefuses(t *testing.T) {
	bad := []string{
		"/Users/guy/repos/upstream.git",
		"./relative/path",
		"~/home/path",
		"C:/repos/x",
		"https://github.com",
		"https://github.com/",
		"not a url",
	}
	for _, in := range bad {
		if _, err := workspace.ProjectIDFromOrigin(in); err == nil {
			t.Errorf("%q: want refusal", in)
		}
	}
}

func TestAssertProjectIDRefusesTraversal(t *testing.T) {
	for _, bad := range []string{"", "../x", "/abs", "a b/c", "github.com/../x"} {
		if _, err := workspace.AssertProjectID(bad); err == nil {
			t.Errorf("%q: want refusal", bad)
		}
	}
	if got, err := workspace.AssertProjectID("local/x"); err != nil || got != "local/x" {
		t.Fatalf("local/x: got %q, %v", got, err)
	}
}

func TestLocalProjectID(t *testing.T) {
	if got := workspace.LocalProjectID("/tmp/some/repo/"); got != "local/repo" {
		t.Fatalf("got %q", got)
	}
}

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func gitRepo(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if origin != "" {
		run("remote", "add", "origin", origin)
	}
	return dir
}

func TestDetectRepoWithOrigin(t *testing.T) {
	needGit(t)
	dir := gitRepo(t, "git@github.com:aeryx-ai/memory.git")
	ws, err := workspace.Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ws.ProjectID != "github.com/aeryx-ai/memory" {
		t.Errorf("project id %q", ws.ProjectID)
	}
	wantRoot, _ := filepath.EvalSymlinks(dir)
	gotRoot, _ := filepath.EvalSymlinks(ws.Root)
	gotGit, _ := filepath.EvalSymlinks(ws.GitRoot)
	if gotRoot != wantRoot || gotGit != wantRoot {
		t.Errorf("root %q git root %q want %q", ws.Root, ws.GitRoot, wantRoot)
	}
}

func TestDetectRepoWithoutOriginAndPlainDir(t *testing.T) {
	needGit(t)
	bare := gitRepo(t, "")
	ws, err := workspace.Detect(bare)
	if err != nil {
		t.Fatal(err)
	}
	if ws.ProjectID != "local/"+filepath.Base(bare) {
		t.Errorf("bare repo: %q", ws.ProjectID)
	}
	plain := t.TempDir()
	ws, err = workspace.Detect(plain)
	if err != nil {
		t.Fatal(err)
	}
	if ws.GitRoot != "" {
		t.Errorf("plain dir has git root %q", ws.GitRoot)
	}
	if ws.ProjectID != "local/"+filepath.Base(plain) {
		t.Errorf("plain dir: %q", ws.ProjectID)
	}
}

func TestDetectSubdirectoryResolvesToRepo(t *testing.T) {
	needGit(t)
	repo := gitRepo(t, "https://github.com/a/b")
	sub := filepath.Join(repo, "deep", "er")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Detect(sub)
	if err != nil {
		t.Fatal(err)
	}
	if ws.ProjectID != "github.com/a/b" {
		t.Errorf("project id %q", ws.ProjectID)
	}
	if filepath.Base(ws.Root) != "er" {
		t.Errorf("root should stay the cwd, got %q", ws.Root)
	}
}

func TestDetectFilesystemOriginFallsBackToLocal(t *testing.T) {
	needGit(t)
	bareOrigin := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "--bare")
	cmd.Dir = bareOrigin
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	repo := gitRepo(t, bareOrigin)
	ws, err := workspace.Detect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if ws.ProjectID != "local/"+filepath.Base(repo) {
		t.Errorf("got %q", ws.ProjectID)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/workspace/ -run . -v 2>&1 | head -5`
Expected: FAIL to build with `undefined: workspace.ProjectIDFromOrigin` (the package does not exist yet).

- [ ] **Step 3: Write the implementation**

```go
// Package workspace identifies the directory a session acts on.
package workspace

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/guygrigsby/rudy/internal/session"
)

// ErrRefused marks an origin or id the memory project id rules reject.
var ErrRefused = errors.New("workspace: refused")

var (
	schemeRe = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://`)
	driveRe  = regexp.MustCompile(`(?i)^[a-z]:`)
	userRe   = regexp.MustCompile(`^[^@/]+@`)
	scpRe    = regexp.MustCompile(`^([^@\s]+@)?([^:/\s]+):(.+)$`)
	portRe   = regexp.MustCompile(`:\d+$`)
	spaceRe  = regexp.MustCompile(`\s`)
)

// Detect resolves cwd to a Workspace. Root is the absolute cwd, GitRoot the repository
// top level or empty, ProjectID the id the memory CLI derives for the same directory.
func Detect(cwd string) (session.Workspace, error) {
	root, err := filepath.Abs(cwd)
	if err != nil {
		return session.Workspace{}, fmt.Errorf("workspace: %w", err)
	}
	ws := session.Workspace{Root: filepath.Clean(root)}
	ws.GitRoot = git(ws.Root, "rev-parse", "--show-toplevel")
	if ws.GitRoot == "" {
		ws.ProjectID = LocalProjectID(ws.Root)
		return ws, nil
	}
	origin := git(ws.GitRoot, "remote", "get-url", "origin")
	if origin == "" {
		ws.ProjectID = LocalProjectID(ws.GitRoot)
		return ws, nil
	}
	id, err := ProjectIDFromOrigin(origin)
	if errors.Is(err, ErrRefused) {
		ws.ProjectID = LocalProjectID(ws.GitRoot)
		return ws, nil
	}
	if err != nil {
		return session.Workspace{}, err
	}
	ws.ProjectID = id
	return ws, nil
}

// git runs one git command in dir and returns its trimmed stdout, or "" on any failure.
func git(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// LocalProjectID is the id for a directory with no usable origin.
func LocalProjectID(dir string) string {
	return "local/" + filepath.Base(filepath.Clean(dir))
}

// ProjectIDFromOrigin turns a git remote URL into host/path. Filesystem paths and
// scheme-only URLs are refused with ErrRefused.
func ProjectIDFromOrigin(url string) (string, error) {
	u := strings.TrimSpace(url)
	if strings.HasPrefix(u, "/") || strings.HasPrefix(u, ".") || strings.HasPrefix(u, "~") || driveRe.MatchString(u) {
		return "", fmt.Errorf("%w: malformed project id origin %q", ErrRefused, u)
	}
	var host, p string
	if schemeRe.MatchString(u) {
		rest := schemeRe.ReplaceAllString(u, "")
		rest = userRe.ReplaceAllString(rest, "")
		i := strings.Index(rest, "/")
		if i < 0 {
			return "", fmt.Errorf("%w: malformed project id origin %q", ErrRefused, u)
		}
		host, p = rest[:i], rest[i+1:]
	} else {
		m := scpRe.FindStringSubmatch(u)
		if m == nil {
			return "", fmt.Errorf("%w: malformed project id origin %q", ErrRefused, u)
		}
		host, p = m[2], m[3]
	}
	host = portRe.ReplaceAllString(strings.ToLower(host), "")
	p = strings.TrimRight(p, "/")
	p = strings.TrimSuffix(p, ".git")
	return AssertProjectID(host + "/" + p)
}

// AssertProjectID refuses ids that could escape a bundle directory.
func AssertProjectID(id string) (string, error) {
	if id == "" || spaceRe.MatchString(id) || strings.HasPrefix(id, "/") {
		return "", fmt.Errorf("%w: malformed project id %q", ErrRefused, id)
	}
	for _, part := range strings.Split(id, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("%w: malformed project id %q", ErrRefused, id)
		}
	}
	return id, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/workspace/ -v`
Expected: PASS for all eight tests (the five git tests skip only when git is absent).

- [ ] **Step 5: Commit**

```bash
git add internal/workspace
git commit -m "workspace: detect root, git root and the memory project id"
bd close <issue-id>
```

---

### Task 7: XDG paths, config loading and secret references

**Files:**
- Create: `internal/config/paths.go`
- Create: `internal/config/config.go`
- Create: `internal/config/secret.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: `session.Mode.Valid()`, `session.ThinkingLevel.Valid()` from Task 2.
- Produces: `config.Paths`, `config.XDG(env func(string) string, home string) Paths`, `config.Config`, `config.ProviderConfig`, `config.Load(paths Paths, overrides map[string]any) (*Config, error)`, `config.Defaults() map[string]any`, `config.ResolveSecret(ref string, env func(string) string, cachePath string) (string, error)`.

Viper only maps environment variables onto keys it already knows, so every key that may be set from the environment needs a default, including the empty-string defaults for `default.provider` and `default.model`.

- [ ] **Step 1: Write the failing tests**

```go
package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/config"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestXDGDefaultsAndOverrides(t *testing.T) {
	p := config.XDG(envOf(nil), "/home/guy")
	if p.Config != "/home/guy/.config/rudy" {
		t.Errorf("config %q", p.Config)
	}
	if p.Data != "/home/guy/.local/share/rudy" {
		t.Errorf("data %q", p.Data)
	}
	if p.Cache != "/home/guy/.cache/rudy" {
		t.Errorf("cache %q", p.Cache)
	}
	if !strings.HasSuffix(p.Runtime, "/rudy") {
		t.Errorf("runtime %q", p.Runtime)
	}
	p = config.XDG(envOf(map[string]string{
		"XDG_CONFIG_HOME": "/x/cfg", "XDG_DATA_HOME": "/x/data",
		"XDG_RUNTIME_DIR": "/x/run", "XDG_CACHE_HOME": "/x/cache",
	}), "/home/guy")
	want := config.Paths{Config: "/x/cfg/rudy", Data: "/x/data/rudy", Runtime: "/x/run/rudy", Cache: "/x/cache/rudy"}
	if p != want {
		t.Errorf("got %+v want %+v", p, want)
	}
}

func TestLoadDefaultsWithoutFile(t *testing.T) {
	c, err := config.Load(config.Paths{Config: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Permissions.Mode != "strict" || c.Default.Thinking != "high" || c.MaxTokens != 8192 {
		t.Errorf("defaults: %+v", c)
	}
	if len(c.Permissions.Dangerous) == 0 || c.Permissions.Dangerous[0] != "rm -rf" {
		t.Errorf("dangerous: %v", c.Permissions.Dangerous)
	}
}

const sampleTOML = `
[default]
provider = "aperture"
model = "cline-pass/kimi-k3"

[permissions]
mode = "permissive"

[providers.aperture]
wire = "openai_chat"
base_url = "https://ai.guy.ts.net/v1"
auth = "env:APERTURE_TOKEN"
headers = { "X-Team" = "rudy" }

[plugins.memory]
dir = "/Users/guy/.agents/memory"
`

func writeConfig(t *testing.T, body string) config.Paths {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Paths{Config: dir}
}

func TestLoadParsesTOML(t *testing.T) {
	c, err := config.Load(writeConfig(t, sampleTOML), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Default.Provider != "aperture" || c.Default.Model != "cline-pass/kimi-k3" {
		t.Errorf("default: %+v", c.Default)
	}
	if c.Permissions.Mode != "permissive" {
		t.Errorf("mode %q", c.Permissions.Mode)
	}
	p := c.Providers["aperture"]
	if p.Wire != "openai_chat" || p.BaseURL != "https://ai.guy.ts.net/v1" || p.Auth != "env:APERTURE_TOKEN" || p.Headers["X-Team"] != "rudy" {
		t.Errorf("provider: %+v", p)
	}
	if c.Plugins["memory"]["dir"] != "/Users/guy/.agents/memory" {
		t.Errorf("plugins: %+v", c.Plugins)
	}
}

func TestLoadEnvOverridesFile(t *testing.T) {
	t.Setenv("RUDY_PERMISSIONS_MODE", "off")
	t.Setenv("RUDY_DEFAULT_MODEL", "cline-pass/glm-5.3")
	c, err := config.Load(writeConfig(t, sampleTOML), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Permissions.Mode != "off" {
		t.Errorf("mode %q", c.Permissions.Mode)
	}
	if c.Default.Model != "cline-pass/glm-5.3" {
		t.Errorf("model %q", c.Default.Model)
	}
}

func TestLoadOverridesWinOverEnvAndFile(t *testing.T) {
	t.Setenv("RUDY_PERMISSIONS_MODE", "off")
	c, err := config.Load(writeConfig(t, sampleTOML), map[string]any{"permissions.mode": "strict"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Permissions.Mode != "strict" {
		t.Errorf("mode %q", c.Permissions.Mode)
	}
}

func TestLoadRefusesInvalid(t *testing.T) {
	cases := map[string]string{
		"bad mode":         strings.Replace(sampleTOML, `mode = "permissive"`, `mode = "yolo"`, 1),
		"unknown provider": strings.Replace(sampleTOML, `provider = "aperture"`, `provider = "nope"`, 1),
		"bad wire":         strings.Replace(sampleTOML, `wire = "openai_chat"`, `wire = "grpc"`, 1),
		"no base url":      strings.Replace(sampleTOML, `base_url = "https://ai.guy.ts.net/v1"`, `base_url = ""`, 1),
	}
	for name, body := range cases {
		if _, err := config.Load(writeConfig(t, body), nil); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestLoadNeverWritesTheFile(t *testing.T) {
	paths := writeConfig(t, sampleTOML)
	file := filepath.Join(paths.Config, "config.toml")
	before, _ := os.Stat(file)
	if _, err := config.Load(paths, map[string]any{"permissions.mode": "off"}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(file)
	body, _ := os.ReadFile(file)
	if string(body) != sampleTOML || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("config file was rewritten")
	}
}

func TestResolveSecret(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "op-secrets.env")
	if err := os.WriteFile(cache, []byte("# comment\nexport OTHER=1\nAPERTURE_TOKEN=\"tok-123\"\nPLAIN=abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := envOf(map[string]string{"FROM_ENV": "env-value"})
	cases := []struct {
		ref, want string
		wantErr   bool
	}{
		{"", "", false},
		{"env:FROM_ENV", "env-value", false},
		{"env:MISSING", "", true},
		{"cache:APERTURE_TOKEN", "tok-123", false},
		{"cache:PLAIN", "abc", false},
		{"cache:OTHER", "1", false},
		{"cache:MISSING", "", true},
		{"vault:x", "", true},
	}
	for _, c := range cases {
		got, err := config.ResolveSecret(c.ref, env, cache)
		if (err != nil) != c.wantErr {
			t.Errorf("%q: err %v", c.ref, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %q want %q", c.ref, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ 2>&1 | head -5`
Expected: FAIL to build with `undefined: config.XDG`.

- [ ] **Step 3: Add viper and write the implementation**

Run: `go get github.com/spf13/viper@v1.21.0`

`internal/config/paths.go`:

```go
// Package config reads rudy's configuration. It never writes it.
package config

import (
	"os"
	"path/filepath"
)

// Paths are the XDG base directories with /rudy appended.
type Paths struct {
	Config  string // $XDG_CONFIG_HOME/rudy
	Data    string // $XDG_DATA_HOME/rudy
	Runtime string // $XDG_RUNTIME_DIR/rudy
	Cache   string // $XDG_CACHE_HOME/rudy
}

// XDG resolves the four directories from env, falling back to the XDG defaults under home.
// XDG_RUNTIME_DIR falls back to the OS temp dir because macOS never sets it.
func XDG(env func(string) string, home string) Paths {
	pick := func(key, fallback string) string {
		if v := env(key); v != "" {
			return filepath.Join(v, "rudy")
		}
		return filepath.Join(fallback, "rudy")
	}
	return Paths{
		Config:  pick("XDG_CONFIG_HOME", filepath.Join(home, ".config")),
		Data:    pick("XDG_DATA_HOME", filepath.Join(home, ".local", "share")),
		Runtime: pick("XDG_RUNTIME_DIR", os.TempDir()),
		Cache:   pick("XDG_CACHE_HOME", filepath.Join(home, ".cache")),
	}
}
```

`internal/config/config.go`:

```go
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"

	"github.com/guygrigsby/rudy/internal/session"
)

// ProviderConfig is one [providers.<name>] table.
type ProviderConfig struct {
	Wire    string            `mapstructure:"wire"`     // "openai_chat"
	BaseURL string            `mapstructure:"base_url"` // ends with /v1
	Auth    string            `mapstructure:"auth"`     // "", "env:NAME", "cache:KEY"
	Headers map[string]string `mapstructure:"headers"`  // extra request headers, verbatim
}

// Config is config.toml after defaults, environment and overrides.
type Config struct {
	Default struct {
		Provider string `mapstructure:"provider"`
		Model    string `mapstructure:"model"`
		Thinking string `mapstructure:"thinking"`
	} `mapstructure:"default"`
	Permissions struct {
		Mode      string   `mapstructure:"mode"`
		Dangerous []string `mapstructure:"dangerous"`
	} `mapstructure:"permissions"`
	Providers map[string]ProviderConfig `mapstructure:"providers"`
	Plugins   map[string]map[string]any `mapstructure:"plugins"`
	MaxTokens int                       `mapstructure:"max_tokens"`
}

// Defaults are the values in force when neither the file nor the environment sets a key.
// Keys with empty defaults exist so RUDY_* environment variables can set them.
func Defaults() map[string]any {
	return map[string]any{
		"default.provider": "",
		"default.model":    "",
		"default.thinking": "high",
		"permissions.mode": "strict",
		"permissions.dangerous": []string{
			"rm -rf", "rm -r", "git push --force", "git push -f", "git reset --hard",
			"git clean", "sudo", "chmod -R", "chown -R", "mkfs", "dd",
		},
		"max_tokens": 8192,
	}
}

// Load reads <paths.Config>/config.toml when it exists, applies RUDY_* environment
// variables (RUDY_PERMISSIONS_MODE sets permissions.mode) and then overrides, validates
// and returns the result. It never writes.
func Load(paths Paths, overrides map[string]any) (*Config, error) {
	v := viper.New()
	for k, val := range Defaults() {
		v.SetDefault(k, val)
	}
	v.SetEnvPrefix("RUDY")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	file := filepath.Join(paths.Config, "config.toml")
	if _, err := os.Stat(file); err == nil {
		v.SetConfigFile(file)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("config: read %s: %w", file, err)
		}
	}
	for k, val := range overrides {
		v.Set(k, val)
	}
	var c Config
	if err := v.Unmarshal(&c); err != nil {
		return nil, fmt.Errorf("config: decode: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	var errs []error
	if !session.Mode(c.Permissions.Mode).Valid() {
		errs = append(errs, fmt.Errorf("config: permissions.mode %q is not strict, permissive or off", c.Permissions.Mode))
	}
	if !session.ThinkingLevel(c.Default.Thinking).Valid() {
		errs = append(errs, fmt.Errorf("config: default.thinking %q is not off, low, medium or high", c.Default.Thinking))
	}
	if len(c.Providers) > 0 {
		if _, ok := c.Providers[c.Default.Provider]; !ok {
			errs = append(errs, fmt.Errorf("config: default.provider %q is not a [providers.*] table", c.Default.Provider))
		}
	}
	for name, p := range c.Providers {
		if p.Wire != "openai_chat" {
			errs = append(errs, fmt.Errorf("config: providers.%s.wire %q is not openai_chat", name, p.Wire))
		}
		if p.BaseURL == "" {
			errs = append(errs, fmt.Errorf("config: providers.%s.base_url is empty", name))
		}
	}
	if c.MaxTokens <= 0 {
		errs = append(errs, fmt.Errorf("config: max_tokens %d must be positive", c.MaxTokens))
	}
	return errors.Join(errs...)
}
```

`internal/config/secret.go`:

```go
package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// ResolveSecret turns an auth reference into its value. "" resolves to "", "env:NAME"
// reads the environment, "cache:KEY" reads KEY=value lines from the 1Password cache file.
func ResolveSecret(ref string, env func(string) string, cachePath string) (string, error) {
	switch {
	case ref == "":
		return "", nil
	case strings.HasPrefix(ref, "env:"):
		name := strings.TrimPrefix(ref, "env:")
		v := env(name)
		if v == "" {
			return "", fmt.Errorf("config: secret %s: %s is unset", ref, name)
		}
		return v, nil
	case strings.HasPrefix(ref, "cache:"):
		key := strings.TrimPrefix(ref, "cache:")
		f, err := os.Open(cachePath)
		if err != nil {
			return "", fmt.Errorf("config: secret %s: %w", ref, err)
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimPrefix(line, "export ")
			k, val, ok := strings.Cut(line, "=")
			if !ok || strings.TrimSpace(k) != key {
				continue
			}
			val = strings.TrimSpace(val)
			if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
				val = val[1 : len(val)-1]
			}
			return val, nil
		}
		if err := sc.Err(); err != nil {
			return "", fmt.Errorf("config: secret %s: %w", ref, err)
		}
		return "", fmt.Errorf("config: secret %s: %s not in %s", ref, key, cachePath)
	default:
		return "", fmt.Errorf("config: secret reference %q must be empty, env:NAME or cache:KEY", ref)
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/config/ -v`
Expected: PASS for all eight tests.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/config
git commit -m "config: XDG paths, viper load with defaults and secret references"
bd close <issue-id>
```

---

### Task 8: Provider port, registry and the retrying HTTP client

**Files:**
- Create: `internal/provider/provider.go`
- Create: `internal/provider/model.go`
- Create: `internal/provider/registry.go`
- Create: `internal/provider/httpx/client.go`
- Test: `internal/provider/model_test.go`
- Test: `internal/provider/registry_test.go`
- Test: `internal/provider/httpx/client_test.go`

**Interfaces:**
- Consumes: `session.ModelRef`, `session.ParseModelRef`, `session.Usage`, `session.ThinkingLevel`, `session.StopReason`, `session.ErrorClass`, `session.Block` from Task 2.
- Produces: every type and function in the `internal/provider` and `internal/provider/httpx` sections of the plan's Interfaces, plus `provider.ErrAmbiguous`, `provider.Error.Attempts`, `httpx.Retryable(status int) bool`, `httpx.AttemptsOf(resp *http.Response) int` and `*httpx.Error{Attempts, Err}` for transport exhaustion.

- [ ] **Step 1: Write the failing tests**

`internal/provider/model_test.go`:

```go
package provider_test

import (
	"testing"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

func TestPricingCost(t *testing.T) {
	aperture := provider.Pricing{Input: "0.00000014", Output: "0.00000028", CacheRead: "0.00000000"}
	cases := []struct {
		name  string
		p     provider.Pricing
		u     session.Usage
		want  string
		known bool
	}{
		{"input only", aperture, session.Usage{Input: 1200}, "0.000168", true},
		{"input and output", aperture, session.Usage{Input: 1200, Output: 80}, "0.000190", true},
		{"cache read priced at zero", aperture, session.Usage{Input: 100, CacheRead: 5000}, "0.000014", true},
		{"zero usage is free", aperture, session.Usage{}, "0.000000", true},
		{"cache write unknown but used", aperture, session.Usage{Input: 1, CacheWrite: 1}, "", false},
		{"no prices at all", provider.Pricing{}, session.Usage{Input: 1}, "", false},
		{"no prices no usage", provider.Pricing{}, session.Usage{}, "0.000000", true},
	}
	for _, c := range cases {
		got, known := c.p.Cost(c.u)
		if known != c.known || got != c.want {
			t.Errorf("%s: got %q %v want %q %v", c.name, got, known, c.want, c.known)
		}
	}
}

func TestErrorString(t *testing.T) {
	e := &provider.Error{Class: session.ErrProvider, Status: 500, Message: "empty response content"}
	if e.Error() != "provider: provider 500 empty response content" {
		t.Fatalf("got %q", e.Error())
	}
}
```

`internal/provider/registry_test.go`:

```go
package provider_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

type fakeProvider struct {
	name   string
	models []provider.Model
	err    error
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Complete(context.Context, provider.Request, func(provider.Part) error) error {
	return errors.New("not implemented")
}
func (f *fakeProvider) ListModels(context.Context) ([]provider.Model, error) {
	return f.models, f.err
}

func model(prov, id string) provider.Model {
	return provider.Model{Ref: session.ModelRef{Provider: prov, Model: id}, ContextWindow: 128000}
}

func TestRegistryRefreshKeepsOldModelsWhenAProviderFails(t *testing.T) {
	a := &fakeProvider{name: "aperture", models: []provider.Model{model("aperture", "cline-pass/kimi-k3"), model("aperture", "gpt-5.6-sol")}}
	b := &fakeProvider{name: "mlx", models: []provider.Model{model("mlx", "qwen3-coder")}}
	snap := filepath.Join(t.TempDir(), "registry.json")
	r := provider.NewRegistry(snap, a, b)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(r.Models()); got != 3 {
		t.Fatalf("want 3 models, got %d", got)
	}
	b.err = errors.New("connection refused")
	b.models = nil
	err := r.Refresh(context.Background())
	if err == nil || !errors.Is(err, b.err) {
		t.Fatalf("want joined error containing the provider failure, got %v", err)
	}
	if got := len(r.Models()); got != 3 {
		t.Fatalf("failing provider must keep its previous models, got %d", got)
	}
}

func TestRegistrySnapshotRoundTrip(t *testing.T) {
	a := &fakeProvider{name: "aperture", models: []provider.Model{{
		Ref:           session.ModelRef{Provider: "aperture", Model: "cline-pass/deepseek-v4-flash"},
		DisplayName:   "DeepSeek V4 Flash",
		ContextWindow: 1000000,
		MaxOutput:     384000,
		Pricing:       provider.Pricing{Input: "0.00000014", Output: "0.00000028", CacheRead: "0.00000000"},
		Capabilities:  provider.Capabilities{Tools: true, Reasoning: true},
	}}}
	snap := filepath.Join(t.TempDir(), "registry.json")
	r := provider.NewRegistry(snap, a)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(snap); err != nil {
		t.Fatalf("snapshot not written: %v", err)
	}
	r2 := provider.NewRegistry(snap, a)
	if err := r2.LoadSnapshot(); err != nil {
		t.Fatal(err)
	}
	got := r2.Models()
	if len(got) != 1 || got[0] != a.models[0] {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	r3 := provider.NewRegistry(filepath.Join(t.TempDir(), "missing.json"), a)
	if err := r3.LoadSnapshot(); err != nil {
		t.Fatalf("missing snapshot must not be an error: %v", err)
	}
}

func TestRegistryResolve(t *testing.T) {
	a := &fakeProvider{name: "aperture", models: []provider.Model{model("aperture", "deepseek-v4-flash"), model("aperture", "gpt-5.6-sol")}}
	b := &fakeProvider{name: "direct", models: []provider.Model{model("direct", "deepseek-v4-flash")}}
	r := provider.NewRegistry(filepath.Join(t.TempDir(), "r.json"), a, b)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m, err := r.Resolve("aperture:deepseek-v4-flash"); err != nil || m.Ref.Provider != "aperture" {
		t.Errorf("qualified: %+v %v", m, err)
	}
	if m, err := r.Resolve("gpt-5.6-sol"); err != nil || m.Ref.Provider != "aperture" {
		t.Errorf("unique bare id: %+v %v", m, err)
	}
	if _, err := r.Resolve("deepseek-v4-flash"); !errors.Is(err, provider.ErrAmbiguous) {
		t.Errorf("ambiguous bare id: %v", err)
	}
	if _, err := r.Resolve("nope"); !errors.Is(err, provider.ErrUnknownModel) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := r.Resolve("aperture:nope"); !errors.Is(err, provider.ErrUnknownModel) {
		t.Errorf("unknown qualified: %v", err)
	}
	if p, ok := r.Provider("direct"); !ok || p.Name() != "direct" {
		t.Errorf("provider lookup")
	}
}
```

`internal/provider/httpx/client_test.go`:

```go
package httpx_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/provider/httpx"
)

func newClient(t *testing.T) (*httpx.Client, *[]time.Duration) {
	t.Helper()
	slept := &[]time.Duration{}
	c := httpx.New("0.1.0")
	c.Sleep = func(d time.Duration) { *slept = append(*slept, d) }
	c.Now = func() time.Time { return time.Date(2026, 9, 7, 20, 0, 0, 0, time.UTC) }
	return c, slept
}

func TestDoRetriesThenSucceedsAndReplaysBody(t *testing.T) {
	var attempts atomic.Int32
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	}))
	defer srv.Close()
	c, slept := newClient(t)
	req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader([]byte(`{"x":1}`)))
	resp, err := c.Do(context.Background(), req, ulid.ULID{})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || attempts.Load() != 3 {
		t.Fatalf("status %d attempts %d", resp.StatusCode, attempts.Load())
	}
	if len(*slept) != 2 || (*slept)[0] != time.Second || (*slept)[1] != 2*time.Second {
		t.Fatalf("backoff %v", *slept)
	}
	for i, b := range bodies {
		if b != `{"x":1}` {
			t.Fatalf("attempt %d body %q", i, b)
		}
	}
}

func TestDoGivesUpAfterFiveAttemptsReturningTheLastResponse(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c, _ := newClient(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := c.Do(context.Background(), req, ulid.ULID{})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 || attempts.Load() != 5 {
		t.Fatalf("status %d attempts %d", resp.StatusCode, attempts.Load())
	}
}

func TestDoHonorsRetryAfterSecondsAndDate(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch attempts.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.Header().Set("Retry-After", "Mon, 07 Sep 2026 20:00:07 GMT")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	c, slept := newClient(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := c.Do(context.Background(), req, ulid.ULID{})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(*slept) != 2 || (*slept)[0] != 3*time.Second || (*slept)[1] != 7*time.Second {
		t.Fatalf("slept %v", *slept)
	}
}

func TestDoSetsHeaders(t *testing.T) {
	var ua, sid string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua, sid = r.Header.Get("User-Agent"), r.Header.Get("X-Rudy-Session")
	}))
	defer srv.Close()
	c, _ := newClient(t)
	id := ulid.Make()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := c.Do(context.Background(), req, id)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.HasPrefix(ua, "rudy/0.1.0 (") || !strings.HasSuffix(ua, ")") {
		t.Errorf("user agent %q", ua)
	}
	if sid != id.String() {
		t.Errorf("session header %q", sid)
	}
	req, _ = http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err = c.Do(context.Background(), req, ulid.ULID{})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if sid != "" {
		t.Errorf("zero session id must send no header, got %q", sid)
	}
}

func TestDoStopsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c, _ := newClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	c.Sleep = func(time.Duration) { cancel() }
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	if _, err := c.Do(ctx, req, ulid.ULID{}); err != context.Canceled {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 7, 20, 0, 0, 0, time.UTC)
	h := http.Header{}
	if _, ok := httpx.RetryAfter(h, now); ok {
		t.Error("absent header")
	}
	h.Set("Retry-After", "120")
	if d, ok := httpx.RetryAfter(h, now); !ok || d != 60*time.Second {
		t.Errorf("seconds capped at 60: %v %v", d, ok)
	}
	h.Set("Retry-After", "Mon, 07 Sep 2026 19:59:00 GMT")
	if d, ok := httpx.RetryAfter(h, now); !ok || d != 0 {
		t.Errorf("past date clamps to zero: %v %v", d, ok)
	}
	h.Set("Retry-After", "soon")
	if _, ok := httpx.RetryAfter(h, now); ok {
		t.Error("garbage must be ignored")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/provider/... 2>&1 | head -5`
Expected: FAIL to build with `undefined: provider.Pricing` and `undefined: httpx.New`.

- [ ] **Step 3: Write the implementation**

`internal/provider/provider.go`:

```go
// Package provider is the port through which the turn loop reaches a model. Wire formats
// live in subpackages; nothing here knows what an HTTP request looks like.
package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/session"
)

// Role is who a message is from, in domain terms.
type Role string

const (
	RoleUser       Role = "user"
	RoleAssistant  Role = "assistant"
	RoleToolResult Role = "tool_result"
)

// Message is one turn of conversation in the shape the loop keeps it.
type Message struct {
	Role      Role
	Content   []session.Block
	ToolUseID string // tool_result only
}

// ToolDef is a tool as the model sees it.
type ToolDef struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// Request is one completion request.
type Request struct {
	Model     session.ModelRef
	System    string
	Messages  []Message
	Tools     []ToolDef
	Thinking  session.ThinkingLevel
	MaxTokens int
	SessionID ulid.ULID // for the X-Rudy-Session header
}

// PartType tags a streamed Part.
type PartType string

const (
	PartTextDelta     PartType = "text_delta"
	PartThinkingDelta PartType = "thinking_delta"
	PartToolUseStart  PartType = "tool_use_start" // ID, Name
	PartToolUseDelta  PartType = "tool_use_delta" // ID, Input fragment in Text
	PartToolUseEnd    PartType = "tool_use_end"   // ID
	PartUsage         PartType = "usage"
	PartStop          PartType = "stop"
)

// Part is one streamed piece of a completion.
type Part struct {
	Type          PartType           `json:"type"`
	Text          string             `json:"text,omitempty"`
	ID            string             `json:"id,omitempty"`
	Name          string             `json:"name,omitempty"`
	Usage         session.Usage      `json:"usage,omitempty"`
	StopReason    session.StopReason `json:"stop_reason,omitempty"`
	StopReasonRaw string             `json:"stop_reason_raw,omitempty"`
}

// Provider is one configured endpoint.
//
// Complete streams parts by calling emit in order until the stream ends. A non-nil error
// from emit stops the stream and is returned. Context cancellation returns ctx.Err().
type Provider interface {
	Name() string
	Complete(ctx context.Context, req Request, emit func(Part) error) error
	ListModels(ctx context.Context) ([]Model, error)
}

// Error is a provider failure after retries. The turn loop records it as turn_failed with
// Retries set from Attempts.
type Error struct {
	Class    session.ErrorClass
	Status   int
	Message  string
	Body     []byte
	Attempts int // attempts httpx made before giving up; a codec fills it from httpx.AttemptsOf or *httpx.Error
}

func (e *Error) Error() string {
	return fmt.Sprintf("provider: %s %d %s", e.Class, e.Status, e.Message)
}
```

`internal/provider/model.go`:

```go
package provider

import (
	"math/big"

	"github.com/guygrigsby/rudy/internal/session"
)

// Pricing is USD per token as decimal strings, verbatim from the provider. "" is unknown.
type Pricing struct {
	Input      string `json:"input"`
	Output     string `json:"output"`
	CacheRead  string `json:"cache_read"`
	CacheWrite string `json:"cache_write"`
}

// Cost prices a usage. It is known only when every price that a non-zero count needs
// is present. The result carries six decimals.
func (p Pricing) Cost(u session.Usage) (usd string, known bool) {
	terms := []struct {
		price string
		n     int64
	}{
		{p.Input, u.Input}, {p.Output, u.Output}, {p.CacheRead, u.CacheRead}, {p.CacheWrite, u.CacheWrite},
	}
	total := new(big.Rat)
	for _, t := range terms {
		if t.n == 0 {
			continue
		}
		r, ok := new(big.Rat).SetString(t.price)
		if !ok {
			return "", false
		}
		total.Add(total, r.Mul(r, new(big.Rat).SetInt64(t.n)))
	}
	return total.FloatString(6), true
}

// Capabilities are what a model can do, from discovery or enrichment.
type Capabilities struct {
	Tools     bool `json:"tools"`
	Vision    bool `json:"vision"`
	Reasoning bool `json:"reasoning"`
}

// Model is one id a provider serves.
type Model struct {
	Ref           session.ModelRef `json:"ref"`
	DisplayName   string           `json:"display_name"`
	ContextWindow int64            `json:"context_window"` // 0 unknown
	MaxOutput     int64            `json:"max_output"`     // 0 unknown
	Pricing       Pricing          `json:"pricing"`
	Capabilities  Capabilities     `json:"capabilities"`
}
```

`internal/provider/registry.go`:

```go
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/guygrigsby/rudy/internal/session"
)

var (
	ErrUnknownModel = errors.New("provider: unknown model")
	ErrAmbiguous    = errors.New("provider: ambiguous model id")
)

// Registry is the discovered model list across every configured provider, with a snapshot
// on disk so a session can open before the first refresh completes.
type Registry struct {
	mu        sync.Mutex
	snapshot  string
	order     []string
	providers map[string]Provider
	models    map[string][]Model // by provider name
}

type snapshotFile struct {
	FetchedAt time.Time `json:"fetched_at"`
	Models    []Model   `json:"models"`
}

func NewRegistry(snapshotPath string, providers ...Provider) *Registry {
	r := &Registry{snapshot: snapshotPath, providers: map[string]Provider{}, models: map[string][]Model{}}
	for _, p := range providers {
		r.order = append(r.order, p.Name())
		r.providers[p.Name()] = p
	}
	return r
}

// Refresh lists models on every provider concurrently. A provider that fails keeps its
// previous models; every failure is joined into the returned error. The snapshot is
// rewritten when at least one provider answered.
func (r *Registry) Refresh(ctx context.Context) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	results := map[string][]Model{}
	for _, name := range r.order {
		p := r.providers[name]
		wg.Add(1)
		go func() {
			defer wg.Done()
			ms, err := p.ListModels(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("provider %s: %w", p.Name(), err))
				return
			}
			results[p.Name()] = ms
		}()
	}
	wg.Wait()
	r.mu.Lock()
	for name, ms := range results {
		r.models[name] = ms
	}
	r.mu.Unlock()
	if len(results) > 0 {
		if err := r.writeSnapshot(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *Registry) writeSnapshot() error {
	if r.snapshot == "" {
		return nil
	}
	data, err := json.MarshalIndent(snapshotFile{FetchedAt: time.Now(), Models: r.Models()}, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.snapshot), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.snapshot), ".registry-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), r.snapshot)
}

// LoadSnapshot fills the registry from the snapshot file. A missing file is not an error.
func (r *Registry) LoadSnapshot() error {
	data, err := os.ReadFile(r.snapshot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("provider: read snapshot: %w", err)
	}
	var s snapshotFile
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("provider: decode snapshot %s: %w", r.snapshot, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.models = map[string][]Model{}
	for _, m := range s.Models {
		r.models[m.Ref.Provider] = append(r.models[m.Ref.Provider], m)
	}
	return nil
}

// Models returns every model sorted by provider then id.
func (r *Registry) Models() []Model {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Model
	for _, ms := range r.models {
		out = append(out, ms...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ref.Provider != out[j].Ref.Provider {
			return out[i].Ref.Provider < out[j].Ref.Provider
		}
		return out[i].Ref.Model < out[j].Ref.Model
	})
	return out
}

// Resolve finds a model by "provider:id" or by a bare id that is unique across providers.
func (r *Registry) Resolve(spec string) (Model, error) {
	if ref, ok := session.ParseModelRef(spec); ok {
		for _, m := range r.Models() {
			if m.Ref == ref {
				return m, nil
			}
		}
		return Model{}, fmt.Errorf("%w: %s", ErrUnknownModel, spec)
	}
	var matches []Model
	for _, m := range r.Models() {
		if m.Ref.Model == spec {
			matches = append(matches, m)
		}
	}
	switch len(matches) {
	case 0:
		return Model{}, fmt.Errorf("%w: %s", ErrUnknownModel, spec)
	case 1:
		return matches[0], nil
	}
	var names []string
	for _, m := range matches {
		names = append(names, m.Ref.String())
	}
	return Model{}, fmt.Errorf("%w: %s matches %s", ErrAmbiguous, spec, strings.Join(names, ", "))
}

// Provider returns a configured provider by name.
func (r *Registry) Provider(name string) (Provider, bool) {
	p, ok := r.providers[name]
	return p, ok
}
```

`internal/provider/httpx/client.go`:

```go
// Package httpx is the one HTTP client every wire codec uses: identifying headers, retry
// with backoff and Retry-After, context cancellation.
package httpx

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/oklog/ulid/v2"
)

// Client wraps http.Client. Sleep and Now are replaceable for tests.
type Client struct {
	HTTP    *http.Client
	Version string
	Sleep   func(time.Duration)
	Now     func() time.Time
}

// New builds a client with no overall timeout (responses stream for minutes) and a 60s
// response header timeout so a dead upstream fails fast.
func New(version string) *Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 60 * time.Second
	return &Client{HTTP: &http.Client{Transport: t}, Version: version, Sleep: time.Sleep, Now: time.Now}
}

var backoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}

// Retryable reports whether a status warrants another attempt.
func Retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// Error is returned when every attempt failed at the transport level. Attempts is how
// many were made. Unwrap yields the last transport error.
type Error struct {
	Attempts int
	Err      error
}

func (e *Error) Error() string {
	return fmt.Sprintf("httpx: gave up after %d attempts: %v", e.Attempts, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

type attemptsKey struct{}

// AttemptsOf reports how many attempts Do made before returning resp. It is carried on the
// returned response's request context so the Do signature stays a plain (*http.Response,
// error). Zero for a response that did not come from Do.
func AttemptsOf(resp *http.Response) int {
	if resp == nil || resp.Request == nil {
		return 0
	}
	n, _ := resp.Request.Context().Value(attemptsKey{}).(int)
	return n
}

// Do sends req with rudy's headers, retrying retryable statuses and transport errors up
// to five attempts. The request body must be replayable through req.GetBody, which
// http.NewRequest sets for bytes.Reader, strings.Reader and bytes.Buffer bodies. On the
// fifth retryable status the response is returned so the caller can classify it, with the
// attempt count readable through AttemptsOf. Transport exhaustion returns *Error.
func (c *Client) Do(ctx context.Context, req *http.Request, sessionID ulid.ULID) (*http.Response, error) {
	req = req.WithContext(ctx)
	req.Header.Set("User-Agent", fmt.Sprintf("rudy/%s (%s/%s)", c.Version, runtime.GOOS, runtime.GOARCH))
	if !sessionID.IsZero() {
		req.Header.Set("X-Rudy-Session", sessionID.String())
	}
	var last error
	var delay time.Duration
	for attempt := range len(backoff) {
		if attempt > 0 {
			if req.GetBody != nil {
				body, err := req.GetBody()
				if err != nil {
					return nil, fmt.Errorf("httpx: replay body: %w", err)
				}
				req.Body = body
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			c.Sleep(delay)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			last = err
			delay = backoff[attempt]
			continue
		}
		if !Retryable(resp.StatusCode) || attempt == len(backoff)-1 {
			resp.Request = resp.Request.WithContext(context.WithValue(resp.Request.Context(), attemptsKey{}, attempt+1))
			return resp, nil
		}
		if d, ok := RetryAfter(resp.Header, c.Now()); ok {
			delay = d
		} else {
			delay = backoff[attempt]
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		last = fmt.Errorf("httpx: %s %s: status %d", req.Method, req.URL, resp.StatusCode)
	}
	return nil, &Error{Attempts: len(backoff), Err: last}
}

// RetryAfter parses a Retry-After header in delta-seconds or HTTP-date form, clamped to
// [0, 60s]. ok is false when the header is absent or unparseable.
func RetryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	v := h.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	} else {
		return 0, false
	}
	if d < 0 {
		d = 0
	}
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d, true
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/provider/... -v`
Expected: PASS for all eleven tests. `TestDoRetriesThenSucceedsAndReplaysBody` proves the body reaches the server on every attempt because `http.NewRequest` sets `GetBody` for a `bytes.Reader`.

- [ ] **Step 5: Commit**

```bash
git add internal/provider
git commit -m "provider: port types, registry with snapshot and a retrying http client"
bd close <issue-id>
```

---

### Task 9: openai_chat codec

**Files:**
- Create: `internal/provider/openaichat/request.go`
- Create: `internal/provider/openaichat/stream.go`
- Create: `internal/provider/openaichat/client.go`
- Create: `internal/provider/openaichat/models.go`
- Test: `internal/provider/openaichat/request_test.go`
- Test: `internal/provider/openaichat/client_test.go`
- Test: `internal/provider/openaichat/models_test.go`
- Fixtures already in the tree: `internal/provider/openaichat/testdata/{kimi-tool-stream.sse,deepseek-text-stream.sse,clinepass-nonstream.json,clinepass-empty-500.json,models.json}`

**Interfaces:**
- Consumes: `session.Block`, `session.BlockText`, `session.BlockImage`, `session.BlockThinking`, `session.BlockToolUse`, `session.Usage`, `session.ModelRef`, `session.StopReason` constants, `session.ErrProvider`, `session.ErrTransport` (Task 2); `provider.Request`, `provider.Message`, `provider.Role`, `provider.ToolDef`, `provider.Part`, `provider.Part*` constants, `provider.Model`, `provider.Pricing`, `provider.Capabilities`, `provider.Error` (Task 8); `httpx.Client.Do(ctx, *http.Request, ulid.ULID) (*http.Response, error)`, `httpx.New(version)`, `httpx.AttemptsOf(resp *http.Response) int` and `*httpx.Error{Attempts, Err}` (Task 8); the role constants `provider.RoleUser`, `provider.RoleAssistant`, `provider.RoleToolResult` and `provider.Error.Attempts` (Task 8). Every `*provider.Error` this codec returns carries `Attempts`: copied from `*httpx.Error` when the transport gave up, from `httpx.AttemptsOf(resp)` when a response arrived, and 1 when the codec itself rejected bytes it read.
- Produces: `openaichat.Options`, `openaichat.New(Options) *Client`, `(*Client).Name()`, `(*Client).Complete(ctx, provider.Request, emit) error`, `(*Client).ListModels(ctx) ([]provider.Model, error)`. Task 16 constructs one `Client` per configured `openai_chat` provider.

Behavior pinned by this task, all of it tested against the recorded fixtures:

- The codec always streams. `stream: true` and `stream_options.include_usage: true` are set on every request. A server that answers with a JSON body instead of an event stream is parsed as one non-streaming completion; a body that is neither (the clinepass `{"data": …}` envelope) is a `provider.Error` naming that, which is the quirk the clinepass plugin fixes in the next plan.
- Reasoning: `delta.reasoning_details[].text` when present, else `delta.reasoning`. Never both, since aperture sends the same text in both fields.
- Tool calls are keyed by `index`. A chunk carrying `id` and `function.name` opens a call and emits `tool_use_start`; every `function.arguments` fragment emits `tool_use_delta`; the first `finish_reason` closes every open call with `tool_use_end`.
- The stop is buffered. `finish_reason` may arrive twice (deepseek sends `stop` in two chunks) and usage may arrive after it. Exactly one `PartStop` is emitted, last, after `PartUsage`.
- Thinking blocks on assistant messages are not sent back to the provider in this plan, and `Request.Thinking` has no wire form here. Both are pass 2 work for the provider plugins.
- Image blocks anywhere in the request return an error naming the limitation.

- [ ] **Step 0: Track the task**

```bash
bd create --title "Task 9: openai_chat codec" --type task
bd update <id> --claim
```

- [ ] **Step 1: Write the failing request-build test**

`internal/provider/openaichat/request_test.go`:

```go
package openaichat

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

func compact(t *testing.T, s string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		t.Fatalf("compact: %v", err)
	}
	return buf.String()
}

func TestBuildRequestGolden(t *testing.T) {
	req := provider.Request{
		Model:  session.ModelRef{Provider: "aperture", Model: "cline-pass/kimi-k3"},
		System: "You are rudy.",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []session.Block{session.TextBlock("read go.mod")}},
			{Role: provider.RoleAssistant, Content: []session.Block{
				{Type: session.BlockThinking, Text: "I should read it.", Signature: "sig"},
				session.ToolUseBlock("call_1", "read", json.RawMessage(`{"path":"go.mod"}`)),
			}},
			{Role: provider.RoleToolResult, ToolUseID: "call_1", Content: []session.Block{session.TextBlock("module x")}},
		},
		Tools: []provider.ToolDef{{
			Name:        "read",
			Description: "Read a file",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}},
		MaxTokens: 512,
	}
	got, err := buildRequest(req)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	want := compact(t, `{
	  "model": "cline-pass/kimi-k3",
	  "messages": [
	    {"role": "system", "content": "You are rudy."},
	    {"role": "user", "content": "read go.mod"},
	    {"role": "assistant", "content": "", "tool_calls": [
	      {"id": "call_1", "type": "function", "function": {"name": "read", "arguments": "{\"path\":\"go.mod\"}"}}
	    ]},
	    {"role": "tool", "content": "module x", "tool_call_id": "call_1"}
	  ],
	  "tools": [
	    {"type": "function", "function": {
	      "name": "read", "description": "Read a file",
	      "parameters": {"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}
	    }}
	  ],
	  "stream": true,
	  "stream_options": {"include_usage": true},
	  "max_tokens": 512
	}`)
	if string(got) != want {
		t.Fatalf("body mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildRequestToolInputIsVerbatim(t *testing.T) {
	raw := json.RawMessage(`{"b":  1, "a": [1,2 ]}`)
	req := provider.Request{
		Model: session.ModelRef{Provider: "p", Model: "m"},
		Messages: []provider.Message{
			{Role: provider.RoleAssistant, Content: []session.Block{session.ToolUseBlock("c", "t", raw)}},
		},
	}
	got, err := buildRequest(req)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	var w struct {
		Messages []struct {
			ToolCalls []struct {
				Function struct{ Arguments string } `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &w); err != nil {
		t.Fatal(err)
	}
	if a := w.Messages[0].ToolCalls[0].Function.Arguments; a != string(raw) {
		t.Fatalf("arguments were re-marshaled: %q", a)
	}
}

func TestBuildRequestRefusesImages(t *testing.T) {
	req := provider.Request{
		Model: session.ModelRef{Provider: "p", Model: "m"},
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []session.Block{{Type: session.BlockImage, MediaType: "image/png", SHA256: "abc"}}},
		},
	}
	_, err := buildRequest(req)
	if !errors.Is(err, errImageUnsupported) {
		t.Fatalf("want errImageUnsupported, got %v", err)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/provider/openaichat/ -run TestBuildRequest -v`
Expected: build failure with `undefined: buildRequest` and `undefined: errImageUnsupported`.

- [ ] **Step 3: Write the request builder**

`internal/provider/openaichat/request.go`:

```go
package openaichat

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

var errImageUnsupported = errors.New("openaichat: image blocks are not supported in this plan")

type wireRequest struct {
	Model         string             `json:"model"`
	Messages      []wireMessage      `json:"messages"`
	Tools         []wireTool         `json:"tools,omitempty"`
	Stream        bool               `json:"stream"`
	StreamOptions *wireStreamOptions `json:"stream_options,omitempty"`
	MaxTokens     int                `json:"max_tokens,omitempty"`
}

type wireStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string           `json:"type"`
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// buildRequest translates a domain request into the chat-completions wire shape.
// Thinking blocks on assistant messages are dropped and req.Thinking is not sent;
// both are provider-plugin work in the next plan. Tool inputs pass through as the
// verbatim bytes the model produced.
func buildRequest(req provider.Request) ([]byte, error) {
	w := wireRequest{
		Model:         req.Model.Model,
		Stream:        true,
		StreamOptions: &wireStreamOptions{IncludeUsage: true},
		MaxTokens:     req.MaxTokens,
	}
	if req.System != "" {
		w.Messages = append(w.Messages, wireMessage{Role: "system", Content: req.System})
	}
	for i, m := range req.Messages {
		wm, err := buildMessage(m)
		if err != nil {
			return nil, fmt.Errorf("openaichat: message %d: %w", i, err)
		}
		w.Messages = append(w.Messages, wm)
	}
	for _, t := range req.Tools {
		w.Tools = append(w.Tools, wireTool{Type: "function", Function: wireToolFunction{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Schema,
		}})
	}
	return json.Marshal(w)
}

func buildMessage(m provider.Message) (wireMessage, error) {
	switch m.Role {
	case provider.RoleUser:
		text, err := textOf(m.Content)
		if err != nil {
			return wireMessage{}, err
		}
		return wireMessage{Role: "user", Content: text}, nil
	case provider.RoleAssistant:
		var texts []string
		var calls []wireToolCall
		for _, b := range m.Content {
			switch b.Type {
			case session.BlockText:
				texts = append(texts, b.Text)
			case session.BlockToolUse:
				calls = append(calls, wireToolCall{
					ID:       b.ID,
					Type:     "function",
					Function: wireFunction{Name: b.Name, Arguments: string(b.Input)},
				})
			case session.BlockThinking:
				// Not sent back in this plan.
			case session.BlockImage:
				return wireMessage{}, errImageUnsupported
			default:
				return wireMessage{}, fmt.Errorf("openaichat: block type %q in assistant message", b.Type)
			}
		}
		return wireMessage{Role: "assistant", Content: strings.Join(texts, "\n"), ToolCalls: calls}, nil
	case provider.RoleToolResult:
		text, err := textOf(m.Content)
		if err != nil {
			return wireMessage{}, err
		}
		return wireMessage{Role: "tool", Content: text, ToolCallID: m.ToolUseID}, nil
	}
	return wireMessage{}, fmt.Errorf("openaichat: unknown role %q", m.Role)
}

// textOf joins text blocks with newlines. Anything else is refused.
func textOf(blocks []session.Block) (string, error) {
	var texts []string
	for _, b := range blocks {
		switch b.Type {
		case session.BlockText:
			texts = append(texts, b.Text)
		case session.BlockImage:
			return "", errImageUnsupported
		default:
			return "", fmt.Errorf("openaichat: block type %q not allowed here", b.Type)
		}
	}
	return strings.Join(texts, "\n"), nil
}
```

- [ ] **Step 4: Run the request tests to verify they pass**

Run: `go test ./internal/provider/openaichat/ -run TestBuildRequest -v`
Expected: PASS for all three.

- [ ] **Step 5: Write the failing streaming tests against the fixtures**

`internal/provider/openaichat/client_test.go`:

```go
package openaichat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/provider/httpx"
	"github.com/guygrigsby/rudy/internal/session"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// serve returns a client pointed at a server that answers every request with
// the given status, content type and body and records the last request seen.
func serve(t *testing.T, status int, contentType string, body []byte) (*Client, *http.Request) {
	t.Helper()
	var last http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = *r
		last.Header = r.Header.Clone()
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	c := New(Options{
		Name:    "aperture",
		BaseURL: srv.URL + "/v1",
		Token:   "tok",
		Headers: map[string]string{"X-Test": "1"},
		HTTP:    httpx.New("test"),
	})
	return c, &last
}

func simpleRequest() provider.Request {
	return provider.Request{
		Model:     session.ModelRef{Provider: "aperture", Model: "cline-pass/kimi-k3"},
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: []session.Block{session.TextBlock("hi")}}},
		MaxTokens: 64,
		SessionID: ulid.Make(),
	}
}

func collect(t *testing.T, c *Client, req provider.Request) []provider.Part {
	t.Helper()
	var parts []provider.Part
	err := c.Complete(context.Background(), req, func(p provider.Part) error {
		parts = append(parts, p)
		return nil
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return parts
}

func withoutThinking(parts []provider.Part) []provider.Part {
	var out []provider.Part
	for _, p := range parts {
		if p.Type != provider.PartThinkingDelta {
			out = append(out, p)
		}
	}
	return out
}

func TestCompleteKimiToolCallStream(t *testing.T) {
	c, last := serve(t, 200, "text/event-stream", fixture(t, "kimi-tool-stream.sse"))
	parts := collect(t, c, simpleRequest())

	if last.URL.Path != "/v1/chat/completions" || last.Method != http.MethodPost {
		t.Fatalf("request went to %s %s", last.Method, last.URL.Path)
	}
	if got := last.Header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := last.Header.Get("X-Test"); got != "1" {
		t.Fatalf("X-Test = %q", got)
	}
	if got := last.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}

	thinking := 0
	for _, p := range parts {
		if p.Type == provider.PartThinkingDelta {
			thinking++
		}
	}
	if thinking == 0 {
		t.Fatal("expected thinking deltas from reasoning_details")
	}
	// Every thinking delta precedes the tool call.
	seenTool := false
	for _, p := range parts {
		if p.Type == provider.PartToolUseStart {
			seenTool = true
		}
		if p.Type == provider.PartThinkingDelta && seenTool {
			t.Fatal("thinking delta after tool_use_start")
		}
	}

	rest := withoutThinking(parts)
	if len(rest) < 5 {
		t.Fatalf("too few non-thinking parts: %+v", rest)
	}
	start := rest[0]
	if start.Type != provider.PartToolUseStart || start.ID != "read_file_0_82619100" || start.Name != "read_file" {
		t.Fatalf("first non-thinking part = %+v", start)
	}
	var args strings.Builder
	i := 1
	for ; i < len(rest) && rest[i].Type == provider.PartToolUseDelta; i++ {
		if rest[i].ID != start.ID {
			t.Fatalf("delta for wrong id %q", rest[i].ID)
		}
		args.WriteString(rest[i].Text)
	}
	if args.String() != `{"path": "go.mod"}` {
		t.Fatalf("arguments = %q", args.String())
	}
	if rest[i].Type != provider.PartToolUseEnd || rest[i].ID != start.ID {
		t.Fatalf("expected tool_use_end, got %+v", rest[i])
	}
	i++
	if rest[i].Type != provider.PartUsage || rest[i].Usage.Output != 126 || rest[i].Usage.Input != 178 {
		t.Fatalf("expected usage 178/126, got %+v", rest[i])
	}
	i++
	stop := rest[i]
	if stop.Type != provider.PartStop || stop.StopReason != session.StopToolUse || stop.StopReasonRaw != "tool_calls" {
		t.Fatalf("expected stop tool_use, got %+v", stop)
	}
	if i != len(rest)-1 {
		t.Fatalf("stop was not the last part: %+v", rest[i+1:])
	}
}

func TestCompleteDeepseekTextStream(t *testing.T) {
	c, _ := serve(t, 200, "text/event-stream", fixture(t, "deepseek-text-stream.sse"))
	parts := collect(t, c, simpleRequest())

	var text strings.Builder
	firstText, lastThinking := -1, -1
	stops := 0
	for i, p := range parts {
		switch p.Type {
		case provider.PartTextDelta:
			text.WriteString(p.Text)
			if firstText < 0 {
				firstText = i
			}
		case provider.PartThinkingDelta:
			lastThinking = i
		case provider.PartStop:
			stops++
			if i != len(parts)-1 {
				t.Fatalf("stop at %d of %d", i, len(parts))
			}
			if p.StopReason != session.StopEndTurn || p.StopReasonRaw != "stop" {
				t.Fatalf("stop = %+v", p)
			}
		case provider.PartUsage:
			if p.Usage.Output != 26 || p.Usage.Input != 9 {
				t.Fatalf("usage = %+v", p.Usage)
			}
		}
	}
	if text.String() != "ok" {
		t.Fatalf("text = %q", text.String())
	}
	if lastThinking < 0 || firstText < 0 || lastThinking > firstText {
		t.Fatalf("thinking must precede text: lastThinking=%d firstText=%d", lastThinking, firstText)
	}
	if stops != 1 {
		t.Fatalf("expected exactly one stop, got %d", stops)
	}
}

func TestCompleteEmptyContent500(t *testing.T) {
	// The fixture is a curl recording: line one is the body, the last line is the status.
	raw := string(fixture(t, "clinepass-empty-500.json"))
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	body := lines[0]
	if lines[len(lines)-1] != "500" {
		t.Fatalf("fixture status line = %q", lines[len(lines)-1])
	}
	c, _ := serve(t, 500, "application/json", []byte(body))
	err := c.Complete(context.Background(), simpleRequest(), func(provider.Part) error { return nil })
	var perr *provider.Error
	if !errors.As(err, &perr) {
		t.Fatalf("want *provider.Error, got %T %v", err, err)
	}
	if perr.Class != session.ErrProvider || perr.Status != 500 || perr.Message != "empty response content" {
		t.Fatalf("got %+v", perr)
	}
	if string(perr.Body) != body {
		t.Fatalf("body not kept verbatim: %q", perr.Body)
	}
}

func TestCompleteOpenAIErrorObject(t *testing.T) {
	c, _ := serve(t, 400, "application/json", []byte(`{"error":{"message":"model not found","type":"invalid_request_error"}}`))
	err := c.Complete(context.Background(), simpleRequest(), func(provider.Part) error { return nil })
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Message != "model not found" || perr.Status != 400 {
		t.Fatalf("got %v", err)
	}
}

func TestCompleteNonStreamingBody(t *testing.T) {
	// The recorded clinepass body wraps the completion in {"data": …}. The plain
	// completion inside it is what a standards-following server would return
	// when it ignores stream: true; the envelope itself is the dialect the
	// clinepass plugin handles in the next plan.
	raw := fixture(t, "clinepass-nonstream.json")
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Data) == 0 {
		t.Fatalf("fixture has no data envelope: %v", err)
	}

	c, _ := serve(t, 200, "application/json", env.Data)
	parts := collect(t, c, simpleRequest())
	var text strings.Builder
	thinking := 0
	for _, p := range parts {
		switch p.Type {
		case provider.PartTextDelta:
			text.WriteString(p.Text)
		case provider.PartThinkingDelta:
			thinking++
		case provider.PartUsage:
			if p.Usage.Output != 20 || p.Usage.Input != 9 {
				t.Fatalf("usage = %+v", p.Usage)
			}
		}
	}
	if text.String() != "ok" || thinking != 1 {
		t.Fatalf("text=%q thinking=%d", text.String(), thinking)
	}
	if last := parts[len(parts)-1]; last.Type != provider.PartStop || last.StopReason != session.StopEndTurn {
		t.Fatalf("last = %+v", last)
	}

	c2, _ := serve(t, 200, "application/json", raw)
	err := c2.Complete(context.Background(), simpleRequest(), func(provider.Part) error { return nil })
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Class != session.ErrProvider || perr.Message != "response is neither an event stream nor a chat completion" {
		t.Fatalf("envelope should be refused, got %v", err)
	}
}

func TestCompleteIdleTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	c := New(Options{Name: "x", BaseURL: srv.URL + "/v1", HTTP: httpx.New("test")})
	c.idle = 50 * time.Millisecond

	var got []provider.Part
	err := c.Complete(context.Background(), simpleRequest(), func(p provider.Part) error {
		got = append(got, p)
		return nil
	})
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Class != session.ErrTransport || !strings.Contains(perr.Message, "idle timeout") {
		t.Fatalf("want idle transport error, got %v", err)
	}
	if len(got) != 1 || got[0].Text != "a" {
		t.Fatalf("parts before the timeout = %+v", got)
	}
}

func TestCompleteCancelFromEmit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	c := New(Options{Name: "x", BaseURL: srv.URL + "/v1", HTTP: httpx.New("test")})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := c.Complete(ctx, simpleRequest(), func(p provider.Part) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestCompleteEmitErrorIsReturnedVerbatim(t *testing.T) {
	c, _ := serve(t, 200, "text/event-stream", fixture(t, "deepseek-text-stream.sse"))
	sentinel := errors.New("stop here")
	err := c.Complete(context.Background(), simpleRequest(), func(p provider.Part) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel, got %v", err)
	}
}
```

- [ ] **Step 6: Run the streaming tests to verify they fail**

Run: `go test ./internal/provider/openaichat/ -run TestComplete -v`
Expected: build failure with `undefined: New`, `undefined: Options` and `c.idle undefined`.

- [ ] **Step 7: Write the stream parser and the client**

`internal/provider/openaichat/stream.go`:

```go
package openaichat

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

// chunk is one chat.completion.chunk. Non-streaming completions are folded into
// the same shape by completeJSON so both paths share the part emitter.
type chunk struct {
	Choices []chunkChoice `json:"choices"`
	Usage   *wireUsage    `json:"usage"`
}

type chunkChoice struct {
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chunkDelta struct {
	Content          string            `json:"content"`
	Reasoning        *string           `json:"reasoning"`
	ReasoningDetails []reasoningDetail `json:"reasoning_details"`
	ToolCalls        []chunkToolCall   `json:"tool_calls"`
}

type reasoningDetail struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type chunkToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireUsage struct {
	PromptTokens             int64 `json:"prompt_tokens"`
	CompletionTokens         int64 `json:"completion_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	PromptTokensDetails      *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u *wireUsage) usage() session.Usage {
	out := session.Usage{
		Input:      u.PromptTokens,
		Output:     u.CompletionTokens,
		CacheWrite: u.CacheCreationInputTokens,
	}
	if u.PromptTokensDetails != nil {
		out.CacheRead = u.PromptTokensDetails.CachedTokens
	}
	return out
}

func mapStop(raw string) session.StopReason {
	switch raw {
	case "stop":
		return session.StopEndTurn
	case "tool_calls":
		return session.StopToolUse
	case "length":
		return session.StopMaxTokens
	case "content_filter":
		return session.StopRefused
	}
	return session.StopOther
}

// emitError wraps an error returned by the caller's emit so the client can hand
// it back verbatim instead of classifying it as a transport failure.
type emitError struct{ err error }

func (e emitError) Error() string { return e.err.Error() }
func (e emitError) Unwrap() error { return e.err }

func unwrapEmit(err error) error {
	var ee emitError
	if errors.As(err, &ee) {
		return ee.err
	}
	return err
}

// streamState turns chunks into parts. It tracks open tool calls by index and
// buffers the stop so it is emitted exactly once, last.
type streamState struct {
	emit  func(provider.Part) error
	open  map[int]string // index to tool_use id
	order []int          // indexes in arrival order
	stop  *provider.Part
}

func newStreamState(emit func(provider.Part) error) *streamState {
	return &streamState{emit: emit, open: map[int]string{}}
}

func (s *streamState) send(p provider.Part) error {
	if err := s.emit(p); err != nil {
		return emitError{err}
	}
	return nil
}

func (s *streamState) chunk(c chunk) error {
	for _, ch := range c.Choices {
		if err := s.delta(ch.Delta); err != nil {
			return err
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			if err := s.closeToolCalls(); err != nil {
				return err
			}
			if s.stop == nil {
				raw := *ch.FinishReason
				s.stop = &provider.Part{Type: provider.PartStop, StopReason: mapStop(raw), StopReasonRaw: raw}
			}
		}
	}
	if c.Usage != nil {
		if err := s.send(provider.Part{Type: provider.PartUsage, Usage: c.Usage.usage()}); err != nil {
			return err
		}
	}
	return nil
}

func (s *streamState) delta(d chunkDelta) error {
	switch {
	case len(d.ReasoningDetails) > 0:
		for _, r := range d.ReasoningDetails {
			if r.Text == "" {
				continue
			}
			if err := s.send(provider.Part{Type: provider.PartThinkingDelta, Text: r.Text}); err != nil {
				return err
			}
		}
	case d.Reasoning != nil && *d.Reasoning != "":
		if err := s.send(provider.Part{Type: provider.PartThinkingDelta, Text: *d.Reasoning}); err != nil {
			return err
		}
	}
	if d.Content != "" {
		if err := s.send(provider.Part{Type: provider.PartTextDelta, Text: d.Content}); err != nil {
			return err
		}
	}
	for _, tc := range d.ToolCalls {
		id, known := s.open[tc.Index]
		if !known {
			if tc.ID == "" || tc.Function.Name == "" {
				continue // a fragment for a call that never started; nothing to attach it to
			}
			id = tc.ID
			s.open[tc.Index] = id
			s.order = append(s.order, tc.Index)
			if err := s.send(provider.Part{Type: provider.PartToolUseStart, ID: id, Name: tc.Function.Name}); err != nil {
				return err
			}
		}
		if tc.Function.Arguments != "" {
			if err := s.send(provider.Part{Type: provider.PartToolUseDelta, ID: id, Text: tc.Function.Arguments}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *streamState) closeToolCalls() error {
	for _, idx := range s.order {
		if err := s.send(provider.Part{Type: provider.PartToolUseEnd, ID: s.open[idx]}); err != nil {
			return err
		}
	}
	s.open = map[int]string{}
	s.order = nil
	return nil
}

// finish emits the buffered stop. A stream that never carried a finish_reason
// stops as other with an empty raw value.
func (s *streamState) finish() error {
	if err := s.closeToolCalls(); err != nil {
		return err
	}
	if s.stop == nil {
		s.stop = &provider.Part{Type: provider.PartStop, StopReason: session.StopOther}
	}
	return s.send(*s.stop)
}

// readSSE feeds every data: line to s until [DONE] or EOF, then finishes.
func readSSE(r io.Reader, s *streamState) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var c chunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			return &provider.Error{Class: session.ErrProvider, Message: "malformed stream chunk: " + err.Error(), Body: []byte(data), Attempts: 1}
		}
		if err := s.chunk(c); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return s.finish()
}
```

`internal/provider/openaichat/client.go`:

```go
package openaichat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/provider/httpx"
	"github.com/guygrigsby/rudy/internal/session"
)

const (
	defaultIdle = 120 * time.Second
	maxErrorBody = 1 << 20
	maxJSONBody  = 8 << 20
)

type Options struct {
	Name    string
	BaseURL string            // ends with /v1
	Token   string            // "" sends no Authorization
	Headers map[string]string // extra, verbatim
	HTTP    *httpx.Client
}

type Client struct {
	opts Options
	idle time.Duration
}

func New(o Options) *Client {
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	return &Client{opts: o, idle: defaultIdle}
}

func (c *Client) Name() string { return c.opts.Name }

func (c *Client) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.opts.BaseURL+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "text/event-stream, application/json")
	if c.opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.opts.Token)
	}
	for k, v := range c.opts.Headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

func (c *Client) Complete(ctx context.Context, req provider.Request, emit func(provider.Part) error) error {
	body, err := buildRequest(req)
	if err != nil {
		return err
	}
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	hreq, err := c.newRequest(ctx, http.MethodPost, "/chat/completions", body)
	if err != nil {
		return err
	}
	resp, err := c.opts.HTTP.Do(ctx, hreq, req.SessionID)
	if err != nil {
		return transportError(parent, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError(resp)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return unwrapEmit(c.completeJSON(resp, emit))
	}

	idle := newIdleReader(resp.Body, c.idle, cancel)
	defer idle.stop()
	err = readSSE(idle, newStreamState(emit))
	switch {
	case err == nil:
		return nil
	case idle.fired():
		return &provider.Error{Class: session.ErrTransport, Message: fmt.Sprintf("idle timeout after %s", c.idle), Attempts: httpx.AttemptsOf(resp)}
	case parent.Err() != nil:
		return parent.Err()
	}
	var ee emitError
	if errors.As(err, &ee) {
		return ee.err
	}
	var perr *provider.Error
	if errors.As(err, &perr) {
		return err
	}
	return &provider.Error{Class: session.ErrTransport, Message: err.Error(), Attempts: httpx.AttemptsOf(resp)}
}

// completion is a non-streaming chat.completion. Only the first choice is used.
type completion struct {
	Choices []struct {
		Message struct {
			Content          string            `json:"content"`
			Reasoning        *string           `json:"reasoning"`
			ReasoningDetails []reasoningDetail `json:"reasoning_details"`
			ToolCalls        []chunkToolCall   `json:"tool_calls"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
}

// completeJSON handles a server that ignored stream: true. The body must be a
// plain chat.completion; the clinepass {"data": …} envelope is refused here and
// unwrapped by the clinepass provider plugin in the next plan.
func (c *Client) completeJSON(resp *http.Response, emit func(provider.Part) error) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBody))
	if err != nil {
		return &provider.Error{Class: session.ErrTransport, Message: err.Error(), Attempts: httpx.AttemptsOf(resp)}
	}
	var comp completion
	if err := json.Unmarshal(body, &comp); err != nil || len(comp.Choices) == 0 {
		return &provider.Error{
			Class:    session.ErrProvider,
			Status:   resp.StatusCode,
			Message:  "response is neither an event stream nor a chat completion",
			Body:     body,
			Attempts: httpx.AttemptsOf(resp),
		}
	}
	ch := comp.Choices[0]
	d := chunkDelta{
		Content:          ch.Message.Content,
		Reasoning:        ch.Message.Reasoning,
		ReasoningDetails: ch.Message.ReasoningDetails,
	}
	for i, tc := range ch.Message.ToolCalls {
		tc.Index = i
		d.ToolCalls = append(d.ToolCalls, tc)
	}
	s := newStreamState(emit)
	if err := s.chunk(chunk{Choices: []chunkChoice{{Delta: d, FinishReason: ch.FinishReason}}, Usage: comp.Usage}); err != nil {
		return err
	}
	return s.finish()
}

// transportError classifies a failure from httpx.Do. When httpx gave up after retries the
// attempt count comes from *httpx.Error; a single failed attempt reports 1.
func transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	attempts := 1
	var he *httpx.Error
	if errors.As(err, &he) {
		attempts = he.Attempts
	}
	return &provider.Error{Class: session.ErrTransport, Message: err.Error(), Attempts: attempts}
}

// statusError reads a non-2xx body and extracts the message from either the
// OpenAI error object {"error":{"message":…}} or the aperture envelope
// {"error":"…","success":false}. The body is kept verbatim. Attempts is how many
// times httpx sent the request before this response came back.
func statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	msg := errorMessage(body)
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return &provider.Error{Class: session.ErrProvider, Status: resp.StatusCode, Message: msg, Body: body, Attempts: httpx.AttemptsOf(resp)}
}

func errorMessage(body []byte) string {
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&env); err != nil || len(env.Error) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(env.Error, &s) == nil {
		return s
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(env.Error, &obj) == nil {
		return obj.Message
	}
	return ""
}

// idleReader cancels the request when no bytes arrive for d. The cancel makes
// the blocked Read return, and fired tells the caller why.
type idleReader struct {
	r     io.Reader
	d     time.Duration
	timer *time.Timer
	fire  atomic.Bool
}

func newIdleReader(r io.Reader, d time.Duration, cancel context.CancelFunc) *idleReader {
	ir := &idleReader{r: r, d: d}
	ir.timer = time.AfterFunc(d, func() {
		ir.fire.Store(true)
		cancel()
	})
	return ir
}

func (ir *idleReader) Read(p []byte) (int, error) {
	n, err := ir.r.Read(p)
	ir.timer.Reset(ir.d)
	return n, err
}

func (ir *idleReader) stop()       { ir.timer.Stop() }
func (ir *idleReader) fired() bool { return ir.fire.Load() }
```

- [ ] **Step 8: Run the streaming tests to verify they pass**

Run: `go test -race ./internal/provider/openaichat/ -run TestComplete -v`
Expected: PASS for all eight. If `TestCompleteIdleTimeout` flakes, the server sleep is 2s and the idle is 50ms; the two are far enough apart that a failure is a real bug, not timing.

- [ ] **Step 9: Write the failing model-listing test**

`internal/provider/openaichat/models_test.go`:

```go
package openaichat

import (
	"context"
	"net/http"
	"testing"
)

func TestListModelsFromFixture(t *testing.T) {
	c, last := serve(t, 200, "application/json", fixture(t, "models.json"))
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if last.URL.Path != "/v1/models" || last.Method != http.MethodGet {
		t.Fatalf("request went to %s %s", last.Method, last.URL.Path)
	}
	if len(models) != 5 {
		t.Fatalf("want 5 models, got %d", len(models))
	}
	wantOrder := []string{
		"anthropic/claude-fable-5",
		"cline-pass/deepseek-v4-flash",
		"cline-pass/kimi-k3",
		"deepseek-v4-pro",
		"gpt-5.6-sol",
	}
	for i, id := range wantOrder {
		if models[i].Ref.Model != id || models[i].Ref.Provider != "aperture" {
			t.Fatalf("model %d = %+v, want %s", i, models[i].Ref, id)
		}
	}

	ds := models[1]
	if ds.DisplayName != "DeepSeek V4 Flash" {
		t.Fatalf("display name = %q", ds.DisplayName)
	}
	if ds.ContextWindow != 1000000 || ds.MaxOutput != 384000 {
		t.Fatalf("window/max = %d/%d", ds.ContextWindow, ds.MaxOutput)
	}
	if ds.Pricing.Input != "0.00000014" || ds.Pricing.Output != "0.00000028" || ds.Pricing.CacheRead != "0.00000000" || ds.Pricing.CacheWrite != "" {
		t.Fatalf("pricing = %+v", ds.Pricing)
	}
	if !ds.Capabilities.Tools || !ds.Capabilities.Reasoning || ds.Capabilities.Vision {
		t.Fatalf("capabilities = %+v", ds.Capabilities)
	}

	gpt := models[4]
	if gpt.Pricing.CacheWrite != "0.00000500" {
		t.Fatalf("gpt cache write = %q", gpt.Pricing.CacheWrite)
	}
	if gpt.Capabilities.Reasoning {
		t.Fatalf("gpt-5.6-sol should not be flagged reasoning by the pass-1 heuristic")
	}
}

func TestListModelsFallsBackToIDForDisplayName(t *testing.T) {
	c, _ := serve(t, 200, "application/json", []byte(`{"object":"list","data":[{"id":"local-model","object":"model"}]}`))
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := models[0]
	if m.DisplayName != "local-model" || m.ContextWindow != 0 || m.Pricing.Input != "" || !m.Capabilities.Tools {
		t.Fatalf("got %+v", m)
	}
}

func TestListModelsNumericPricingKeptVerbatim(t *testing.T) {
	c, _ := serve(t, 200, "application/json", []byte(`{"data":[{"id":"m","pricing":{"input":0.000001,"output":"0.000002"}}]}`))
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if models[0].Pricing.Input != "0.000001" || models[0].Pricing.Output != "0.000002" {
		t.Fatalf("pricing = %+v", models[0].Pricing)
	}
}

func TestListModelsReasoningFromSupportedParameters(t *testing.T) {
	c, _ := serve(t, 200, "application/json", []byte(`{"data":[{"id":"x","supported_parameters":["reasoning"],"architecture":{"input_modalities":["text","image"]}}]}`))
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !models[0].Capabilities.Reasoning || !models[0].Capabilities.Vision {
		t.Fatalf("capabilities = %+v", models[0].Capabilities)
	}
}

func TestListModelsErrorStatus(t *testing.T) {
	c, _ := serve(t, 503, "application/json", []byte(`{"error":"warming up"}`))
	_, err := c.ListModels(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
}
```

- [ ] **Step 10: Run it to verify it fails**

Run: `go test ./internal/provider/openaichat/ -run TestListModels -v`
Expected: build failure with `c.ListModels undefined`.

- [ ] **Step 11: Write the model listing**

`internal/provider/openaichat/models.go`:

```go
package openaichat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

const maxModelsBody = 32 << 20

type modelsResponse struct {
	Data []wireModel `json:"data"`
}

// wireModel is the /v1/models entry with the extension fields aperture adds:
// display_name, context_window_tokens, max_output_tokens and pricing. Plain
// OpenAI servers send only id and owned_by, which leaves the rest zero.
type wireModel struct {
	ID                  string                     `json:"id"`
	DisplayName         string                     `json:"display_name"`
	ContextWindowTokens int64                      `json:"context_window_tokens"`
	MaxOutputTokens     int64                      `json:"max_output_tokens"`
	Pricing             map[string]json.RawMessage `json:"pricing"`
	SupportedParameters []string                   `json:"supported_parameters"`
	Architecture        struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
}

func (c *Client) ListModels(ctx context.Context) ([]provider.Model, error) {
	hreq, err := c.newRequest(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.opts.HTTP.Do(ctx, hreq, ulid.ULID{})
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, statusError(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBody))
	if err != nil {
		return nil, &provider.Error{Class: session.ErrTransport, Message: err.Error(), Attempts: httpx.AttemptsOf(resp)}
	}
	var mr modelsResponse
	if err := json.Unmarshal(body, &mr); err != nil {
		return nil, &provider.Error{Class: session.ErrProvider, Status: resp.StatusCode, Message: "malformed models list: " + err.Error(), Body: body, Attempts: httpx.AttemptsOf(resp)}
	}
	out := make([]provider.Model, 0, len(mr.Data))
	for _, m := range mr.Data {
		out = append(out, c.toModel(m))
	}
	slices.SortFunc(out, func(a, b provider.Model) int {
		return strings.Compare(a.Ref.Model, b.Ref.Model)
	})
	return out, nil
}

func (c *Client) toModel(m wireModel) provider.Model {
	name := m.DisplayName
	if name == "" {
		name = m.ID
	}
	return provider.Model{
		Ref:           session.ModelRef{Provider: c.opts.Name, Model: m.ID},
		DisplayName:   name,
		ContextWindow: m.ContextWindowTokens,
		MaxOutput:     m.MaxOutputTokens,
		Pricing: provider.Pricing{
			Input:      priceString(m.Pricing["input"]),
			Output:     priceString(m.Pricing["output"]),
			CacheRead:  priceString(m.Pricing["input_cache_read"]),
			CacheWrite: priceString(m.Pricing["input_cache_write"]),
		},
		Capabilities: provider.Capabilities{
			Tools:     true,
			Reasoning: reasoningHeuristic(m),
			Vision:    slices.Contains(m.Architecture.InputModalities, "image"),
		},
	}
}

// priceString keeps a price verbatim whether the provider sent it as a JSON
// string or a bare number. Never converted through float.
func priceString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// reasoningHeuristic is pass 1: /v1/models carries no capability flags on most
// providers, so the id and supported_parameters stand in. Listed as open in the
// design spec.
func reasoningHeuristic(m wireModel) bool {
	id := strings.ToLower(m.ID)
	for _, w := range []string{"reason", "deepseek", "kimi"} {
		if strings.Contains(id, w) {
			return true
		}
	}
	return slices.Contains(m.SupportedParameters, "reasoning")
}
```

- [ ] **Step 12: Run the whole package**

Run: `go test -race ./internal/provider/openaichat/ -v`
Expected: PASS, every test.

- [ ] **Step 13: Lint and commit**

Run: `make check`
Expected: clean.

```bash
git add internal/provider/openaichat go.mod go.sum
git commit -m "provider: openai_chat codec with streaming, tool calls, reasoning and model listing"
bd close <id>
```

---

### Task 10: tool types, gate and matcher

**Files:**
- Create: `internal/tool/tool.go`
- Create: `internal/gate/gate.go`
- Create: `internal/gate/matcher.go`
- Test: `internal/gate/gate_test.go`
- Test: `internal/gate/matcher_test.go`

**Interfaces:**
- Consumes: `session.Mode*`, `session.Decision`, `session.Allow`, `session.Deny`, `session.DecidedBy` constants, `session.Matcher`, `session.Workspace`, `session.Block` (Task 2). `mvdan.cc/sh/v3/syntax` v3.14.1.
- Produces: `tool.Safety`, `tool.Safe`, `tool.Unsafe`, `tool.Call`, `tool.Result`, `tool.Tool` exactly as in the Interfaces section; `gate.Input`, `gate.Verdict`, `gate.New(dangerous []string) *Gate`, `(*Gate).Evaluate(Input) Verdict`, `(*Gate).MatcherFor(tool string, args json.RawMessage) session.Matcher`, `(*Gate).Dangerous(tool string, args json.RawMessage) (bool, string)`. Task 13 calls `Evaluate` before every tool call; Task 15's tools declare their `Safety`.

Decision order in `Evaluate`, first match wins:

1. `Safe` → allow, `ByClass`, reason `safe tool`.
2. mode `off` → allow, `ByMode`, reason `mode off`.
3. an allowance equal to the matcher → allow, `ByAllowance`, reason `allowance <tool> <prefix>`.
4. mode `permissive` and not dangerous → allow, `ByMode`, reason `mode permissive`.
5. mode `permissive` and dangerous → ask, reason `dangerous <entry>`.
6. mode `strict` → ask, reason `mode strict`.
7. any other mode value → ask, reason `mode strict (unknown mode "<value>")`; unknown fails closed.
8. an ask with no asker present → deny, `ByNoAsker`, reason `no asker attached`.

An ask verdict has `Ask: true` and empty `Decision` and `DecidedBy`; the caller records the asker's answer.

Matcher rules, pinned by probing `mvdan.cc/sh/v3/syntax` v3.14.1:

- For any tool other than `bash`, `Prefix` is empty and `Dangerous` is false.
- For `bash`, `Args` is `{"command": string}`. The command is parsed; every `*syntax.CallExpr` with arguments becomes one call whose words are rendered as: literal parts verbatim, single-quoted text verbatim, double-quoted parts concatenated, any expansion printed in its source form. Assignments before a command (`FOO=1 make`) are not words.
- `Prefix` is the first call's first two words joined by a space. `go test ./...` → `go test`; `cd x && rm -rf y` → `cd x`; `echo "a b"` → `echo a b`; `FOO=1 make build` → `make build`; `ls` → `ls`.
- A parse error falls back to the first two whitespace-separated words of the raw command, and the dangerous check then treats the whole raw command as one call.
- `Dangerous` is true when any call's joined words equal a dangerous entry or start with the entry followed by a space. `rm -rf /x` matches `rm -rf`; `rm -rfoo` does not; `sudo apt install x` matches `sudo`; `if [ -f x ]; then rm -rf y; fi` matches through its second call.

- [ ] **Step 0: Track the task**

```bash
bd create --title "Task 10: tool types, gate and matcher" --type task
bd update <id> --claim
go get mvdan.cc/sh/v3@v3.14.1
```

- [ ] **Step 1: Write the tool types**

`internal/tool/tool.go`, types only, exercised by the gate tests below and by every tool plugin in Task 15:

```go
// Package tool defines what a plugin registers as a callable and what the
// loop hands it. The safety class is the one switch that drives both the gate
// and the durable-record rule.
package tool

import (
	"context"
	"encoding/json"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/session"
)

type Safety string

const (
	Safe   Safety = "safe"
	Unsafe Safety = "unsafe"
)

// Call is one invocation. Input is the model's bytes, never re-marshaled.
type Call struct {
	ID        string
	Name      string
	Input     json.RawMessage
	Workspace session.Workspace
	SessionID ulid.ULID
}

// Result is what the model sees. IsError means the tool ran and failed; a
// non-nil error from Invoke means it could not run at all.
type Result struct {
	Content []session.Block
	IsError bool
}

type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Safety      Safety
	Invoke      func(ctx context.Context, call Call) (Result, error)
}
```

- [ ] **Step 2: Write the failing gate tests**

`internal/gate/gate_test.go`:

```go
package gate

import (
	"encoding/json"
	"testing"

	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

func bashArgs(cmd string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return b
}

func TestEvaluateTable(t *testing.T) {
	g := New([]string{"rm -rf", "sudo", "git push --force"})
	cases := []struct {
		name       string
		tool       string
		safety     tool.Safety
		mode       session.Mode
		asker      bool
		allowances []session.Matcher
		args       json.RawMessage
		wantDec    session.Decision
		wantBy     session.DecidedBy
		wantAsk    bool
		wantReason string
	}{
		{name: "safe tool always runs", tool: "read", safety: tool.Safe, mode: session.ModeStrict, args: json.RawMessage(`{}`),
			wantDec: session.Allow, wantBy: session.ByClass, wantReason: "safe tool"},
		{name: "mode off runs unsafe", tool: "bash", safety: tool.Unsafe, mode: session.ModeOff, args: bashArgs("rm -rf /"),
			wantDec: session.Allow, wantBy: session.ByMode, wantReason: "mode off"},
		{name: "strict asks", tool: "bash", safety: tool.Unsafe, mode: session.ModeStrict, asker: true, args: bashArgs("go test ./..."),
			wantAsk: true, wantReason: "mode strict"},
		{name: "strict with no asker denies", tool: "bash", safety: tool.Unsafe, mode: session.ModeStrict, args: bashArgs("go test ./..."),
			wantDec: session.Deny, wantBy: session.ByNoAsker, wantReason: "no asker attached"},
		{name: "permissive runs the ordinary", tool: "bash", safety: tool.Unsafe, mode: session.ModePermissive, args: bashArgs("go test ./..."),
			wantDec: session.Allow, wantBy: session.ByMode, wantReason: "mode permissive"},
		{name: "permissive asks on dangerous", tool: "bash", safety: tool.Unsafe, mode: session.ModePermissive, asker: true, args: bashArgs("rm -rf build"),
			wantAsk: true, wantReason: "dangerous rm -rf"},
		{name: "permissive dangerous no asker denies", tool: "bash", safety: tool.Unsafe, mode: session.ModePermissive, args: bashArgs("sudo make install"),
			wantDec: session.Deny, wantBy: session.ByNoAsker, wantReason: "no asker attached"},
		{name: "allowance beats strict", tool: "bash", safety: tool.Unsafe, mode: session.ModeStrict, args: bashArgs("go test ./internal/..."),
			allowances: []session.Matcher{{Tool: "bash", Prefix: "go test"}},
			wantDec:    session.Allow, wantBy: session.ByAllowance, wantReason: "allowance bash go test"},
		{name: "allowance beats dangerous", tool: "bash", safety: tool.Unsafe, mode: session.ModePermissive, args: bashArgs("rm -rf build"),
			allowances: []session.Matcher{{Tool: "bash", Prefix: "rm -rf"}},
			wantDec:    session.Allow, wantBy: session.ByAllowance, wantReason: "allowance bash rm -rf"},
		{name: "allowance for a non-bash tool", tool: "write", safety: tool.Unsafe, mode: session.ModeStrict, args: json.RawMessage(`{"path":"x"}`),
			allowances: []session.Matcher{{Tool: "write"}},
			wantDec:    session.Allow, wantBy: session.ByAllowance, wantReason: "allowance write "},
		{name: "non-matching allowance still asks", tool: "bash", safety: tool.Unsafe, mode: session.ModeStrict, asker: true, args: bashArgs("go build"),
			allowances: []session.Matcher{{Tool: "bash", Prefix: "go test"}},
			wantAsk:    true, wantReason: "mode strict"},
		{name: "unknown mode fails closed without asker", tool: "bash", safety: tool.Unsafe, mode: session.Mode("yolo"), args: bashArgs("ls"),
			wantDec: session.Deny, wantBy: session.ByNoAsker, wantReason: "no asker attached"},
		{name: "unknown mode asks with asker", tool: "bash", safety: tool.Unsafe, mode: session.Mode("yolo"), asker: true, args: bashArgs("ls"),
			wantAsk: true, wantReason: `mode strict (unknown mode "yolo")`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := g.Evaluate(Input{
				Tool:         tc.tool,
				Safety:       tc.safety,
				Mode:         tc.mode,
				Args:         tc.args,
				Allowances:   tc.allowances,
				AskerPresent: tc.asker,
			})
			if v.Ask != tc.wantAsk || v.Decision != tc.wantDec || v.DecidedBy != tc.wantBy || v.Reason != tc.wantReason {
				t.Fatalf("got %+v", v)
			}
			if v.Matcher.Tool != tc.tool {
				t.Fatalf("matcher tool = %q", v.Matcher.Tool)
			}
		})
	}
}

func TestEvaluateReturnsMatcher(t *testing.T) {
	g := New(nil)
	v := g.Evaluate(Input{Tool: "bash", Safety: tool.Unsafe, Mode: session.ModeStrict, AskerPresent: true, Args: bashArgs("git status --short")})
	if v.Matcher != (session.Matcher{Tool: "bash", Prefix: "git status"}) {
		t.Fatalf("matcher = %+v", v.Matcher)
	}
}
```

`internal/gate/matcher_test.go`:

```go
package gate

import (
	"encoding/json"
	"testing"

	"github.com/guygrigsby/rudy/internal/session"
)

func TestMatcherForBash(t *testing.T) {
	g := New(nil)
	cases := map[string]string{
		"go test ./...":                   "go test",
		"cd x && rm -rf y":                "cd x",
		`echo "a b"`:                      "echo a b",
		"FOO=1 make build":                "make build",
		"ls":                              "ls",
		"echo $HOME/x | wc -l":            "echo $HOME/x",
		"if [ -f x ]; then rm -rf y; fi":  "[ -f",
		`echo "unterminated`:              `echo "unterminated`,
		"":                                "",
	}
	for cmd, want := range cases {
		got := g.MatcherFor("bash", bashArgs(cmd))
		if got != (session.Matcher{Tool: "bash", Prefix: want}) {
			t.Errorf("%q: got %+v, want prefix %q", cmd, got, want)
		}
	}
}

func TestMatcherForOtherToolsAndBadArgs(t *testing.T) {
	g := New(nil)
	if got := g.MatcherFor("write", json.RawMessage(`{"path":"x"}`)); got != (session.Matcher{Tool: "write"}) {
		t.Fatalf("write matcher = %+v", got)
	}
	if got := g.MatcherFor("bash", json.RawMessage(`not json`)); got != (session.Matcher{Tool: "bash"}) {
		t.Fatalf("bad args matcher = %+v", got)
	}
	if got := g.MatcherFor("bash", json.RawMessage(`{"cmd":"ls"}`)); got != (session.Matcher{Tool: "bash"}) {
		t.Fatalf("missing command matcher = %+v", got)
	}
}

func TestDangerous(t *testing.T) {
	g := New([]string{"rm -rf", "sudo", "git push --force"})
	cases := []struct {
		cmd   string
		want  bool
		entry string
	}{
		{"rm -rf /tmp/x", true, "rm -rf"},
		{"rm -rfoo", false, ""},
		{"rm -rf", true, "rm -rf"},
		{"rmdir foo", false, ""},
		{"sudo apt install x", true, "sudo"},
		{"cd x && rm -rf y", true, "rm -rf"},
		{"if [ -f x ]; then rm -rf y; fi", true, "rm -rf"},
		{"git push --force origin main", true, "git push --force"},
		{"git push origin main", false, ""},
		{`rm -rf "unterminated`, true, "rm -rf"},
		{"", false, ""},
	}
	for _, tc := range cases {
		got, entry := g.Dangerous("bash", bashArgs(tc.cmd))
		if got != tc.want || entry != tc.entry {
			t.Errorf("%q: got %v %q, want %v %q", tc.cmd, got, entry, tc.want, tc.entry)
		}
	}
	if got, _ := g.Dangerous("write", json.RawMessage(`{"path":"/etc/passwd"}`)); got {
		t.Fatal("non-bash tools are never dangerous by prefix")
	}
}
```

- [ ] **Step 3: Run them to verify they fail**

Run: `go test ./internal/gate/ -v`
Expected: build failure with `undefined: New`, `undefined: Input`.

- [ ] **Step 4: Write the gate**

`internal/gate/gate.go`:

```go
// Package gate decides whether a tool call runs, asks or is refused. It is a
// pure function of the safety class, the permission mode, the session's
// allowances and whether anyone is attached who can answer.
package gate

import (
	"encoding/json"
	"fmt"

	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

type Input struct {
	Tool         string
	Safety       tool.Safety
	Mode         session.Mode
	Args         json.RawMessage
	Allowances   []session.Matcher
	AskerPresent bool
}

type Verdict struct {
	Decision  session.Decision
	DecidedBy session.DecidedBy
	Ask       bool // true means Decision is not final and the asker must answer
	Matcher   session.Matcher
	Reason    string
}

type Gate struct{ dangerous []string }

func New(dangerous []string) *Gate {
	return &Gate{dangerous: append([]string(nil), dangerous...)}
}

func (g *Gate) Evaluate(in Input) Verdict {
	m := g.MatcherFor(in.Tool, in.Args)
	if in.Safety == tool.Safe {
		return Verdict{Decision: session.Allow, DecidedBy: session.ByClass, Matcher: m, Reason: "safe tool"}
	}
	if in.Mode == session.ModeOff {
		return Verdict{Decision: session.Allow, DecidedBy: session.ByMode, Matcher: m, Reason: "mode off"}
	}
	for _, a := range in.Allowances {
		if a == m {
			return Verdict{Decision: session.Allow, DecidedBy: session.ByAllowance, Matcher: m, Reason: fmt.Sprintf("allowance %s %s", m.Tool, m.Prefix)}
		}
	}
	var askReason string
	switch in.Mode {
	case session.ModePermissive:
		dangerous, entry := g.Dangerous(in.Tool, in.Args)
		if !dangerous {
			return Verdict{Decision: session.Allow, DecidedBy: session.ByMode, Matcher: m, Reason: "mode permissive"}
		}
		askReason = "dangerous " + entry
	case session.ModeStrict:
		askReason = "mode strict"
	default:
		askReason = fmt.Sprintf("mode strict (unknown mode %q)", string(in.Mode))
	}
	if !in.AskerPresent {
		return Verdict{Decision: session.Deny, DecidedBy: session.ByNoAsker, Matcher: m, Reason: "no asker attached"}
	}
	return Verdict{Ask: true, Matcher: m, Reason: askReason}
}
```

`internal/gate/matcher.go`:

```go
package gate

import (
	"encoding/json"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/guygrigsby/rudy/internal/session"
)

// MatcherFor classifies a tool input into the key allowances are matched on.
// Only bash has a prefix: the first simple command's first two words.
func (g *Gate) MatcherFor(tool string, args json.RawMessage) session.Matcher {
	if tool != "bash" {
		return session.Matcher{Tool: tool}
	}
	cmd, ok := bashCommand(args)
	if !ok {
		return session.Matcher{Tool: tool}
	}
	calls, err := shellCalls(cmd)
	if err != nil || len(calls) == 0 {
		return session.Matcher{Tool: tool, Prefix: firstWords(strings.Fields(cmd), 2)}
	}
	return session.Matcher{Tool: tool, Prefix: firstWords(calls[0], 2)}
}

// Dangerous reports whether any simple command in a bash input starts with a
// dangerous entry, and which entry matched.
func (g *Gate) Dangerous(tool string, args json.RawMessage) (bool, string) {
	if tool != "bash" {
		return false, ""
	}
	cmd, ok := bashCommand(args)
	if !ok {
		return false, ""
	}
	calls, err := shellCalls(cmd)
	if err != nil || len(calls) == 0 {
		calls = [][]string{strings.Fields(cmd)}
	}
	for _, call := range calls {
		joined := strings.Join(call, " ")
		for _, d := range g.dangerous {
			if joined == d || strings.HasPrefix(joined, d+" ") {
				return true, d
			}
		}
	}
	return false, ""
}

func bashCommand(args json.RawMessage) (string, bool) {
	var a struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &a); err != nil || a.Command == "" {
		return "", false
	}
	return a.Command, true
}

func firstWords(words []string, n int) string {
	if len(words) > n {
		words = words[:n]
	}
	return strings.Join(words, " ")
}

// shellCalls parses src and returns the words of every simple command in
// source order. Assignments are not words.
func shellCalls(src string) ([][]string, error) {
	f, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		return nil, err
	}
	var calls [][]string
	syntax.Walk(f, func(n syntax.Node) bool {
		if c, ok := n.(*syntax.CallExpr); ok && len(c.Args) > 0 {
			words := make([]string, 0, len(c.Args))
			for _, a := range c.Args {
				words = append(words, wordText(a))
			}
			calls = append(calls, words)
		}
		return true
	})
	return calls, nil
}

func wordText(w *syntax.Word) string {
	var b strings.Builder
	for _, p := range w.Parts {
		b.WriteString(partText(p))
	}
	return b.String()
}

// partText renders a word part: quotes are dropped, expansions keep their
// source form so $HOME stays $HOME.
func partText(p syntax.WordPart) string {
	switch p := p.(type) {
	case *syntax.Lit:
		return p.Value
	case *syntax.SglQuoted:
		return p.Value
	case *syntax.DblQuoted:
		var b strings.Builder
		for _, q := range p.Parts {
			b.WriteString(partText(q))
		}
		return b.String()
	}
	var b strings.Builder
	_ = syntax.NewPrinter().Print(&b, p)
	return b.String()
}
```

- [ ] **Step 5: Run the gate tests to verify they pass**

Run: `go test -race ./internal/gate/ ./internal/tool/ -v`
Expected: PASS for every case. If `TestMatcherForBash` disagrees on `if [ -f x ]; then rm -rf y; fi`, the first call is the test command `[ -f x ]`, whose first two words are `[` and `-f`; that expectation is deliberate.

- [ ] **Step 6: Lint and commit**

Run: `make check`
Expected: clean.

```bash
git add internal/tool internal/gate go.mod go.sum
git commit -m "gate: evaluate, matcher and the dangerous set"
bd close <id>
```

---

### Task 11: plugin registry

**Files:**
- Create: `internal/plugin/plugin.go`
- Create: `internal/plugin/registry.go`
- Test: `internal/plugin/registry_test.go`

**Interfaces:**
- Consumes: `tool.Tool` (Task 10); `provider.Provider` (Task 8); `session.Workspace` (Task 2).
- Produces: `plugin.Plugin`, `plugin.Host`, `plugin.Command`, `plugin.CommandCall`, `plugin.Action` with `plugin.SubmitPrompt`, `plugin.Notice`, `plugin.NoAction`; `plugin.State`, `plugin.Status`, `plugin.ErrDuplicate`; `plugin.NewRegistry(config, notice) *Registry`, `(*Registry).Load(ctx, plugins...)`, `Tools()`, `Tool(name)`, `Commands()`, `Command(name)`, `Providers()`, `Statuses()`. Task 14 builds the server's tool set and command table from the registry; Task 15 and Task 16 implement `Plugin`.

Rules the registry enforces:

- Plugins load in the order given. Each is `loading` while `Init` runs, then `ready` or `failed`.
- A capability is owned by the plugin that registered it first. A second registration under the same name returns `ErrDuplicate` from `Register*`, the registry keeps the first and records the notice `plugin <name>: <kind> <capability> already registered by <owner>`. The plugin may ignore the error and still finish `ready`.
- An `Init` that returns an error or panics marks the plugin `failed` with the reason, rolls back every capability it registered and records the notice `plugin <name>: failed: <reason>`. A half-initialized plugin contributes nothing.
- Accessors return capabilities in registration order.

- [ ] **Step 0: Track the task**

```bash
bd create --title "Task 11: plugin registry" --type task
bd update <id> --claim
```

- [ ] **Step 1: Write the failing registry tests**

`internal/plugin/registry_test.go`:

```go
package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

type fakePlugin struct {
	name string
	init func(ctx context.Context, h Host) error
}

func (f fakePlugin) Name() string                             { return f.name }
func (f fakePlugin) Init(ctx context.Context, h Host) error { return f.init(ctx, h) }

func namedTool(name string) tool.Tool {
	return tool.Tool{Name: name, Description: name, Safety: tool.Safe}
}

type fakeProvider struct{ name string }

func (p fakeProvider) Name() string { return p.name }
func (p fakeProvider) Complete(context.Context, provider.Request, func(provider.Part) error) error {
	return nil
}
func (p fakeProvider) ListModels(context.Context) ([]provider.Model, error) { return nil, nil }

func newTestRegistry(config map[string]map[string]any) (*Registry, *[]string) {
	var notices []string
	r := NewRegistry(config, func(s string) { notices = append(notices, s) })
	return r, &notices
}

func TestLoadRegistersInOrder(t *testing.T) {
	r, notices := newTestRegistry(nil)
	r.Load(context.Background(),
		fakePlugin{"a", func(_ context.Context, h Host) error {
			if err := h.RegisterTool(namedTool("read")); err != nil {
				return err
			}
			return h.RegisterCommand(Command{Name: "init", Run: func(context.Context, CommandCall) (Action, error) { return NoAction{}, nil }})
		}},
		fakePlugin{"b", func(_ context.Context, h Host) error {
			if err := h.RegisterTool(namedTool("bash")); err != nil {
				return err
			}
			return h.RegisterProvider(fakeProvider{"aperture"})
		}},
	)
	tools := r.Tools()
	if len(tools) != 2 || tools[0].Name != "read" || tools[1].Name != "bash" {
		t.Fatalf("tools = %+v", tools)
	}
	if _, ok := r.Tool("bash"); !ok {
		t.Fatal("bash missing")
	}
	if _, ok := r.Tool("nope"); ok {
		t.Fatal("nope should be absent")
	}
	if cmds := r.Commands(); len(cmds) != 1 || cmds[0].Name != "init" {
		t.Fatalf("commands = %+v", cmds)
	}
	if _, ok := r.Command("init"); !ok {
		t.Fatal("init missing")
	}
	if provs := r.Providers(); len(provs) != 1 || provs[0].Name() != "aperture" {
		t.Fatalf("providers = %+v", provs)
	}
	st := r.Statuses()
	if len(st) != 2 || st[0].Name != "a" || st[0].State != StateReady || st[1].Name != "b" || st[1].State != StateReady {
		t.Fatalf("statuses = %+v", st)
	}
	if len(*notices) != 0 {
		t.Fatalf("unexpected notices %v", *notices)
	}
}

func TestDuplicateToolKeepsFirstAndNotices(t *testing.T) {
	r, notices := newTestRegistry(nil)
	var second error
	r.Load(context.Background(),
		fakePlugin{"first", func(_ context.Context, h Host) error { return h.RegisterTool(namedTool("read")) }},
		fakePlugin{"second", func(_ context.Context, h Host) error {
			second = h.RegisterTool(tool.Tool{Name: "read", Description: "impostor", Safety: tool.Unsafe})
			return nil // the plugin chooses to carry on
		}},
	)
	if !errors.Is(second, ErrDuplicate) {
		t.Fatalf("second registration error = %v", second)
	}
	got, _ := r.Tool("read")
	if got.Description != "read" {
		t.Fatalf("first registration was replaced: %+v", got)
	}
	if len(r.Tools()) != 1 {
		t.Fatalf("tools = %+v", r.Tools())
	}
	if len(*notices) != 1 || (*notices)[0] != "plugin second: tool read already registered by first" {
		t.Fatalf("notices = %v", *notices)
	}
	st := r.Statuses()
	if st[1].State != StateReady {
		t.Fatalf("second should still be ready: %+v", st[1])
	}
}

func TestDuplicateCommandAndProvider(t *testing.T) {
	r, notices := newTestRegistry(nil)
	r.Load(context.Background(),
		fakePlugin{"one", func(_ context.Context, h Host) error {
			_ = h.RegisterCommand(Command{Name: "x", Run: func(context.Context, CommandCall) (Action, error) { return NoAction{}, nil }})
			return h.RegisterProvider(fakeProvider{"p"})
		}},
		fakePlugin{"two", func(_ context.Context, h Host) error {
			_ = h.RegisterCommand(Command{Name: "x", Run: func(context.Context, CommandCall) (Action, error) { return NoAction{}, nil }})
			_ = h.RegisterProvider(fakeProvider{"p"})
			return nil
		}},
	)
	want := []string{
		"plugin two: command x already registered by one",
		"plugin two: provider p already registered by one",
	}
	if strings.Join(*notices, "|") != strings.Join(want, "|") {
		t.Fatalf("notices = %v", *notices)
	}
	if len(r.Commands()) != 1 || len(r.Providers()) != 1 {
		t.Fatalf("commands=%d providers=%d", len(r.Commands()), len(r.Providers()))
	}
}

func TestInitErrorFailsPluginAndRollsBack(t *testing.T) {
	r, notices := newTestRegistry(nil)
	r.Load(context.Background(),
		fakePlugin{"broken", func(_ context.Context, h Host) error {
			_ = h.RegisterTool(namedTool("read"))
			_ = h.RegisterCommand(Command{Name: "c", Run: func(context.Context, CommandCall) (Action, error) { return NoAction{}, nil }})
			_ = h.RegisterProvider(fakeProvider{"p"})
			return errors.New("config missing")
		}},
		fakePlugin{"fine", func(_ context.Context, h Host) error { return h.RegisterTool(namedTool("bash")) }},
	)
	if _, ok := r.Tool("read"); ok {
		t.Fatal("failed plugin's tool must be rolled back")
	}
	if _, ok := r.Command("c"); ok {
		t.Fatal("failed plugin's command must be rolled back")
	}
	if len(r.Providers()) != 0 {
		t.Fatal("failed plugin's provider must be rolled back")
	}
	if _, ok := r.Tool("bash"); !ok {
		t.Fatal("later plugin still loads")
	}
	st := r.Statuses()
	if st[0].State != StateFailed || st[0].Reason != "config missing" {
		t.Fatalf("status = %+v", st[0])
	}
	if len(*notices) != 1 || (*notices)[0] != "plugin broken: failed: config missing" {
		t.Fatalf("notices = %v", *notices)
	}
}

func TestInitPanicFailsPlugin(t *testing.T) {
	r, _ := newTestRegistry(nil)
	r.Load(context.Background(),
		fakePlugin{"boom", func(_ context.Context, h Host) error {
			_ = h.RegisterTool(namedTool("read"))
			panic("nil map write")
		}},
	)
	st := r.Statuses()
	if st[0].State != StateFailed || !strings.Contains(st[0].Reason, "panic: nil map write") {
		t.Fatalf("status = %+v", st[0])
	}
	if len(r.Tools()) != 0 {
		t.Fatal("panicking plugin's tool must be rolled back")
	}
}

func TestHostConfigAndNotice(t *testing.T) {
	r, notices := newTestRegistry(map[string]map[string]any{"cfg": {"threshold": 3}})
	var seen map[string]any
	r.Load(context.Background(),
		fakePlugin{"cfg", func(_ context.Context, h Host) error {
			seen = h.Config()
			h.Notice("hello")
			return nil
		}},
		fakePlugin{"nocfg", func(_ context.Context, h Host) error {
			if h.Config() == nil {
				return errors.New("Config must return an empty map, not nil")
			}
			return nil
		}},
	)
	if seen["threshold"] != 3 {
		t.Fatalf("config = %v", seen)
	}
	if len(*notices) != 1 || (*notices)[0] != "plugin cfg: hello" {
		t.Fatalf("notices = %v", *notices)
	}
	if st := r.Statuses(); st[1].State != StateReady {
		t.Fatalf("nocfg = %+v", st[1])
	}
}

func TestEmptyNamesRefused(t *testing.T) {
	r, _ := newTestRegistry(nil)
	var errs []error
	r.Load(context.Background(),
		fakePlugin{"p", func(_ context.Context, h Host) error {
			errs = append(errs, h.RegisterTool(tool.Tool{}))
			errs = append(errs, h.RegisterCommand(Command{}))
			return nil
		}},
	)
	for _, err := range errs {
		if err == nil {
			t.Fatal("empty name must be refused")
		}
	}
	if len(r.Tools()) != 0 || len(r.Commands()) != 0 {
		t.Fatal("nothing should be registered")
	}
}

var _ = session.Workspace{} // CommandCall carries one; keep the import honest
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/plugin/ -v`
Expected: build failure with `undefined: Host`, `undefined: NewRegistry`.

- [ ] **Step 3: Write the plugin types and the registry**

`internal/plugin/plugin.go`:

```go
// Package plugin is how every capability arrives: tools, slash commands and
// providers, whether the plugin is linked into the binary or spawned. Built-ins
// use exactly this interface; nothing in the kernel has a private path.
package plugin

import (
	"context"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// Action is what a slash command asks the server to do.
type Action interface{ isAction() }

// SubmitPrompt has the server append a user_message with Text and start a turn.
type SubmitPrompt struct{ Text string }

// Notice shows text to the client without touching the session log.
type Notice struct{ Text string }

type NoAction struct{}

func (SubmitPrompt) isAction() {}
func (Notice) isAction()       {}
func (NoAction) isAction()     {}

type CommandCall struct {
	SessionID ulid.ULID
	Workspace session.Workspace
	Args      string
}

type Command struct {
	Name        string // without the slash
	Description string
	Run         func(ctx context.Context, call CommandCall) (Action, error)
}

type Host interface {
	RegisterTool(t tool.Tool) error
	RegisterCommand(c Command) error
	RegisterProvider(p provider.Provider) error
	Config() map[string]any // the plugin's [plugins.<name>] table, never nil
	Notice(text string)
}

type Plugin interface {
	Name() string
	Init(ctx context.Context, h Host) error
}

type State string

const (
	StateLoading State = "loading"
	StateReady   State = "ready"
	StateFailed  State = "failed"
	StateStopped State = "stopped"
)

type Status struct {
	Name   string
	State  State
	Reason string // failed only
}
```

`internal/plugin/registry.go`:

```go
package plugin

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/tool"
)

var ErrDuplicate = errors.New("plugin: already registered")

type owned[T any] struct {
	owner string
	value T
}

type Registry struct {
	mu        sync.RWMutex
	config    map[string]map[string]any
	notice    func(string)
	tools     map[string]owned[tool.Tool]
	toolOrder []string
	commands  map[string]owned[Command]
	cmdOrder  []string
	providers map[string]owned[provider.Provider]
	provOrder []string
	statuses  []Status
}

func NewRegistry(config map[string]map[string]any, notice func(string)) *Registry {
	if notice == nil {
		notice = func(string) {}
	}
	return &Registry{
		config:    config,
		notice:    notice,
		tools:     map[string]owned[tool.Tool]{},
		commands:  map[string]owned[Command]{},
		providers: map[string]owned[provider.Provider]{},
	}
}

// Load initializes each plugin in order. A failure never aborts the load: the
// plugin is marked failed, its registrations are rolled back and a notice is
// recorded.
func (r *Registry) Load(ctx context.Context, plugins ...Plugin) {
	for _, p := range plugins {
		name := p.Name()
		idx := r.setStatus(Status{Name: name, State: StateLoading})
		h := &host{r: r, name: name}
		if err := safeInit(ctx, p, h); err != nil {
			r.rollback(name)
			r.statusAt(idx, Status{Name: name, State: StateFailed, Reason: err.Error()})
			r.notice(fmt.Sprintf("plugin %s: failed: %s", name, err.Error()))
			continue
		}
		r.statusAt(idx, Status{Name: name, State: StateReady})
	}
}

func safeInit(ctx context.Context, p Plugin, h Host) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic: %v", rec)
		}
	}()
	return p.Init(ctx, h)
}

func (r *Registry) setStatus(s Status) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses = append(r.statuses, s)
	return len(r.statuses) - 1
}

func (r *Registry) statusAt(i int, s Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses[i] = s
}

func (r *Registry) rollback(owner string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.toolOrder = dropOwned(r.toolOrder, r.tools, owner)
	r.cmdOrder = dropOwned(r.cmdOrder, r.commands, owner)
	r.provOrder = dropOwned(r.provOrder, r.providers, owner)
}

func dropOwned[T any](order []string, m map[string]owned[T], owner string) []string {
	kept := order[:0]
	for _, name := range order {
		if m[name].owner == owner {
			delete(m, name)
			continue
		}
		kept = append(kept, name)
	}
	return kept
}

func (r *Registry) Tools() []tool.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]tool.Tool, 0, len(r.toolOrder))
	for _, n := range r.toolOrder {
		out = append(out, r.tools[n].value)
	}
	return out
}

func (r *Registry) Tool(name string) (tool.Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	o, ok := r.tools[name]
	return o.value, ok
}

func (r *Registry) Commands() []Command {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Command, 0, len(r.cmdOrder))
	for _, n := range r.cmdOrder {
		out = append(out, r.commands[n].value)
	}
	return out
}

func (r *Registry) Command(name string) (Command, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	o, ok := r.commands[name]
	return o.value, ok
}

func (r *Registry) Providers() []provider.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]provider.Provider, 0, len(r.provOrder))
	for _, n := range r.provOrder {
		out = append(out, r.providers[n].value)
	}
	return out
}

func (r *Registry) Statuses() []Status {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Status(nil), r.statuses...)
}

// host is the per-plugin view of the registry handed to Init.
type host struct {
	r    *Registry
	name string
}

func (h *host) RegisterTool(t tool.Tool) error {
	return register(h, "tool", t.Name, t, h.r.tools, &h.r.toolOrder)
}

func (h *host) RegisterCommand(c Command) error {
	return register(h, "command", c.Name, c, h.r.commands, &h.r.cmdOrder)
}

func (h *host) RegisterProvider(p provider.Provider) error {
	if p == nil {
		return errors.New("plugin: nil provider")
	}
	return register(h, "provider", p.Name(), p, h.r.providers, &h.r.provOrder)
}

func register[T any](h *host, kind, name string, value T, m map[string]owned[T], order *[]string) error {
	if name == "" {
		return fmt.Errorf("plugin %s: %s name empty", h.name, kind)
	}
	h.r.mu.Lock()
	defer h.r.mu.Unlock()
	if existing, ok := m[name]; ok {
		h.r.notice(fmt.Sprintf("plugin %s: %s %s already registered by %s", h.name, kind, name, existing.owner))
		return fmt.Errorf("%w: %s %q owned by %s", ErrDuplicate, kind, name, existing.owner)
	}
	m[name] = owned[T]{owner: h.name, value: value}
	*order = append(*order, name)
	return nil
}

func (h *host) Config() map[string]any {
	if c, ok := h.r.config[h.name]; ok && c != nil {
		return c
	}
	return map[string]any{}
}

func (h *host) Notice(text string) {
	h.r.notice(fmt.Sprintf("plugin %s: %s", h.name, text))
}
```

- [ ] **Step 4: Run the registry tests to verify they pass**

Run: `go test -race ./internal/plugin/ -v`
Expected: PASS for all seven.

- [ ] **Step 5: Lint and commit**

Run: `make check`
Expected: clean.

```bash
git add internal/plugin
git commit -m "plugin: registry with capability ownership and failure isolation"
bd close <id>
```

---

### Task 12: Protocol envelope, in-memory pipe and client

**Files:**
- Create: `internal/protocol/jsonrpc.go`
- Create: `internal/protocol/methods.go`
- Create: `internal/protocol/conn.go`
- Create: `internal/protocol/client.go`
- Test: `internal/protocol/conn_test.go`
- Test: `internal/protocol/client_test.go`
- Test: `internal/protocol/jsonrpc_test.go`

**Interfaces:**
- Consumes: `session.ErrInvariant`, `session.ErrLocked`, `session.Entry`, `session.Block`, `session.Summary` and the enum types from Task 2 and Task 5; `provider.Error`, `provider.ErrUnknownModel`, `provider.Model`, `provider.Part` from Task 8.
- Produces: everything under `### internal/protocol` in the plan header, plus `const Version = "2.0"`, `const CodeInternal = -32603`, `const CodeMethodNotFound = -32601`, `var ErrInvalidArgument`, `func NewError(code int, msg string, data any) *Error`, `func ErrorFrom(err error) *Error`, `func NewRequest(id int64, method string, params any) (Request, error)`, `func NewNotification(method string, params any) (Request, error)`, `func NewResponse(id json.RawMessage, result any) (Response, error)`, `func NewErrorResponse(id json.RawMessage, e *Error) Response`, `func (r Request) IsNotification() bool`. Task 14 builds every server reply with these.

- [ ] **Step 1: Write the failing pipe test**

```go
// internal/protocol/conn_test.go
package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
)

func TestPipeRoundTrip(t *testing.T) {
	a, b := Pipe()
	defer a.Close()
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	go func() {
		if err := a.Send(ctx, map[string]string{"hello": "b"}); err != nil {
			t.Error(err)
		}
	}()
	got, err := b.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"hello":"b"}` {
		t.Fatalf("b got %s", got)
	}

	go func() {
		if err := b.Send(ctx, json.RawMessage(`{"hello":"a"}`)); err != nil {
			t.Error(err)
		}
	}()
	got, err = a.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"hello":"a"}` {
		t.Fatalf("a got %s", got)
	}
}

func TestPipeCloseGivesPeerEOF(t *testing.T) {
	a, b := Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Recv(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("peer Recv after Close: want io.EOF, got %v", err)
	}
	if err := b.Send(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("Send to a closed peer must fail")
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close must be a no-op, got %v", err)
	}
}

func TestPipeRecvHonorsContext(t *testing.T) {
	a, _ := Pipe()
	defer a.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := a.Recv(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline, got %v", err)
	}
}
```

- [ ] **Step 2: Run the pipe test to verify it fails**

Run: `go test ./internal/protocol/ -run TestPipe -v`
Expected: FAIL with `undefined: Pipe`

- [ ] **Step 3: Write the envelope, the method tables and the pipe**

```go
// internal/protocol/jsonrpc.go
package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

// Version is the JSON-RPC version every message carries.
const Version = "2.0"

// Request is a JSON-RPC 2.0 request or, when ID is absent, a notification.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification reports whether the request expects no response.
func (r Request) IsNotification() bool { return len(r.ID) == 0 }

// Response is a JSON-RPC 2.0 response. Exactly one of Result and Error is set.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is the JSON-RPC error object. It is also a Go error.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("jsonrpc %d: %s", e.Code, e.Message) }

// The closed error taxonomy from the contracts.
const (
	CodeInvalidArgument    = -32602
	CodeMethodNotFound     = -32601
	CodeInternal           = -32603
	CodeNotFound           = -32001
	CodeRefusedByInvariant = -32002
	CodeNoAsker            = -32003
	CodeConflict           = -32004
	CodeUnauthorized       = -32005
	CodeUnavailable        = -32006
	CodeProviderError      = -32007
	CodePluginError        = -32008
	CodeInterrupted        = -32009
)

// ErrInvalidArgument is wrapped by handlers that reject a request shape or value.
var ErrInvalidArgument = errors.New("protocol: invalid argument")

// NewError builds an Error.
func NewError(code int, msg string, data any) *Error {
	return &Error{Code: code, Message: msg, Data: data}
}

// ErrorFrom maps a Go error to the taxonomy. An *Error passes through unchanged.
func ErrorFrom(err error) *Error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	var provErr *provider.Error
	if errors.As(err, &provErr) {
		return NewError(CodeProviderError, provErr.Message, map[string]any{
			"status": provErr.Status,
			"body":   string(provErr.Body),
		})
	}
	switch {
	case errors.Is(err, session.ErrInvariant):
		return NewError(CodeRefusedByInvariant, err.Error(), nil)
	case errors.Is(err, provider.ErrUnknownModel):
		return NewError(CodeNotFound, err.Error(), nil)
	case errors.Is(err, session.ErrLocked):
		return NewError(CodeUnavailable, err.Error(), nil)
	case errors.Is(err, context.Canceled):
		return NewError(CodeInterrupted, err.Error(), nil)
	case errors.Is(err, ErrInvalidArgument):
		return NewError(CodeInvalidArgument, err.Error(), nil)
	}
	return NewError(CodeInternal, err.Error(), nil)
}

// NewRequest builds a request with an integer id.
func NewRequest(id int64, method string, params any) (Request, error) {
	p, err := marshalParams(params)
	if err != nil {
		return Request{}, err
	}
	idb, err := json.Marshal(id)
	if err != nil {
		return Request{}, err
	}
	return Request{JSONRPC: Version, ID: idb, Method: method, Params: p}, nil
}

// NewNotification builds a request without an id.
func NewNotification(method string, params any) (Request, error) {
	p, err := marshalParams(params)
	if err != nil {
		return Request{}, err
	}
	return Request{JSONRPC: Version, Method: method, Params: p}, nil
}

// NewResponse builds a success response. A nil result encodes as {}.
func NewResponse(id json.RawMessage, result any) (Response, error) {
	if result == nil {
		result = struct{}{}
	}
	b, err := json.Marshal(result)
	if err != nil {
		return Response{}, err
	}
	return Response{JSONRPC: Version, ID: id, Result: b}, nil
}

// NewErrorResponse builds an error response.
func NewErrorResponse(id json.RawMessage, e *Error) Response {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return Response{JSONRPC: Version, ID: id, Error: e}
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("protocol: marshal params: %w", err)
	}
	return b, nil
}
```

```go
// internal/protocol/methods.go
package protocol

import (
	"encoding/json"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

// Methods, client to server.
const (
	MethodClientHello        = "client.hello"
	MethodSessionOpen        = "session.open"
	MethodSessionResume      = "session.resume"
	MethodSessionFork        = "session.fork"
	MethodSessionList        = "session.list"
	MethodSessionClose       = "session.close"
	MethodSessionSubmit      = "session.submit"
	MethodSessionInterrupt   = "session.interrupt"
	MethodSessionAnswer      = "session.answer"
	MethodSessionSetModel    = "session.set_model"
	MethodSessionSetMode     = "session.set_mode"
	MethodSessionSetThinking = "session.set_thinking"
	MethodSessionSetTitle    = "session.set_title"
	MethodRegistryList       = "registry.list"
	MethodRegistryRefresh    = "registry.refresh"
	MethodCommandRun         = "command.run"
)

// Notifications, server to client.
const (
	NotifyEntryAppended       = "entry.appended"
	NotifyStreamDelta         = "stream.delta"
	NotifyTurnState           = "turn.state"
	NotifyPermissionRequested = "permission.requested"
	NotifyNotice              = "notice"
)

type ClientHelloParams struct {
	Client  string `json:"client"`
	Version string `json:"version"`
	Asker   bool   `json:"asker"`
}

type ClientHelloResult struct {
	Server  string `json:"server"`
	Version string `json:"version"`
}

type SessionOpenParams struct {
	Cwd      string `json:"cwd"`
	Model    string `json:"model,omitempty"`
	Mode     string `json:"mode,omitempty"`
	Thinking string `json:"thinking,omitempty"`
	Agent    string `json:"agent,omitempty"`
}

type SessionInfo struct {
	SessionID string                `json:"session_id"`
	Workspace session.Workspace     `json:"workspace"`
	Model     session.ModelRef      `json:"model"`
	Mode      session.Mode          `json:"mode"`
	Thinking  session.ThinkingLevel `json:"thinking"`
	Title     string                `json:"title"`
}

type SessionResumeParams struct {
	SessionID string `json:"session_id"`
}

type SessionForkParams struct {
	SessionID string `json:"session_id"`
	AtEntryID string `json:"at_entry_id"`
}

type SessionListResult struct {
	Sessions []session.Summary `json:"sessions"`
}

type SessionCloseParams struct {
	SessionID string `json:"session_id"`
}

type SessionSubmitParams struct {
	SessionID string          `json:"session_id"`
	Content   []session.Block `json:"content"`
	Source    session.Source  `json:"source"`
}

type SessionSubmitResult struct {
	TurnID string `json:"turn_id"`
}

type SessionInterruptParams struct {
	SessionID string            `json:"session_id"`
	How       session.Interrupt `json:"how"`
}

type SessionAnswerParams struct {
	SessionID string           `json:"session_id"`
	ToolUseID string           `json:"tool_use_id"`
	Decision  session.Decision `json:"decision"`
	Scope     session.Scope    `json:"scope"`
	Reason    string           `json:"reason"`
}

type SessionSetModelParams struct {
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
}

type SessionSetModeParams struct {
	SessionID string       `json:"session_id"`
	Mode      session.Mode `json:"mode"`
}

type SessionSetThinkingParams struct {
	SessionID string                `json:"session_id"`
	Thinking  session.ThinkingLevel `json:"thinking"`
}

type SessionSetTitleParams struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
}

type RegistryListResult struct {
	Models []provider.Model `json:"models"`
}

type CommandRunParams struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	Args      string `json:"args"`
}

type CommandRunResult struct {
	TurnID string `json:"turn_id,omitempty"`
	Notice string `json:"notice,omitempty"`
}

type EntryAppended struct {
	SessionID string        `json:"session_id"`
	Entry     session.Entry `json:"entry"`
}

type StreamDelta struct {
	SessionID string        `json:"session_id"`
	TurnID    string        `json:"turn_id"`
	Part      provider.Part `json:"part"`
}

type TurnStateChanged struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	State     string `json:"state"`
}

type PermissionRequested struct {
	SessionID string          `json:"session_id"`
	TurnID    string          `json:"turn_id"`
	ToolUseID string          `json:"tool_use_id"`
	Tool      string          `json:"tool"`
	Input     json.RawMessage `json:"input"`
	Matcher   session.Matcher `json:"matcher"`
}

type NoticeParams struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}
```

```go
// internal/protocol/conn.go
package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// Conn carries one JSON message at a time in either direction.
type Conn interface {
	Send(ctx context.Context, msg any) error
	Recv(ctx context.Context) (json.RawMessage, error)
	Close() error
}

// ErrConnClosed is returned by Send and Recv on a Conn the caller already closed.
var ErrConnClosed = errors.New("protocol: connection closed")

type pipeConn struct {
	out        chan<- json.RawMessage
	in         <-chan json.RawMessage
	closed     chan struct{}
	peerClosed chan struct{}
	once       sync.Once
}

// Pipe returns two connected in-memory Conns. Messages are unbuffered, so Send returns
// only once the peer has received. Closing one end makes the peer's Recv return io.EOF.
func Pipe() (client Conn, server Conn) {
	c2s := make(chan json.RawMessage)
	s2c := make(chan json.RawMessage)
	cClosed := make(chan struct{})
	sClosed := make(chan struct{})
	client = &pipeConn{out: c2s, in: s2c, closed: cClosed, peerClosed: sClosed}
	server = &pipeConn{out: s2c, in: c2s, closed: sClosed, peerClosed: cClosed}
	return client, server
}

func (p *pipeConn) Send(ctx context.Context, msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	select {
	case <-p.closed:
		return ErrConnClosed
	case <-p.peerClosed:
		return io.ErrClosedPipe
	default:
	}
	select {
	case p.out <- b:
		return nil
	case <-p.closed:
		return ErrConnClosed
	case <-p.peerClosed:
		return io.ErrClosedPipe
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *pipeConn) Recv(ctx context.Context) (json.RawMessage, error) {
	select {
	case b := <-p.in:
		return b, nil
	case <-p.closed:
		return nil, ErrConnClosed
	case <-p.peerClosed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *pipeConn) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}
```

- [ ] **Step 4: Run the pipe test to verify it passes**

Run: `go test ./internal/protocol/ -run TestPipe -v`
Expected: PASS for all three

- [ ] **Step 5: Write the failing client and error tests**

```go
// internal/protocol/client_test.go
package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// fakeServer answers requests on conn with handle and lets a test push notifications.
type fakeServer struct {
	conn   Conn
	handle func(req Request) Response
}

func (f *fakeServer) run(ctx context.Context) {
	for {
		raw, err := f.conn.Recv(ctx)
		if err != nil {
			return
		}
		var req Request
		if err := json.Unmarshal(raw, &req); err != nil {
			continue
		}
		if err := f.conn.Send(ctx, f.handle(req)); err != nil {
			return
		}
	}
}

func TestClientCallSuccess(t *testing.T) {
	cc, sc := Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	srv := &fakeServer{conn: sc, handle: func(req Request) Response {
		if req.Method != "echo" {
			t.Errorf("method %q", req.Method)
		}
		resp, _ := NewResponse(req.ID, map[string]any{"got": json.RawMessage(req.Params)})
		return resp
	}}
	go srv.run(ctx)

	c := NewClient(cc)
	defer c.Close()
	var out struct {
		Got map[string]int `json:"got"`
	}
	if err := c.Call(ctx, "echo", map[string]int{"n": 7}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Got["n"] != 7 {
		t.Fatalf("got %+v", out)
	}
}

func TestClientCallError(t *testing.T) {
	cc, sc := Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	srv := &fakeServer{conn: sc, handle: func(req Request) Response {
		return NewErrorResponse(req.ID, NewError(CodeNotFound, "no such session", nil))
	}}
	go srv.run(ctx)

	c := NewClient(cc)
	defer c.Close()
	err := c.Call(ctx, "session.resume", SessionResumeParams{SessionID: "x"}, nil)
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("want *Error, got %v", err)
	}
	if pe.Code != CodeNotFound || pe.Message != "no such session" {
		t.Fatalf("got %+v", pe)
	}
}

func TestClientNotificationsOrderedWhileCallPending(t *testing.T) {
	cc, sc := Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	srv := &fakeServer{conn: sc, handle: func(req Request) Response {
		for i := range 3 {
			n, _ := NewNotification(NotifyNotice, NoticeParams{Level: "info", Text: string(rune('a' + i))})
			if err := sc.Send(ctx, n); err != nil {
				t.Error(err)
			}
		}
		resp, _ := NewResponse(req.ID, nil)
		return resp
	}}
	go srv.run(ctx)

	c := NewClient(cc)
	defer c.Close()
	if err := c.Call(ctx, "anything", nil, nil); err != nil {
		t.Fatal(err)
	}
	var texts []string
	for range 3 {
		select {
		case n := <-c.Notifications():
			var p NoticeParams
			if err := json.Unmarshal(n.Params, &p); err != nil {
				t.Fatal(err)
			}
			if n.Method != NotifyNotice {
				t.Fatalf("method %q", n.Method)
			}
			texts = append(texts, p.Text)
		case <-ctx.Done():
			t.Fatal("notifications not delivered")
		}
	}
	if got := texts[0] + texts[1] + texts[2]; got != "abc" {
		t.Fatalf("order %q", got)
	}
}

func TestClientCallAfterPeerClose(t *testing.T) {
	cc, sc := Pipe()
	c := NewClient(cc)
	defer c.Close()
	if err := sc.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Call(ctx, "anything", nil, nil); err == nil {
		t.Fatal("Call on a closed peer must fail")
	}
}
```

```go
// internal/protocol/jsonrpc_test.go
package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

func TestErrorFrom(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"passthrough", NewError(CodeConflict, "taken", nil), CodeConflict},
		{"wrapped passthrough", fmt.Errorf("outer: %w", NewError(CodeNoAsker, "nobody", nil)), CodeNoAsker},
		{"invariant", fmt.Errorf("fork_point after first: %w", session.ErrInvariant), CodeRefusedByInvariant},
		{"unknown model", fmt.Errorf("%w: x", provider.ErrUnknownModel), CodeNotFound},
		{"provider", &provider.Error{Class: session.ErrProvider, Status: 500, Message: "boom", Body: []byte("x")}, CodeProviderError},
		{"locked", session.ErrLocked, CodeUnavailable},
		{"canceled", context.Canceled, CodeInterrupted},
		{"invalid argument", fmt.Errorf("cwd: %w", ErrInvalidArgument), CodeInvalidArgument},
		{"other", errors.New("disk on fire"), CodeInternal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ErrorFrom(c.err)
			if got.Code != c.code {
				t.Fatalf("code %d, want %d (%v)", got.Code, c.code, got)
			}
		})
	}
	got := ErrorFrom(&provider.Error{Status: 429, Message: "slow", Body: []byte("b")})
	data, ok := got.Data.(map[string]any)
	if !ok || data["status"] != 429 || data["body"] != "b" {
		t.Fatalf("provider data %#v", got.Data)
	}
}

func TestEnvelopes(t *testing.T) {
	req, err := NewRequest(1, MethodClientHello, ClientHelloParams{Client: "test", Version: "0", Asker: false})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(req)
	want := `{"jsonrpc":"2.0","id":1,"method":"client.hello","params":{"client":"test","version":"0","asker":false}}`
	if string(b) != want {
		t.Fatalf("request %s", b)
	}
	n, _ := NewNotification(NotifyNotice, nil)
	if !n.IsNotification() {
		t.Fatal("notification must have no id")
	}
	b, _ = json.Marshal(n)
	if string(b) != `{"jsonrpc":"2.0","method":"notice"}` {
		t.Fatalf("notification %s", b)
	}
	resp, _ := NewResponse(json.RawMessage("1"), nil)
	b, _ = json.Marshal(resp)
	if string(b) != `{"jsonrpc":"2.0","id":1,"result":{}}` {
		t.Fatalf("response %s", b)
	}
	e := NewErrorResponse(nil, NewError(CodeInvalidArgument, "bad", nil))
	b, _ = json.Marshal(e)
	if string(b) != `{"jsonrpc":"2.0","id":null,"error":{"code":-32602,"message":"bad"}}` {
		t.Fatalf("error response %s", b)
	}
}
```

- [ ] **Step 6: Run the client tests to verify they fail**

Run: `go test ./internal/protocol/ -run 'TestClient|TestErrorFrom|TestEnvelopes' -v`
Expected: FAIL with `undefined: NewClient` (the envelope and ErrorFrom tests compile against Step 3 and pass once the package compiles)

- [ ] **Step 7: Write the client**

```go
// internal/protocol/client.go
package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

// Notification is a server-to-client message without an id.
type Notification struct {
	Method string
	Params json.RawMessage
}

// Client multiplexes calls and notifications over one Conn. Callers must drain
// Notifications; the channel holds 256 before the reader blocks.
type Client struct {
	conn    Conn
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan Response
	notes   chan Notification
	done    chan struct{}
	err     error
	once    sync.Once
}

// NewClient starts the reader goroutine.
func NewClient(conn Conn) *Client {
	c := &Client{
		conn:    conn,
		pending: make(map[int64]chan Response),
		notes:   make(chan Notification, 256),
		done:    make(chan struct{}),
	}
	go c.read()
	return c
}

// Notifications yields server notifications in arrival order. It is closed when the
// connection ends.
func (c *Client) Notifications() <-chan Notification { return c.notes }

// Call sends a request and waits for its response. A JSON-RPC error is returned as
// *Error. result may be nil.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	id := c.nextID.Add(1)
	req, err := NewRequest(id, method, params)
	if err != nil {
		return err
	}
	ch := make(chan Response, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return c.err
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.conn.Send(ctx, req); err != nil {
		return err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.err != nil {
			return c.err
		}
		return io.EOF
	}
}

// Close ends the connection and the reader.
func (c *Client) Close() error {
	var err error
	c.once.Do(func() { err = c.conn.Close() })
	return err
}

type incoming struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
}

func (c *Client) read() {
	defer close(c.notes)
	defer close(c.done)
	ctx := context.Background()
	for {
		raw, err := c.conn.Recv(ctx)
		if err != nil {
			c.fail(err)
			return
		}
		var m incoming
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) == 0:
			c.notes <- Notification{Method: m.Method, Params: m.Params}
		case m.Method != "":
			// A server-to-client request. Nothing in this plan handles one; answer so the
			// server does not hang.
			resp := NewErrorResponse(m.ID, NewError(CodeMethodNotFound, "client handles no requests", nil))
			_ = c.conn.Send(ctx, resp)
		default:
			id, err := strconv.ParseInt(string(m.ID), 10, 64)
			if err != nil {
				continue
			}
			c.mu.Lock()
			ch, ok := c.pending[id]
			c.mu.Unlock()
			if ok {
				ch <- Response{JSONRPC: Version, ID: m.ID, Result: m.Result, Error: m.Error}
			}
		}
	}
}

func (c *Client) fail(err error) {
	if errors.Is(err, ErrConnClosed) {
		err = io.EOF
	}
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
}
```

- [ ] **Step 8: Run the whole package with the race detector**

Run: `go test -race ./internal/protocol/ -v`
Expected: PASS for every test

- [ ] **Step 9: Commit**

```bash
go vet ./internal/protocol/ && gofmt -l internal/protocol
git add internal/protocol
git commit -m "protocol: JSON-RPC envelope, in-memory pipe and client"
bd close <task-issue-id>
```

---

### Task 13: System prompt, request assembly and the turn runner

**Files:**
- Create: `internal/turn/system.go`
- Create: `internal/turn/assemble.go`
- Create: `internal/turn/runner.go`
- Test: `internal/turn/system_test.go`
- Test: `internal/turn/assemble_test.go`
- Test: `internal/turn/runner_test.go`

**Interfaces:**
- Consumes: `session.Session` and its methods (`Append`, `RequestContext`, `Model`, `Mode`, `Thinking`, `Allowances`, `Workspace`, `ID`, `Dir`), the payload types and enums from Task 2, `session.ReadLog` from Task 3, `session.OpenStore` and `session.Open` from Tasks 4 and 5; `tool.Tool`, `tool.Call`, `tool.Result`, `tool.Safe`, `tool.Unsafe` from Task 10; `provider.Provider`, `provider.Request`, `provider.Message`, `provider.ToolDef`, `provider.Part`, the `Part*` constants, `provider.Model`, `provider.Error` from Task 8; `gate.Gate`, `gate.New`, `gate.Input`, `gate.Verdict` from Task 10. Entry payloads are stored as values (`session.UserMessage{}`, not a pointer), as Task 2 defines them.
- Produces: everything under `### internal/turn` in the plan header. Task 14 constructs a `Runner` per live session, implements `Asker` and `Observer`, and calls `Run` and `Interrupt`.

- [ ] **Step 1: Write the failing system prompt test**

```go
// internal/turn/system_test.go
package turn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/session"
)

func TestSystemPromptBase(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := session.Workspace{Root: t.TempDir(), ProjectID: "local/x"}
	got := SystemPrompt(ws, "0.1.0")
	for _, want := range []string{"rudy 0.1.0", ws.Root, "edit tool", "tests"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "AGENTS.md") {
		t.Fatalf("no AGENTS.md exists, prompt must not mention one:\n%s", got)
	}
}

func TestSystemPromptIncludesAgentsFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".agents", "AGENTS.md"), []byte("global rule one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("project rule two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := SystemPrompt(session.Workspace{Root: root}, "0.1.0")
	pi := strings.Index(got, "project rule two")
	gi := strings.Index(got, "global rule one")
	if pi < 0 || gi < 0 {
		t.Fatalf("both files must appear:\n%s", got)
	}
	if pi > gi {
		t.Fatalf("workspace AGENTS.md must precede the global one:\n%s", got)
	}
	if !strings.Contains(got, "## AGENTS.md ("+filepath.Join(root, "AGENTS.md")+")") {
		t.Fatalf("section header must name the file:\n%s", got)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/turn/ -run TestSystemPrompt -v`
Expected: FAIL with `undefined: SystemPrompt`

- [ ] **Step 3: Write the system prompt**

```go
// internal/turn/system.go
package turn

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/guygrigsby/rudy/internal/session"
)

// SystemPrompt is the base prompt followed by the workspace's AGENTS.md and the global
// ~/.agents/AGENTS.md, each under its own heading, when they exist.
func SystemPrompt(ws session.Workspace, version string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are rudy %s, a coding agent working in the workspace at %s.\n", version, ws.Root)
	b.WriteString("Use the tools the API gives you; their names and schemas are authoritative. ")
	b.WriteString("Prefer the edit tool over rewriting whole files. ")
	b.WriteString("Run the tests before claiming work is done. ")
	b.WriteString("Keep replies short and lead with the result.\n")

	paths := []string{filepath.Join(ws.Root, "AGENTS.md")}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".agents", "AGENTS.md"))
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "\n## AGENTS.md (%s)\n\n%s\n", p, strings.TrimRight(string(data), "\n"))
	}
	return b.String()
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./internal/turn/ -run TestSystemPrompt -v`
Expected: PASS

- [ ] **Step 5: Write the failing assembly test**

```go
// internal/turn/assemble_test.go
package turn

import (
	"encoding/json"
	"testing"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

func openTestSession(t *testing.T, mode session.Mode) *session.Session {
	t.Helper()
	st, err := session.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := session.Open(st, session.SessionOpened{
		SchemaVersion: 1,
		RudyVersion:   "test",
		Workspace:     session.Workspace{Root: t.TempDir(), ProjectID: "local/test"},
		Model:         session.ModelRef{Provider: "fake", Model: "m1"},
		Thinking:      session.ThinkingHigh,
		Mode:          mode,
		Agent:         "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAssembleMapsEntriesToMessages(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	mustAppend(t, s, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hi")}})
	mustAppend(t, s, session.AssistantMessage{
		Model: s.Model(), Thinking: s.Thinking(),
		Content:    []session.Block{session.TextBlock("reading"), session.ToolUseBlock("tu1", "read", json.RawMessage(`{"path":"go.mod"}`))},
		StopReason: session.StopToolUse, StopReasonRaw: "tool_calls",
	})
	mustAppend(t, s, session.PermissionDecision{ToolUseID: "tu1", Tool: "read", Mode: session.ModeStrict, Matcher: session.Matcher{Tool: "read"}, Decision: session.Allow, DecidedBy: session.ByClass, Scope: session.ScopeOnce, Reason: "safe"})
	mustAppend(t, s, session.ToolResult{ToolUseID: "tu1", Outcome: session.OutcomeOK, Content: []session.Block{session.TextBlock("module x")}, DurationMS: 3})
	mustAppend(t, s, session.ModeChange{Mode: session.ModeOff})

	tools := []tool.Tool{{Name: "read", Description: "read a file", Schema: json.RawMessage(`{"type":"object"}`), Safety: tool.Safe}}
	req := Assemble(s, tools, "SYSTEM", 4096)

	if req.System != "SYSTEM" || req.MaxTokens != 4096 || req.Model != s.Model() || req.Thinking != session.ThinkingHigh || req.SessionID != s.ID() {
		t.Fatalf("header fields %+v", req)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "read" || string(req.Tools[0].Schema) != `{"type":"object"}` {
		t.Fatalf("tools %+v", req.Tools)
	}
	wantRoles := []provider.Role{provider.RoleUser, provider.RoleAssistant, provider.RoleToolResult}
	if len(req.Messages) != len(wantRoles) {
		t.Fatalf("messages %+v", req.Messages)
	}
	for i, r := range wantRoles {
		if req.Messages[i].Role != r {
			t.Fatalf("message %d role %q want %q", i, req.Messages[i].Role, r)
		}
	}
	if req.Messages[2].ToolUseID != "tu1" || req.Messages[2].Content[0].Text != "module x" {
		t.Fatalf("tool result message %+v", req.Messages[2])
	}
	if string(req.Messages[1].Content[1].Input) != `{"path":"go.mod"}` {
		t.Fatalf("tool_use input must be verbatim, got %s", req.Messages[1].Content[1].Input)
	}
}

func TestAssembleStartsFromCompaction(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	first := mustAppend(t, s, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("old")}})
	last := mustAppend(t, s, session.AssistantMessage{Model: s.Model(), Thinking: s.Thinking(), Content: []session.Block{session.TextBlock("older reply")}, StopReason: session.StopEndTurn, StopReasonRaw: "stop"})
	mustAppend(t, s, session.Compaction{Summary: "we discussed old things", FirstEntryID: first.ID, LastEntryID: last.ID})
	mustAppend(t, s, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("new")}})

	req := Assemble(s, nil, "S", 100)
	if len(req.Messages) != 2 {
		t.Fatalf("want summary then new message, got %+v", req.Messages)
	}
	if req.Messages[0].Role != provider.RoleUser || req.Messages[0].Content[0].Text != "Summary of the conversation so far:\nwe discussed old things" {
		t.Fatalf("summary message %+v", req.Messages[0])
	}
	if req.Messages[1].Content[0].Text != "new" {
		t.Fatalf("second message %+v", req.Messages[1])
	}
}

func mustAppend(t *testing.T, s *session.Session, p session.Payload) session.Entry {
	t.Helper()
	e, err := s.Append(p)
	if err != nil {
		t.Fatalf("append %T: %v", p, err)
	}
	return e
}
```

- [ ] **Step 6: Run it to verify it fails**

Run: `go test ./internal/turn/ -run TestAssemble -v`
Expected: FAIL with `undefined: Assemble`

- [ ] **Step 7: Write the assembler**

```go
// internal/turn/assemble.go
package turn

import (
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// Assemble builds the provider request from the session's request context. A leading
// compaction entry becomes a user message carrying its summary. Entry kinds that are not
// conversation content are skipped.
func Assemble(s *session.Session, tools []tool.Tool, system string, maxTokens int) provider.Request {
	req := provider.Request{
		Model:     s.Model(),
		System:    system,
		Thinking:  s.Thinking(),
		MaxTokens: maxTokens,
		SessionID: s.ID(),
	}
	for _, t := range tools {
		req.Tools = append(req.Tools, provider.ToolDef{Name: t.Name, Description: t.Description, Schema: t.Schema})
	}
	for _, e := range s.RequestContext() {
		switch p := e.Payload.(type) {
		case session.Compaction:
			req.Messages = append(req.Messages, provider.Message{
				Role:    provider.RoleUser,
				Content: []session.Block{session.TextBlock("Summary of the conversation so far:\n" + p.Summary)},
			})
		case session.UserMessage:
			req.Messages = append(req.Messages, provider.Message{Role: provider.RoleUser, Content: p.Content})
		case session.AssistantMessage:
			req.Messages = append(req.Messages, provider.Message{Role: provider.RoleAssistant, Content: p.Content})
		case session.ToolResult:
			req.Messages = append(req.Messages, provider.Message{Role: provider.RoleToolResult, Content: p.Content, ToolUseID: p.ToolUseID})
		}
	}
	return req
}
```

`provider.RoleUser`, `provider.RoleAssistant` and `provider.RoleToolResult` are the three `Role` constants Task 8 defines for `"user"`, `"assistant"` and `"tool_result"`.

- [ ] **Step 8: Run it to verify it passes**

Run: `go test ./internal/turn/ -run TestAssemble -v`
Expected: PASS

- [ ] **Step 9: Write the failing runner tests**

```go
// internal/turn/runner_test.go
package turn

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// pause is a sentinel part: the scripted provider blocks on release until the test lets
// it continue, or returns ctx.Err() when the runner cancels first.
const pause provider.PartType = "test_pause"

type scripted struct {
	mu       sync.Mutex
	scripts  [][]provider.Part
	calls    int
	requests []provider.Request
	release  chan struct{}
	err      error // returned instead of streaming when set
}

func (p *scripted) Name() string { return "fake" }

func (p *scripted) ListModels(context.Context) ([]provider.Model, error) { return nil, nil }

func (p *scripted) Complete(ctx context.Context, req provider.Request, emit func(provider.Part) error) error {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	if p.err != nil {
		p.mu.Unlock()
		return p.err
	}
	if p.calls >= len(p.scripts) {
		p.mu.Unlock()
		return &provider.Error{Class: session.ErrInternal, Message: "script exhausted"}
	}
	parts := p.scripts[p.calls]
	p.calls++
	p.mu.Unlock()
	for _, part := range parts {
		if part.Type == pause {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-p.release:
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := emit(part); err != nil {
			return err
		}
	}
	return nil
}

func text(s string) provider.Part { return provider.Part{Type: provider.PartTextDelta, Text: s} }

func stop(reason session.StopReason, raw string) provider.Part {
	return provider.Part{Type: provider.PartStop, StopReason: reason, StopReasonRaw: raw}
}

func usage(in, out int64) provider.Part {
	return provider.Part{Type: provider.PartUsage, Usage: session.Usage{Input: in, Output: out}}
}

func toolCall(id, name, input string) []provider.Part {
	return []provider.Part{
		{Type: provider.PartToolUseStart, ID: id, Name: name},
		{Type: provider.PartToolUseDelta, ID: id, Text: input},
		{Type: provider.PartToolUseEnd, ID: id},
	}
}

type recorder struct {
	mu      sync.Mutex
	entries []session.Entry
	states  []State
	deltas  []provider.Part
}

func (r *recorder) EntryAppended(e session.Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, e)
}

func (r *recorder) Delta(_ string, p provider.Part) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deltas = append(r.deltas, p)
}

func (r *recorder) StateChanged(_ string, s State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, s)
}

func (r *recorder) kinds() []session.Kind {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]session.Kind, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e.Kind)
	}
	return out
}

type askerFunc func(ctx context.Context, q Question) (Answer, error)

func (f askerFunc) Ask(ctx context.Context, q Question) (Answer, error) { return f(ctx, q) }

type toolSet map[string]tool.Tool

func (ts toolSet) Tool(name string) (tool.Tool, bool) { t, ok := ts[name]; return t, ok }

func (ts toolSet) Tools() []tool.Tool {
	out := make([]tool.Tool, 0, len(ts))
	for _, t := range ts {
		out = append(out, t)
	}
	return out
}

func echoTool(safety tool.Safety, name string) tool.Tool {
	return tool.Tool{Name: name, Description: "echo", Schema: json.RawMessage(`{"type":"object"}`), Safety: safety,
		Invoke: func(_ context.Context, c tool.Call) (tool.Result, error) {
			return tool.Result{Content: []session.Block{session.TextBlock("echo:" + string(c.Input))}}, nil
		}}
}

func newRunner(t *testing.T, s *session.Session, p *scripted, tools toolSet, asker Asker, rec *recorder) *Runner {
	t.Helper()
	return NewRunner(Config{
		Session:   s,
		Provider:  p,
		Model:     provider.Model{Ref: s.Model(), ContextWindow: 100000},
		Tools:     tools,
		Gate:      gate.New([]string{"rm -rf"}),
		Asker:     asker,
		Observer:  rec,
		System:    "SYSTEM",
		MaxTokens: 1024,
	})
}

func userMsg(src session.Source, s string) session.UserMessage {
	return session.UserMessage{Source: src, Content: []session.Block{session.TextBlock(s)}}
}

func equalKinds(a, b []session.Kind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRunTextOnly(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	p := &scripted{scripts: [][]provider.Part{{text("Hel"), text("lo"), usage(10, 2), stop(session.StopEndTurn, "stop")}}}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{}, nil, rec)

	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "hi")); err != nil {
		t.Fatal(err)
	}
	if r.State() != Completed {
		t.Fatalf("state %s", r.State())
	}
	want := []session.Kind{session.KindUserMessage, session.KindAssistantMessage}
	if got := rec.kinds(); !equalKinds(got, want) {
		t.Fatalf("entries %v", got)
	}
	am := rec.entries[1].Payload.(session.AssistantMessage)
	if len(am.Content) != 1 || am.Content[0].Text != "Hello" || am.Usage.Input != 10 || am.StopReason != session.StopEndTurn || am.StopReasonRaw != "stop" {
		t.Fatalf("assistant message %+v", am)
	}
	if len(rec.deltas) != 4 {
		t.Fatalf("every part must reach the observer, got %d", len(rec.deltas))
	}
	if len(p.requests) != 1 || p.requests[0].System != "SYSTEM" || p.requests[0].Messages[0].Content[0].Text != "hi" {
		t.Fatalf("request %+v", p.requests)
	}
}

func TestRunToolFlowStrictAskerAllows(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	var seenAllowOnDisk bool
	unsafe := tool.Tool{Name: "bash", Description: "run", Schema: json.RawMessage(`{"type":"object"}`), Safety: tool.Unsafe,
		Invoke: func(_ context.Context, c tool.Call) (tool.Result, error) {
			entries, err := session.ReadLog(s.Dir())
			if err != nil {
				t.Error(err)
			}
			for _, e := range entries {
				if d, ok := e.Payload.(session.PermissionDecision); ok && d.ToolUseID == c.ID && d.Decision == session.Allow {
					seenAllowOnDisk = true
				}
			}
			return tool.Result{Content: []session.Block{session.TextBlock("ran")}}, nil
		}}
	p := &scripted{scripts: [][]provider.Part{
		append(append([]provider.Part{text("running")}, toolCall("tu1", "bash", `{"command":"go test ./..."}`)...), usage(5, 5), stop(session.StopToolUse, "tool_calls")),
		{text("done"), usage(6, 1), stop(session.StopEndTurn, "stop")},
	}}
	var asked Question
	asker := askerFunc(func(_ context.Context, q Question) (Answer, error) {
		asked = q
		return Answer{Decision: session.Allow, Scope: session.ScopeOnce, Reason: "looks fine"}, nil
	})
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{"bash": unsafe}, asker, rec)

	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "test it")); err != nil {
		t.Fatal(err)
	}
	want := []session.Kind{session.KindUserMessage, session.KindAssistantMessage, session.KindPermissionDecision, session.KindToolResult, session.KindAssistantMessage}
	if got := rec.kinds(); !equalKinds(got, want) {
		t.Fatalf("entries %v", got)
	}
	if !seenAllowOnDisk {
		t.Fatal("the allow decision must be on disk before the tool runs")
	}
	if asked.ToolUseID != "tu1" || asked.Tool != "bash" || string(asked.Input) != `{"command":"go test ./..."}` {
		t.Fatalf("question %+v", asked)
	}
	pd := rec.entries[2].Payload.(session.PermissionDecision)
	if pd.Decision != session.Allow || pd.DecidedBy != session.ByAsker || pd.Scope != session.ScopeOnce || pd.Reason != "looks fine" || pd.Mode != session.ModeStrict {
		t.Fatalf("decision %+v", pd)
	}
	tr := rec.entries[3].Payload.(session.ToolResult)
	if tr.Outcome != session.OutcomeOK || tr.Content[0].Text != "ran" || tr.ToolUseID != "tu1" {
		t.Fatalf("tool result %+v", tr)
	}
	wantStates := []State{Streaming, AwaitingPermission, RunningTool, Streaming, Completed}
	if len(rec.states) != len(wantStates) {
		t.Fatalf("states %v", rec.states)
	}
	for i := range wantStates {
		if rec.states[i] != wantStates[i] {
			t.Fatalf("states %v want %v", rec.states, wantStates)
		}
	}
	if len(p.requests) != 2 || p.requests[1].Messages[2].Role != provider.RoleToolResult {
		t.Fatalf("second request must carry the tool result: %+v", p.requests[1].Messages)
	}
}

func TestRunToolDenied(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	invoked := false
	unsafe := echoTool(tool.Unsafe, "bash")
	unsafe.Invoke = func(context.Context, tool.Call) (tool.Result, error) { invoked = true; return tool.Result{}, nil }
	p := &scripted{scripts: [][]provider.Part{
		append(toolCall("tu1", "bash", `{"command":"ls"}`), stop(session.StopToolUse, "tool_calls")),
		{text("ok, skipping"), stop(session.StopEndTurn, "stop")},
	}}
	asker := askerFunc(func(context.Context, Question) (Answer, error) {
		return Answer{Decision: session.Deny, Scope: session.ScopeOnce, Reason: "not now"}, nil
	})
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{"bash": unsafe}, asker, rec)
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "list")); err != nil {
		t.Fatal(err)
	}
	if invoked {
		t.Fatal("denied tool must not run")
	}
	pd := rec.entries[2].Payload.(session.PermissionDecision)
	if pd.Decision != session.Deny || pd.DecidedBy != session.ByAsker || pd.Reason != "not now" {
		t.Fatalf("decision %+v", pd)
	}
	tr := rec.entries[3].Payload.(session.ToolResult)
	if tr.Outcome != session.OutcomeError || tr.Content[0].Text != "denied: not now" {
		t.Fatalf("tool result %+v", tr)
	}
}

func TestRunStrictWithoutAskerDenies(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	p := &scripted{scripts: [][]provider.Part{
		append(toolCall("tu1", "bash", `{"command":"ls"}`), stop(session.StopToolUse, "tool_calls")),
		{text("understood"), stop(session.StopEndTurn, "stop")},
	}}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{"bash": echoTool(tool.Unsafe, "bash")}, nil, rec)
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "list")); err != nil {
		t.Fatal(err)
	}
	pd := rec.entries[2].Payload.(session.PermissionDecision)
	if pd.Decision != session.Deny || pd.DecidedBy != session.ByNoAsker {
		t.Fatalf("decision %+v", pd)
	}
	for _, st := range rec.states {
		if st == AwaitingPermission {
			t.Fatal("no asker means no waiting")
		}
	}
}

func TestRunSafeToolNeverAsks(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	p := &scripted{scripts: [][]provider.Part{
		append(toolCall("tu1", "read", `{"path":"a"}`), stop(session.StopToolUse, "tool_calls")),
		{text("read it"), stop(session.StopEndTurn, "stop")},
	}}
	asker := askerFunc(func(context.Context, Question) (Answer, error) {
		t.Fatal("safe tool must not ask")
		return Answer{}, nil
	})
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{"read": echoTool(tool.Safe, "read")}, asker, rec)
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "read a")); err != nil {
		t.Fatal(err)
	}
	pd := rec.entries[2].Payload.(session.PermissionDecision)
	if pd.Decision != session.Allow || pd.DecidedBy != session.ByClass {
		t.Fatalf("decision %+v", pd)
	}
}

func TestRunOffModeAllowsByMode(t *testing.T) {
	s := openTestSession(t, session.ModeOff)
	p := &scripted{scripts: [][]provider.Part{
		append(toolCall("tu1", "bash", `{"command":"rm -rf build"}`), stop(session.StopToolUse, "tool_calls")),
		{text("gone"), stop(session.StopEndTurn, "stop")},
	}}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{"bash": echoTool(tool.Unsafe, "bash")}, nil, rec)
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "clean")); err != nil {
		t.Fatal(err)
	}
	pd := rec.entries[2].Payload.(session.PermissionDecision)
	if pd.Decision != session.Allow || pd.DecidedBy != session.ByMode {
		t.Fatalf("decision %+v", pd)
	}
}

func TestRunPermissiveAsksForDangerous(t *testing.T) {
	s := openTestSession(t, session.ModePermissive)
	p := &scripted{scripts: [][]provider.Part{
		append(toolCall("tu1", "bash", `{"command":"rm -rf build"}`), stop(session.StopToolUse, "tool_calls")),
		{text("gone"), stop(session.StopEndTurn, "stop")},
	}}
	asked := false
	asker := askerFunc(func(context.Context, Question) (Answer, error) {
		asked = true
		return Answer{Decision: session.Allow, Scope: session.ScopeSession, Reason: "yes for this session"}, nil
	})
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{"bash": echoTool(tool.Unsafe, "bash")}, asker, rec)
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "clean")); err != nil {
		t.Fatal(err)
	}
	if !asked {
		t.Fatal("dangerous command in permissive mode must ask")
	}
	pd := rec.entries[2].Payload.(session.PermissionDecision)
	if pd.Scope != session.ScopeSession || pd.DecidedBy != session.ByAsker {
		t.Fatalf("decision %+v", pd)
	}
	if len(s.Allowances()) != 1 {
		t.Fatalf("session allowance must be derivable, got %v", s.Allowances())
	}
}

func TestRunUnknownToolIsAnErrorResult(t *testing.T) {
	s := openTestSession(t, session.ModeOff)
	p := &scripted{scripts: [][]provider.Part{
		append(toolCall("tu1", "nope", `{}`), stop(session.StopToolUse, "tool_calls")),
		{text("sorry"), stop(session.StopEndTurn, "stop")},
	}}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{}, nil, rec)
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "x")); err != nil {
		t.Fatal(err)
	}
	want := []session.Kind{session.KindUserMessage, session.KindAssistantMessage, session.KindToolResult, session.KindAssistantMessage}
	if got := rec.kinds(); !equalKinds(got, want) {
		t.Fatalf("entries %v", got)
	}
	tr := rec.entries[2].Payload.(session.ToolResult)
	if tr.Outcome != session.OutcomeError || tr.Content[0].Text != "unknown tool nope" {
		t.Fatalf("tool result %+v", tr)
	}
}

func TestSteerMidStreamThenContinue(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	p := &scripted{
		release: make(chan struct{}),
		scripts: [][]provider.Part{
			{text("Half"), {Type: pause}, text(" never sent"), stop(session.StopEndTurn, "stop")},
			{text("Steered"), stop(session.StopEndTurn, "stop")},
		},
	}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{}, nil, rec)

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), userMsg(session.SourceTyped, "go")) }()
	waitForDelta(t, rec, 1)
	r.Interrupt(session.InterruptSteer)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after steer")
	}
	if r.State() != Steering {
		t.Fatalf("state %s", r.State())
	}
	am := rec.entries[1].Payload.(session.AssistantMessage)
	if am.StopReason != session.StopInterrupted || am.Content[0].Text != "Half" || am.StopReasonRaw != "" {
		t.Fatalf("partial message %+v", am)
	}
	turnID := r.TurnID()

	if err := r.Run(context.Background(), userMsg(session.SourceSteer, "do it differently")); err != nil {
		t.Fatal(err)
	}
	if r.State() != Completed || r.TurnID() != turnID {
		t.Fatalf("state %s turn %s want %s", r.State(), r.TurnID(), turnID)
	}
	want := []session.Kind{session.KindUserMessage, session.KindAssistantMessage, session.KindUserMessage, session.KindAssistantMessage}
	if got := rec.kinds(); !equalKinds(got, want) {
		t.Fatalf("entries %v", got)
	}
	if len(p.requests) != 2 || len(p.requests[1].Messages) != 3 {
		t.Fatalf("the steer request must carry the partial and the steer: %+v", p.requests[1].Messages)
	}
}

func TestCancelMidStream(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	p := &scripted{release: make(chan struct{}), scripts: [][]provider.Part{{text("Half"), {Type: pause}, stop(session.StopEndTurn, "stop")}}}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{}, nil, rec)

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), userMsg(session.SourceTyped, "go")) }()
	waitForDelta(t, rec, 1)
	r.Interrupt(session.InterruptCancel)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if r.State() != Idle {
		t.Fatalf("state %s", r.State())
	}
	want := []session.Kind{session.KindUserMessage, session.KindAssistantMessage, session.KindTurnInterrupted}
	if got := rec.kinds(); !equalKinds(got, want) {
		t.Fatalf("entries %v", got)
	}
	if ti := rec.entries[2].Payload.(session.TurnInterrupted); ti.How != session.InterruptCancel || ti.TurnID != rec.entries[0].ID {
		t.Fatalf("interrupted %+v want turn %s", ti, rec.entries[0].ID)
	}
}

func TestSteerDuringToolKillsIt(t *testing.T) {
	s := openTestSession(t, session.ModeOff)
	started := make(chan struct{})
	slow := tool.Tool{Name: "bash", Description: "slow", Schema: json.RawMessage(`{"type":"object"}`), Safety: tool.Unsafe,
		Invoke: func(ctx context.Context, _ tool.Call) (tool.Result, error) {
			close(started)
			<-ctx.Done()
			return tool.Result{Content: []session.Block{session.TextBlock("partial output")}}, ctx.Err()
		}}
	p := &scripted{scripts: [][]provider.Part{
		append(toolCall("tu1", "bash", `{"command":"sleep 100"}`), stop(session.StopToolUse, "tool_calls")),
	}}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{"bash": slow}, nil, rec)

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), userMsg(session.SourceTyped, "sleep")) }()
	<-started
	r.Interrupt(session.InterruptSteer)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return")
	}
	if r.State() != Steering {
		t.Fatalf("state %s", r.State())
	}
	tr := rec.entries[3].Payload.(session.ToolResult)
	if tr.Outcome != session.OutcomeKilled || tr.Content[0].Text != "partial output" {
		t.Fatalf("tool result %+v", tr)
	}
}

func TestProviderErrorFailsTurn(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	p := &scripted{err: &provider.Error{Class: session.ErrProvider, Status: 502, Message: "bad gateway", Body: []byte("x"), Attempts: 5}}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{}, nil, rec)
	err := r.Run(context.Background(), userMsg(session.SourceTyped, "hi"))
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("want *provider.Error, got %v", err)
	}
	if r.State() != Failed {
		t.Fatalf("state %s", r.State())
	}
	tf := rec.entries[1].Payload.(session.TurnFailed)
	if tf.Class != session.ErrProvider || tf.Message != "bad gateway" || tf.Retries != 5 {
		t.Fatalf("turn_failed %+v", tf)
	}
	if tf.TurnID != rec.entries[0].ID || r.TurnID() != rec.entries[0].ID.String() {
		t.Fatalf("turn id %s want the user_message id %s", tf.TurnID, rec.entries[0].ID)
	}
}

func TestPanickingToolIsAPluginFault(t *testing.T) {
	s := openTestSession(t, session.ModeOff)
	p := &scripted{scripts: [][]provider.Part{append(toolCall("t1", "boom", `{}`), stop(session.StopToolUse, "tool_calls"))}}
	rec := &recorder{}
	tools := toolSet{"boom": tool.Tool{Name: "boom", Safety: tool.Unsafe, Invoke: func(context.Context, tool.Call) (tool.Result, error) {
		panic("nil map write")
	}}}
	r := newRunner(t, s, p, tools, nil, rec)
	err := r.Run(context.Background(), userMsg(session.SourceTyped, "go"))
	if err == nil || r.State() != Failed {
		t.Fatalf("err %v state %s", err, r.State())
	}
	tf := rec.entries[len(rec.entries)-1].Payload.(session.TurnFailed)
	if tf.Class != session.ErrPlugin || tf.Retries != 0 || !strings.Contains(tf.Message, "nil map write") {
		t.Fatalf("turn_failed %+v", tf)
	}
}

func TestCallerContextCancelIsCancel(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	p := &scripted{release: make(chan struct{}), scripts: [][]provider.Part{{text("Half"), {Type: pause}, stop(session.StopEndTurn, "stop")}}}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{}, nil, rec)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, userMsg(session.SourceTyped, "go")) }()
	waitForDelta(t, rec, 1)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return")
	}
	if r.State() != Idle {
		t.Fatalf("state %s", r.State())
	}
	if got := rec.kinds(); got[len(got)-1] != session.KindTurnInterrupted {
		t.Fatalf("entries %v", got)
	}
}

func TestInterruptWhenIdleIsNoop(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	rec := &recorder{}
	r := newRunner(t, s, &scripted{}, toolSet{}, nil, rec)
	r.Interrupt(session.InterruptCancel)
	if r.State() != Idle || len(rec.entries) != 0 {
		t.Fatalf("idle interrupt must change nothing: %s %v", r.State(), rec.kinds())
	}
}

func waitForDelta(t *testing.T, rec *recorder, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		got := len(rec.deltas)
		rec.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waited for %d deltas", n)
}
```

- [ ] **Step 10: Run the runner tests to verify they fail**

Run: `go test ./internal/turn/ -run 'TestRun|TestSteer|TestCancel|TestProvider|TestCaller|TestInterrupt' -v`
Expected: FAIL with `undefined: NewRunner`

- [ ] **Step 11: Write the runner**

```go
// internal/turn/runner.go
package turn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// State is the turn's position in its lifecycle.
type State string

const (
	Idle               State = "idle"
	Streaming          State = "streaming"
	RunningTool        State = "running_tool"
	AwaitingPermission State = "awaiting_permission"
	Steering           State = "steering"
	Completed          State = "completed"
	Failed             State = "failed"
)

// Question is a permission request for one tool_use.
type Question struct {
	ToolUseID string
	Tool      string
	Input     json.RawMessage
	Matcher   session.Matcher
}

// Answer is the asker's reply.
type Answer struct {
	Decision session.Decision
	Scope    session.Scope
	Reason   string
}

// Asker answers permission questions. A nil Asker means nobody can answer.
type Asker interface {
	Ask(ctx context.Context, q Question) (Answer, error)
}

// Observer sees every entry, every stream part and every state change.
type Observer interface {
	EntryAppended(e session.Entry)
	Delta(turnID string, p provider.Part)
	StateChanged(turnID string, s State)
}

// Tools resolves tool names for a turn.
type Tools interface {
	Tool(name string) (tool.Tool, bool)
	Tools() []tool.Tool
}

// Config wires one Runner.
type Config struct {
	Session   *session.Session
	Provider  provider.Provider
	Model     provider.Model
	Tools     Tools
	Gate      *gate.Gate
	Asker     Asker
	Observer  Observer
	System    string
	MaxTokens int
}

type noopObserver struct{}

func (noopObserver) EntryAppended(session.Entry)      {}
func (noopObserver) Delta(string, provider.Part)      {}
func (noopObserver) StateChanged(string, State)       {}

// Runner drives one turn at a time over a session.
type Runner struct {
	cfg Config

	mu        sync.Mutex
	state     State
	turn      ulid.ULID         // id of the user_message entry that started the current or most recent turn
	interrupt session.Interrupt // pending interrupt, cleared by takeInterrupt
	cancel    context.CancelFunc
}

// NewRunner returns an idle runner.
func NewRunner(c Config) *Runner {
	if c.Observer == nil {
		c.Observer = noopObserver{}
	}
	return &Runner{cfg: c, state: Idle}
}

// TurnID is the id of the current or most recent turn: the ULID of the user_message entry
// that started it, as a string. Empty before the first turn.
func (r *Runner) TurnID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.turn.IsZero() {
		return ""
	}
	return r.turn.String()
}

// State is the current state.
func (r *Runner) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// Interrupt stops the current step. Steer leaves the runner in Steering so the next Run
// with a steer message continues the turn; cancel appends turn_interrupted and returns to
// Idle. Cancel overrides a pending steer. Interrupting an idle, completed or failed runner
// changes nothing.
func (r *Runner) Interrupt(how session.Interrupt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state {
	case Idle, Completed, Failed:
		return
	case Steering:
		if how == session.InterruptCancel {
			r.appendLocked(session.TurnInterrupted{TurnID: r.turn, How: session.InterruptCancel})
			r.setStateLocked(Idle)
		}
		return
	}
	if r.interrupt != session.InterruptCancel {
		r.interrupt = how
	}
	if r.cancel != nil {
		r.cancel()
	}
}

// Run appends msg and drives the loop until Completed, Failed, Steering or Idle after a
// cancel. It returns nil for Completed, Steering and an interrupt cancel, the provider or
// internal error for Failed, and ctx.Err() when the caller's context ended the turn.
func (r *Runner) Run(ctx context.Context, msg session.UserMessage) error {
	r.mu.Lock()
	switch r.state {
	case Steering:
		if msg.Source != session.SourceSteer {
			r.mu.Unlock()
			return fmt.Errorf("%w: a steering turn continues only with a steer message", session.ErrInvariant)
		}
	case Idle, Completed, Failed:
		if msg.Source == session.SourceSteer {
			r.mu.Unlock()
			return fmt.Errorf("%w: steer without a steering turn", session.ErrInvariant)
		}
	default:
		r.mu.Unlock()
		return fmt.Errorf("%w: turn already active", session.ErrInvariant)
	}
	starting := r.state != Steering
	r.interrupt = ""
	r.mu.Unlock()

	e, err := r.append(msg)
	if err != nil {
		return r.fail(session.ErrInternal, err)
	}
	if starting {
		// The turn id is the id of the user_message that started it. A steer message
		// continues the turn under the same id.
		r.mu.Lock()
		r.turn = e.ID
		r.mu.Unlock()
	}
	return r.loop(ctx)
}

func (r *Runner) loop(ctx context.Context) error {
	for {
		r.setState(Streaming)
		am, err := r.stream(ctx)
		if how := r.takeInterrupt(); how != "" {
			am.StopReason = session.StopInterrupted
			am.StopReasonRaw = ""
			am.Content = dropIncompleteToolUses(am.Content)
			if _, aerr := r.append(am); aerr != nil {
				return r.fail(session.ErrInternal, aerr)
			}
			return r.finishInterrupt(how)
		}
		if err != nil {
			if ctx.Err() != nil {
				am.StopReason = session.StopInterrupted
				am.StopReasonRaw = ""
				am.Content = dropIncompleteToolUses(am.Content)
				if _, aerr := r.append(am); aerr != nil {
					return r.fail(session.ErrInternal, aerr)
				}
				_ = r.finishInterrupt(session.InterruptCancel)
				return ctx.Err()
			}
			var pe *provider.Error
			if errors.As(err, &pe) {
				return r.fail(pe.Class, err)
			}
			return r.fail(session.ErrTransport, err)
		}
		if _, err := r.append(am); err != nil {
			return r.fail(session.ErrInternal, err)
		}

		var toolUses []session.Block
		for _, b := range am.Content {
			if b.Type == session.BlockToolUse {
				toolUses = append(toolUses, b)
			}
		}
		if len(toolUses) == 0 {
			r.setState(Completed)
			return nil
		}
		for _, tu := range toolUses {
			done, err := r.runTool(ctx, tu)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		}
	}
}

// stream runs one completion and accumulates the assistant message. The returned message
// is partial when err is non-nil.
func (r *Runner) stream(ctx context.Context) (session.AssistantMessage, error) {
	stepCtx, cancel := context.WithCancel(ctx)
	r.setCancel(cancel)
	defer cancel()

	acc := newAccumulator()
	req := Assemble(r.cfg.Session, r.cfg.Tools.Tools(), r.cfg.System, r.cfg.MaxTokens)
	turnID := r.TurnID()
	err := r.cfg.Provider.Complete(stepCtx, req, func(p provider.Part) error {
		r.cfg.Observer.Delta(turnID, p)
		acc.add(p)
		return nil
	})
	am := session.AssistantMessage{
		Model:         r.cfg.Session.Model(),
		Thinking:      r.cfg.Session.Thinking(),
		Content:       acc.blocks(),
		Usage:         acc.usage,
		StopReason:    acc.stop,
		StopReasonRaw: acc.stopRaw,
	}
	if err == nil && am.StopReason == "" {
		am.StopReason = session.StopOther
	}
	return am, err
}

// runTool gates and runs one tool_use. done is true when an interrupt ended the turn.
func (r *Runner) runTool(ctx context.Context, tu session.Block) (done bool, err error) {
	t, ok := r.cfg.Tools.Tool(tu.Name)
	if !ok {
		_, err := r.append(session.ToolResult{
			ToolUseID: tu.ID,
			Outcome:   session.OutcomeError,
			Content:   []session.Block{session.TextBlock("unknown tool " + tu.Name)},
		})
		if err != nil {
			return true, r.fail(session.ErrInternal, err)
		}
		return false, nil
	}

	s := r.cfg.Session
	verdict := r.cfg.Gate.Evaluate(gate.Input{
		Tool:         tu.Name,
		Safety:       t.Safety,
		Mode:         s.Mode(),
		Args:         tu.Input,
		Allowances:   s.Allowances(),
		AskerPresent: r.cfg.Asker != nil,
	})
	dec := session.PermissionDecision{
		ToolUseID: tu.ID,
		Tool:      tu.Name,
		Mode:      s.Mode(),
		Matcher:   verdict.Matcher,
		Decision:  verdict.Decision,
		DecidedBy: verdict.DecidedBy,
		Scope:     session.ScopeOnce,
		Reason:    verdict.Reason,
	}
	if verdict.Ask {
		r.setState(AwaitingPermission)
		askCtx, cancel := context.WithCancel(ctx)
		r.setCancel(cancel)
		ans, askErr := r.cfg.Asker.Ask(askCtx, Question{ToolUseID: tu.ID, Tool: tu.Name, Input: tu.Input, Matcher: verdict.Matcher})
		cancel()
		if how := r.takeInterrupt(); how != "" {
			if _, err := r.append(session.ToolResult{ToolUseID: tu.ID, Outcome: session.OutcomeKilled,
				Content: []session.Block{session.TextBlock("interrupted while awaiting permission")}}); err != nil {
				return true, r.fail(session.ErrInternal, err)
			}
			return true, r.finishInterrupt(how)
		}
		if ctx.Err() != nil {
			if _, err := r.append(session.ToolResult{ToolUseID: tu.ID, Outcome: session.OutcomeKilled,
				Content: []session.Block{session.TextBlock("interrupted while awaiting permission")}}); err != nil {
				return true, r.fail(session.ErrInternal, err)
			}
			_ = r.finishInterrupt(session.InterruptCancel)
			return true, ctx.Err()
		}
		switch {
		case askErr != nil:
			dec.Decision, dec.DecidedBy, dec.Reason = session.Deny, session.ByNoAsker, "asker failed: "+askErr.Error()
		default:
			dec.Decision, dec.DecidedBy = ans.Decision, session.ByAsker
			dec.Reason = ans.Reason
			if dec.Reason == "" {
				dec.Reason = "asker"
			}
			if ans.Decision == session.Allow && ans.Scope == session.ScopeSession {
				dec.Scope = session.ScopeSession
			}
		}
	}
	if _, err := r.append(dec); err != nil {
		return true, r.fail(session.ErrInternal, err)
	}
	if dec.Decision == session.Deny {
		_, err := r.append(session.ToolResult{
			ToolUseID: tu.ID,
			Outcome:   session.OutcomeError,
			Content:   []session.Block{session.TextBlock("denied: " + dec.Reason)},
		})
		if err != nil {
			return true, r.fail(session.ErrInternal, err)
		}
		return false, nil
	}

	r.setState(RunningTool)
	if t.Invoke == nil {
		return true, r.fail(session.ErrPlugin, fmt.Errorf("tool %s has no Invoke", tu.Name))
	}
	toolCtx, cancel := context.WithCancel(ctx)
	r.setCancel(cancel)
	start := time.Now()
	res, invokeErr, panicked := invokeTool(t, toolCtx, tool.Call{
		ID:        tu.ID,
		Name:      tu.Name,
		Input:     tu.Input,
		Workspace: s.Workspace(),
		SessionID: s.ID(),
	})
	cancel()
	dur := time.Since(start).Milliseconds()
	if panicked != nil {
		// A panic is a plugin fault, not a tool that ran and reported an error.
		return true, r.fail(session.ErrPlugin, fmt.Errorf("tool %s panicked: %v", tu.Name, panicked))
	}
	content := res.Content
	if how := r.takeInterrupt(); how != "" {
		if len(content) == 0 {
			content = []session.Block{session.TextBlock("killed")}
		}
		if _, err := r.append(session.ToolResult{ToolUseID: tu.ID, Outcome: session.OutcomeKilled, Content: content, DurationMS: dur}); err != nil {
			return true, r.fail(session.ErrInternal, err)
		}
		return true, r.finishInterrupt(how)
	}
	if ctx.Err() != nil {
		if len(content) == 0 {
			content = []session.Block{session.TextBlock("killed")}
		}
		if _, err := r.append(session.ToolResult{ToolUseID: tu.ID, Outcome: session.OutcomeKilled, Content: content, DurationMS: dur}); err != nil {
			return true, r.fail(session.ErrInternal, err)
		}
		_ = r.finishInterrupt(session.InterruptCancel)
		return true, ctx.Err()
	}
	outcome := session.OutcomeOK
	switch {
	case invokeErr != nil:
		outcome = session.OutcomeError
		content = []session.Block{session.TextBlock(invokeErr.Error())}
	case res.IsError:
		outcome = session.OutcomeError
	}
	if len(content) == 0 {
		content = []session.Block{session.TextBlock("")}
	}
	if _, err := r.append(session.ToolResult{ToolUseID: tu.ID, Outcome: outcome, Content: content, DurationMS: dur}); err != nil {
		return true, r.fail(session.ErrInternal, err)
	}
	return false, nil
}

func (r *Runner) finishInterrupt(how session.Interrupt) error {
	if how == session.InterruptSteer {
		r.setState(Steering)
		return nil
	}
	r.mu.Lock()
	turn := r.turn
	r.mu.Unlock()
	if _, err := r.append(session.TurnInterrupted{TurnID: turn, How: session.InterruptCancel}); err != nil {
		return r.fail(session.ErrInternal, err)
	}
	r.setState(Idle)
	return nil
}

// invokeTool runs a tool's Invoke and converts a panic into a returned value so the runner
// can record it as a plugin fault instead of crashing the server.
func invokeTool(t tool.Tool, ctx context.Context, call tool.Call) (res tool.Result, err error, panicked any) {
	defer func() {
		if p := recover(); p != nil {
			panicked = p
		}
	}()
	res, err = t.Invoke(ctx, call)
	return res, err, nil
}

// fail records the failure. Retries comes from provider.Error.Attempts when the cause is a
// provider failure and is zero otherwise. A tool whose Invoke returns a non-nil error is a
// tool_result with outcome error, not a failure; ErrPlugin is reserved for a panic or a
// nil Invoke, which are programming faults in a plugin.
func (r *Runner) fail(class session.ErrorClass, err error) error {
	retries := 0
	var pe *provider.Error
	if errors.As(err, &pe) {
		retries = pe.Attempts
	}
	r.mu.Lock()
	turn := r.turn
	r.mu.Unlock()
	if _, aerr := r.cfg.Session.Append(session.TurnFailed{TurnID: turn, Class: class, Message: err.Error(), Retries: retries}); aerr == nil {
		r.notifyLast()
	}
	r.setState(Failed)
	return err
}

func (r *Runner) append(p session.Payload) (session.Entry, error) {
	e, err := r.cfg.Session.Append(p)
	if err != nil {
		return e, err
	}
	r.cfg.Observer.EntryAppended(e)
	return e, nil
}

// appendLocked is append for callers holding r.mu; the observer is invoked after unlock by
// the caller's return path, so it records nothing here beyond the entry.
func (r *Runner) appendLocked(p session.Payload) {
	if e, err := r.cfg.Session.Append(p); err == nil {
		r.cfg.Observer.EntryAppended(e)
	}
}

func (r *Runner) notifyLast() {
	entries := r.cfg.Session.Entries()
	if len(entries) > 0 {
		r.cfg.Observer.EntryAppended(entries[len(entries)-1])
	}
}

func (r *Runner) setState(s State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setStateLocked(s)
}

func (r *Runner) setStateLocked(s State) {
	if r.state == s {
		return
	}
	r.state = s
	if s != Streaming && s != RunningTool && s != AwaitingPermission {
		r.cancel = nil
	}
	id := ""
	if !r.turn.IsZero() {
		id = r.turn.String()
	}
	r.cfg.Observer.StateChanged(id, s)
}

func (r *Runner) setCancel(c context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancel = c
	if r.interrupt != "" {
		c()
	}
}

func (r *Runner) takeInterrupt() session.Interrupt {
	r.mu.Lock()
	defer r.mu.Unlock()
	how := r.interrupt
	r.interrupt = ""
	return how
}

// accumulator folds stream parts into content blocks in arrival order.
type accumulator struct {
	blocksOut []session.Block
	inputs    map[string]*strings.Builder
	open      map[string]int // tool_use id to index in blocksOut
	usage     session.Usage
	stop      session.StopReason
	stopRaw   string
}

func newAccumulator() *accumulator {
	return &accumulator{inputs: map[string]*strings.Builder{}, open: map[string]int{}}
}

func (a *accumulator) add(p provider.Part) {
	switch p.Type {
	case provider.PartTextDelta:
		a.appendText(session.BlockText, p.Text)
	case provider.PartThinkingDelta:
		a.appendText(session.BlockThinking, p.Text)
	case provider.PartToolUseStart:
		a.open[p.ID] = len(a.blocksOut)
		a.inputs[p.ID] = &strings.Builder{}
		a.blocksOut = append(a.blocksOut, session.Block{Type: session.BlockToolUse, ID: p.ID, Name: p.Name})
	case provider.PartToolUseDelta:
		if b, ok := a.inputs[p.ID]; ok {
			b.WriteString(p.Text)
		}
	case provider.PartToolUseEnd:
		a.finishToolUse(p.ID)
	case provider.PartUsage:
		a.usage = a.usage.Add(p.Usage)
	case provider.PartStop:
		a.stop = p.StopReason
		a.stopRaw = p.StopReasonRaw
	}
}

func (a *accumulator) appendText(t session.BlockType, s string) {
	n := len(a.blocksOut)
	if n > 0 && a.blocksOut[n-1].Type == t {
		a.blocksOut[n-1].Text += s
		return
	}
	a.blocksOut = append(a.blocksOut, session.Block{Type: t, Text: s})
}

func (a *accumulator) finishToolUse(id string) {
	i, ok := a.open[id]
	if !ok {
		return
	}
	in := a.inputs[id].String()
	if strings.TrimSpace(in) == "" {
		in = "{}"
	}
	a.blocksOut[i].Input = json.RawMessage(in)
	delete(a.open, id)
	delete(a.inputs, id)
}

// blocks finalizes any still-open tool_use with whatever input has arrived.
func (a *accumulator) blocks() []session.Block {
	for id := range a.open {
		a.finishToolUse(id)
	}
	return a.blocksOut
}

// dropIncompleteToolUses removes tool_use blocks whose input is not valid JSON. They
// only occur in an interrupted message and never run.
func dropIncompleteToolUses(blocks []session.Block) []session.Block {
	out := blocks[:0]
	for _, b := range blocks {
		if b.Type == session.BlockToolUse && !json.Valid(b.Input) {
			continue
		}
		out = append(out, b)
	}
	return out
}
```

Notes for the implementer:

- `Interrupt` holds `r.mu` while cancelling; `setCancel` also takes `r.mu` and fires the new cancel immediately when an interrupt is already pending, so an interrupt that lands between two steps is never lost.
- `fail` appends `turn_failed` directly through the session rather than `append` so a failing observer cannot mask the error path; it then notifies with the last entry.
- The `Steering` branch in `Interrupt` appends under the lock. That is safe because no `Run` is executing while the runner is `Steering`.
- A tool's `Invoke` returning `ctx.Err()` after an interrupt is reported as `killed`, not `error`; the interrupt check runs before the error check.

- [ ] **Step 12: Run the runner tests with the race detector**

Run: `go test -race ./internal/turn/ -v`
Expected: PASS for every test, including the earlier system prompt and assembly tests

- [ ] **Step 13: Commit**

```bash
go vet ./internal/turn/ && gofmt -l internal/turn
git add internal/turn
git commit -m "turn: system prompt, request assembly and the turn runner"
bd close <task-issue-id>
```

### Task 14: Protocol server, live sessions, asker routing and fan-out

**Files:**
- Create: `internal/server/server.go`
- Create: `internal/server/session_live.go`
- Create: `internal/server/conn.go`
- Test: `internal/server/server_test.go`

**Interfaces:**
- Consumes: `session.Store`, `session.Open`, `session.Load`, `(*session.Session).Fork/Append/Entries/Model/Mode/Thinking/Title/Workspace/ID/Close`, `session.ErrLocked`, `session.ErrInvariant`; `provider.Registry.Resolve/Provider/Models/Refresh`; `plugin.Registry` as `turn.Tools` plus `Command(name)`; `gate.Gate`; `turn.NewRunner`, `turn.Config`, `turn.Runner.Run/Interrupt/State/TurnID`, `turn.Asker`, `turn.Observer`, `turn.SystemPrompt`, `turn.State` constants; `protocol.Conn`, `protocol.Request`, `protocol.Response`, `protocol.Error`, every `protocol.Method*`, `protocol.Notify*`, param and result struct; `workspace.Detect(cwd string) (session.Workspace, error)`; `config.Config`.
- Produces: `server.Deps`, `server.New(d Deps) *Server`, `(*Server).Serve(ctx, conn) error`, `(*Server).Shutdown(ctx) error` exactly as in the Interfaces section, plus two response types the header did not list:

```go
// EntryIDResult answers session.set_model, set_mode, set_thinking and set_title.
type EntryIDResult struct {
	EntryID string `json:"entry_id"`
}

// InterruptResult answers session.interrupt.
type InterruptResult struct {
	TurnID string `json:"turn_id"`
	State  string `json:"state"`
}
```

The contracts' `client.hello` carries `{name, version, protocol_version, asker}`; this plan's `protocol.ClientHelloParams` carries `{client, version, asker}` per the Interfaces section, which is authoritative for code. `protocol_version` arrives with the socket transport plan.

- [ ] **Step 1: Claim the task**

```bash
bd create --title "server: protocol dispatch, live sessions, asker routing and fan-out" --type task
bd update <id> --claim
```

- [ ] **Step 2: Write the failing end-to-end test**

`internal/server/server_test.go`:

```go
package server_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/server"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// scriptProvider answers odd calls with a tool_use for "danger" and even calls with "done".
// When block is non-nil it streams one delta and then waits for ctx or block.
type scriptProvider struct {
	mu    sync.Mutex
	calls int
	block chan struct{}
}

func (p *scriptProvider) Name() string { return "fake" }

func (p *scriptProvider) ListModels(ctx context.Context) ([]provider.Model, error) {
	return []provider.Model{{
		Ref:           session.ModelRef{Provider: "fake", Model: "m1"},
		DisplayName:   "Fake 1",
		ContextWindow: 100000,
		Capabilities:  provider.Capabilities{Tools: true},
	}}, nil
}

func (p *scriptProvider) Complete(ctx context.Context, req provider.Request, emit func(provider.Part) error) error {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	if p.block != nil {
		if err := emit(provider.Part{Type: provider.PartTextDelta, Text: "thinking"}); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.block:
		}
	}
	var parts []provider.Part
	if n%2 == 1 {
		parts = []provider.Part{
			{Type: provider.PartTextDelta, Text: "Looking."},
			{Type: provider.PartToolUseStart, ID: "tu" + itoa(n), Name: "danger"},
			{Type: provider.PartToolUseDelta, ID: "tu" + itoa(n), Text: `{"x":1}`},
			{Type: provider.PartToolUseEnd, ID: "tu" + itoa(n)},
			{Type: provider.PartUsage, Usage: session.Usage{Input: 10, Output: 5}},
			{Type: provider.PartStop, StopReason: session.StopToolUse, StopReasonRaw: "tool_calls"},
		}
	} else {
		parts = []provider.Part{
			{Type: provider.PartTextDelta, Text: "done"},
			{Type: provider.PartUsage, Usage: session.Usage{Input: 20, Output: 1}},
			{Type: provider.PartStop, StopReason: session.StopEndTurn, StopReasonRaw: "stop"},
		}
	}
	for _, part := range parts {
		if err := emit(part); err != nil {
			return err
		}
	}
	return nil
}

func itoa(n int) string { return string(rune('0' + n)) }

type fakePlugin struct {
	prov      *scriptProvider
	toolCalls int32
}

func (f *fakePlugin) Name() string { return "fake" }

func (f *fakePlugin) Init(ctx context.Context, h plugin.Host) error {
	if err := h.RegisterProvider(f.prov); err != nil {
		return err
	}
	err := h.RegisterTool(tool.Tool{
		Name:        "danger",
		Description: "an unsafe fake tool",
		Schema:      json.RawMessage(`{"type":"object"}`),
		Safety:      tool.Unsafe,
		Invoke: func(ctx context.Context, call tool.Call) (tool.Result, error) {
			atomic.AddInt32(&f.toolCalls, 1)
			return tool.Result{Content: []session.Block{session.TextBlock("ran")}}, nil
		},
	})
	if err != nil {
		return err
	}
	return h.RegisterCommand(plugin.Command{
		Name:        "hello",
		Description: "submits hi",
		Run: func(ctx context.Context, call plugin.CommandCall) (plugin.Action, error) {
			return plugin.SubmitPrompt{Text: "hi"}, nil
		},
	})
}

type harness struct {
	srv *server.Server
	ws  string
	fp  *fakePlugin
}

func newHarness(t *testing.T, prov *scriptProvider) *harness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	store, err := session.OpenStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	fp := &fakePlugin{prov: prov}
	plugins := plugin.NewRegistry(nil, func(string) {})
	plugins.Load(ctx, fp)
	reg := provider.NewRegistry(filepath.Join(dir, "registry.json"), plugins.Providers()...)
	if err := reg.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Default.Provider = "fake"
	cfg.Default.Model = "m1"
	cfg.Default.Thinking = "off"
	cfg.Permissions.Mode = "strict"
	cfg.MaxTokens = 1000
	srv := server.New(server.Deps{
		Version:  "test",
		Config:   cfg,
		Store:    store,
		Registry: reg,
		Plugins:  plugins,
		Gate:     gate.New(nil),
	})
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return &harness{srv: srv, ws: t.TempDir(), fp: fp}
}

func (h *harness) dial(t *testing.T, asker bool) *protocol.Client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cc, sc := protocol.Pipe()
	go func() { _ = h.srv.Serve(ctx, sc) }()
	cl := protocol.NewClient(cc)
	t.Cleanup(func() { _ = cl.Close(); cancel() })
	var hr protocol.ClientHelloResult
	if err := cl.Call(ctx, protocol.MethodClientHello, protocol.ClientHelloParams{Client: "test", Version: "0", Asker: asker}, &hr); err != nil {
		t.Fatalf("hello: %v", err)
	}
	return cl
}

func (h *harness) open(t *testing.T, cl *protocol.Client) protocol.SessionInfo {
	t.Helper()
	var info protocol.SessionInfo
	if err := cl.Call(context.Background(), protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: h.ws}, &info); err != nil {
		t.Fatalf("open: %v", err)
	}
	return info
}

// drain reads notifications until stop returns true or the deadline passes.
func drain(t *testing.T, cl *protocol.Client, stop func(protocol.Notification) bool) []protocol.Notification {
	t.Helper()
	var got []protocol.Notification
	deadline := time.After(5 * time.Second)
	for {
		select {
		case n := <-cl.Notifications():
			got = append(got, n)
			if stop(n) {
				return got
			}
		case <-deadline:
			t.Fatalf("timeout after %d notifications: %v", len(got), methods(got))
		}
	}
}

func methods(ns []protocol.Notification) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.Method)
	}
	return out
}

func entries(t *testing.T, ns []protocol.Notification) []session.Entry {
	t.Helper()
	var out []session.Entry
	for _, n := range ns {
		if n.Method != protocol.NotifyEntryAppended {
			continue
		}
		var ea protocol.EntryAppended
		if err := json.Unmarshal(n.Params, &ea); err != nil {
			t.Fatal(err)
		}
		out = append(out, ea.Entry)
	}
	return out
}

func kinds(es []session.Entry) []session.Kind {
	out := make([]session.Kind, 0, len(es))
	for _, e := range es {
		out = append(out, e.Kind)
	}
	return out
}

func equalKinds(a, b []session.Kind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTurnWithAskerAllows(t *testing.T) {
	h := newHarness(t, &scriptProvider{})
	cl := h.dial(t, true)
	info := h.open(t, cl)
	ctx := context.Background()
	var sub protocol.SessionSubmitResult
	err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID,
		Content:   []session.Block{session.TextBlock("go")},
		Source:    session.SourceTyped,
	}, &sub)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if sub.TurnID == "" {
		t.Fatal("empty turn id")
	}
	ns := drain(t, cl, func(n protocol.Notification) bool {
		switch n.Method {
		case protocol.NotifyPermissionRequested:
			var pr protocol.PermissionRequested
			_ = json.Unmarshal(n.Params, &pr)
			if pr.Tool != "danger" {
				t.Errorf("asked about %q", pr.Tool)
			}
			go func() {
				_ = cl.Call(ctx, protocol.MethodSessionAnswer, protocol.SessionAnswerParams{
					SessionID: info.SessionID, ToolUseID: pr.ToolUseID,
					Decision: session.Allow, Scope: session.ScopeOnce, Reason: "test allows",
				}, &struct{}{})
			}()
		case protocol.NotifyTurnState:
			var ts protocol.TurnStateChanged
			_ = json.Unmarshal(n.Params, &ts)
			return ts.State == "completed"
		}
		return false
	})
	es := entries(t, ns)
	want := []session.Kind{
		session.KindSessionOpened, session.KindUserMessage, session.KindAssistantMessage,
		session.KindPermissionDecision, session.KindToolResult, session.KindAssistantMessage,
	}
	if !equalKinds(kinds(es), want) {
		t.Fatalf("kinds = %v, want %v", kinds(es), want)
	}
	pd := es[3].Payload.(session.PermissionDecision)
	if pd.Decision != session.Allow || pd.DecidedBy != session.ByAsker || pd.Scope != session.ScopeOnce {
		t.Fatalf("decision = %+v", pd)
	}
	tr := es[4].Payload.(session.ToolResult)
	if tr.Outcome != session.OutcomeOK {
		t.Fatalf("outcome = %s", tr.Outcome)
	}
	if atomic.LoadInt32(&h.fp.toolCalls) != 1 {
		t.Fatalf("tool ran %d times", h.fp.toolCalls)
	}
	// Resume from a second connection replays every entry.
	cl2 := h.dial(t, false)
	var info2 protocol.SessionInfo
	if err := cl2.Call(ctx, protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: info.SessionID}, &info2); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if info2.SessionID != info.SessionID {
		t.Fatalf("resumed %s, want %s", info2.SessionID, info.SessionID)
	}
	seen := 0
	drain(t, cl2, func(n protocol.Notification) bool {
		if n.Method == protocol.NotifyEntryAppended {
			seen++
		}
		return seen == len(want)
	})
}

func TestStrictWithoutAskerDenies(t *testing.T) {
	h := newHarness(t, &scriptProvider{})
	cl := h.dial(t, false)
	info := h.open(t, cl)
	ctx := context.Background()
	var sub protocol.SessionSubmitResult
	if err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Content: []session.Block{session.TextBlock("go")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatal(err)
	}
	ns := drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyTurnState {
			return false
		}
		var ts protocol.TurnStateChanged
		_ = json.Unmarshal(n.Params, &ts)
		return ts.State == "completed"
	})
	es := entries(t, ns)
	var pd *session.PermissionDecision
	var tr *session.ToolResult
	for _, e := range es {
		switch p := e.Payload.(type) {
		case session.PermissionDecision:
			pd = &p
		case session.ToolResult:
			tr = &p
		}
	}
	if pd == nil || pd.Decision != session.Deny || pd.DecidedBy != session.ByNoAsker {
		t.Fatalf("decision = %+v", pd)
	}
	if tr == nil || tr.Outcome != session.OutcomeError {
		t.Fatalf("result = %+v", tr)
	}
	if atomic.LoadInt32(&h.fp.toolCalls) != 0 {
		t.Fatal("tool ran without permission")
	}
	for _, n := range ns {
		if n.Method == protocol.NotifyPermissionRequested {
			t.Fatal("asked a non-asker")
		}
	}
}

func TestInterruptCancelMidStream(t *testing.T) {
	prov := &scriptProvider{block: make(chan struct{})}
	h := newHarness(t, prov)
	cl := h.dial(t, true)
	info := h.open(t, cl)
	ctx := context.Background()
	var sub protocol.SessionSubmitResult
	if err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Content: []session.Block{session.TextBlock("go")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatal(err)
	}
	drain(t, cl, func(n protocol.Notification) bool { return n.Method == protocol.NotifyStreamDelta })
	var ir server.InterruptResult
	if err := cl.Call(ctx, protocol.MethodSessionInterrupt, protocol.SessionInterruptParams{
		SessionID: info.SessionID, How: session.InterruptCancel,
	}, &ir); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	ns := drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyEntryAppended {
			return false
		}
		var ea protocol.EntryAppended
		_ = json.Unmarshal(n.Params, &ea)
		return ea.Entry.Kind == session.KindTurnInterrupted
	})
	last := entries(t, ns)
	ti := last[len(last)-1].Payload.(session.TurnInterrupted)
	if ti.How != session.InterruptCancel {
		t.Fatalf("how = %s", ti.How)
	}
	// A second typed submit now starts a fresh turn instead of a conflict.
	close(prov.block)
	prov.block = nil
	if err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Content: []session.Block{session.TextBlock("again")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatalf("second submit: %v", err)
	}
}

func TestCommandRunSubmitsPrompt(t *testing.T) {
	h := newHarness(t, &scriptProvider{})
	cl := h.dial(t, true)
	info := h.open(t, cl)
	ctx := context.Background()
	var cr protocol.CommandRunResult
	if err := cl.Call(ctx, protocol.MethodCommandRun, protocol.CommandRunParams{SessionID: info.SessionID, Name: "hello"}, &cr); err != nil {
		t.Fatalf("command.run: %v", err)
	}
	if cr.TurnID == "" {
		t.Fatal("command did not start a turn")
	}
	ns := drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyEntryAppended {
			return false
		}
		var ea protocol.EntryAppended
		_ = json.Unmarshal(n.Params, &ea)
		return ea.Entry.Kind == session.KindUserMessage
	})
	es := entries(t, ns)
	um := es[len(es)-1].Payload.(session.UserMessage)
	if um.Content[0].Text != "hi" || um.Source != session.SourceTyped {
		t.Fatalf("user message = %+v", um)
	}
	var missing protocol.CommandRunResult
	err := cl.Call(ctx, protocol.MethodCommandRun, protocol.CommandRunParams{SessionID: info.SessionID, Name: "nope"}, &missing)
	var pe *protocol.Error
	if !errorsAs(err, &pe) || pe.Code != protocol.CodeNotFound {
		t.Fatalf("unknown command err = %v", err)
	}
}

func TestHelloMustComeFirst(t *testing.T) {
	h := newHarness(t, &scriptProvider{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cc, sc := protocol.Pipe()
	go func() { _ = h.srv.Serve(ctx, sc) }()
	cl := protocol.NewClient(cc)
	defer cl.Close()
	var out protocol.SessionListResult
	err := cl.Call(ctx, protocol.MethodSessionList, struct{}{}, &out)
	var pe *protocol.Error
	if !errorsAs(err, &pe) || pe.Code != protocol.CodeUnauthorized {
		t.Fatalf("err = %v", err)
	}
}

func errorsAs(err error, target **protocol.Error) bool {
	for err != nil {
		if pe, ok := err.(*protocol.Error); ok {
			*target = pe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/server/ -run . -v 2>&1 | head -20`
Expected: build failure `undefined: server.New` (the package does not exist yet).

- [ ] **Step 4: Write the connection with its ordered outbox**

`internal/server/conn.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/protocol"
)

// conn is one client connection. Everything sent to the client goes through the outbox so
// notifications and responses leave in the order they were produced.
type conn struct {
	id    int
	c     protocol.Conn
	hello bool
	asker bool

	mu    sync.Mutex
	queue []any
	wake  chan struct{}
	subs  map[ulid.ULID]*liveSession
}

func newConn(id int, c protocol.Conn) *conn {
	return &conn{id: id, c: c, wake: make(chan struct{}, 1), subs: map[ulid.ULID]*liveSession{}}
}

func (cn *conn) send(msg any) {
	cn.mu.Lock()
	cn.queue = append(cn.queue, msg)
	cn.mu.Unlock()
	select {
	case cn.wake <- struct{}{}:
	default:
	}
}

func (cn *conn) notify(method string, params any) {
	b, err := json.Marshal(params)
	if err != nil {
		return
	}
	cn.send(protocol.Request{JSONRPC: "2.0", Method: method, Params: b})
}

// pump drains the outbox to the transport until ctx ends or a send fails.
func (cn *conn) pump(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-cn.wake:
		}
		for {
			cn.mu.Lock()
			if len(cn.queue) == 0 {
				cn.mu.Unlock()
				break
			}
			msg := cn.queue[0]
			cn.queue = cn.queue[1:]
			cn.mu.Unlock()
			if err := cn.c.Send(ctx, msg); err != nil {
				return
			}
		}
	}
}
```

- [ ] **Step 5: Write the live session, asker and fan-out**

`internal/server/session_live.go`:

```go
package server

import (
	"context"
	"errors"
	"sync"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/turn"
)

var errNoAsker = errors.New("server: no asker attached")

// liveSession is a loaded session with its subscribers and, while a turn runs, its runner.
type liveSession struct {
	mu         sync.Mutex
	sess       *session.Session
	model      provider.Model
	runner     *turn.Runner
	turnCancel context.CancelFunc
	conns      []*conn
	pending    map[string]chan turn.Answer
}

func newLive(sess *session.Session, m provider.Model) *liveSession {
	return &liveSession{sess: sess, model: m, pending: map[string]chan turn.Answer{}}
}

// askers must be called with mu held.
func (ls *liveSession) askers() []*conn {
	var out []*conn
	for _, c := range ls.conns {
		if c.asker {
			out = append(out, c)
		}
	}
	return out
}

func (ls *liveSession) subscribe(cn *conn) {
	ls.mu.Lock()
	ls.conns = append(ls.conns, cn)
	ls.mu.Unlock()
	cn.mu.Lock()
	cn.subs[ls.sess.ID()] = ls
	cn.mu.Unlock()
}

// unsubscribe returns how many connections remain and whether a turn is active.
func (ls *liveSession) unsubscribe(cn *conn) (remaining int, active bool) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	kept := ls.conns[:0]
	for _, c := range ls.conns {
		if c != cn {
			kept = append(kept, c)
		}
	}
	ls.conns = kept
	cn.mu.Lock()
	delete(cn.subs, ls.sess.ID())
	cn.mu.Unlock()
	return len(ls.conns), ls.runner != nil && isActive(ls.runner.State())
}

func isActive(s turn.State) bool {
	switch s {
	case turn.Streaming, turn.RunningTool, turn.AwaitingPermission, turn.Steering:
		return true
	}
	return false
}

func (ls *liveSession) broadcast(method string, params any) {
	ls.mu.Lock()
	conns := append([]*conn(nil), ls.conns...)
	ls.mu.Unlock()
	for _, c := range conns {
		c.notify(method, params)
	}
}

func (ls *liveSession) replay(cn *conn) {
	sid := ls.sess.ID().String()
	for _, e := range ls.sess.Entries() {
		cn.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: sid, Entry: e})
	}
}

// appendAndBroadcast is for entries the server appends itself; runner appends reach
// subscribers through the fanout observer.
func (ls *liveSession) appendAndBroadcast(p session.Payload) (session.Entry, error) {
	e, err := ls.sess.Append(p)
	if err != nil {
		return session.Entry{}, err
	}
	ls.broadcast(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: ls.sess.ID().String(), Entry: e})
	return e, nil
}

func (ls *liveSession) info() protocol.SessionInfo {
	return protocol.SessionInfo{
		SessionID: ls.sess.ID().String(),
		Workspace: ls.sess.Workspace(),
		Model:     ls.sess.Model(),
		Mode:      ls.sess.Mode(),
		Thinking:  ls.sess.Thinking(),
		Title:     ls.sess.Title(),
	}
}

// latestEntryID is the newest entry of any of the given kinds.
func (ls *liveSession) latestEntryID(kinds ...session.Kind) string {
	es := ls.sess.Entries()
	for i := len(es) - 1; i >= 0; i-- {
		for _, k := range kinds {
			if es[i].Kind == k {
				return es[i].ID.String()
			}
		}
	}
	return ""
}

// fanout forwards runner events to every subscriber.
type fanout struct {
	ls  *liveSession
	sid string
}

func (f *fanout) EntryAppended(e session.Entry) {
	f.ls.broadcast(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: f.sid, Entry: e})
}

func (f *fanout) Delta(turnID string, p provider.Part) {
	f.ls.broadcast(protocol.NotifyStreamDelta, protocol.StreamDelta{SessionID: f.sid, TurnID: turnID, Part: p})
}

func (f *fanout) StateChanged(turnID string, s turn.State) {
	f.ls.broadcast(protocol.NotifyTurnState, protocol.TurnStateChanged{SessionID: f.sid, TurnID: turnID, State: string(s)})
}

// liveAsker routes a permission question to the asker connections and waits for session.answer.
type liveAsker struct {
	ls     *liveSession
	sid    string
	runner *turn.Runner
}

func (a *liveAsker) Ask(ctx context.Context, q turn.Question) (turn.Answer, error) {
	ch := make(chan turn.Answer, 1)
	a.ls.mu.Lock()
	askers := a.ls.askers()
	if len(askers) > 0 {
		a.ls.pending[q.ToolUseID] = ch
	}
	a.ls.mu.Unlock()
	if len(askers) == 0 {
		return turn.Answer{}, errNoAsker
	}
	turnID := ""
	if a.runner != nil {
		turnID = a.runner.TurnID()
	}
	for _, c := range askers {
		c.notify(protocol.NotifyPermissionRequested, protocol.PermissionRequested{
			SessionID: a.sid, TurnID: turnID, ToolUseID: q.ToolUseID, Tool: q.Tool, Input: q.Input, Matcher: q.Matcher,
		})
	}
	select {
	case ans := <-ch:
		return ans, nil
	case <-ctx.Done():
		a.ls.mu.Lock()
		delete(a.ls.pending, q.ToolUseID)
		a.ls.mu.Unlock()
		return turn.Answer{}, ctx.Err()
	}
}
```

- [ ] **Step 6: Write the server and dispatch**

`internal/server/server.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"sync"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/turn"
	"github.com/guygrigsby/rudy/internal/workspace"
)

type Deps struct {
	Version  string
	Config   *config.Config
	Store    *session.Store
	Registry *provider.Registry
	Plugins  *plugin.Registry
	Gate     *gate.Gate
}

// EntryIDResult answers session.set_model, set_mode, set_thinking and set_title.
type EntryIDResult struct {
	EntryID string `json:"entry_id"`
}

// InterruptResult answers session.interrupt.
type InterruptResult struct {
	TurnID string `json:"turn_id"`
	State  string `json:"state"`
}

type Server struct {
	d      Deps
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	live   map[ulid.ULID]*liveSession
	nextID int
}

func New(d Deps) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{d: d, ctx: ctx, cancel: cancel, live: map[ulid.ULID]*liveSession{}}
}

// Serve runs one connection until it closes.
func (s *Server) Serve(ctx context.Context, c protocol.Conn) error {
	s.mu.Lock()
	s.nextID++
	cn := newConn(s.nextID, c)
	s.mu.Unlock()
	pumpCtx, stopPump := context.WithCancel(ctx)
	go cn.pump(pumpCtx)
	defer func() {
		s.detachAll(cn)
		stopPump()
	}()
	for {
		raw, err := c.Recv(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		var req protocol.Request
		if err := json.Unmarshal(raw, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
			cn.send(protocol.Response{JSONRPC: "2.0", ID: req.ID, Error: perr(protocol.CodeInvalidArgument, "malformed request")})
			continue
		}
		if len(req.ID) == 0 {
			continue // client notifications are not part of this plan
		}
		result, e := s.dispatch(ctx, cn, req)
		resp := protocol.Response{JSONRPC: "2.0", ID: req.ID, Error: e}
		if e == nil {
			b, err := json.Marshal(result)
			if err != nil {
				resp.Error = perr(protocol.CodeUnavailable, err.Error())
			} else {
				resp.Result = b
			}
		}
		cn.send(resp)
	}
}

// Shutdown cancels running turns and closes every live session.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	lives := make([]*liveSession, 0, len(s.live))
	for _, ls := range s.live {
		lives = append(lives, ls)
	}
	s.live = map[ulid.ULID]*liveSession{}
	s.mu.Unlock()
	for _, ls := range lives {
		ls.mu.Lock()
		if ls.runner != nil && isActive(ls.runner.State()) {
			ls.runner.Interrupt(session.InterruptCancel)
		}
		ls.mu.Unlock()
	}
	s.cancel()
	var errs []error
	for _, ls := range lives {
		if err := ls.sess.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func perr(code int, msg string) *protocol.Error {
	return &protocol.Error{Code: code, Message: msg}
}

func decode(raw json.RawMessage, into any) *protocol.Error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return perr(protocol.CodeInvalidArgument, err.Error())
	}
	return nil
}

func (s *Server) dispatch(ctx context.Context, cn *conn, req protocol.Request) (any, *protocol.Error) {
	if req.Method == protocol.MethodClientHello {
		var p protocol.ClientHelloParams
		if e := decode(req.Params, &p); e != nil {
			return nil, e
		}
		if cn.hello {
			return nil, perr(protocol.CodeRefusedByInvariant, "hello already received")
		}
		cn.hello = true
		cn.asker = p.Asker
		return protocol.ClientHelloResult{Server: "rudy", Version: s.d.Version}, nil
	}
	if !cn.hello {
		return nil, perr(protocol.CodeUnauthorized, "client.hello must be the first request")
	}
	switch req.Method {
	case protocol.MethodSessionOpen:
		var p protocol.SessionOpenParams
		if e := decode(req.Params, &p); e != nil {
			return nil, e
		}
		return s.open(cn, p)
	case protocol.MethodSessionResume:
		var p protocol.SessionResumeParams
		if e := decode(req.Params, &p); e != nil {
			return nil, e
		}
		return s.resume(cn, p)
	case protocol.MethodSessionFork:
		var p protocol.SessionForkParams
		if e := decode(req.Params, &p); e != nil {
			return nil, e
		}
		return s.fork(cn, p)
	case protocol.MethodSessionList:
		sums, err := s.d.Store.List()
		if err != nil {
			return nil, perr(protocol.CodeUnavailable, err.Error())
		}
		return protocol.SessionListResult{Sessions: sums}, nil
	case protocol.MethodSessionClose:
		var p protocol.SessionCloseParams
		if e := decode(req.Params, &p); e != nil {
			return nil, e
		}
		ls, e := s.lookup(p.SessionID)
		if e != nil {
			return nil, e
		}
		s.detach(cn, ls)
		return struct{}{}, nil
	case protocol.MethodSessionSubmit:
		var p protocol.SessionSubmitParams
		if e := decode(req.Params, &p); e != nil {
			return nil, e
		}
		ls, e := s.lookup(p.SessionID)
		if e != nil {
			return nil, e
		}
		if len(p.Content) == 0 {
			return nil, perr(protocol.CodeInvalidArgument, "empty content")
		}
		if p.Source == "" {
			p.Source = session.SourceTyped
		}
		if p.Source != session.SourceTyped && p.Source != session.SourceSteer {
			return nil, perr(protocol.CodeInvalidArgument, "source must be typed or steer")
		}
		tid, e := s.startTurn(ls, session.UserMessage{Source: p.Source, Content: p.Content})
		if e != nil {
			return nil, e
		}
		return protocol.SessionSubmitResult{TurnID: tid}, nil
	case protocol.MethodSessionInterrupt:
		var p protocol.SessionInterruptParams
		if e := decode(req.Params, &p); e != nil {
			return nil, e
		}
		ls, e := s.lookup(p.SessionID)
		if e != nil {
			return nil, e
		}
		if !p.How.Valid() {
			return nil, perr(protocol.CodeInvalidArgument, "how must be steer or cancel")
		}
		ls.mu.Lock()
		r := ls.runner
		ls.mu.Unlock()
		if r == nil || !isActive(r.State()) {
			return nil, perr(protocol.CodeRefusedByInvariant, "no active turn")
		}
		r.Interrupt(p.How)
		return InterruptResult{TurnID: r.TurnID(), State: string(r.State())}, nil
	case protocol.MethodSessionAnswer:
		var p protocol.SessionAnswerParams
		if e := decode(req.Params, &p); e != nil {
			return nil, e
		}
		if !cn.asker {
			return nil, perr(protocol.CodeUnauthorized, "connection did not declare asker")
		}
		ls, e := s.lookup(p.SessionID)
		if e != nil {
			return nil, e
		}
		if !p.Decision.Valid() || !p.Scope.Valid() {
			return nil, perr(protocol.CodeInvalidArgument, "invalid decision or scope")
		}
		ls.mu.Lock()
		ch, ok := ls.pending[p.ToolUseID]
		delete(ls.pending, p.ToolUseID)
		ls.mu.Unlock()
		if !ok {
			return nil, perr(protocol.CodeNotFound, "no pending question for "+p.ToolUseID)
		}
		ch <- turn.Answer{Decision: p.Decision, Scope: p.Scope, Reason: p.Reason}
		return struct{}{}, nil
	case protocol.MethodSessionSetModel:
		var p protocol.SessionSetModelParams
		if e := decode(req.Params, &p); e != nil {
			return nil, e
		}
		ls, e := s.lookup(p.SessionID)
		if e != nil {
			return nil, e
		}
		m, err := s.d.Registry.Resolve(p.Model)
		if err != nil {
			return nil, perr(protocol.CodeNotFound, err.Error())
		}
		ls.mu.Lock()
		defer ls.mu.Unlock()
		if m.Ref == ls.sess.Model() {
			return EntryIDResult{EntryID: ls.latestEntryID(session.KindModelChange, session.KindSessionOpened)}, nil
		}
		e2, err := ls.appendAndBroadcast(session.ModelChange{Model: m.Ref})
		if err != nil {
			return nil, perr(protocol.CodeRefusedByInvariant, err.Error())
		}
		ls.model = m
		return EntryIDResult{EntryID: e2.ID.String()}, nil
	case protocol.MethodSessionSetMode:
		var p protocol.SessionSetModeParams
		if e := decode(req.Params, &p); e != nil {
			return nil, e
		}
		if !p.Mode.Valid() {
			return nil, perr(protocol.CodeInvalidArgument, "invalid mode")
		}
		return s.setEntry(p.SessionID, session.KindModeChange, func(ls *liveSession) (bool, session.Payload) {
			return ls.sess.Mode() == p.Mode, session.ModeChange{Mode: p.Mode}
		})
	case protocol.MethodSessionSetThinking:
		var p protocol.SessionSetThinkingParams
		if e := decode(req.Params, &p); e != nil {
			return nil, e
		}
		if !p.Thinking.Valid() {
			return nil, perr(protocol.CodeInvalidArgument, "invalid thinking level")
		}
		return s.setEntry(p.SessionID, session.KindThinkingChange, func(ls *liveSession) (bool, session.Payload) {
			return ls.sess.Thinking() == p.Thinking, session.ThinkingChange{Thinking: p.Thinking}
		})
	case protocol.MethodSessionSetTitle:
		var p protocol.SessionSetTitleParams
		if e := decode(req.Params, &p); e != nil {
			return nil, e
		}
		if p.Title == "" {
			return nil, perr(protocol.CodeInvalidArgument, "empty title")
		}
		return s.setEntry(p.SessionID, session.KindTitleChange, func(ls *liveSession) (bool, session.Payload) {
			return ls.sess.Title() == p.Title, session.TitleChange{Title: p.Title}
		})
	case protocol.MethodRegistryList:
		return protocol.RegistryListResult{Models: s.d.Registry.Models()}, nil
	case protocol.MethodRegistryRefresh:
		if err := s.d.Registry.Refresh(ctx); err != nil {
			cn.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "warn", Text: "registry refresh: " + err.Error()})
		}
		return protocol.RegistryListResult{Models: s.d.Registry.Models()}, nil
	case protocol.MethodCommandRun:
		var p protocol.CommandRunParams
		if e := decode(req.Params, &p); e != nil {
			return nil, e
		}
		ls, e := s.lookup(p.SessionID)
		if e != nil {
			return nil, e
		}
		cmd, ok := s.d.Plugins.Command(p.Name)
		if !ok {
			return nil, perr(protocol.CodeNotFound, "unknown command /"+p.Name)
		}
		act, err := cmd.Run(ctx, plugin.CommandCall{SessionID: ls.sess.ID(), Workspace: ls.sess.Workspace(), Args: p.Args})
		if err != nil {
			return nil, perr(protocol.CodePluginError, err.Error())
		}
		switch a := act.(type) {
		case plugin.SubmitPrompt:
			tid, e := s.startTurn(ls, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock(a.Text)}})
			if e != nil {
				return nil, e
			}
			return protocol.CommandRunResult{TurnID: tid}, nil
		case plugin.Notice:
			cn.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "info", Text: a.Text})
			return protocol.CommandRunResult{Notice: a.Text}, nil
		default:
			return protocol.CommandRunResult{}, nil
		}
	default:
		return nil, perr(protocol.CodeNotFound, "unknown method "+req.Method)
	}
}

func (s *Server) lookup(id string) (*liveSession, *protocol.Error) {
	sid, err := ulid.Parse(id)
	if err != nil {
		return nil, perr(protocol.CodeInvalidArgument, "bad session id")
	}
	s.mu.Lock()
	ls, ok := s.live[sid]
	s.mu.Unlock()
	if !ok {
		return nil, perr(protocol.CodeNotFound, "session not open: "+id)
	}
	return ls, nil
}

func (s *Server) setEntry(id string, kind session.Kind, f func(*liveSession) (same bool, p session.Payload)) (any, *protocol.Error) {
	ls, e := s.lookup(id)
	if e != nil {
		return nil, e
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	same, p := f(ls)
	if same {
		return EntryIDResult{EntryID: ls.latestEntryID(kind, session.KindSessionOpened)}, nil
	}
	e2, err := ls.appendAndBroadcast(p)
	if err != nil {
		return nil, perr(protocol.CodeRefusedByInvariant, err.Error())
	}
	return EntryIDResult{EntryID: e2.ID.String()}, nil
}

func (s *Server) open(cn *conn, p protocol.SessionOpenParams) (any, *protocol.Error) {
	ws, err := workspace.Detect(p.Cwd)
	if err != nil {
		return nil, perr(protocol.CodeInvalidArgument, err.Error())
	}
	spec := p.Model
	if spec == "" {
		spec = s.d.Config.Default.Provider + ":" + s.d.Config.Default.Model
	}
	m, err := s.d.Registry.Resolve(spec)
	if err != nil {
		return nil, perr(protocol.CodeNotFound, err.Error())
	}
	mode := session.Mode(p.Mode)
	if mode == "" {
		mode = session.Mode(s.d.Config.Permissions.Mode)
	}
	thinking := session.ThinkingLevel(p.Thinking)
	if thinking == "" {
		thinking = session.ThinkingLevel(s.d.Config.Default.Thinking)
	}
	if !mode.Valid() || !thinking.Valid() {
		return nil, perr(protocol.CodeInvalidArgument, "invalid mode or thinking level")
	}
	agent := p.Agent
	if agent == "" {
		agent = "default"
	}
	sess, err := session.Open(s.d.Store, session.SessionOpened{
		SchemaVersion: 1, RudyVersion: s.d.Version, Workspace: ws, Model: m.Ref, Thinking: thinking, Mode: mode, Agent: agent,
	})
	if err != nil {
		return nil, perr(protocol.CodeUnavailable, err.Error())
	}
	ls := newLive(sess, m)
	s.mu.Lock()
	s.live[sess.ID()] = ls
	s.mu.Unlock()
	s.attach(cn, ls)
	return ls.info(), nil
}

func (s *Server) resume(cn *conn, p protocol.SessionResumeParams) (any, *protocol.Error) {
	sid, err := ulid.Parse(p.SessionID)
	if err != nil {
		return nil, perr(protocol.CodeInvalidArgument, "bad session id")
	}
	s.mu.Lock()
	ls, ok := s.live[sid]
	s.mu.Unlock()
	if !ok {
		sess, err := session.Load(s.d.Store, sid)
		if err != nil {
			return nil, loadErr(err)
		}
		m, err := s.d.Registry.Resolve(sess.Model().String())
		if err != nil {
			m = provider.Model{Ref: sess.Model()}
			cn.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "warn", Text: "model not in registry: " + sess.Model().String()})
		}
		ls = newLive(sess, m)
		s.mu.Lock()
		if existing, raced := s.live[sid]; raced {
			ls = existing
			_ = sess.Close()
		} else {
			s.live[sid] = ls
		}
		s.mu.Unlock()
	}
	s.attach(cn, ls)
	return ls.info(), nil
}

func (s *Server) fork(cn *conn, p protocol.SessionForkParams) (any, *protocol.Error) {
	sid, err := ulid.Parse(p.SessionID)
	if err != nil {
		return nil, perr(protocol.CodeInvalidArgument, "bad session id")
	}
	at, err := ulid.Parse(p.AtEntryID)
	if err != nil {
		return nil, perr(protocol.CodeInvalidArgument, "bad entry id")
	}
	s.mu.Lock()
	parent, ok := s.live[sid]
	s.mu.Unlock()
	var child *session.Session
	if ok {
		parent.mu.Lock()
		child, err = parent.sess.Fork(s.d.Store, at)
		parent.mu.Unlock()
	} else {
		loaded, lerr := session.Load(s.d.Store, sid)
		if lerr != nil {
			return nil, loadErr(lerr)
		}
		child, err = loaded.Fork(s.d.Store, at)
		_ = loaded.Close()
	}
	if err != nil {
		if errors.Is(err, session.ErrInvariant) {
			return nil, perr(protocol.CodeNotFound, err.Error())
		}
		return nil, perr(protocol.CodeUnavailable, err.Error())
	}
	m, err := s.d.Registry.Resolve(child.Model().String())
	if err != nil {
		m = provider.Model{Ref: child.Model()}
	}
	ls := newLive(child, m)
	s.mu.Lock()
	s.live[child.ID()] = ls
	s.mu.Unlock()
	s.attach(cn, ls)
	return ls.info(), nil
}

func loadErr(err error) *protocol.Error {
	switch {
	case errors.Is(err, session.ErrLocked):
		return perr(protocol.CodeUnavailable, err.Error())
	case errors.Is(err, fs.ErrNotExist):
		return perr(protocol.CodeNotFound, err.Error())
	default:
		return perr(protocol.CodeUnavailable, err.Error())
	}
}

func (s *Server) attach(cn *conn, ls *liveSession) {
	ls.subscribe(cn)
	ls.replay(cn)
}

// detach drops one subscription and closes the session when nothing else holds it.
func (s *Server) detach(cn *conn, ls *liveSession) {
	remaining, active := ls.unsubscribe(cn)
	if remaining > 0 || active {
		return
	}
	s.mu.Lock()
	delete(s.live, ls.sess.ID())
	s.mu.Unlock()
	_ = ls.sess.Close()
}

func (s *Server) detachAll(cn *conn) {
	cn.mu.Lock()
	subs := make([]*liveSession, 0, len(cn.subs))
	for _, ls := range cn.subs {
		subs = append(subs, ls)
	}
	cn.mu.Unlock()
	for _, ls := range subs {
		s.detach(cn, ls)
	}
}

// startTurn appends nothing itself; the runner appends the user message. It refuses a typed
// message while a turn is active and a steer message unless the turn is steering.
func (s *Server) startTurn(ls *liveSession, msg session.UserMessage) (string, *protocol.Error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.runner != nil {
		st := ls.runner.State()
		if msg.Source == session.SourceSteer {
			if st != turn.Steering {
				return "", perr(protocol.CodeRefusedByInvariant, "session is not steering")
			}
			r := ls.runner
			go s.runTurn(ls, r, msg)
			return r.TurnID(), nil
		}
		if isActive(st) {
			return "", perr(protocol.CodeConflict, "a turn is active")
		}
	} else if msg.Source == session.SourceSteer {
		return "", perr(protocol.CodeRefusedByInvariant, "session is not steering")
	}
	prov, ok := s.d.Registry.Provider(ls.model.Ref.Provider)
	if !ok {
		return "", perr(protocol.CodeUnavailable, "provider not loaded: "+ls.model.Ref.Provider)
	}
	sid := ls.sess.ID().String()
	la := &liveAsker{ls: ls, sid: sid}
	var asker turn.Asker
	if len(ls.askers()) > 0 {
		asker = la
	}
	r := turn.NewRunner(turn.Config{
		Session:   ls.sess,
		Provider:  prov,
		Model:     ls.model,
		Tools:     s.d.Plugins,
		Gate:      s.d.Gate,
		Asker:     asker,
		Observer:  &fanout{ls: ls, sid: sid},
		System:    turn.SystemPrompt(ls.sess.Workspace(), s.d.Version),
		MaxTokens: s.d.Config.MaxTokens,
	})
	la.runner = r
	ls.runner = r
	go s.runTurn(ls, r, msg)
	return r.TurnID(), nil
}

func (s *Server) runTurn(ls *liveSession, r *turn.Runner, msg session.UserMessage) {
	ctx, cancel := context.WithCancel(s.ctx)
	ls.mu.Lock()
	ls.turnCancel = cancel
	ls.mu.Unlock()
	_ = r.Run(ctx, msg) // failures are already recorded as turn_failed entries by the runner
	cancel()
	ls.mu.Lock()
	if ls.runner == r && r.State() != turn.Steering {
		ls.runner = nil
	}
	ls.mu.Unlock()
}
```

- [ ] **Step 7: Run the tests**

Run: `go test -race ./internal/server/ -v`
Expected: `PASS` for `TestTurnWithAskerAllows`, `TestStrictWithoutAskerDenies`, `TestInterruptCancelMidStream`, `TestCommandRunSubmitsPrompt`, `TestHelloMustComeFirst`.

If `TestInterruptCancelMidStream` hangs on the second submit, the runner did not return to `Idle` after cancel; that is a Task 13 defect, not a server one. Check `r.State()` after `Interrupt(InterruptCancel)`.

- [ ] **Step 8: Lint and commit**

```bash
make check
git add internal/server
git commit -m "server: protocol dispatch, live sessions, asker routing and fan-out"
bd close <id>
```

---

### Task 15: Built-in tool plugins

Six plugins, one package each, plus one shared helper package created in 15a. Every path a tool receives is confined to the workspace root through `os.OpenRoot`; a path that resolves outside the root is a tool error, never a filesystem access.

#### Task 15a: read tool and the fsroot helper

**Files:**
- Create: `internal/plugins/tools/fsroot/fsroot.go`
- Create: `internal/plugins/tools/read/read.go`
- Test: `internal/plugins/tools/fsroot/fsroot_test.go`
- Test: `internal/plugins/tools/read/read_test.go`

**Interfaces:**
- Consumes: `tool.Tool`, `tool.Call`, `tool.Result`, `tool.Safe`, `plugin.Plugin`, `plugin.Host.RegisterTool`, `session.TextBlock`.
- Produces:

```go
package fsroot

var ErrEscape = errors.New("path escapes the workspace")

// Rel converts p to a path relative to root for use with os.Root. Absolute paths inside root
// are rebased. A path that would leave root is ErrEscape.
func Rel(root, p string) (string, error)
// WriteAtomic writes rel inside root through a temp file and rename, creating parent dirs.
func WriteAtomic(root *os.Root, rel string, data []byte) error
// IsBinary reports a NUL byte in the first 8000 bytes.
func IsBinary(b []byte) bool
// Text and Fail build tool results.
func Text(s string) tool.Result
func Fail(format string, args ...any) tool.Result
```

and `read.New() plugin.Plugin` registering tool `read`.

- [ ] **Step 1: Claim**

```bash
bd create --title "tools: read and the fsroot helper" --type task
bd update <id> --claim
```

- [ ] **Step 2: Write the failing fsroot test**

`internal/plugins/tools/fsroot/fsroot_test.go`:

```go
package fsroot_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugins/tools/fsroot"
)

func TestRel(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		in   string
		want string
		err  bool
	}{
		{"a.txt", "a.txt", false},
		{"./sub/b.txt", "sub/b.txt", false},
		{filepath.Join(root, "sub", "c.txt"), "sub/c.txt", false},
		{"../etc/passwd", "", true},
		{"sub/../../x", "", true},
		{"/etc/passwd", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := fsroot.Rel(root, c.in)
		if (err != nil) != c.err {
			t.Errorf("Rel(%q) err = %v, want err %v", c.in, err, c.err)
			continue
		}
		if !c.err && filepath.ToSlash(got) != c.want {
			t.Errorf("Rel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWriteAtomicCreatesParents(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := fsroot.WriteAtomic(root, "a/b/c.txt", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "a", "b", "c.txt"))
	if err != nil || string(b) != "hi" {
		t.Fatalf("read back %q, %v", b, err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "a", "b"))
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %d entries", len(entries))
	}
}

func TestIsBinary(t *testing.T) {
	if fsroot.IsBinary([]byte("plain text\n")) {
		t.Fatal("text flagged binary")
	}
	if !fsroot.IsBinary([]byte("ab\x00cd")) {
		t.Fatal("NUL not flagged")
	}
}
```

- [ ] **Step 3: Write the failing read test**

`internal/plugins/tools/read/read_test.go`:

```go
package read_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/read"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// captureHost records the one tool a plugin registers.
type captureHost struct{ tools []tool.Tool }

func (h *captureHost) RegisterTool(t tool.Tool) error             { h.tools = append(h.tools, t); return nil }
func (h *captureHost) RegisterCommand(plugin.Command) error       { return nil }
func (h *captureHost) RegisterProvider(provider.Provider) error   { return nil }
func (h *captureHost) Config() map[string]any                     { return nil }
func (h *captureHost) Notice(string)                              {}

func load(t *testing.T, p plugin.Plugin) tool.Tool {
	t.Helper()
	h := &captureHost{}
	if err := p.Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if len(h.tools) != 1 {
		t.Fatalf("registered %d tools", len(h.tools))
	}
	return h.tools[0]
}

func call(t *testing.T, tl tool.Tool, root string, input string) tool.Result {
	t.Helper()
	res, err := tl.Invoke(context.Background(), tool.Call{
		ID: "tu1", Name: tl.Name, Input: json.RawMessage(input),
		Workspace: session.Workspace{Root: root, ProjectID: "local/x"},
	})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	return res
}

func text(r tool.Result) string { return r.Content[0].Text }

func TestReadNumbersLines(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("one\ntwo\nthree\nfour\nfive\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := load(t, read.New())
	if tl.Name != "read" || tl.Safety != tool.Safe {
		t.Fatalf("tool = %+v", tl)
	}
	res := call(t, tl, ws, `{"path":"a.txt"}`)
	if res.IsError || !strings.HasPrefix(text(res), "1\tone\n2\ttwo\n") {
		t.Fatalf("got %q err=%v", text(res), res.IsError)
	}
	res = call(t, tl, ws, `{"path":"a.txt","offset":4,"limit":1}`)
	if text(res) != "4\tfour\n… 1 more lines\n" {
		t.Fatalf("got %q", text(res))
	}
	res = call(t, tl, ws, `{"path":"a.txt","offset":9}`)
	if !res.IsError || !strings.Contains(text(res), "past the end") {
		t.Fatalf("got %q err=%v", text(res), res.IsError)
	}
}

func TestReadRefusesEscapeBinaryAndMissing(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "bin"), []byte("ab\x00cd"), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := load(t, read.New())
	res := call(t, tl, ws, `{"path":"../etc/passwd"}`)
	if !res.IsError || !strings.Contains(text(res), "escapes") {
		t.Fatalf("escape: %q err=%v", text(res), res.IsError)
	}
	res = call(t, tl, ws, `{"path":"bin"}`)
	if !res.IsError || !strings.Contains(text(res), "binary") {
		t.Fatalf("binary: %q", text(res))
	}
	res = call(t, tl, ws, `{"path":"nope.txt"}`)
	if !res.IsError {
		t.Fatal("missing file did not error")
	}
}
```

- [ ] **Step 4: Run both to verify they fail**

Run: `go test ./internal/plugins/tools/... 2>&1 | head -5`
Expected: build failure, `package github.com/guygrigsby/rudy/internal/plugins/tools/fsroot is not in std` or `undefined: read.New`.

- [ ] **Step 5: Write fsroot**

`internal/plugins/tools/fsroot/fsroot.go`:

```go
// Package fsroot confines tool file access to the workspace root.
package fsroot

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

var ErrEscape = errors.New("path escapes the workspace")

// Rel converts p to a path relative to root for use with os.Root. Absolute paths inside root
// are rebased. A path that would leave root is ErrEscape. Symlink escapes are caught later
// by os.Root itself.
func Rel(root, p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(p) {
		r, err := filepath.Rel(root, p)
		if err != nil {
			return "", ErrEscape
		}
		p = r
	}
	p = filepath.Clean(p)
	if p == ".." || strings.HasPrefix(p, ".."+string(filepath.Separator)) {
		return "", ErrEscape
	}
	return p, nil
}

// WriteAtomic writes data to rel inside root through a temp file and rename.
func WriteAtomic(root *os.Root, rel string, data []byte) error {
	if dir := filepath.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := fmt.Sprintf("%s.rudy-%d.tmp", rel, os.Getpid())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = root.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return root.Rename(tmp, rel)
}

// IsBinary reports a NUL byte within the first 8000 bytes.
func IsBinary(b []byte) bool {
	n := min(len(b), 8000)
	return bytes.IndexByte(b[:n], 0) >= 0
}

func Text(s string) tool.Result {
	return tool.Result{Content: []session.Block{session.TextBlock(s)}}
}

func Fail(format string, args ...any) tool.Result {
	return tool.Result{Content: []session.Block{session.TextBlock(fmt.Sprintf(format, args...))}, IsError: true}
}
```

- [ ] **Step 6: Write the read plugin**

`internal/plugins/tools/read/read.go`:

```go
// Package read is the built-in read tool: a numbered window of a text file.
package read

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/fsroot"
	"github.com/guygrigsby/rudy/internal/tool"
)

const schema = `{"type":"object","properties":{"path":{"type":"string","description":"File path, relative to the workspace root or absolute inside it"},"offset":{"type":"integer","minimum":1,"description":"First line to return, 1-based, default 1"},"limit":{"type":"integer","minimum":1,"description":"Maximum lines to return, default 2000"}},"required":["path"],"additionalProperties":false}`

const (
	defaultLimit = 2000
	maxLineBytes = 2000
)

type args struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

type readPlugin struct{}

func New() plugin.Plugin { return readPlugin{} }

func (readPlugin) Name() string { return "tools.read" }

func (readPlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterTool(tool.Tool{
		Name:        "read",
		Description: "Read a text file from the workspace. Output is one line per row as LINE<TAB>TEXT. Use offset and limit to page through large files.",
		Schema:      json.RawMessage(schema),
		Safety:      tool.Safe,
		Invoke:      invoke,
	})
}

func invoke(ctx context.Context, call tool.Call) (tool.Result, error) {
	var a args
	if err := json.Unmarshal(call.Input, &a); err != nil {
		return fsroot.Fail("read: bad input: %v", err), nil
	}
	if a.Offset < 1 {
		a.Offset = 1
	}
	if a.Limit < 1 {
		a.Limit = defaultLimit
	}
	rel, err := fsroot.Rel(call.Workspace.Root, a.Path)
	if err != nil {
		return fsroot.Fail("read: %v", err), nil
	}
	root, err := os.OpenRoot(call.Workspace.Root)
	if err != nil {
		return tool.Result{}, err
	}
	defer root.Close()
	f, err := root.Open(rel)
	if err != nil {
		return fsroot.Fail("read: %v", err), nil
	}
	defer f.Close()
	head := make([]byte, 8000)
	n, _ := io.ReadFull(f, head)
	if fsroot.IsBinary(head[:n]) {
		return fsroot.Fail("read: %s is a binary file", a.Path), nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return tool.Result{}, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var b strings.Builder
	line, shown, more := 0, 0, 0
	for sc.Scan() {
		line++
		if line < a.Offset {
			continue
		}
		if shown >= a.Limit {
			more++
			continue
		}
		text := sc.Text()
		if len(text) > maxLineBytes {
			text = text[:maxLineBytes] + " … [line truncated]"
		}
		fmt.Fprintf(&b, "%d\t%s\n", line, text)
		shown++
	}
	if err := sc.Err(); err != nil {
		return fsroot.Fail("read: %v", err), nil
	}
	if shown == 0 && line < a.Offset {
		return fsroot.Fail("read: %s has %d lines, offset %d is past the end", a.Path, line, a.Offset), nil
	}
	if more > 0 {
		fmt.Fprintf(&b, "… %d more lines\n", more)
	}
	return fsroot.Text(b.String()), nil
}
```

- [ ] **Step 7: Run the tests**

Run: `go test -race ./internal/plugins/tools/... -v`
Expected: `PASS` for `TestRel`, `TestWriteAtomicCreatesParents`, `TestIsBinary`, `TestReadNumbersLines`, `TestReadRefusesEscapeBinaryAndMissing`.

- [ ] **Step 8: Commit**

```bash
git add internal/plugins/tools/fsroot internal/plugins/tools/read
git commit -m "tools: read"
bd close <id>
```

#### Task 15b: write tool

**Files:**
- Create: `internal/plugins/tools/write/write.go`
- Test: `internal/plugins/tools/write/write_test.go`

**Interfaces:**
- Consumes: `fsroot.Rel`, `fsroot.WriteAtomic`, `fsroot.Text`, `fsroot.Fail`; the `captureHost`, `load` and `call` helpers are repeated per test package since they are eight lines.
- Produces: `write.New() plugin.Plugin` registering tool `write`, unsafe.

- [ ] **Step 1: Claim**

```bash
bd create --title "tools: write" --type task
bd update <id> --claim
```

- [ ] **Step 2: Write the failing test**

`internal/plugins/tools/write/write_test.go`:

```go
package write_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/write"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

type captureHost struct{ tools []tool.Tool }

func (h *captureHost) RegisterTool(t tool.Tool) error           { h.tools = append(h.tools, t); return nil }
func (h *captureHost) RegisterCommand(plugin.Command) error     { return nil }
func (h *captureHost) RegisterProvider(provider.Provider) error { return nil }
func (h *captureHost) Config() map[string]any                   { return nil }
func (h *captureHost) Notice(string)                            {}

func load(t *testing.T) tool.Tool {
	t.Helper()
	h := &captureHost{}
	if err := write.New().Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	return h.tools[0]
}

func call(t *testing.T, tl tool.Tool, root, input string) tool.Result {
	t.Helper()
	res, err := tl.Invoke(context.Background(), tool.Call{ID: "tu1", Name: tl.Name, Input: json.RawMessage(input), Workspace: session.Workspace{Root: root}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestWriteCreatesAndReports(t *testing.T) {
	ws := t.TempDir()
	tl := load(t)
	if tl.Name != "write" || tl.Safety != tool.Unsafe {
		t.Fatalf("tool = %+v", tl)
	}
	res := call(t, tl, ws, `{"path":"sub/dir/new.txt","content":"hello\n"}`)
	if res.IsError || res.Content[0].Text != "wrote 6 bytes to sub/dir/new.txt" {
		t.Fatalf("got %q err=%v", res.Content[0].Text, res.IsError)
	}
	b, err := os.ReadFile(filepath.Join(ws, "sub", "dir", "new.txt"))
	if err != nil || string(b) != "hello\n" {
		t.Fatalf("file = %q, %v", b, err)
	}
	res = call(t, tl, ws, `{"path":"sub/dir/new.txt","content":"replaced"}`)
	b, _ = os.ReadFile(filepath.Join(ws, "sub", "dir", "new.txt"))
	if res.IsError || string(b) != "replaced" {
		t.Fatalf("overwrite: %q err=%v", b, res.IsError)
	}
}

func TestWriteRefusesEscape(t *testing.T) {
	ws := t.TempDir()
	res := call(t, load(t), ws, `{"path":"../escape.txt","content":"x"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "escapes") {
		t.Fatalf("got %q err=%v", res.Content[0].Text, res.IsError)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(ws), "escape.txt")); err == nil {
		t.Fatal("file written outside the workspace")
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/plugins/tools/write/ 2>&1 | head -3`
Expected: `undefined: write.New`.

- [ ] **Step 4: Write the plugin**

`internal/plugins/tools/write/write.go`:

```go
// Package write is the built-in write tool: create or replace one file.
package write

import (
	"context"
	"encoding/json"
	"os"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/fsroot"
	"github.com/guygrigsby/rudy/internal/tool"
)

const schema = `{"type":"object","properties":{"path":{"type":"string","description":"File path, relative to the workspace root or absolute inside it"},"content":{"type":"string","description":"The complete new file content"}},"required":["path","content"],"additionalProperties":false}`

type args struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type writePlugin struct{}

func New() plugin.Plugin { return writePlugin{} }

func (writePlugin) Name() string { return "tools.write" }

func (writePlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterTool(tool.Tool{
		Name:        "write",
		Description: "Create or overwrite a file in the workspace with the given content. Parent directories are created. Prefer edit for changes to an existing file.",
		Schema:      json.RawMessage(schema),
		Safety:      tool.Unsafe,
		Invoke:      invoke,
	})
}

func invoke(ctx context.Context, call tool.Call) (tool.Result, error) {
	var a args
	if err := json.Unmarshal(call.Input, &a); err != nil {
		return fsroot.Fail("write: bad input: %v", err), nil
	}
	rel, err := fsroot.Rel(call.Workspace.Root, a.Path)
	if err != nil {
		return fsroot.Fail("write: %v", err), nil
	}
	root, err := os.OpenRoot(call.Workspace.Root)
	if err != nil {
		return tool.Result{}, err
	}
	defer root.Close()
	if err := fsroot.WriteAtomic(root, rel, []byte(a.Content)); err != nil {
		return fsroot.Fail("write: %v", err), nil
	}
	return fsroot.Text("wrote " + itoa(len(a.Content)) + " bytes to " + a.Path), nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
```

Replace `itoa` with `strconv.Itoa` and import `strconv`; the helper above exists only so the step is complete on its own. The committed file uses `strconv.Itoa`.

- [ ] **Step 5: Run the tests**

Run: `go test -race ./internal/plugins/tools/write/ -v`
Expected: `PASS` for both tests.

- [ ] **Step 6: Commit**

```bash
git add internal/plugins/tools/write
git commit -m "tools: write"
bd close <id>
```

#### Task 15c: edit tool

**Files:**
- Create: `internal/plugins/tools/edit/edit.go`
- Test: `internal/plugins/tools/edit/edit_test.go`

**Interfaces:**
- Consumes: `fsroot.Rel`, `fsroot.WriteAtomic`, `fsroot.IsBinary`, `fsroot.Text`, `fsroot.Fail`.
- Produces: `edit.New() plugin.Plugin` registering tool `edit`, unsafe.

- [ ] **Step 1: Claim**

```bash
bd create --title "tools: edit" --type task
bd update <id> --claim
```

- [ ] **Step 2: Write the failing test**

`internal/plugins/tools/edit/edit_test.go`:

```go
package edit_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/edit"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

type captureHost struct{ tools []tool.Tool }

func (h *captureHost) RegisterTool(t tool.Tool) error           { h.tools = append(h.tools, t); return nil }
func (h *captureHost) RegisterCommand(plugin.Command) error     { return nil }
func (h *captureHost) RegisterProvider(provider.Provider) error { return nil }
func (h *captureHost) Config() map[string]any                   { return nil }
func (h *captureHost) Notice(string)                            {}

func load(t *testing.T) tool.Tool {
	t.Helper()
	h := &captureHost{}
	if err := edit.New().Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	return h.tools[0]
}

func call(t *testing.T, tl tool.Tool, root, input string) tool.Result {
	t.Helper()
	res, err := tl.Invoke(context.Background(), tool.Call{ID: "tu1", Name: tl.Name, Input: json.RawMessage(input), Workspace: session.Workspace{Root: root}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func write(t *testing.T, ws, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, ws, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(ws, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEditReplacesUniqueMatch(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "f.go", "a := 1\nb := 2\n")
	tl := load(t)
	if tl.Name != "edit" || tl.Safety != tool.Unsafe {
		t.Fatalf("tool = %+v", tl)
	}
	res := call(t, tl, ws, `{"path":"f.go","old":"b := 2","new":"b := 3"}`)
	if res.IsError || res.Content[0].Text != "replaced 1 occurrence in f.go" {
		t.Fatalf("got %q err=%v", res.Content[0].Text, res.IsError)
	}
	if read(t, ws, "f.go") != "a := 1\nb := 3\n" {
		t.Fatalf("file = %q", read(t, ws, "f.go"))
	}
}

func TestEditRefusesAmbiguousUnlessReplaceAll(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "f.txt", "x x x")
	tl := load(t)
	res := call(t, tl, ws, `{"path":"f.txt","old":"x","new":"y"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "3 times") {
		t.Fatalf("got %q err=%v", res.Content[0].Text, res.IsError)
	}
	res = call(t, tl, ws, `{"path":"f.txt","old":"x","new":"y","replace_all":true}`)
	if res.IsError || res.Content[0].Text != "replaced 3 occurrences in f.txt" || read(t, ws, "f.txt") != "y y y" {
		t.Fatalf("got %q file=%q", res.Content[0].Text, read(t, ws, "f.txt"))
	}
}

func TestEditNotFoundEmptyAndEscape(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "f.txt", "abc")
	tl := load(t)
	res := call(t, tl, ws, `{"path":"f.txt","old":"zzz","new":"y"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "not found") {
		t.Fatalf("not found: %q", res.Content[0].Text)
	}
	res = call(t, tl, ws, `{"path":"f.txt","old":"","new":"y"}`)
	if !res.IsError {
		t.Fatal("empty old accepted")
	}
	res = call(t, tl, ws, `{"path":"../f.txt","old":"a","new":"b"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "escapes") {
		t.Fatalf("escape: %q", res.Content[0].Text)
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/plugins/tools/edit/ 2>&1 | head -3`
Expected: `undefined: edit.New`.

- [ ] **Step 4: Write the plugin**

`internal/plugins/tools/edit/edit.go`:

```go
// Package edit is the built-in edit tool: replace an exact substring in one file.
package edit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/fsroot"
	"github.com/guygrigsby/rudy/internal/tool"
)

const schema = `{"type":"object","properties":{"path":{"type":"string","description":"File path, relative to the workspace root or absolute inside it"},"old":{"type":"string","description":"Exact text to replace; must occur exactly once unless replace_all"},"new":{"type":"string","description":"Replacement text"},"replace_all":{"type":"boolean","description":"Replace every occurrence"}},"required":["path","old","new"],"additionalProperties":false}`

type args struct {
	Path       string `json:"path"`
	Old        string `json:"old"`
	New        string `json:"new"`
	ReplaceAll bool   `json:"replace_all"`
}

type editPlugin struct{}

func New() plugin.Plugin { return editPlugin{} }

func (editPlugin) Name() string { return "tools.edit" }

func (editPlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterTool(tool.Tool{
		Name:        "edit",
		Description: "Replace exact text in a file. old must match exactly once; pass replace_all to change every occurrence. Read the file first so old matches byte for byte.",
		Schema:      json.RawMessage(schema),
		Safety:      tool.Unsafe,
		Invoke:      invoke,
	})
}

func invoke(ctx context.Context, call tool.Call) (tool.Result, error) {
	var a args
	if err := json.Unmarshal(call.Input, &a); err != nil {
		return fsroot.Fail("edit: bad input: %v", err), nil
	}
	if a.Old == "" {
		return fsroot.Fail("edit: old must not be empty"), nil
	}
	rel, err := fsroot.Rel(call.Workspace.Root, a.Path)
	if err != nil {
		return fsroot.Fail("edit: %v", err), nil
	}
	root, err := os.OpenRoot(call.Workspace.Root)
	if err != nil {
		return tool.Result{}, err
	}
	defer root.Close()
	b, err := root.ReadFile(rel)
	if err != nil {
		return fsroot.Fail("edit: %v", err), nil
	}
	if fsroot.IsBinary(b) {
		return fsroot.Fail("edit: %s is a binary file", a.Path), nil
	}
	content := string(b)
	count := strings.Count(content, a.Old)
	switch {
	case count == 0:
		return fsroot.Fail("edit: old text not found in %s", a.Path), nil
	case count > 1 && !a.ReplaceAll:
		return fsroot.Fail("edit: old text occurs %d times in %s; make it unique or pass replace_all", count, a.Path), nil
	}
	if a.ReplaceAll {
		content = strings.ReplaceAll(content, a.Old, a.New)
	} else {
		content = strings.Replace(content, a.Old, a.New, 1)
	}
	if err := fsroot.WriteAtomic(root, rel, []byte(content)); err != nil {
		return fsroot.Fail("edit: %v", err), nil
	}
	noun := "occurrence"
	if count != 1 {
		noun = "occurrences"
	}
	return fsroot.Text(fmt.Sprintf("replaced %d %s in %s", count, noun, a.Path)), nil
}
```

- [ ] **Step 5: Run the tests**

Run: `go test -race ./internal/plugins/tools/edit/ -v`
Expected: `PASS` for all three tests.

- [ ] **Step 6: Commit**

```bash
git add internal/plugins/tools/edit
git commit -m "tools: edit"
bd close <id>
```

#### Task 15d: bash tool

**Files:**
- Create: `internal/plugins/tools/bash/bash.go`
- Test: `internal/plugins/tools/bash/bash_test.go`

**Interfaces:**
- Consumes: `fsroot.Text`, `fsroot.Fail`.
- Produces: `bash.New() plugin.Plugin` registering tool `bash`, unsafe. Unix only: `Setpgid` is not available on Windows and this plan does not target it.

- [ ] **Step 1: Claim**

```bash
bd create --title "tools: bash" --type task
bd update <id> --claim
```

- [ ] **Step 2: Write the failing test**

`internal/plugins/tools/bash/bash_test.go`:

```go
package bash_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/bash"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

type captureHost struct{ tools []tool.Tool }

func (h *captureHost) RegisterTool(t tool.Tool) error           { h.tools = append(h.tools, t); return nil }
func (h *captureHost) RegisterCommand(plugin.Command) error     { return nil }
func (h *captureHost) RegisterProvider(provider.Provider) error { return nil }
func (h *captureHost) Config() map[string]any                   { return nil }
func (h *captureHost) Notice(string)                            {}

func load(t *testing.T) tool.Tool {
	t.Helper()
	h := &captureHost{}
	if err := bash.New().Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	return h.tools[0]
}

func TestBashRunsInWorkspaceAndReportsExit(t *testing.T) {
	ws := t.TempDir()
	tl := load(t)
	if tl.Name != "bash" || tl.Safety != tool.Unsafe {
		t.Fatalf("tool = %+v", tl)
	}
	res, err := tl.Invoke(context.Background(), tool.Call{Input: json.RawMessage(`{"command":"pwd; echo out; echo err 1>&2"}`), Workspace: session.Workspace{Root: ws}})
	if err != nil {
		t.Fatal(err)
	}
	out := res.Content[0].Text
	if res.IsError || !strings.Contains(out, "out\n") || !strings.Contains(out, "err\n") {
		t.Fatalf("got %q err=%v", out, res.IsError)
	}
	if !strings.HasSuffix(strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]), ws[strings.LastIndex(ws, "/"):]) {
		t.Fatalf("cwd line = %q, want suffix of %q", strings.SplitN(out, "\n", 2)[0], ws)
	}
	res, err = tl.Invoke(context.Background(), tool.Call{Input: json.RawMessage(`{"command":"echo boom; exit 3"}`), Workspace: session.Workspace{Root: ws}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content[0].Text, "exit status 3") || !strings.Contains(res.Content[0].Text, "boom") {
		t.Fatalf("got %q err=%v", res.Content[0].Text, res.IsError)
	}
}

func TestBashCancelKillsProcessGroup(t *testing.T) {
	ws := t.TempDir()
	tl := load(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res, err := tl.Invoke(ctx, tool.Call{Input: json.RawMessage(`{"command":"echo started; sleep 30; echo never"}`), Workspace: session.Workspace{Root: ws}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("took %s to return after cancel", time.Since(start))
	}
	if !strings.Contains(res.Content[0].Text, "started") || strings.Contains(res.Content[0].Text, "never") {
		t.Fatalf("partial output = %q", res.Content[0].Text)
	}
}

func TestBashTimeoutIsAnError(t *testing.T) {
	ws := t.TempDir()
	tl := load(t)
	res, err := tl.Invoke(context.Background(), tool.Call{Input: json.RawMessage(`{"command":"sleep 5","timeout_ms":300}`), Workspace: session.Workspace{Root: ws}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content[0].Text, "timed out after 300ms") {
		t.Fatalf("got %q err=%v", res.Content[0].Text, res.IsError)
	}
}

func TestBashTruncatesOutput(t *testing.T) {
	ws := t.TempDir()
	tl := load(t)
	res, err := tl.Invoke(context.Background(), tool.Call{Input: json.RawMessage(`{"command":"head -c 50000 /dev/zero | tr '\\0' 'a'"}`), Workspace: session.Workspace{Root: ws}})
	if err != nil {
		t.Fatal(err)
	}
	out := res.Content[0].Text
	if len(out) > 31000 || !strings.Contains(out, "[output truncated at 30000 bytes]") {
		t.Fatalf("len=%d truncated marker missing", len(out))
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/plugins/tools/bash/ 2>&1 | head -3`
Expected: `undefined: bash.New`.

- [ ] **Step 4: Write the plugin**

`internal/plugins/tools/bash/bash.go`:

```go
// Package bash is the built-in bash tool: run one shell command in the workspace.
package bash

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/fsroot"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

const schema = `{"type":"object","properties":{"command":{"type":"string","description":"The command to run with bash -c, in the workspace root"},"timeout_ms":{"type":"integer","minimum":1,"maximum":600000,"description":"Wall clock limit, default 120000"}},"required":["command"],"additionalProperties":false}`

const (
	defaultTimeout = 120 * time.Second
	maxTimeout     = 600 * time.Second
	maxOutput      = 30000
	termGrace      = 2 * time.Second
)

type args struct {
	Command   string `json:"command"`
	TimeoutMS int    `json:"timeout_ms"`
}

type bashPlugin struct{}

func New() plugin.Plugin { return bashPlugin{} }

func (bashPlugin) Name() string { return "tools.bash" }

func (bashPlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterTool(tool.Tool{
		Name:        "bash",
		Description: "Run a shell command in the workspace root and return its combined output. Use for builds, tests, git and anything a terminal does. Output is capped at 30000 bytes.",
		Schema:      json.RawMessage(schema),
		Safety:      tool.Unsafe,
		Invoke:      invoke,
	})
}

// cappedBuffer keeps the first maxOutput bytes and remembers that more arrived.
type cappedBuffer struct {
	mu        sync.Mutex
	b         strings.Builder
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := maxOutput - c.b.Len()
	if room <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		c.b.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	c.b.Write(p)
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.truncated {
		return c.b.String() + "\n[output truncated at 30000 bytes]\n"
	}
	return c.b.String()
}

func invoke(ctx context.Context, call tool.Call) (tool.Result, error) {
	var a args
	if err := json.Unmarshal(call.Input, &a); err != nil {
		return fsroot.Fail("bash: bad input: %v", err), nil
	}
	if strings.TrimSpace(a.Command) == "" {
		return fsroot.Fail("bash: command must not be empty"), nil
	}
	timeout := defaultTimeout
	if a.TimeoutMS > 0 {
		timeout = time.Duration(a.TimeoutMS) * time.Millisecond
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out := &cappedBuffer{}
	cmd := exec.CommandContext(runCtx, "bash", "-c", a.Command)
	cmd.Dir = call.Workspace.Root
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = termGrace
	if err := cmd.Start(); err != nil {
		return fsroot.Fail("bash: %v", err), nil
	}
	err := cmd.Wait()
	// Whatever survived SIGTERM and the grace period dies with the group.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)

	text := out.String()
	if ctx.Err() != nil {
		return tool.Result{Content: []session.Block{session.TextBlock(text + "\n[killed]\n")}, IsError: true}, ctx.Err()
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return fsroot.Fail("%s\n[timed out after %s]", text, timeout), nil
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fsroot.Fail("%sexit status %d", ensureNewline(text), ee.ExitCode()), nil
		}
		return fsroot.Fail("%sbash: %v", ensureNewline(text), err), nil
	}
	return fsroot.Text(text), nil
}

func ensureNewline(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

var _ = fmt.Sprintf
```

Remove the trailing `var _ = fmt.Sprintf` and the `fmt` import before committing; they exist only so the snippet compiles if `fmt` stays unused while iterating.

- [ ] **Step 5: Run the tests**

Run: `go test -race ./internal/plugins/tools/bash/ -v`
Expected: `PASS` for all four tests. `TestBashTimeoutIsAnError` expects the string `timed out after 300ms`, which is what `time.Duration(300 * time.Millisecond).String()` prints.

- [ ] **Step 6: Commit**

```bash
git add internal/plugins/tools/bash
git commit -m "tools: bash"
bd close <id>
```

#### Task 15e: grep tool

**Files:**
- Create: `internal/plugins/tools/grep/grep.go`
- Test: `internal/plugins/tools/grep/grep_test.go`

**Interfaces:**
- Consumes: `fsroot.Rel`, `fsroot.IsBinary`, `fsroot.Text`, `fsroot.Fail`; `github.com/bmatcuk/doublestar/v4` (`go get github.com/bmatcuk/doublestar/v4@v4.10.0`).
- Produces: `grep.New() plugin.Plugin` registering tool `grep`, safe.

- [ ] **Step 1: Claim**

```bash
bd create --title "tools: grep" --type task
bd update <id> --claim
go get github.com/bmatcuk/doublestar/v4@v4.10.0
```

- [ ] **Step 2: Write the failing test**

`internal/plugins/tools/grep/grep_test.go`:

```go
package grep_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/grep"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

type captureHost struct{ tools []tool.Tool }

func (h *captureHost) RegisterTool(t tool.Tool) error           { h.tools = append(h.tools, t); return nil }
func (h *captureHost) RegisterCommand(plugin.Command) error     { return nil }
func (h *captureHost) RegisterProvider(provider.Provider) error { return nil }
func (h *captureHost) Config() map[string]any                   { return nil }
func (h *captureHost) Notice(string)                            {}

func load(t *testing.T) tool.Tool {
	t.Helper()
	h := &captureHost{}
	if err := grep.New().Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	return h.tools[0]
}

func call(t *testing.T, tl tool.Tool, root, input string) tool.Result {
	t.Helper()
	res, err := tl.Invoke(context.Background(), tool.Call{Input: json.RawMessage(input), Workspace: session.Workspace{Root: root}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func seed(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	files := map[string]string{
		"a.go":        "package a\nfunc Hello() {}\n",
		"sub/b.go":    "package sub\n// Hello again\n",
		"sub/c.txt":   "hello lowercase\n",
		".git/config": "Hello in git\n",
		"bin.dat":     "Hello\x00binary",
	}
	for name, content := range files {
		p := filepath.Join(ws, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

func TestGrepFindsMatchesSkippingGitAndBinary(t *testing.T) {
	ws := seed(t)
	tl := load(t)
	if tl.Name != "grep" || tl.Safety != tool.Safe {
		t.Fatalf("tool = %+v", tl)
	}
	res := call(t, tl, ws, `{"pattern":"Hello"}`)
	out := res.Content[0].Text
	if res.IsError {
		t.Fatal(out)
	}
	for _, want := range []string{"a.go:2:func Hello() {}", "sub/b.go:2:// Hello again"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	for _, no := range []string{".git/", "bin.dat", "c.txt"} {
		if strings.Contains(out, no) {
			t.Errorf("unexpected %q in %q", no, out)
		}
	}
}

func TestGrepGlobPathAndMax(t *testing.T) {
	ws := seed(t)
	tl := load(t)
	res := call(t, tl, ws, `{"pattern":"(?i)hello","glob":"**/*.txt"}`)
	if out := res.Content[0].Text; !strings.Contains(out, "sub/c.txt:1:") || strings.Contains(out, "a.go") {
		t.Fatalf("glob: %q", out)
	}
	res = call(t, tl, ws, `{"pattern":"Hello","path":"sub"}`)
	if out := res.Content[0].Text; !strings.Contains(out, "sub/b.go:2:") || strings.Contains(out, "a.go") {
		t.Fatalf("path: %q", out)
	}
	res = call(t, tl, ws, `{"pattern":"(?i)hello","max":1}`)
	if out := res.Content[0].Text; strings.Count(out, "\n") != 2 || !strings.Contains(out, "truncated at 1 matches") {
		t.Fatalf("max: %q", out)
	}
	res = call(t, tl, ws, `{"pattern":"nomatch"}`)
	if res.IsError || res.Content[0].Text != "no matches\n" {
		t.Fatalf("none: %q err=%v", res.Content[0].Text, res.IsError)
	}
}

func TestGrepRefusesBadPatternAndEscape(t *testing.T) {
	ws := seed(t)
	tl := load(t)
	if res := call(t, tl, ws, `{"pattern":"("}`); !res.IsError {
		t.Fatal("bad regexp accepted")
	}
	if res := call(t, tl, ws, `{"pattern":"x","path":"../"}`); !res.IsError || !strings.Contains(res.Content[0].Text, "escapes") {
		t.Fatalf("escape: %q", res.Content[0].Text)
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/plugins/tools/grep/ 2>&1 | head -3`
Expected: `undefined: grep.New`.

- [ ] **Step 4: Write the plugin**

`internal/plugins/tools/grep/grep.go`:

```go
// Package grep is the built-in grep tool: regexp search over workspace files.
package grep

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/fsroot"
	"github.com/guygrigsby/rudy/internal/tool"
)

const schema = `{"type":"object","properties":{"pattern":{"type":"string","description":"Go regular expression"},"path":{"type":"string","description":"Directory to search, relative to the workspace root; default the root"},"glob":{"type":"string","description":"Only files whose relative path matches this glob, e.g. **/*.go"},"max":{"type":"integer","minimum":1,"description":"Maximum matches, default 200"}},"required":["pattern"],"additionalProperties":false}`

const (
	defaultMax  = 200
	maxFileSize = 1 << 20
)

type args struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
	Glob    string `json:"glob"`
	Max     int    `json:"max"`
}

type grepPlugin struct{}

func New() plugin.Plugin { return grepPlugin{} }

func (grepPlugin) Name() string { return "tools.grep" }

func (grepPlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterTool(tool.Tool{
		Name:        "grep",
		Description: "Search file contents with a Go regular expression. Output is path:line:text. Skips .git, binaries and files over 1MB.",
		Schema:      json.RawMessage(schema),
		Safety:      tool.Safe,
		Invoke:      invoke,
	})
}

func invoke(ctx context.Context, call tool.Call) (tool.Result, error) {
	var a args
	if err := json.Unmarshal(call.Input, &a); err != nil {
		return fsroot.Fail("grep: bad input: %v", err), nil
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return fsroot.Fail("grep: %v", err), nil
	}
	if a.Max < 1 {
		a.Max = defaultMax
	}
	if a.Glob != "" && !doublestar.ValidatePattern(a.Glob) {
		return fsroot.Fail("grep: bad glob %q", a.Glob), nil
	}
	start := "."
	if a.Path != "" {
		rel, err := fsroot.Rel(call.Workspace.Root, a.Path)
		if err != nil {
			return fsroot.Fail("grep: %v", err), nil
		}
		start = fsToSlash(rel)
	}
	root, err := os.OpenRoot(call.Workspace.Root)
	if err != nil {
		return tool.Result{}, err
	}
	defer root.Close()
	fsys := root.FS()

	var b strings.Builder
	matches := 0
	truncated := false
	walkErr := fs.WalkDir(fsys, start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if a.Glob != "" {
			ok, _ := doublestar.Match(a.Glob, p)
			if !ok {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxFileSize {
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil || fsroot.IsBinary(data) {
			return nil
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 0, 64*1024), maxFileSize+1)
		line := 0
		for sc.Scan() {
			line++
			if !re.Match(sc.Bytes()) {
				continue
			}
			if matches >= a.Max {
				truncated = true
				return fs.SkipAll
			}
			matches++
			fmt.Fprintf(&b, "%s:%d:%s\n", p, line, sc.Text())
		}
		return nil
	})
	if walkErr != nil && walkErr != fs.SkipAll {
		return fsroot.Fail("grep: %v", walkErr), nil
	}
	if matches == 0 {
		return fsroot.Text("no matches\n"), nil
	}
	if truncated {
		fmt.Fprintf(&b, "… truncated at %d matches\n", a.Max)
	}
	return fsroot.Text(b.String()), nil
}

func fsToSlash(rel string) string {
	s := strings.ReplaceAll(rel, string(os.PathSeparator), "/")
	return path.Clean(s)
}
```

- [ ] **Step 5: Run the tests**

Run: `go test -race ./internal/plugins/tools/grep/ -v`
Expected: `PASS` for all three tests.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/plugins/tools/grep
git commit -m "tools: grep"
bd close <id>
```

#### Task 15f: glob tool

**Files:**
- Create: `internal/plugins/tools/glob/glob.go`
- Test: `internal/plugins/tools/glob/glob_test.go`

**Interfaces:**
- Consumes: `fsroot.Rel`, `fsroot.Text`, `fsroot.Fail`, `doublestar.Glob`, `doublestar.ValidatePattern`.
- Produces: `glob.New() plugin.Plugin` registering tool `glob`, safe.

- [ ] **Step 1: Claim**

```bash
bd create --title "tools: glob" --type task
bd update <id> --claim
```

- [ ] **Step 2: Write the failing test**

`internal/plugins/tools/glob/glob_test.go`:

```go
package glob_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/glob"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

type captureHost struct{ tools []tool.Tool }

func (h *captureHost) RegisterTool(t tool.Tool) error           { h.tools = append(h.tools, t); return nil }
func (h *captureHost) RegisterCommand(plugin.Command) error     { return nil }
func (h *captureHost) RegisterProvider(provider.Provider) error { return nil }
func (h *captureHost) Config() map[string]any                   { return nil }
func (h *captureHost) Notice(string)                            {}

func load(t *testing.T) tool.Tool {
	t.Helper()
	h := &captureHost{}
	if err := glob.New().Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	return h.tools[0]
}

func call(t *testing.T, tl tool.Tool, root, input string) tool.Result {
	t.Helper()
	res, err := tl.Invoke(context.Background(), tool.Call{Input: json.RawMessage(input), Workspace: session.Workspace{Root: root}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func seed(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	for _, name := range []string{"a.go", "sub/b.go", "sub/deep/c.go", "sub/d.txt", ".git/HEAD"} {
		p := filepath.Join(ws, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

func TestGlobListsSortedRelativePaths(t *testing.T) {
	ws := seed(t)
	tl := load(t)
	if tl.Name != "glob" || tl.Safety != tool.Safe {
		t.Fatalf("tool = %+v", tl)
	}
	res := call(t, tl, ws, `{"pattern":"**/*.go"}`)
	if res.IsError || res.Content[0].Text != "a.go\nsub/b.go\nsub/deep/c.go\n" {
		t.Fatalf("got %q err=%v", res.Content[0].Text, res.IsError)
	}
	res = call(t, tl, ws, `{"pattern":"*","path":"sub"}`)
	if res.Content[0].Text != "sub/b.go\nsub/d.txt\nsub/deep\n" {
		t.Fatalf("path: %q", res.Content[0].Text)
	}
	res = call(t, tl, ws, `{"pattern":"**/HEAD"}`)
	if res.IsError || res.Content[0].Text != "no matches\n" {
		t.Fatalf(".git leaked: %q", res.Content[0].Text)
	}
}

func TestGlobRefusesBadPatternAndEscape(t *testing.T) {
	ws := seed(t)
	tl := load(t)
	if res := call(t, tl, ws, `{"pattern":"[bad"}`); !res.IsError {
		t.Fatal("bad pattern accepted")
	}
	if res := call(t, tl, ws, `{"pattern":"*","path":"../"}`); !res.IsError || !strings.Contains(res.Content[0].Text, "escapes") {
		t.Fatalf("escape: %q", res.Content[0].Text)
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/plugins/tools/glob/ 2>&1 | head -3`
Expected: `undefined: glob.New`.

- [ ] **Step 4: Write the plugin**

`internal/plugins/tools/glob/glob.go`:

```go
// Package glob is the built-in glob tool: list workspace paths matching a pattern.
package glob

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/fsroot"
	"github.com/guygrigsby/rudy/internal/tool"
)

const schema = `{"type":"object","properties":{"pattern":{"type":"string","description":"Glob with ** support, e.g. **/*.go"},"path":{"type":"string","description":"Directory to search, relative to the workspace root; default the root"}},"required":["pattern"],"additionalProperties":false}`

const maxResults = 1000

type args struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

type globPlugin struct{}

func New() plugin.Plugin { return globPlugin{} }

func (globPlugin) Name() string { return "tools.glob" }

func (globPlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterTool(tool.Tool{
		Name:        "glob",
		Description: "List files and directories matching a glob pattern, relative to the workspace root, sorted. Skips .git. Capped at 1000 results.",
		Schema:      json.RawMessage(schema),
		Safety:      tool.Safe,
		Invoke:      invoke,
	})
}

func invoke(ctx context.Context, call tool.Call) (tool.Result, error) {
	var a args
	if err := json.Unmarshal(call.Input, &a); err != nil {
		return fsroot.Fail("glob: bad input: %v", err), nil
	}
	if !doublestar.ValidatePattern(a.Pattern) {
		return fsroot.Fail("glob: bad pattern %q", a.Pattern), nil
	}
	root, err := os.OpenRoot(call.Workspace.Root)
	if err != nil {
		return tool.Result{}, err
	}
	defer root.Close()
	var fsys fs.FS = root.FS()
	prefix := ""
	if a.Path != "" {
		rel, err := fsroot.Rel(call.Workspace.Root, a.Path)
		if err != nil {
			return fsroot.Fail("glob: %v", err), nil
		}
		prefix = path.Clean(strings.ReplaceAll(rel, string(os.PathSeparator), "/"))
		if prefix != "." {
			fsys, err = fs.Sub(fsys, prefix)
			if err != nil {
				return fsroot.Fail("glob: %v", err), nil
			}
		} else {
			prefix = ""
		}
	}
	found, err := doublestar.Glob(fsys, a.Pattern)
	if err != nil {
		return fsroot.Fail("glob: %v", err), nil
	}
	out := make([]string, 0, len(found))
	for _, p := range found {
		if p == ".git" || strings.HasPrefix(p, ".git/") || strings.Contains(p, "/.git/") {
			continue
		}
		if prefix != "" {
			p = prefix + "/" + p
		}
		out = append(out, p)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return fsroot.Text("no matches\n"), nil
	}
	truncated := false
	if len(out) > maxResults {
		out = out[:maxResults]
		truncated = true
	}
	text := strings.Join(out, "\n") + "\n"
	if truncated {
		text += "… truncated at 1000 results\n"
	}
	return fsroot.Text(text), nil
}
```

- [ ] **Step 5: Run the tests**

Run: `go test -race ./internal/plugins/tools/... -v`
Expected: `PASS` across all six tool packages and fsroot.

- [ ] **Step 6: Commit**

```bash
git add internal/plugins/tools/glob
git commit -m "tools: glob"
bd close <id>
```

---

### Task 16: `/init` command plugin and the openai_chat provider plugin

**Files:**
- Create: `internal/plugins/initcmd/init.go`
- Create: `internal/plugins/openaichat/plugin.go`
- Test: `internal/plugins/initcmd/init_test.go`
- Test: `internal/plugins/openaichat/plugin_test.go`

**Interfaces:**
- Consumes: `plugin.Plugin`, `plugin.Host`, `plugin.Command`, `plugin.CommandCall`, `plugin.SubmitPrompt`; `config.ProviderConfig`; a `resolve func(ref string) (string, error)` the caller builds from `config.ResolveSecret` (Task 7), which returns an error when an `env:NAME` variable is unset or a `cache:KEY` key is missing; `httpx.Client`; `openaichat.New(openaichat.Options)`, `openaichat.Options{Name, BaseURL, Token, Headers, HTTP}` (Task 9).
- Produces:

```go
package initcmd
const Prompt = "…" // the exact text below
func New() plugin.Plugin // registers slash command "init"

package openaichatplugin // import path internal/plugins/openaichat; named so it never collides with the codec package
func New(providers map[string]config.ProviderConfig, http *httpx.Client, resolve func(ref string) (string, error)) plugin.Plugin
```

Task 17 calls exactly `openaichatplugin.New(cfg.Providers, httpc, resolve)`.

- [ ] **Step 1: Claim**

```bash
bd create --title "plugins: /init command" --type task
bd update <id> --claim
```

- [ ] **Step 2: Write the failing init test**

`internal/plugins/initcmd/init_test.go`:

```go
package initcmd_test

import (
	"context"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/initcmd"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

type captureHost struct{ cmds []plugin.Command }

func (h *captureHost) RegisterTool(tool.Tool) error               { return nil }
func (h *captureHost) RegisterCommand(c plugin.Command) error     { h.cmds = append(h.cmds, c); return nil }
func (h *captureHost) RegisterProvider(provider.Provider) error   { return nil }
func (h *captureHost) Config() map[string]any                     { return nil }
func (h *captureHost) Notice(string)                              {}

func TestInitSubmitsThePrompt(t *testing.T) {
	h := &captureHost{}
	if err := initcmd.New().Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if len(h.cmds) != 1 || h.cmds[0].Name != "init" {
		t.Fatalf("commands = %+v", h.cmds)
	}
	act, err := h.cmds[0].Run(context.Background(), plugin.CommandCall{Workspace: session.Workspace{Root: "/tmp/x"}})
	if err != nil {
		t.Fatal(err)
	}
	sp, ok := act.(plugin.SubmitPrompt)
	if !ok {
		t.Fatalf("action %T", act)
	}
	for _, want := range []string{"AGENTS.md", "write tool", "Under 80 lines", "read it first"} {
		if !strings.Contains(sp.Text, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if sp.Text != initcmd.Prompt {
		t.Fatal("prompt differs from the exported constant")
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/plugins/initcmd/ 2>&1 | head -3`
Expected: `undefined: initcmd.New`.

- [ ] **Step 4: Write the plugin**

`internal/plugins/initcmd/init.go`:

```go
// Package initcmd is the /init slash command: create or revise the workspace's AGENTS.md.
package initcmd

import (
	"context"

	"github.com/guygrigsby/rudy/internal/plugin"
)

// Prompt is the user message /init submits. It is the whole behavior of the command.
const Prompt = `Initialize this workspace's AGENTS.md.

Inspect the repository with the glob, grep and read tools: README and other top-level docs; build files such as Makefile, go.mod, package.json, pyproject.toml and Cargo.toml; the directory layout two levels deep; the test layout; CI configuration; and any existing AGENTS.md or CLAUDE.md.

If ./AGENTS.md exists, read it first and revise it: keep what is still true, correct what is not.

Then write ./AGENTS.md with the write tool. Contents, in this order:
1. What the project is, in two or three sentences.
2. How to build, test and lint, with the exact commands.
3. Conventions you observed: language version, formatting, naming, commit style, anything a contributor would be corrected on.
4. Where things are: a short map of the important directories and entry points.

Under 80 lines. No placeholders, no sections you could not fill from the repository, no speculation. When done, reply with one sentence saying what you wrote.`

type initPlugin struct{}

func New() plugin.Plugin { return initPlugin{} }

func (initPlugin) Name() string { return "init" }

func (initPlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterCommand(plugin.Command{
		Name:        "init",
		Description: "Create or revise AGENTS.md for this workspace",
		Run: func(ctx context.Context, call plugin.CommandCall) (plugin.Action, error) {
			return plugin.SubmitPrompt{Text: Prompt}, nil
		},
	})
}
```

- [ ] **Step 5: Run the test, then commit**

Run: `go test -race ./internal/plugins/initcmd/ -v`
Expected: `PASS`.

```bash
git add internal/plugins/initcmd
git commit -m "plugins: /init command"
bd close <id>
bd create --title "plugins: openai_chat provider plugin" --type task
bd update <id> --claim
```

- [ ] **Step 6: Write the failing provider plugin test**

`internal/plugins/openaichat/plugin_test.go`:

```go
package openaichatplugin_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/plugin"
	openaichatplugin "github.com/guygrigsby/rudy/internal/plugins/openaichat"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/provider/httpx"
	"github.com/guygrigsby/rudy/internal/tool"
)

type captureHost struct {
	providers []provider.Provider
	notices   []string
}

func (h *captureHost) RegisterTool(tool.Tool) error                 { return nil }
func (h *captureHost) RegisterCommand(plugin.Command) error         { return nil }
func (h *captureHost) RegisterProvider(p provider.Provider) error   { h.providers = append(h.providers, p); return nil }
func (h *captureHost) Config() map[string]any                       { return nil }
func (h *captureHost) Notice(s string)                              { h.notices = append(h.notices, s) }

func TestRegistersOnePerOpenAIChatEntryAndSkipsBadSecrets(t *testing.T) {
	providers := map[string]config.ProviderConfig{
		"aperture": {Wire: "openai_chat", BaseURL: "https://ai.example/v1", Auth: ""},
		"mlx":      {Wire: "openai_chat", BaseURL: "http://localhost:8080/v1", Auth: "env:MLX_TOKEN"},
		"broken":   {Wire: "openai_chat", BaseURL: "https://x/v1", Auth: "env:MISSING"},
		"other":    {Wire: "anthropic_messages", BaseURL: "https://y"},
	}
	// resolve stands in for config.ResolveSecret bound to a fake environment.
	resolve := func(ref string) (string, error) {
		switch ref {
		case "":
			return "", nil
		case "env:MLX_TOKEN":
			return "secret", nil
		}
		return "", fmt.Errorf("secret %s: not set", ref)
	}
	h := &captureHost{}
	p := openaichatplugin.New(providers, httpx.New("test"), resolve)
	if p.Name() != "openai_chat" {
		t.Fatalf("name = %s", p.Name())
	}
	if err := p.Init(context.Background(), h); err != nil {
		t.Fatalf("init must not fail on a bad secret: %v", err)
	}
	names := map[string]bool{}
	for _, pr := range h.providers {
		names[pr.Name()] = true
	}
	if len(h.providers) != 2 || !names["aperture"] || !names["mlx"] {
		t.Fatalf("registered %v", names)
	}
	if len(h.notices) != 1 || !strings.Contains(h.notices[0], "broken") {
		t.Fatalf("notices = %v", h.notices)
	}
}
```

- [ ] **Step 7: Run to verify it fails**

Run: `go test ./internal/plugins/openaichat/ 2>&1 | head -3`
Expected: `undefined: openaichatplugin.New`.

- [ ] **Step 8: Write the plugin**

`internal/plugins/openaichat/plugin.go`:

```go
// Package openaichatplugin is the provider plugin for every configured openai_chat endpoint.
// The directory is internal/plugins/openaichat; the package name differs from the codec's so
// importers never need an alias to hold both.
package openaichatplugin

import (
	"context"
	"fmt"
	"sort"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/provider/httpx"
	codec "github.com/guygrigsby/rudy/internal/provider/openaichat"
)

type providerPlugin struct {
	providers map[string]config.ProviderConfig
	http      *httpx.Client
	resolve   func(ref string) (string, error)
}

// New builds the plugin from the [providers] config table. resolve turns an auth reference
// ("", env:NAME or cache:KEY) into a token; the caller binds config.ResolveSecret to the real
// environment and cache path.
func New(providers map[string]config.ProviderConfig, http *httpx.Client, resolve func(ref string) (string, error)) plugin.Plugin {
	return &providerPlugin{providers: providers, http: http, resolve: resolve}
}

func (p *providerPlugin) Name() string { return "openai_chat" }

// Init registers one provider per openai_chat entry. An entry whose secret cannot be resolved
// is skipped with a notice; the plugin stays ready so the other providers keep working.
func (p *providerPlugin) Init(ctx context.Context, h plugin.Host) error {
	names := make([]string, 0, len(p.providers))
	for name := range p.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		pc := p.providers[name]
		if pc.Wire != "openai_chat" {
			continue
		}
		token, err := p.resolve(pc.Auth)
		if err != nil {
			h.Notice(fmt.Sprintf("provider %s skipped: %v", name, err))
			continue
		}
		client := codec.New(codec.Options{Name: name, BaseURL: pc.BaseURL, Token: token, Headers: pc.Headers, HTTP: p.http})
		if err := h.RegisterProvider(client); err != nil {
			h.Notice(fmt.Sprintf("provider %s skipped: %v", name, err))
		}
	}
	return nil
}
```

- [ ] **Step 9: Run the test, lint and commit**

Run: `go test -race ./internal/plugins/openaichat/ -v && make check`
Expected: `PASS`, `make check` green.

```bash
git add internal/plugins/openaichat
git commit -m "plugins: openai_chat provider plugin"
bd close <id>
```

### Task 17: Wiring and the headless printer client

**Files:**
- Create: `internal/cli/wire.go`
- Create: `internal/cli/print.go`
- Create: `internal/cli/print_test.go`
- Create: `internal/cli/wire_test.go`
- Modify: `internal/cli/root.go` (replaced wholesale with the file in Step 8; Task 1's `version` variable and `Version()` stay in this file so the Makefile's `-X github.com/guygrigsby/rudy/internal/cli.version=` keeps working, and `NewRoot()` keeps its Task 1 signature)
- Modify: `cmd/rudy/main.go` (replaced wholesale with the file in Step 9; maps `ExitError` to the process exit code)

**Interfaces:**
- Consumes: `config.XDG`, `config.Load`, `config.ResolveSecret`, `config.Paths`, `config.Config` (Task 7); `session.OpenStore`, `session.Store.List`, `session.Summary`, `session.Block`, `session.TextBlock`, `session.Entry` with value-typed payloads `session.AssistantMessage` and `session.TurnFailed` (Tasks 2 to 5); `httpx.New` (Task 8); `provider.Registry`, `provider.Model`, `provider.Pricing.Cost`, `provider.Part` (Task 8); `plugin.NewRegistry`, `plugin.Registry.Load`, `plugin.Registry.Providers`, `plugin.Plugin`, `plugin.Host`, `plugin.Command`, `plugin.SubmitPrompt` (Task 11); `gate.New` (Task 10); `protocol.Pipe`, `protocol.NewClient`, `protocol.Client.Call`, `protocol.Client.Notifications`, the method and notification constants and param structs (Task 12); `server.New`, `server.Deps`, `server.Server.Serve`, `server.Server.Shutdown`, `server.EntryIDResult` (the result of `session.set_model`, `set_mode`, `set_thinking` and `set_title`), `server.InterruptResult` (Task 14; `session.submit` answers `protocol.SessionSubmitResult` and `command.run` answers `protocol.CommandRunResult`); the Task 14 fan-out forwards every `Runner.StateChanged`, including the `idle` the runner enters after a cancel, as `turn.state`, which is what this printer maps to exit 130; tool plugin constructors `read.New()`, `write.New()`, `edit.New()`, `bash.New()`, `grep.New()`, `glob.New()` each returning `plugin.Plugin` (Task 15); `initcmd.New() plugin.Plugin` and `openaichatplugin.New(providers map[string]config.ProviderConfig, http *httpx.Client, resolve func(ref string) (string, error)) plugin.Plugin` (Task 16).
- Produces: `cli.Built`, `cli.BuildOptions`, `cli.Build`, `cli.BuiltinPlugins`, `cli.BuiltinTools`, `cli.ExitError`, `cli.NewRoot`, unexported `buildFunc`, `newRoot`, `registerPrint`, `runPrint`, `printOptions`, `readPrompt`, `textOf`. Task 18 adds subcommands through `newRoot`.

- [ ] **Step 0: Track the task**

```bash
bd create --title "Task 17: wiring and the headless printer client" --type task
bd update <id> --claim
```

- [ ] **Step 1: Write the failing wiring test**

`internal/cli/wire_test.go`:

```go
package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

// fakeProvider replays a script of parts, one script per Complete call, and records every
// request it saw. fail, when set, is returned from every Complete call instead.
type fakeProvider struct {
	mu       sync.Mutex
	script   [][]provider.Part
	fail     error
	calls    int
	requests []provider.Request
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) ListModels(ctx context.Context) ([]provider.Model, error) {
	return []provider.Model{{
		Ref:           session.ModelRef{Provider: "fake", Model: "m"},
		DisplayName:   "Fake M",
		ContextWindow: 100000,
		MaxOutput:     8192,
		Pricing:       provider.Pricing{Input: "0.000001", Output: "0.000002"},
		Capabilities:  provider.Capabilities{Tools: true},
	}}, nil
}

func (f *fakeProvider) Complete(ctx context.Context, req provider.Request, emit func(provider.Part) error) error {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	if f.fail != nil {
		f.mu.Unlock()
		return f.fail
	}
	i := f.calls
	f.calls++
	if i >= len(f.script) {
		f.mu.Unlock()
		return &provider.Error{Class: session.ErrInternal, Message: "fake script exhausted"}
	}
	parts := f.script[i]
	f.mu.Unlock()
	for _, p := range parts {
		if err := emit(p); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeProvider) request(i int) provider.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

type fakePlugin struct{ p *fakeProvider }

func (fakePlugin) Name() string { return "fake" }

func (f fakePlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterProvider(f.p)
}

// say scripts a text-only reply.
func say(text string) []provider.Part {
	return []provider.Part{
		{Type: provider.PartTextDelta, Text: text},
		{Type: provider.PartUsage, Usage: session.Usage{Input: 10, Output: 2}},
		{Type: provider.PartStop, StopReason: session.StopEndTurn, StopReasonRaw: "stop"},
	}
}

// callTool scripts one tool call and a tool_use stop.
func callTool(id, name, input string) []provider.Part {
	return []provider.Part{
		{Type: provider.PartToolUseStart, ID: id, Name: name},
		{Type: provider.PartToolUseDelta, ID: id, Text: input},
		{Type: provider.PartToolUseEnd, ID: id},
		{Type: provider.PartUsage, Usage: session.Usage{Input: 10, Output: 5}},
		{Type: provider.PartStop, StopReason: session.StopToolUse, StopReasonRaw: "tool_calls"},
	}
}

// testBuilder points every XDG root at a temp dir, replaces the provider plugins with the fake
// and returns a buildFunc that opens mode off on model fake:m.
func testBuilder(t *testing.T, fp *fakeProvider, extra ...plugin.Plugin) buildFunc {
	t.Helper()
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(base, "run"))
	plugins := append(BuiltinTools(), fakePlugin{fp})
	plugins = append(plugins, extra...)
	return func(ctx context.Context, stderr io.Writer) (*Built, error) {
		return Build(ctx, BuildOptions{
			Version: "test",
			Overrides: map[string]any{
				"default.provider": "fake",
				"default.model":    "m",
				"permissions.mode": "off",
			},
			Plugins: plugins,
			Home:    base,
			Stderr:  stderr,
		})
	}
}

func TestBuildLoadsPluginsAndRegistry(t *testing.T) {
	fp := &fakeProvider{}
	b, err := testBuilder(t, fp)(context.Background(), io.Discard)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer b.Server.Shutdown(context.Background())
	if _, ok := b.Plugins.Tool("glob"); !ok {
		t.Fatal("glob tool not registered")
	}
	m, err := b.Registry.Resolve("fake:m")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.DisplayName != "Fake M" {
		t.Fatalf("DisplayName = %q", m.DisplayName)
	}
	if _, err := os.Stat(filepath.Join(b.Paths.Cache, "registry.json")); err != nil {
		t.Fatalf("snapshot not written: %v", err)
	}
}

func TestBuildFailsWithNoModels(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(base, "run"))
	_, err := Build(context.Background(), BuildOptions{
		Version:   "test",
		Overrides: map[string]any{"default.provider": "none", "default.model": "m"},
		Plugins:   BuiltinTools(),
		Home:      base,
		Stderr:    io.Discard,
	})
	if err == nil {
		t.Fatal("expected an error with no providers and no snapshot")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/cli -run 'TestBuild' -v`
Expected: FAIL to compile with `undefined: buildFunc`, `undefined: Build`, `undefined: BuildOptions`, `undefined: BuiltinTools`.

- [ ] **Step 3: Write `internal/cli/wire.go`**

```go
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/initcmd"
	openaichatplugin "github.com/guygrigsby/rudy/internal/plugins/openaichat"
	"github.com/guygrigsby/rudy/internal/plugins/tools/bash"
	"github.com/guygrigsby/rudy/internal/plugins/tools/edit"
	"github.com/guygrigsby/rudy/internal/plugins/tools/glob"
	"github.com/guygrigsby/rudy/internal/plugins/tools/grep"
	"github.com/guygrigsby/rudy/internal/plugins/tools/read"
	"github.com/guygrigsby/rudy/internal/plugins/tools/write"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/provider/httpx"
	"github.com/guygrigsby/rudy/internal/server"
	"github.com/guygrigsby/rudy/internal/session"
)

// Built is everything a command needs after wiring.
type Built struct {
	Version  string
	Paths    config.Paths
	Config   *config.Config
	Store    *session.Store
	Registry *provider.Registry
	Plugins  *plugin.Registry
	Server   *server.Server
}

// BuildOptions tunes wiring. Zero values mean the real environment.
type BuildOptions struct {
	Version   string
	Overrides map[string]any     // config keys that win over file and env, dotted ("default.model")
	Plugins   []plugin.Plugin    // nil means BuiltinPlugins
	Env       func(string) string // nil means os.Getenv
	Home      string             // "" means os.UserHomeDir
	Stderr    io.Writer          // nil means os.Stderr
}

// buildFunc is what commands call to wire a server; tests substitute fakes through it.
type buildFunc func(ctx context.Context, stderr io.Writer) (*Built, error)

// Build wires config, store, plugins, registry, gate and server. It never writes config.
func Build(ctx context.Context, o BuildOptions) (*Built, error) {
	env := o.Env
	if env == nil {
		env = os.Getenv
	}
	home := o.Home
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("home directory: %w", err)
		}
		home = h
	}
	stderr := o.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	paths := config.XDG(env, home)
	cfg, err := config.Load(paths, o.Overrides)
	if err != nil {
		return nil, err
	}
	store, err := session.OpenStore(filepath.Join(paths.Data, "sessions"))
	if err != nil {
		return nil, err
	}
	httpc := httpx.New(o.Version)
	notice := func(text string) { fmt.Fprintln(stderr, "rudy:", text) }
	plugins := plugin.NewRegistry(cfg.Plugins, notice)
	set := o.Plugins
	if set == nil {
		set = BuiltinPlugins(cfg, httpc, home, env)
	}
	plugins.Load(ctx, set...)
	if err := os.MkdirAll(paths.Cache, 0o700); err != nil {
		return nil, fmt.Errorf("cache dir: %w", err)
	}
	registry := provider.NewRegistry(filepath.Join(paths.Cache, "registry.json"), plugins.Providers()...)
	if err := registry.LoadSnapshot(); err != nil {
		return nil, err
	}
	if err := registry.Refresh(ctx); err != nil {
		if len(registry.Models()) == 0 {
			return nil, fmt.Errorf("model registry: %w", err)
		}
		notice("registry refresh: " + err.Error())
	}
	g := gate.New(cfg.Permissions.Dangerous)
	srv := server.New(server.Deps{
		Version:  o.Version,
		Config:   cfg,
		Store:    store,
		Registry: registry,
		Plugins:  plugins,
		Gate:     g,
	})
	return &Built{
		Version:  o.Version,
		Paths:    paths,
		Config:   cfg,
		Store:    store,
		Registry: registry,
		Plugins:  plugins,
		Server:   srv,
	}, nil
}

// BuiltinPlugins is the linked-in set: the six tools, /init and the openai_chat providers.
func BuiltinPlugins(cfg *config.Config, httpc *httpx.Client, home string, env func(string) string) []plugin.Plugin {
	cache := filepath.Join(home, "Library", "Caches", "op-secrets.env")
	resolve := func(ref string) (string, error) { return config.ResolveSecret(ref, env, cache) }
	return append(BuiltinTools(), initcmd.New(), openaichatplugin.New(cfg.Providers, httpc, resolve))
}

// BuiltinTools is the six tool plugins alone, for tests that supply their own provider.
func BuiltinTools() []plugin.Plugin {
	return []plugin.Plugin{read.New(), write.New(), edit.New(), bash.New(), grep.New(), glob.New()}
}

// storeFromEnv opens the session store without wiring providers, for commands that only read.
func storeFromEnv(env func(string) string, home string) (*session.Store, error) {
	paths := config.XDG(env, home)
	return session.OpenStore(filepath.Join(paths.Data, "sessions"))
}
```

- [ ] **Step 4: Run the wiring tests**

Run: `go test ./internal/cli -run 'TestBuild' -v`
Expected: PASS for both.

- [ ] **Step 5: Write the failing printer tests**

`internal/cli/print_test.go`:

```go
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

type helloCommandPlugin struct{}

func (helloCommandPlugin) Name() string { return "hello" }

func (helloCommandPlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterCommand(plugin.Command{
		Name:        "hello",
		Description: "submit a greeting",
		Run: func(ctx context.Context, call plugin.CommandCall) (plugin.Action, error) {
			return plugin.SubmitPrompt{Text: "hi " + call.Args}, nil
		},
	})
}

func TestReadPrompt(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		stdin string
		tty   bool
		want  string
	}{
		{"args only on a tty", []string{"fix", "it"}, "", true, "fix it"},
		{"stdin only", nil, "log line\n", false, "log line"},
		{"stdin then args", []string{"explain"}, "boom\n", false, "boom\n\nexplain"},
		{"empty stdin keeps args", []string{"x"}, "", false, "x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := readPrompt(c.args, strings.NewReader(c.stdin), c.tty)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestReadPromptRejectsOversizedStdin(t *testing.T) {
	big := strings.NewReader(strings.Repeat("x", maxStdin+1))
	if _, err := readPrompt(nil, big, false); err == nil {
		t.Fatal("expected an error for stdin over 10MB")
	}
}

func TestPrintText(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{script: [][]provider.Part{say("ok")}}
	var out bytes.Buffer
	code, err := runPrint(context.Background(), printOptions{Output: "text"}, "Reply with exactly: ok", testBuilder(t, fp), &out, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if out.String() != "ok\n" {
		t.Fatalf("stdout %q", out.String())
	}
	req := fp.request(0)
	if len(req.Messages) != 1 || req.Messages[0].Role != provider.RoleUser {
		t.Fatalf("first request messages: %+v", req.Messages)
	}
}

func TestPrintJSON(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{script: [][]provider.Part{say("ok")}}
	var out bytes.Buffer
	code, err := runPrint(context.Background(), printOptions{Output: "json"}, "hi", testBuilder(t, fp), &out, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	var res printResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("not JSON: %v: %s", err, out.String())
	}
	if res.SessionID == "" || res.Result != "ok" || res.StopReason != session.StopEndTurn {
		t.Fatalf("result %+v", res)
	}
	if res.Usage.Input != 10 || res.Usage.Output != 2 {
		t.Fatalf("usage %+v", res.Usage)
	}
	if res.Cost != "0.000014" {
		t.Fatalf("cost %q", res.Cost)
	}
}

func TestPrintStreamJSON(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{script: [][]provider.Part{say("ok")}}
	var out bytes.Buffer
	code, err := runPrint(context.Background(), printOptions{Output: "stream-json"}, "hi", testBuilder(t, fp), &out, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	methods := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var n struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &n); err != nil {
			t.Fatalf("line %q is not JSON: %v", line, err)
		}
		methods[n.Method]++
	}
	if methods["entry.appended"] < 2 || methods["turn.state"] == 0 || methods["stream.delta"] == 0 {
		t.Fatalf("methods seen: %v", methods)
	}
}

func TestPrintToolRoundTrip(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(ws)
	fp := &fakeProvider{script: [][]provider.Part{
		callTool("tu_1", "glob", `{"pattern":"*.txt"}`),
		say("done"),
	}}
	var out bytes.Buffer
	code, err := runPrint(context.Background(), printOptions{Output: "text"}, "list files", testBuilder(t, fp), &out, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if out.String() != "done\n" {
		t.Fatalf("stdout %q", out.String())
	}
	second := fp.request(1)
	last := second.Messages[len(second.Messages)-1]
	if last.Role != provider.RoleToolResult || last.ToolUseID != "tu_1" {
		t.Fatalf("last message %+v", last)
	}
	if !strings.Contains(textOf(last.Content), "a.txt") {
		t.Fatalf("tool result %q", textOf(last.Content))
	}
}

func TestPrintContinueReusesSession(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{script: [][]provider.Part{say("one"), say("two")}}
	build := testBuilder(t, fp)
	var out1, out2 bytes.Buffer
	if code, err := runPrint(context.Background(), printOptions{Output: "json"}, "first", build, &out1, io.Discard); err != nil || code != 0 {
		t.Fatalf("first: code %d err %v", code, err)
	}
	if code, err := runPrint(context.Background(), printOptions{Output: "json", Continue: true}, "second", build, &out2, io.Discard); err != nil || code != 0 {
		t.Fatalf("second: code %d err %v", code, err)
	}
	var r1, r2 printResult
	json.Unmarshal(out1.Bytes(), &r1)
	json.Unmarshal(out2.Bytes(), &r2)
	if r1.SessionID == "" || r1.SessionID != r2.SessionID {
		t.Fatalf("session ids %q %q", r1.SessionID, r2.SessionID)
	}
	if got := len(fp.request(1).Messages); got != 3 {
		t.Fatalf("second request carries %d messages, want 3", got)
	}
}

func TestPrintContinueWithNoSession(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{}
	var errb bytes.Buffer
	code, _ := runPrint(context.Background(), printOptions{Output: "text", Continue: true}, "x", testBuilder(t, fp), io.Discard, &errb)
	if code != 2 || !strings.Contains(errb.String(), "no session") {
		t.Fatalf("code %d stderr %q", code, errb.String())
	}
}

func TestPrintSlashCommand(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{script: [][]provider.Part{say("ok")}}
	build := testBuilder(t, fp, helloCommandPlugin{})
	var out bytes.Buffer
	code, err := runPrint(context.Background(), printOptions{Output: "text"}, "/hello there", build, &out, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if out.String() != "ok\n" {
		t.Fatalf("stdout %q", out.String())
	}
	if got := textOf(fp.request(0).Messages[0].Content); got != "hi there" {
		t.Fatalf("submitted prompt %q", got)
	}
}

func TestPrintUnknownSlashCommand(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{}
	var errb bytes.Buffer
	code, _ := runPrint(context.Background(), printOptions{Output: "text"}, "/nope", testBuilder(t, fp), io.Discard, &errb)
	if code != 2 || !strings.Contains(errb.String(), "unknown command /nope") {
		t.Fatalf("code %d stderr %q", code, errb.String())
	}
}

func TestPrintProviderFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{fail: &provider.Error{Class: session.ErrProvider, Status: 500, Message: "boom"}}
	var errb bytes.Buffer
	code, _ := runPrint(context.Background(), printOptions{Output: "text"}, "hi", testBuilder(t, fp), io.Discard, &errb)
	if code != 1 || !strings.Contains(errb.String(), "boom") {
		t.Fatalf("code %d stderr %q", code, errb.String())
	}
}

func TestRootPrintFlag(t *testing.T) {
	t.Chdir(t.TempDir())
	old := stdinIsTerminal
	stdinIsTerminal = func() bool { return true }
	t.Cleanup(func() { stdinIsTerminal = old })
	fp := &fakeProvider{script: [][]provider.Part{say("ok")}}
	root := newRoot("test", testBuilder(t, fp))
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs([]string{"-p", "--output", "json", "hello"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute: %v stderr %s", err, errb.String())
	}
	var res printResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.Result != "ok" {
		t.Fatalf("stdout %q err %v", out.String(), err)
	}
}

func TestRootWithoutPrintExplains(t *testing.T) {
	root := newRoot("test", testBuilder(t, &fakeProvider{}))
	var errb bytes.Buffer
	root.SetErr(&errb)
	root.SetArgs([]string{"hello"})
	err := root.ExecuteContext(context.Background())
	var ee ExitError
	if !errorsAs(err, &ee) || ee.Code != 2 || !strings.Contains(errb.String(), "--print") {
		t.Fatalf("err %v stderr %q", err, errb.String())
	}
}
```

Add this helper at the bottom of `print_test.go` so the test file needs no extra import:

```go
func errorsAs(err error, target *ExitError) bool {
	for err != nil {
		if e, ok := err.(ExitError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
```

- [ ] **Step 6: Run them to verify they fail**

Run: `go test ./internal/cli -run 'TestReadPrompt|TestPrint|TestRoot' -v`
Expected: FAIL to compile with `undefined: readPrompt`, `undefined: runPrint`, `undefined: printOptions`, `undefined: printResult`, `undefined: newRoot`, `undefined: ExitError`, `undefined: stdinIsTerminal`, `undefined: textOf`, `undefined: maxStdin`.

- [ ] **Step 7: Write `internal/cli/print.go`**

```go
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/session"
)

const maxStdin = 10 << 20

// ExitError carries a process exit code out of a cobra RunE. main maps it to os.Exit.
type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("exit %d", e.Code) }

type printOptions struct {
	Output   string
	Model    string
	Mode     string
	Thinking string
	Resume   string
	Continue bool
}

// printResult is the --output json shape.
type printResult struct {
	SessionID  string             `json:"session_id"`
	Result     string             `json:"result"`
	Usage      session.Usage      `json:"usage"`
	Cost       string             `json:"cost"`
	StopReason session.StopReason `json:"stop_reason"`
}

// stdinIsTerminal is a variable so tests can pretend.
var stdinIsTerminal = func() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return true
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// registerPrint adds the --print flags and the root run function.
func registerPrint(root *cobra.Command, build buildFunc) {
	var headless bool
	var o printOptions
	f := root.Flags()
	f.BoolVarP(&headless, "print", "p", false, "run one prompt headless and print the result")
	f.StringVar(&o.Output, "output", "text", "text, json or stream-json")
	f.StringVar(&o.Model, "model", "", "provider:id, or a model id unique across providers")
	f.StringVar(&o.Mode, "mode", "", "strict, permissive or off")
	f.StringVar(&o.Thinking, "thinking", "", "off, low, medium or high")
	f.StringVar(&o.Resume, "resume", "", "session id to continue")
	f.BoolVar(&o.Continue, "continue", false, "continue the newest session for this directory")
	root.Args = cobra.ArbitraryArgs
	root.RunE = func(cmd *cobra.Command, args []string) error {
		stderr := cmd.ErrOrStderr()
		if !headless {
			fmt.Fprintln(stderr, "the TUI is not built yet; run with --print")
			return ExitError{2}
		}
		switch o.Output {
		case "text", "json", "stream-json":
		default:
			fmt.Fprintf(stderr, "unknown --output %q, want text, json or stream-json\n", o.Output)
			return ExitError{2}
		}
		prompt, err := readPrompt(args, cmd.InOrStdin(), stdinIsTerminal())
		if err != nil {
			fmt.Fprintln(stderr, err)
			return ExitError{2}
		}
		if prompt == "" {
			fmt.Fprintln(stderr, "no prompt: pass it as an argument or on stdin")
			return ExitError{2}
		}
		code, err := runPrint(cmd.Context(), o, prompt, build, cmd.OutOrStdout(), stderr)
		if err != nil {
			fmt.Fprintln(stderr, err)
			if code == 0 {
				code = 1
			}
		}
		if code != 0 {
			return ExitError{code}
		}
		return nil
	}
}

// readPrompt joins the positional words and, when stdin is piped, prepends its content.
func readPrompt(args []string, stdin io.Reader, tty bool) (string, error) {
	prompt := strings.Join(args, " ")
	if tty {
		return prompt, nil
	}
	data, err := io.ReadAll(io.LimitReader(stdin, maxStdin+1))
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	if len(data) > maxStdin {
		return "", errors.New("stdin exceeds 10MB; write it to a file and name the file in the prompt")
	}
	piped := strings.TrimRight(string(data), "\n")
	switch {
	case piped == "":
		return prompt, nil
	case prompt == "":
		return piped, nil
	default:
		return piped + "\n\n" + prompt, nil
	}
}

// runPrint runs one turn against an embedded server and prints it. It returns the process
// exit code: 0 completed, 1 failed, 2 usage, 130 interrupted.
func runPrint(ctx context.Context, o printOptions, prompt string, build buildFunc, stdout, stderr io.Writer) (int, error) {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	b, err := build(ctx, stderr)
	if err != nil {
		return 1, err
	}
	srvCtx, cancelSrv := context.WithCancel(context.Background())
	defer func() {
		cancelSrv()
		shutdownCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = b.Server.Shutdown(shutdownCtx)
	}()
	clientConn, serverConn := protocol.Pipe()
	go func() { _ = b.Server.Serve(srvCtx, serverConn) }()
	client := protocol.NewClient(clientConn)
	defer client.Close()

	// Calls use a background context so an interrupt can still be delivered after ctx ends.
	bg := context.Background()
	var hello protocol.ClientHelloResult
	if err := client.Call(bg, protocol.MethodClientHello, protocol.ClientHelloParams{Client: "rudy-print", Version: b.Version, Asker: false}, &hello); err != nil {
		return 1, fmt.Errorf("hello: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 1, err
	}
	info, code, err := openOrResume(bg, client, b, o, cwd)
	if err != nil {
		return code, err
	}

	turnID, code, err := submit(bg, client, info.SessionID, prompt, stdout, stderr)
	if err != nil || code != 0 {
		return code, err
	}

	out := &turnOutput{turnID: turnID}
	enc := json.NewEncoder(stdout)
	for {
		select {
		case <-ctx.Done():
			ictx, done := context.WithTimeout(bg, 5*time.Second)
			_ = client.Call(ictx, protocol.MethodSessionInterrupt, protocol.SessionInterruptParams{SessionID: info.SessionID, How: session.InterruptCancel}, nil)
			done()
			return 130, nil
		case n, ok := <-client.Notifications():
			if !ok {
				return 1, errors.New("server closed the connection")
			}
			if o.Output == "stream-json" {
				if err := enc.Encode(map[string]any{"method": n.Method, "params": json.RawMessage(n.Params)}); err != nil {
					return 1, err
				}
			}
			finished, err := out.observe(n)
			if err != nil {
				return 1, err
			}
			if !finished {
				continue
			}
			return out.finish(o, b, info, stdout, stderr)
		}
	}
}

// openOrResume opens a new session or resumes one named by --resume or --continue and applies
// the --model, --mode and --thinking flags to it.
func openOrResume(ctx context.Context, client *protocol.Client, b *Built, o printOptions, cwd string) (protocol.SessionInfo, int, error) {
	var info protocol.SessionInfo
	id := o.Resume
	if o.Continue && id == "" {
		newest, err := newestFor(b.Store, cwd)
		if err != nil {
			return info, 2, err
		}
		id = newest
	}
	if id == "" {
		err := client.Call(ctx, protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: cwd, Model: o.Model, Mode: o.Mode, Thinking: o.Thinking}, &info)
		if err != nil {
			return info, 1, fmt.Errorf("open session: %w", err)
		}
		return info, 0, nil
	}
	if _, err := ulid.Parse(id); err != nil {
		return info, 2, fmt.Errorf("--resume %q is not a session id", id)
	}
	if err := client.Call(ctx, protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: id}, &info); err != nil {
		return info, 1, fmt.Errorf("resume %s: %w", id, err)
	}
	// The set_* methods answer server.EntryIDResult, not SessionInfo, so info is updated here
	// from the same values the server accepted.
	var set server.EntryIDResult
	if o.Model != "" {
		if err := client.Call(ctx, protocol.MethodSessionSetModel, protocol.SessionSetModelParams{SessionID: id, Model: o.Model}, &set); err != nil {
			return info, 1, err
		}
		m, err := b.Registry.Resolve(o.Model)
		if err != nil {
			return info, 1, err
		}
		info.Model = m.Ref
	}
	if o.Mode != "" {
		if err := client.Call(ctx, protocol.MethodSessionSetMode, protocol.SessionSetModeParams{SessionID: id, Mode: session.Mode(o.Mode)}, &set); err != nil {
			return info, 1, err
		}
		info.Mode = session.Mode(o.Mode)
	}
	if o.Thinking != "" {
		if err := client.Call(ctx, protocol.MethodSessionSetThinking, protocol.SessionSetThinkingParams{SessionID: id, Thinking: session.ThinkingLevel(o.Thinking)}, &set); err != nil {
			return info, 1, err
		}
		info.Thinking = session.ThinkingLevel(o.Thinking)
	}
	return info, 0, nil
}

// newestFor returns the newest session opened on cwd.
func newestFor(st *session.Store, cwd string) (string, error) {
	want, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		want = cwd
	}
	list, err := st.List()
	if err != nil {
		return "", err
	}
	for _, s := range list {
		root, err := filepath.EvalSymlinks(s.Workspace.Root)
		if err != nil {
			root = s.Workspace.Root
		}
		if root == want {
			return s.ID.String(), nil
		}
	}
	return "", fmt.Errorf("no session for %s; drop --continue to start one", cwd)
}

// submit sends the prompt, routing a leading slash word to command.run. It returns the turn id,
// or exit code 2 for an unknown command, or 0 with an empty turn id when a command produced no turn.
func submit(ctx context.Context, client *protocol.Client, sessionID, prompt string, stdout, stderr io.Writer) (string, int, error) {
	if strings.HasPrefix(prompt, "/") {
		name, args, _ := strings.Cut(strings.TrimPrefix(prompt, "/"), " ")
		var res protocol.CommandRunResult
		err := client.Call(ctx, protocol.MethodCommandRun, protocol.CommandRunParams{SessionID: sessionID, Name: name, Args: strings.TrimSpace(args)}, &res)
		var perr *protocol.Error
		if errors.As(err, &perr) && perr.Code == protocol.CodeNotFound {
			fmt.Fprintf(stderr, "unknown command /%s\n", name)
			return "", 2, nil
		}
		if err != nil {
			return "", 1, fmt.Errorf("/%s: %w", name, err)
		}
		if res.TurnID == "" {
			if res.Notice != "" {
				fmt.Fprintln(stdout, res.Notice)
			}
			return "", 0, nil
		}
		return res.TurnID, 0, nil
	}
	var res protocol.SessionSubmitResult
	params := protocol.SessionSubmitParams{SessionID: sessionID, Content: []session.Block{session.TextBlock(prompt)}, Source: session.SourceTyped}
	if err := client.Call(ctx, protocol.MethodSessionSubmit, params, &res); err != nil {
		return "", 1, fmt.Errorf("submit: %w", err)
	}
	return res.TurnID, 0, nil
}

// turnOutput folds notifications into what the printer reports.
type turnOutput struct {
	turnID     string
	text       string
	usage      session.Usage
	stopReason session.StopReason
	failure    session.TurnFailed // zero until a turn_failed entry arrives
	state      string
}

// observe folds one notification in and reports whether the turn is over.
func (t *turnOutput) observe(n protocol.Notification) (bool, error) {
	switch n.Method {
	case protocol.NotifyEntryAppended:
		var ea protocol.EntryAppended
		if err := json.Unmarshal(n.Params, &ea); err != nil {
			return false, fmt.Errorf("entry.appended: %w", err)
		}
		switch p := ea.Entry.Payload.(type) {
		case session.AssistantMessage:
			t.text = textOf(p.Content)
			t.usage = t.usage.Add(p.Usage)
			t.stopReason = p.StopReason
		case session.TurnFailed:
			t.failure = p
		}
	case protocol.NotifyTurnState:
		var ts protocol.TurnStateChanged
		if err := json.Unmarshal(n.Params, &ts); err != nil {
			return false, fmt.Errorf("turn.state: %w", err)
		}
		if ts.TurnID != t.turnID {
			return false, nil
		}
		t.state = ts.State
		switch ts.State {
		case "completed", "failed", "idle":
			return true, nil
		}
	}
	return false, nil
}

// finish prints the result in the requested shape and returns the exit code.
func (t *turnOutput) finish(o printOptions, b *Built, info protocol.SessionInfo, stdout, stderr io.Writer) (int, error) {
	if t.state == "failed" {
		fmt.Fprintf(stderr, "turn failed (%s, %d retries): %s\n", t.failure.Class, t.failure.Retries, t.failure.Message)
		return 1, nil
	}
	switch o.Output {
	case "text":
		if t.text != "" {
			fmt.Fprintln(stdout, t.text)
		}
	case "json":
		cost := ""
		if m, err := b.Registry.Resolve(info.Model.String()); err == nil {
			if c, known := m.Pricing.Cost(t.usage); known {
				cost = c
			}
		}
		res := printResult{SessionID: info.SessionID, Result: t.text, Usage: t.usage, Cost: cost, StopReason: t.stopReason}
		if err := json.NewEncoder(stdout).Encode(res); err != nil {
			return 1, err
		}
	}
	if t.state == "idle" {
		return 130, nil
	}
	return 0, nil
}

// textOf joins the text blocks of a message.
func textOf(blocks []session.Block) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == session.BlockText {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
```

- [ ] **Step 8: Replace `internal/cli/root.go`**

```go
package cli

import (
	"context"
	"io"

	"github.com/spf13/cobra"
)

// version is set by the linker:
// -X github.com/guygrigsby/rudy/internal/cli.version=<v>
var version = "dev"

// Version reports the build version.
func Version() string { return version }

// NewRoot is the rudy command with the real wiring. Same signature as Task 1.
func NewRoot() *cobra.Command {
	return newRoot(version, func(ctx context.Context, stderr io.Writer) (*Built, error) {
		return Build(ctx, BuildOptions{Version: version, Stderr: stderr})
	})
}

// newRoot takes the wiring as a parameter so tests can substitute a fake provider.
func newRoot(version string, build buildFunc) *cobra.Command {
	root := &cobra.Command{
		Use:           "rudy [prompt]",
		Short:         "a coding agent harness",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	registerPrint(root, build)
	return root
}
```

- [ ] **Step 9: Replace `cmd/rudy/main.go`**

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/guygrigsby/rudy/internal/cli"
)

func main() {
	err := cli.NewRoot().ExecuteContext(context.Background())
	if err == nil {
		return
	}
	var ee cli.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.Code)
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
```

- [ ] **Step 10: Run the printer tests**

Run: `go test -race ./internal/cli -v`
Expected: PASS for every test in the package. If `TestPrintJSON` reports a cost other than `0.000014`, `Pricing.Cost` (Task 8) is not rendering six decimals; fix it there, not here.

- [ ] **Step 11: Build and run the binary once without config**

Run: `go build -o bin/rudy ./cmd/rudy && ./bin/rudy -p "hi"; echo "exit $?"`
Expected: stderr `model registry: …` naming the missing providers, exit 1, because no config names a provider yet. Task 19 supplies the config.

- [ ] **Step 12: Commit**

```bash
git add internal/cli cmd/rudy/main.go
git commit -m "cli: wiring and the headless printer client"
bd close <id>
```

---

### Task 18: `rudy models` and `rudy sessions list`

**Files:**
- Create: `internal/cli/models.go`
- Create: `internal/cli/sessions.go`
- Create: `internal/cli/models_test.go`
- Create: `internal/cli/sessions_test.go`
- Modify: `internal/cli/root.go` (add the two subcommands in `newRoot`)

**Interfaces:**
- Consumes: `buildFunc`, `Built`, `storeFromEnv` (Task 17); `provider.Model`, `provider.Pricing`, `provider.Registry.Models` (Task 8); `session.Store.List`, `session.Summary`, `session.Open`, `session.SessionOpened` (Tasks 4 and 5).
- Produces: `newModelsCommand(build buildFunc) *cobra.Command`, `newSessionsCommand() *cobra.Command`, `renderModels`, `renderSessions`, `per1M`.

- [ ] **Step 0: Track the task**

```bash
bd create --title "Task 18: models and sessions list commands" --type task
bd update <id> --claim
```

- [ ] **Step 1: Write the failing models tests**

`internal/cli/models_test.go`:

```go
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

func TestPer1M(t *testing.T) {
	cases := map[string]string{
		"0.00000014": "$0.14",
		"0.000001":   "$1.00",
		"0.00003":    "$30.00",
		"":           "-",
		"nonsense":   "-",
	}
	for in, want := range cases {
		if got := per1M(in); got != want {
			t.Errorf("per1M(%q) = %q want %q", in, got, want)
		}
	}
}

func TestRenderModels(t *testing.T) {
	models := []provider.Model{
		{Ref: session.ModelRef{Provider: "aperture", Model: "cline-pass/kimi-k3"}, DisplayName: "Kimi K3", ContextWindow: 262144, Pricing: provider.Pricing{Input: "0.0000006", Output: "0.0000025"}},
		{Ref: session.ModelRef{Provider: "mlx", Model: "qwen3-coder"}},
	}
	var out bytes.Buffer
	if err := renderModels(&out, models); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want header plus two rows, got %q", out.String())
	}
	if !strings.HasPrefix(lines[0], "PROVIDER") {
		t.Fatalf("header %q", lines[0])
	}
	if !strings.Contains(lines[1], "cline-pass/kimi-k3") || !strings.Contains(lines[1], "$0.60") || !strings.Contains(lines[1], "$2.50") || !strings.Contains(lines[1], "262144") {
		t.Fatalf("row %q", lines[1])
	}
	if !strings.Contains(lines[2], "qwen3-coder") || strings.Count(lines[2], "-") < 3 {
		t.Fatalf("row with unknowns %q", lines[2])
	}
}

func TestModelsCommand(t *testing.T) {
	fp := &fakeProvider{}
	root := newRoot("test", testBuilder(t, fp))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"models"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "fake") || !strings.Contains(out.String(), "Fake M") {
		t.Fatalf("output %q", out.String())
	}
	out.Reset()
	root.SetArgs([]string{"models", "--json"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute --json: %v", err)
	}
	var models []provider.Model
	if err := json.Unmarshal(out.Bytes(), &models); err != nil || len(models) != 1 {
		t.Fatalf("json output %q err %v", out.String(), err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/cli -run 'TestPer1M|TestRenderModels|TestModelsCommand' -v`
Expected: FAIL to compile with `undefined: per1M`, `undefined: renderModels`.

- [ ] **Step 3: Write `internal/cli/models.go`**

```go
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/guygrigsby/rudy/internal/provider"
)

func newModelsCommand(build buildFunc) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "models",
		Short: "list the discovered models with context windows and prices",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := build(cmd.Context(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer b.Server.Shutdown(context.Background())
			models := b.Registry.Models()
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(models)
			}
			return renderModels(cmd.OutOrStdout(), models)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the registry as JSON")
	return cmd
}

// renderModels prints one row per model. Prices are per million tokens.
func renderModels(w io.Writer, models []provider.Model) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tMODEL\tCONTEXT\tIN/1M\tOUT/1M\tNAME")
	for _, m := range models {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			m.Ref.Provider, m.Ref.Model, intOrDash(m.ContextWindow),
			per1M(m.Pricing.Input), per1M(m.Pricing.Output), m.DisplayName)
	}
	return tw.Flush()
}

// per1M turns a per-token decimal string into dollars per million tokens, or "-" when unknown.
func per1M(perToken string) string {
	if perToken == "" {
		return "-"
	}
	r, ok := new(big.Rat).SetString(perToken)
	if !ok {
		return "-"
	}
	r.Mul(r, big.NewRat(1_000_000, 1))
	return "$" + r.FloatString(2)
}

func intOrDash(n int64) string {
	if n == 0 {
		return "-"
	}
	return strconv.FormatInt(n, 10)
}
```

- [ ] **Step 4: Register it in `newRoot`**

In `internal/cli/root.go`, after `registerPrint(root, build)`:

```go
	root.AddCommand(newModelsCommand(build), newSessionsCommand())
```

`newSessionsCommand` arrives in Step 7; leave the line in and expect the compile error until then, or add it after Step 7. Either order ends green.

- [ ] **Step 5: Write the failing sessions tests**

`internal/cli/sessions_test.go`:

```go
package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/session"
)

func openTestSession(t *testing.T, st *session.Store, root string) *session.Session {
	t.Helper()
	s, err := session.Open(st, session.SessionOpened{
		SchemaVersion: 1,
		RudyVersion:   "test",
		Workspace:     session.Workspace{Root: root, ProjectID: "local/" + filepath.Base(root)},
		Model:         session.ModelRef{Provider: "fake", Model: "m"},
		Thinking:      session.ThinkingOff,
		Mode:          session.ModeOff,
		Agent:         "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRenderSessionsNewestFirst(t *testing.T) {
	st, err := session.OpenStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	first := openTestSession(t, st, "/tmp/alpha")
	second := openTestSession(t, st, "/tmp/beta")
	list, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := renderSessions(&out, list); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "ID") {
		t.Fatalf("output %q", out.String())
	}
	if !strings.Contains(lines[1], second.ID().String()) || !strings.Contains(lines[1], "/tmp/beta") {
		t.Fatalf("first row should be the newest session: %q", lines[1])
	}
	if !strings.Contains(lines[2], first.ID().String()) || !strings.Contains(lines[2], "fake:m") {
		t.Fatalf("second row %q", lines[2])
	}
}

func TestSessionsListCommand(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("HOME", base)
	st, err := session.OpenStore(filepath.Join(base, "data", "rudy", "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	s := openTestSession(t, st, "/tmp/gamma")
	s.Close()
	root := newRoot("test", testBuilder(t, &fakeProvider{}))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"sessions", "list"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), s.ID().String()) || !strings.Contains(out.String(), "/tmp/gamma") {
		t.Fatalf("output %q", out.String())
	}
}
```

Note the second test sets `XDG_DATA_HOME` before `testBuilder` runs; `testBuilder` then overrides it with its own temp dir. Keep the store path in this test aligned with `testBuilder` by reading `XDG_DATA_HOME` after calling it instead. Replace the first three lines of `TestSessionsListCommand` with:

```go
	root := newRoot("test", testBuilder(t, &fakeProvider{}))
	st, err := session.OpenStore(filepath.Join(os.Getenv("XDG_DATA_HOME"), "rudy", "sessions"))
	if err != nil {
		t.Fatal(err)
	}
```

and add `"os"` to the imports. The remainder of the test is unchanged.

- [ ] **Step 6: Run them to verify they fail**

Run: `go test ./internal/cli -run 'TestRenderSessions|TestSessionsList' -v`
Expected: FAIL to compile with `undefined: renderSessions`, `undefined: newSessionsCommand`.

- [ ] **Step 7: Write `internal/cli/sessions.go`**

```go
package cli

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/guygrigsby/rudy/internal/session"
)

func newSessionsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "list and manage sessions",
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "list sessions, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			st, err := storeFromEnv(os.Getenv, home)
			if err != nil {
				return err
			}
			summaries, err := st.List()
			if err != nil {
				return err
			}
			return renderSessions(cmd.OutOrStdout(), summaries)
		},
	}
	cmd.AddCommand(list)
	return cmd
}

// renderSessions prints one row per session in the order Store.List returns them.
func renderSessions(w io.Writer, summaries []session.Summary) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tOPENED\tWORKSPACE\tMODEL\tFORKED")
	for _, s := range summaries {
		forked := ""
		if s.Forked {
			forked = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			s.ID.String(), s.OpenedAt.Local().Format("2006-01-02 15:04"), s.Workspace.Root, s.Model.String(), forked)
	}
	return tw.Flush()
}
```

- [ ] **Step 8: Run the package tests**

Run: `go test -race ./internal/cli -v`
Expected: PASS for every test, including Task 17's.

- [ ] **Step 9: Commit**

```bash
git add internal/cli
git commit -m "cli: models and sessions list"
bd close <id>
```

---

### Task 19: Real-path verification, README and closeout

**Files:**
- Create: `README.md`
- Create: `~/.config/rudy/config.toml` (outside the repo, the owner's machine)

**Interfaces:**
- Consumes: the built binary from Tasks 1 to 18.
- Produces: nothing in code. This task proves the printer works through the real path against the aperture proxy and records how.

- [ ] **Step 0: Track the task**

```bash
bd create --title "Task 19: real-path verification and README" --type task
bd update <id> --claim
```

- [ ] **Step 1: Write the owner's config**

Create `~/.config/rudy/config.toml`:

```toml
[default]
provider = "aperture"
model = "cline-pass/kimi-k3"
thinking = "off"

[permissions]
mode = "off"

[providers.aperture]
wire = "openai_chat"
base_url = "https://ai.guy.ts.net/v1"

[providers.mlx]
wire = "openai_chat"
base_url = "http://localhost:8080/v1"
```

Neither provider needs a token; the proxy authenticates by tailnet and mlx is local. Leave `auth` unset.

- [ ] **Step 2: Build**

Run: `make build && ls -la bin/rudy`
Expected: the binary exists and `make build` printed nothing but the go build line.

- [ ] **Step 3: The registry**

Run: `bin/rudy models | head -20`
Expected: a header row, then aperture rows including one starting `aperture  cline-pass/kimi-k3` with a non-empty IN/1M price such as `$0.60` and a context window such as `262144`. If mlx is not running, stderr shows one line `rudy: registry refresh: mlx: …` and the aperture rows still print, because the snapshot from the first successful refresh exists. On the very first run with mlx down there is no snapshot yet; the aperture rows still print because Refresh keeps every provider that succeeded.

Run: `ls -la ~/.cache/rudy/registry.json`
Expected: the snapshot file exists.

- [ ] **Step 4: A text turn**

Run: `bin/rudy -p "Reply with exactly: ok"; echo "exit $?"`
Expected: stdout `ok`, exit 0.

- [ ] **Step 5: A tool round trip with cost**

Run, from the repo root: `bin/rudy -p --output json "What files are in this directory? Use the glob tool with pattern '*'. Then answer in one line."`
Expected: one JSON line with `session_id`, a `result` naming files such as `Makefile` and `go.mod`, `usage` with non-zero `input` and `output`, a `cost` such as `0.001234` and `stop_reason` `end_turn`.

Run: `bin/rudy -p --output stream-json "Use the read tool on go.mod and tell me the module path." | grep -c '"method":"entry.appended"'`
Expected: at least 4, being the user message, the assistant tool call, the permission decision, the tool result and the final assistant message.

- [ ] **Step 6: Strict mode with no asker denies**

Run: `bin/rudy -p --mode strict "Create a file named PROBE.txt containing hi"; echo "exit $?"; ls PROBE.txt`
Expected: the assistant explains it could not write the file, exit 0 and `ls` reports no such file. Then confirm the log:

```bash
id=$(bin/rudy sessions list | sed -n 2p | awk '{print $1}')
grep -c '"decided_by":"no_asker"' ~/.local/share/rudy/sessions/$id/entries.jsonl
```

Expected: 1 or more.

- [ ] **Step 7: `/init` in a scratch clone**

```bash
tmp=$(mktemp -d)
git clone -q . "$tmp/rudy"
cd "$tmp/rudy" && rm -f AGENTS.md
"$OLDPWD/bin/rudy" -p "/init"; echo "exit $?"
head -40 AGENTS.md
cd "$OLDPWD"
```

Expected: exit 0, and `AGENTS.md` exists with sections on what rudy is, how to build and test and conventions drawn from `CLAUDE.md` and the Makefile. The real repo's `AGENTS.md` symlink is untouched: `ls -la AGENTS.md` still shows `-> CLAUDE.md`.

- [ ] **Step 8: Sessions and the durable-record rule**

Run: `bin/rudy sessions list`
Expected: one row per run above, newest first, each with this repo's path and `aperture:cline-pass/kimi-k3`.

Run, for the session from Step 5:

```bash
id=$(bin/rudy sessions list | sed -n 3p | awk '{print $1}')
python3 - "$HOME/.local/share/rudy/sessions/$id/entries.jsonl" <<'EOF'
import json, sys
decided = set()
for line in open(sys.argv[1]):
    e = json.loads(line)
    if e["kind"] == "permission_decision":
        decided.add(e["tool_use_id"])
    if e["kind"] == "tool_result":
        assert e["tool_use_id"] in decided, "tool_result before permission_decision: " + line
print("ok: every tool_result follows its permission_decision")
EOF
```

Expected: `ok: every tool_result follows its permission_decision`.

- [ ] **Step 9: Write `README.md`**

```markdown
# rudy

A coding agent harness in Go. One binary, one JSON-RPC protocol, sessions as
append-only JSONL logs, every tool and provider a plugin. The terminal client
and the server mode are the next plans; this build is the kernel and the
headless printer.

## Build

    make            # bin/rudy
    make check      # fmt, vet, lint, test
    make install    # go install ./cmd/rudy

## Configure

`~/.config/rudy/config.toml` names providers and one default model. The
registry itself is discovered from each provider's `/v1/models`.

    [default]
    provider = "aperture"
    model = "cline-pass/kimi-k3"

    [permissions]
    mode = "strict"        # strict, permissive or off

    [providers.aperture]
    wire = "openai_chat"
    base_url = "https://ai.guy.ts.net/v1"

    [providers.mlx]
    wire = "openai_chat"
    base_url = "http://localhost:8080/v1"

`auth = "env:NAME"` or `auth = "cache:KEY"` adds a bearer token from the
environment or from `~/Library/Caches/op-secrets.env`.

## Run headless

    rudy -p "Reply with exactly: ok"
    rudy -p --output json "Summarize this repository"
    git diff main | rudy -p "report typos in this diff"
    rudy -p --continue "now fix the first one"
    rudy -p --mode off "run the tests and fix what fails"
    rudy -p "/init"                       # writes ./AGENTS.md

`--output stream-json` prints every protocol notification as one JSON line.
Exit codes: 0 completed, 1 the turn failed, 2 usage, 130 interrupted.

In strict mode with no client able to answer, every unsafe tool call is
denied and recorded; pass `--mode permissive` or `--mode off` for unattended
runs.

## Inspect

    rudy models          # the discovered registry with prices
    rudy sessions list   # sessions, newest first

Sessions live under `~/.local/share/rudy/sessions/<id>/entries.jsonl`.

## Design

`docs/specs/2026-09-07-rudy-design.md` is the design; the domain model,
contracts and ADRs sit beside it.
```

- [ ] **Step 10: Quality gate and closeout**

Run: `make check`
Expected: green.

Run: `bd list --status open`
Expected: no open issues from this plan. Close any straggler with `bd close <id>`.

- [ ] **Step 11: Commit**

```bash
git add README.md
git commit -m "docs: README and kernel verification notes"
bd close <id>
```

Do not push. Pushing is the owner's call.

---

## Beads

Epic `rudy-k0`. Task N is issue `rudy-k0.N`, so Task 7 is `rudy-k0.7`. Dependencies mirror the package order above; `bd ready` lists what can start. Claim with `bd update rudy-k0.N --claim`, close in the task's final commit step.

## Plans that follow

Each is written once this plan's code exists to reference, in this order:

1. `memory-go`: the OKF SDK in the memory repository, byte-compatible with the Node implementation. Sequenced first because rudy's memory plugin imports it.
2. Plugins: `anthropic_messages` codec and plugin, the clinepass dialect, skills loader with `rudy skills migrate`, hooks runner over the eight hook points, subagents, compaction, the MCP plugin over the go-sdk, the memory plugin, spawned plugins over stdio, `rudy mcp` and `rudy plugin` commands.
3. TUI: Bubble Tea v2 client, slots, transcript rendering from entries and deltas, vimbubble ported to bubbles v2, pi's keys, Esc semantics, themes, `rudy keys migrate pi` and `rudy themes migrate pi`.
4. Server: `rudy serve` on the unix socket, attach and reattach with replay, session lock handoff, `rudy update`.

## Spec coverage

| spec section | tasks |
|---|---|
| Session, entry kinds, fork by reference, recovery | 2, 3, 4, 5 |
| Permissions, three modes, allow before run, no asker | 10, 13 |
| Providers and registry, `/v1/models`, headers, retries | 8, 9, 16 |
| Plugins, kernel versus plugin split, degrade on failure | 11, 15, 16 |
| Protocol, one binary one protocol, in-memory transport | 12, 14 |
| CLI, `rudy -p`, `rudy models`, `rudy sessions`, `/init` | 16, 17, 18 |
| Durability and errors | 3, 4, 8, 13 |
| Testing, fixtures, no network | every task; 9 for the recorded fixtures |
| Real-path verification | 19 |

Not in this plan, by design: the TUI, hooks, skills, subagents, compaction, MCP, memory, `anthropic_messages`, clinepass, `rudy serve`, `rudy mcp`, `rudy plugin`, `rudy update`, migrations, ACP.
