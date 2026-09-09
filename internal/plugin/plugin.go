// Package plugin is how every capability arrives: tools, slash commands and
// providers, whether the plugin is linked into the binary or spawned. Built-ins
// use exactly this interface; nothing in the kernel has a private path.
package plugin

import (
	"context"
	"errors"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// The render vocabulary a plugin draws with. Defined in internal/protocol, where the
// status.updated and widget.updated payloads that carry them are, and aliased here so a
// plugin author writes plugin.Span and never imports the wire package.
type (
	Span       = protocol.Span
	WidgetSlot = protocol.WidgetSlot
	StatusItem = protocol.StatusItem
	Widget     = protocol.Widget
)

const (
	SlotHeader      = protocol.SlotHeader
	SlotAboveEditor = protocol.SlotAboveEditor
	SlotBelowEditor = protocol.SlotBelowEditor
)

// ErrNoServer is what Note and Connect return before a server has handed the registry its
// services: a plugin loaded by a test, or one whose Init runs before SetServices.
var ErrNoServer = errors.New("plugin: no server attached yet")

// Services is what the server hands the registry so a host can reach it. Set once, after
// server.New and before Load. The kernel has no private path: Connect gives a plugin the
// same JSON-RPC surface a spawned plugin dials, authenticated as caller class plugin.
type Services struct {
	Note          func(sessionID ulid.ULID, plugin string, text string, role session.NoteRole) error
	Connect       func(ctx context.Context, plugin string) (protocol.Conn, error)
	StatusChanged func()         // after any SetStatus; the server broadcasts status.updated
	WidgetChanged func(w Widget) // after any SetWidget; the server broadcasts widget.updated
	// ProvidersChanged is called after Fail has withdrawn a plugin's providers, with the
	// set that survives. The provider registry keeps its own copy of the provider set, so
	// withdrawing one here is invisible to a turn until that copy is replaced.
	ProvidersChanged func(ps []provider.Provider)
}

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
	RegisterHook(h HookHandler) error // invalid point or nil Handle refused
	Config() map[string]any           // the plugin's [plugins.<name>] table, never nil
	Notice(text string)

	// SetStatus replaces this plugin's status item under key; empty content clears it. A
	// plugin can only ever touch its own: items are keyed by owner.
	SetStatus(key string, content []Span)
	// SetWidget replaces this plugin's widget under key. An unknown slot is refused.
	SetWidget(key string, slot WidgetSlot, content []Span) error

	// Note appends a note entry to a live session. ErrNoServer before attach.
	Note(sessionID ulid.ULID, text string, role session.NoteRole) error
	// Connect returns a client authenticated as caller class plugin. ErrNoServer before
	// attach. The caller owns the client and must Close it.
	Connect(ctx context.Context) (*protocol.Client, error)

	// What the whole registry has committed, for a plugin that needs to see its peers (a
	// /help command listing commands, a status item counting tools).
	Commands() []Command
	Tools() []tool.Tool
	Statuses() []Status
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
