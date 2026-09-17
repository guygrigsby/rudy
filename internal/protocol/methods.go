package protocol

import (
	"encoding/json"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// ProtocolVersion is the wire version the server and a spawned plugin agree on in
// plugin.init. A plugin answering anything else is refused and never loads.
const ProtocolVersion = 1

// Methods, client to server.
const (
	MethodClientHello        = "client.hello"
	MethodServerShutdown     = "server.shutdown"
	MethodSessionOpen        = "session.open"
	MethodSessionResume      = "session.resume"
	MethodSessionFork        = "session.fork"
	MethodSessionList        = "session.list"
	MethodSessionClose       = "session.close"
	MethodSessionSubmit      = "session.submit"
	MethodSessionShell       = "session.shell"
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
	MethodCommandList        = "command.list"
)

// Methods, plugin to server. A connection may send these only when its caller class is
// plugin; a client gets unauthorized.
const (
	MethodPluginAppendNote       = "plugin.append_note"
	MethodPluginRegisterTool     = "plugin.register_tool"
	MethodPluginRegisterCommand  = "plugin.register_command"
	MethodPluginRegisterHook     = "plugin.register_hook"
	MethodPluginRegisterWidget   = "plugin.register_widget"
	MethodPluginRegisterProvider = "plugin.register_provider"
	MethodPluginRegisterAgent    = "plugin.register_agent"
	MethodPluginSetStatus        = "plugin.set_status"
)

// Methods, server to plugin. Only a spawned plugin is ever called: a linked plugin is the
// same Go interface on the other side of a function call.
const (
	MethodPluginInit         = "plugin.init"
	MethodToolInvoke         = "tool.invoke"
	MethodToolCancel         = "tool.cancel"
	MethodHookFire           = "hook.fire"
	MethodCommandInvoke      = "command.invoke"
	MethodProviderComplete   = "provider.complete"
	MethodProviderListModels = "provider.list_models"
)

// Notifications, plugin to server.
const (
	NotifyToolProgress  = "tool.progress"
	NotifyProviderDelta = "provider.delta"
)

// Notifications, server to client.
const (
	NotifyEntryAppended       = "entry.appended"
	NotifyStreamDelta         = "stream.delta"
	NotifyTurnState           = "turn.state"
	NotifyToolState           = "tool.state"
	NotifyPermissionRequested = "permission.requested"
	NotifyNotice              = "notice"
	NotifyStatusUpdated       = "status.updated"
	NotifyWidgetUpdated       = "widget.updated"
	NotifyPluginState         = "plugin.state"
	NotifyServerStopped       = "server.stopped"
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
	// InstanceID identifies this process-lifetime server. It is never persisted.
	InstanceID string `json:"instance_id"`
	// Home is the server process's home directory. A client on another machine places the
	// workspace under it (<home>/<cwd relative to its own home>); a local client ignores it.
	Home string `json:"home"`
}

type ServerState string

const (
	ServerStateRunning      ServerState = "running"
	ServerStateShuttingDown ServerState = "shutting_down"
	ServerStateStopped      ServerState = "stopped"
)

type ServerShutdownResult struct {
	InstanceID string      `json:"instance_id"`
	State      ServerState `json:"state"`
}

type ServerStoppedParams struct {
	InstanceID string      `json:"instance_id"`
	State      ServerState `json:"state"`
}

type SessionOpenParams struct {
	Cwd      string `json:"cwd"`
	Model    string `json:"model,omitempty"`
	Mode     string `json:"mode,omitempty"`
	Thinking string `json:"thinking,omitempty"`
	Agent    string `json:"agent,omitempty"`
	// Tools narrows the session's tool set to these names. It only ever removes: a name the
	// agent definition or the parent did not hold is dropped rather than refused, since the
	// set is an intersection and asking for less than you are owed is not an error (ADR 0028).
	// No omitempty: nil (absent or explicit null) means no narrowing, an explicit empty list
	// means narrow to nothing, and omitempty collapses both to the same wire bytes, which lost
	// the caller's own "give this subagent nothing" the moment the value crossed a real
	// json.Marshal on its way to the wire (rudy-review round 1 on task 5).
	Tools []string `json:"tools"`
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
	SessionID string `json:"session_id"`
	// TurnID is the turn the question was asked in, as permission.requested carried it.
	// An answer names it so a decision cannot land on a later turn's question that
	// happens to reuse the tool_use id: consent is given for one call of one turn.
	TurnID    string           `json:"turn_id"`
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

// CommandInfo is one registered slash command as a client sees it: what to type and what
// it does. A client asks for the set once (see MethodCommandList) and completes from it.
// SessionShellParams is a shell command the operator ran from the composer with `!`.
type SessionShellParams struct {
	SessionID string `json:"session_id"`
	Command   string `json:"command"`
}

// SessionShellResult is where it landed and whether it failed. The command and its output
// reach every attached client as the entry.appended notification for the user_message,
// which is the same way a typed message arrives, so nothing is sent twice.
type SessionShellResult struct {
	EntryID string `json:"entry_id"`
	IsError bool   `json:"is_error"`
}

type CommandInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// CommandListResult is every registered command in registration order. The set is fixed
// once every plugin has answered plugin.init, so there is no notification that changes it.
// The client's own commands are not in it: /exit and /quit never reach a server.
type CommandListResult struct {
	Commands []CommandInfo `json:"commands"`
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

// ToolStateChanged reports where one tool call has got to. Tool calls of an assistant message
// run concurrently (ADR 0028), so turn.state cannot say which of several is waiting on the
// operator; this can. Not replayed: a finished call is its tool_result entry.
type ToolStateChanged struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	ToolUseID string `json:"tool_use_id"`
	Name      string `json:"name"`
	State     string `json:"state"`
}

// The states a tool call is reported in. turn.ToolState carries the same three values; the
// server converts by taking the string, so a drift test holds them together.
const (
	ToolStateRunning            = "running"
	ToolStateAwaitingPermission = "awaiting_permission"
	ToolStateDone               = "done"
)

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

// The plugin registration payloads. A spawned plugin asserts these under its own name; the
// connection's caller class is what the name comes from, never the params.
//
// Point and Slot travel as strings rather than the plugin package's own types: internal/plugin
// imports this package for its wire vocabulary (Span, WidgetSlot), so nothing here can import
// it back. The server converts at the edge, which is also where an unknown value is refused.
type PluginRegisterToolParams struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Safety      tool.Safety     `json:"safety"`
}

type PluginRegisterCommandParams struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type PluginRegisterHookParams struct {
	Point    string `json:"point"`
	Priority int    `json:"priority"`
}

type PluginRegisterWidgetParams struct {
	Key     string     `json:"key"`
	Slot    WidgetSlot `json:"slot"`
	Content []Span     `json:"content"`
}

type PluginSetStatusParams struct {
	Key     string `json:"key"`
	Content []Span `json:"content"`
}

// PluginRegisterProviderParams names the wire the plugin speaks: custom means the server
// calls provider.complete on the plugin itself.
type PluginRegisterProviderParams struct {
	Name string `json:"name"`
	Wire string `json:"wire"`
}

// PluginRegisterAgentParams carries the same fields agents/<name>.md does. Tools is a pointer
// so an absent key (every tool) stays distinguishable from an explicit empty list (no tool),
// exactly as the file's frontmatter does; a plain []string cannot make that distinction once
// decoded.
type PluginRegisterAgentParams struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Prompt      string    `json:"prompt"`
	Tools       *[]string `json:"tools,omitempty"`
	Model       string    `json:"model,omitempty"`
	Thinking    string    `json:"thinking,omitempty"`
	MaxTurns    int       `json:"max_turns,omitempty"`
}

