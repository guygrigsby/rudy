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
