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
	RegisterHook(h HookHandler) error // invalid point or nil Handle refused
	Config() map[string]any           // the plugin's [plugins.<name>] table, never nil
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
