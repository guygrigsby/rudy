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