// The provider wires a plugin may register.
const (
	WireCustom            = "custom"
	WireOpenAIChat        = "openai_chat"
	WireAnthropicMessages = "anthropic_messages"
)

// PluginInitParams is the first request the server sends a spawned plugin. Config is the
// plugin's [plugins.<name>] table verbatim.
type PluginInitParams struct {
	Name            string         `json:"name"`
	Version         string         `json:"version"`
	ProtocolVersion int            `json:"protocol_version"`
	Config          map[string]any `json:"config"`
	WorkspaceRoots  []string       `json:"workspace_roots"`
}

type PluginInitResult struct {
	Name            string `json:"name"`
	Version         string `json:"version"`
	ProtocolVersion int    `json:"protocol_version"`
}

type ToolInvokeParams struct {
	SessionID string            `json:"session_id"`
	ToolUseID string            `json:"tool_use_id"`
	Name      string            `json:"name"`
	Input     json.RawMessage   `json:"input"`
	Workspace session.Workspace `json:"workspace"`
	TimeoutMS int64             `json:"timeout_ms"`
}

type ToolInvokeResult struct {
	Content []session.Block `json:"content"`
	IsError bool            `json:"is_error"`
}

type ToolCancelParams struct {
	ToolUseID string `json:"tool_use_id"`
}

// ToolProgress is a running tool saying it is still working. Nothing in this plan renders
// it; it is delivered to the plugin adapter and dropped there rather than dropped here,
// where a later renderer would have to reopen the transport to find it.
type ToolProgress struct {
	ToolUseID string `json:"tool_use_id"`
	Text      string `json:"text"`
}

type HookFireParams struct {
	Point     string          `json:"point"`
	SessionID string          `json:"session_id"`
	TurnID    string          `json:"turn_id"`
	Payload   json.RawMessage `json:"payload"`
}

// HookFireResult carries the point's own result shape, or nothing for a point that returns
// nothing and for a handler that means pass.
type HookFireResult struct {
	Result json.RawMessage `json:"result"`
}

type CommandInvokeParams struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	Args      string `json:"args"`
	// The session's own facts, so a spawned command can report what it is about to change
	// rather than asking for them back over the wire.
	Mode     session.Mode          `json:"mode"`
	Model    string                `json:"model"`
	Thinking session.ThinkingLevel `json:"thinking"`
}

// CommandInvokeResult is what the command asks the server to do. A non-empty Prompt is
// submitted as a user message; a non-empty Notice is shown to the client.
type CommandInvokeResult struct {
	Prompt string `json:"prompt,omitempty"`
	Notice string `json:"notice,omitempty"`
}

type ProviderCompleteParams struct {
	RequestID string                `json:"request_id"`
	Model     session.ModelRef      `json:"model"`
	System    string                `json:"system"`
	Messages  []provider.Message    `json:"messages"`
	Tools     []provider.ToolDef    `json:"tools"`
	Thinking  session.ThinkingLevel `json:"thinking"`
	MaxTokens int                   `json:"max_tokens"`
}

type ProviderCompleteResult struct {
	StopReason    session.StopReason `json:"stop_reason"`
	StopReasonRaw string             `json:"stop_reason_raw"`
	Usage         session.Usage      `json:"usage"`
}

// ProviderDelta is one streamed part of a completion in flight, keyed by the request it
// belongs to.
type ProviderDelta struct {
	RequestID string        `json:"request_id"`
	Part      provider.Part `json:"part"`
}

type ProviderListModelsResult struct {
	Models []provider.Model `json:"models"`
}
