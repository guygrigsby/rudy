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
	MethodSessionCompact     = "session.compact"
	MethodRegistryList       = "registry.list"
	MethodRegistryRefresh    = "registry.refresh"
	MethodCommandRun         = "command.run"
)

// Methods, plugin to server. A connection may send these only when its caller class is
// plugin; a client gets unauthorized.
const (
	MethodPluginAppendNote = "plugin.append_note"
)

// Notifications, server to client.
const (
	NotifyEntryAppended       = "entry.appended"
	NotifyStreamDelta         = "stream.delta"
	NotifyTurnState           = "turn.state"
	NotifyPermissionRequested = "permission.requested"
	NotifyNotice              = "notice"
	NotifyStatusUpdated       = "status.updated"
	NotifyWidgetUpdated       = "widget.updated"
	NotifyPluginState         = "plugin.state"
)

// Span is one run of text with a theme role, the only thing a plugin may put in a status
// item or a widget: no colors, no layout, no escape codes. Span, WidgetSlot, StatusItem and
// Widget live here rather than in internal/plugin because they are wire shapes carried by
// status.updated and widget.updated, and internal/plugin imports this package (a Host
// connects with a protocol.Client). internal/plugin aliases them, so a plugin author still
// writes plugin.Span.
type Span struct {
	Text string `json:"text"`
	Role string `json:"role"`
}

// WidgetSlot is where the client draws a widget. The set is closed.
type WidgetSlot string

const (
	SlotHeader      WidgetSlot = "header"
	SlotAboveEditor WidgetSlot = "above_editor"
	SlotBelowEditor WidgetSlot = "below_editor"
)

func (s WidgetSlot) Valid() bool {
	switch s {
	case SlotHeader, SlotAboveEditor, SlotBelowEditor:
		return true
	}
	return false
}

// StatusItem is one plugin's cell in the status line, keyed by owner and key: a plugin can
// only ever replace its own.
type StatusItem struct {
	Owner   string `json:"owner"`
	Key     string `json:"key"`
	Content []Span `json:"content"`
}

// Widget is one plugin's block in a slot, keyed by owner and key.
type Widget struct {
	Owner   string     `json:"owner"`
	Key     string     `json:"key"`
	Slot    WidgetSlot `json:"slot"`
	Content []Span     `json:"content"`
}

// StatusUpdated carries the whole status line, latest wins.
type StatusUpdated struct {
	Items []StatusItem `json:"items"`
}

// WidgetUpdated is one widget, latest wins per owner and key.
type WidgetUpdated = Widget

// PluginState is one plugin's load state. Origin is linked or spawned.
type PluginState struct {
	Name   string `json:"name"`
	Origin string `json:"origin"`
	State  string `json:"state"`
	Reason string `json:"reason"`
}

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
	// Parent names the session and tool_use a child session hangs off. Only a plugin may
	// send it; a client gets invalid_argument.
	Parent *ParentRef `json:"parent,omitempty"`
}

// ParentRef is the tool_use of a parent session that a child session answers.
type ParentRef struct {
	SessionID string `json:"session_id"`
	ToolUseID string `json:"tool_use_id"`
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

// SessionCompactParams asks for a compaction now. Instructions steer the model's summary and
// skip the before_compaction hook: a caller who said what the summary is for does not want a
// handler answering a different question.
type SessionCompactParams struct {
	SessionID    string `json:"session_id"`
	Instructions string `json:"instructions,omitempty"`
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
	// SessionID is set when the command opened another session, a fork.
	SessionID string `json:"session_id,omitempty"`
}

type PluginAppendNoteParams struct {
	SessionID string           `json:"session_id"`
	Text      string           `json:"text"`
	Role      session.NoteRole `json:"role"`
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
