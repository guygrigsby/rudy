// Package app is rudy's Bubble Tea client: one model over one protocol connection,
// drawing the slots ui.layout.slots names and nothing else.
//
// The client speaks the protocol and only the protocol. Nothing here appends an entry,
// reaches into the session store or links the server package: what is on screen is what
// the server said, folded into a transcript, a status line, widgets and notices. Every
// render choice is a config field, every plugin's status item and widget is keyed by its
// owner so no plugin can clear another's, and nothing draws that ui.layout.slots and
// ui.status.items did not place.
//
// The model is owned by the update loop. The one goroutine it keeps is the notification
// pump, which reads a single notification and returns it as a message; Update re-arms it.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tui/input"
	"github.com/guygrigsby/rudy/internal/tui/keys"
	"github.com/guygrigsby/rudy/internal/tui/theme"
	"github.com/guygrigsby/rudy/internal/tui/transcript"
)

// The config values this file reads by name. Each is a value config already validated.
const (
	renderAltscreen = "altscreen"
	thinkingShown   = "shown"
	thinkingHidden  = "hidden"
	diffBackground  = "background"

	slotHeader     = "header"
	slotTranscript = "transcript"
	slotInput      = "input"
	slotStatus     = "status"
)

// The notice levels a server sends (docs/specs/rudy-contracts.md, notice).
const (
	levelInfo  = "info"
	levelWarn  = "warn"
	levelError = "error"
)

// pluginFailed is the plugin.state a client turns into a notice; loading and ready pass
// without a word.
const pluginFailed = "failed"

// The size the model draws at until the first tea.WindowSizeMsg. A terminal always sends
// one, so this only has to be sane rather than right.
const (
	defaultWidth  = 80
	defaultHeight = 24
)

// maxNotices is how many notices are kept. A notice is chrome, not history: it never
// reaches the log, so the oldest fall off rather than pushing the transcript off screen.
const maxNotices = 20

// Options are what a caller hands the model: the config and its resolved theme and keys,
// a client already greeted, and the session it already opened or resumed.
type Options struct {
	Config  *config.Config
	Theme   theme.Theme
	Keys    *keys.Table
	Client  *protocol.Client
	Session protocol.SessionInfo // already opened or resumed by the caller
	Models  []provider.Model     // for the picker and the context percent
	Version string
	Cwd     string
	// Prompt seeds the editor with a draft rather than sending it. The positional words
	// of `rudy <prompt>` land here: the design opens ready to type, and a prompt written
	// on the command line is a draft the user has already typed, not a turn they have
	// already sent, so Enter is still what sends it.
	Prompt string
	// Changelog is CHANGELOG.md as the binary carries it, which the startup header reads
	// the newest release out of. Empty draws no news (ADR 0016).
	Changelog string
	// Now fixes the clock the header's greeting and tips are resolved against. Zero means
	// time.Now, which is what a run does; a test sets it so a golden is not a clock.
	Now time.Time
	// Workspace overrides the workspace status item. Empty means read it from git in
	// Cwd, which is what a run does; a test sets it so drawing a status line never
	// depends on the directory the test happens to run in.
	Workspace string
}

// notice is one thing the client has to say that the log never will: a server notice, a
// plugin that failed, a connection that ended.
type notice struct {
	level string
	text  string
}

// Model is the client. Zero value is not usable; call New.
type Model struct {
	cfg  *config.Config
	th   theme.Theme
	keys *keys.Table
	cl   *protocol.Client
	// cwd is the caller's directory, kept for the calls that need it: a session this
	// client opens later is opened on the same one.
	cwd string

	// session mirrors what the server says about the session: the model, mode, thinking
	// and title the status line reads, moved by the change entries the log appends.
	session protocol.SessionInfo

	tr *transcript.Transcript
	ed *input.Editor
	// vp scrolls the transcript in altscreen. Inline rendering has no viewport: the
	// terminal's own scrollback is the scroll.
	vp viewport.Model
	// spin is the glyph the turn status item draws while a turn runs, and spinning is
	// whether its tick loop is armed: a client at rest schedules nothing (status.go).
	spin     spinner.Model
	spinning bool

	// status holds the plugin half of the status line, keyed "<owner>:<key>", which is
	// also how ui.status.items names one.
	status map[string]protocol.StatusItem
	// widgets are every plugin's widget in arrival order, one entry per owner and key.
	widgets []protocol.Widget
	notices []notice

	// turn is what the client knows about the turn in flight: the server's own state
	// mirrored, the question standing on screen and what Esc does next (turn.go).
	turn turnControl

	// header is the startup header: where it is drawn, how far its reveal has got and the
	// greeting it was opened with (header.go).
	header headerState
	// version and changelog are what the header's title and its news are drawn from.
	version   string
	changelog string
	// pick is the picker standing in the editor's place, nil when none is up. While one
	// stands it owns the keyboard (picker.go).
	pick *picker
	// commands are what the slash menu lists: command.list's answer in registration
	// order, then the two the client answers itself (slash.go). Asked once on connect,
	// since nothing registers a command after its plugin has answered plugin.init.
	commands []protocol.CommandInfo
	// menu is the slash menu's own state, which is only ever a selection and a dismissal:
	// what it lists comes from the draft.
	menu menuState
	// switching is the session switch waiting for its answer, nil when none is. It holds
	// the session being left and the notifications that arrived while it was in flight,
	// which for a resume, an open and a fork are the new session's replay: the server
	// sends every entry before the answer that names the session they belong to.
	switching *switchPending
	// replaying is set while a switch folds the log it held, so the per-turn commit
	// inline rendering does is left to the one ordered print at the end of it.
	replaying bool
	// seen are the entries already folded into this session, so a log the server replays
	// twice leaves one row and one usage total. Emptied by a switch, with the transcript.
	seen map[string]bool
	// awaitLog is set from a switch until the head of the new session's log arrives, so
	// the transcript is never built from the middle of a replay (see entry).
	awaitLog bool
	// showThinking is ui.transcript.thinking, overridable in memory by
	// app.thinking.toggle. It lives here rather than in cfg because the config is shared
	// with the server and is read, never written.
	showThinking bool

	// models is the registry as this client last saw it, and model is the session's own
	// entry in it, for the context percent and the cost. A model the registry does not
	// know leaves model zero, and both items go quiet rather than guessing.
	models []provider.Model
	model  provider.Model
	// usage is every assistant message's usage so far, which is what the cost prices.
	usage session.Usage
	// lastPrompt is the last assistant message's prompt tokens, which is what fills the
	// context window; the sum above would count every turn's prompt again.
	lastPrompt int64

	workspace string
	// wsFixed is Options.Workspace having been set: that caller owns the workspace item
	// for the life of the client, a session switch included. Without it the item is read
	// from git in the new session's own root.
	wsFixed bool
	width   int
	height  int

	// disconnected is set once the server is gone. Editing still works, so a draft can be
	// read back and copied out; what stops is anything that needs the server, which is
	// submitting and the calls a picker makes (Task 8).
	disconnected bool
}

// New builds the client at the default size, focused and empty. The caller has already
// greeted the server and opened or resumed o.Session.
func New(o Options) *Model {
	table := o.Keys
	if table == nil {
		table = keys.Default()
	}
	cfg := o.Config
	m := &Model{
		cfg:          cfg,
		th:           o.Theme,
		keys:         table,
		cl:           o.Client,
		cwd:          o.Cwd,
		session:      o.Session,
		models:       o.Models,
		status:       make(map[string]protocol.StatusItem),
		seen:         make(map[string]bool),
		showThinking: cfg.UI.Transcript.Thinking == thinkingShown,
		// The client's own are there from the first keystroke; command.list's answer is
		// prepended to them when it lands.
		commands:  clientCommands,
		workspace: o.Workspace,
		wsFixed:   o.Workspace != "",
		width:     defaultWidth,
		height:    defaultHeight,
	}
	m.version, m.changelog = o.Version, o.Changelog
	m.header = m.newHeader(o)
	m.tr = m.newTranscript()
	m.ed = input.New(cfg.UI.Vim, o.Theme, defaultWidth, table)
	if o.Prompt != "" {
		m.ed.SetText(o.Prompt)
	}
	m.vp = viewport.New(viewport.WithWidth(defaultWidth), viewport.WithHeight(defaultHeight))
	m.spin = newSpinner()
	m.model = pickModel(m.models, m.session.Model)
	if m.workspace == "" {
		m.workspace = detectWorkspace(m.session.Workspace.GitRoot, m.cwd)
	}
	return m
}

// newTranscript is an empty transcript at the current width and render config. It is one
// function because a session switch builds a second one and the two must not drift: what
// the client draws must not depend on which session it happens to be on. ShowThinking
// comes from the model rather than from config, so a switch keeps whatever
// app.thinking.toggle last said.
func (m *Model) newTranscript() *transcript.Transcript {
	return transcript.New(transcript.Options{
		Width:            m.width,
		ToolCollapsed:    m.cfg.UI.Transcript.ToolCollapsed,
		ToolPreviewLines: m.cfg.UI.Transcript.ToolPreviewLines,
		ShowThinking:     m.showThinking,
		UserPrefix:       m.cfg.UI.Transcript.UserPrefix,
		BlockGap:         m.cfg.UI.Transcript.BlockGap,
		DiffBackground:   m.cfg.UI.Diff.Style == diffBackground,
	}, m.th)
}

// pickModel is the registry's entry for ref, or the zero model when the registry has no
// such id.
func pickModel(models []provider.Model, ref session.ModelRef) provider.Model {
	for _, mo := range models {
		if mo.Ref == ref {
			return mo
		}
	}
	return provider.Model{}
}

// Init arms the notification pump and focuses the editor. A session whose model the
// caller's snapshot does not list also asks for the registry, through the same setModel
// every later change goes through.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(pump(m.cl), m.ed.Focus(), m.setModel(m.session.Model), m.call(protocol.MethodCommandList, nil))
}

// setModel takes ref as the session's model and finds it in the registry snapshot. One
// the snapshot does not carry asks the server for a fresh registry rather than leaving
// the context percent and the cost blank for the rest of the session: the design
// refreshes on session open, picker open and model-not-found, never on a timer.
func (m *Model) setModel(ref session.ModelRef) tea.Cmd {
	m.session.Model = ref
	m.model = pickModel(m.models, ref)
	if m.model.Ref == ref {
		return nil
	}
	return m.call(protocol.MethodRegistryList, nil)
}

// Update folds one message in. A notification re-arms the pump, so exactly one is in
// flight at a time and they land in the order the server sent them.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case NotificationMsg:
		return m, tea.Batch(m.notification(protocol.Notification(msg)), pump(m.cl))
	case DisconnectedMsg:
		m.disconnected = true
		// No turn.state will arrive for a turn in flight now, so the mirror rests and the
		// spinner stops here rather than on an edge that never comes.
		m.turn = turnControl{}
		m.spinning = false
		text := "disconnected from the server"
		if msg.Err != nil {
			text += ": " + msg.Err.Error()
		}
		m.note(levelError, text)
		return m, nil
	case CallResultMsg:
		return m, m.callResult(msg)
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		// The header needs a width before it can be drawn at all, so the first size is
		// what starts it: the inline print, or the first step of the reveal.
		return m, m.headerStart()
	case headerTickMsg:
		return m, m.headerTicked()
	case tea.KeyPressMsg:
		return m, m.key(msg)
	case tea.MouseClickMsg:
		m.click(msg.Y)
		return m, nil
	case tea.MouseWheelMsg:
		m.wheel(msg.Button)
		return m, nil
	case spinner.TickMsg:
		return m, m.spinTicked(msg)
	}
	// Everything else, a cursor blink among it, belongs to the editor.
	return m, m.ed.Update(msg)
}

// notification dispatches one server notification by method. It returns a command for the
// methods that need one (the inline commit hangs off turn.state); the folding itself
// happens here, on the update loop.
func (m *Model) notification(n protocol.Notification) tea.Cmd {
	if sessionScoped(n.Method) {
		switch sid := sessionOf(n); {
		case m.switching != nil:
			// A switch in flight: which session these belong to is not settled until the
			// answer names it. The new session's whole log arrives this way, ahead of the
			// answer (docs/specs/rudy-contracts.md: session.open, session.resume and
			// session.fork replay their entries first and return once the replay
			// finishes), so folding them now would put them in the transcript the switch
			// is about to throw away.
			m.switching.buffer = append(m.switching.buffer, n)
			return nil
		case sid != "" && sid != m.session.SessionID:
			// Another session's, which is not the one on screen: a client that has just
			// switched stays attached to the session it left until its close lands, and
			// the answer that switched it overtakes what was sent before it (a
			// notification arrives one per update, an answer in a single message), so
			// the two cross. One that names no session is nobody else's and is folded.
			return nil
		}
	}
	switch n.Method {
	case protocol.NotifyEntryAppended:
		var p protocol.EntryAppended
		if m.decode(n, &p) {
			return m.entry(p.Entry)
		}
	case protocol.NotifyStreamDelta:
		var p protocol.StreamDelta
		if m.decode(n, &p) {
			// What separates the turn item's "thinking" from its "streaming" is whether
			// an answer has begun; the server reports both as the streaming state.
			if p.Part.Type == provider.PartTextDelta {
				m.turn.streamed = true
			}
			m.tr.Delta(p.TurnID, p.Part)
		}
	case protocol.NotifyTurnState:
		var p protocol.TurnStateChanged
		if m.decode(n, &p) {
			return m.turnChanged(p)
		}
	case protocol.NotifyPermissionRequested:
		var p protocol.PermissionRequested
		if m.decode(n, &p) {
			m.requested(p)
		}
	case protocol.NotifyNotice:
		var p protocol.NoticeParams
		if m.decode(n, &p) {
			m.note(p.Level, p.Text)
		}
	case protocol.NotifyStatusUpdated:
		var p protocol.StatusUpdated
		if m.decode(n, &p) {
			m.setStatus(p.Items)
		}
	case protocol.NotifyWidgetUpdated:
		var w protocol.Widget
		if m.decode(n, &w) {
			m.setWidget(w)
		}
	case protocol.NotifyPluginState:
		var p protocol.PluginState
		if m.decode(n, &p) && p.State == pluginFailed {
			// A plugin that fails degrades with a notice and never refuses boot
			// (docs/specs/2026-09-07-rudy-design.md); loading and ready say nothing.
			m.note(levelError, "plugin "+p.Name+" failed: "+p.Reason)
		}
	}
	return nil
}

// decode reads a notification's params, turning a malformed one into a notice rather than
// dropping it: a server and a client that disagree about a shape is worth seeing.
func (m *Model) decode(n protocol.Notification, v any) bool {
	if err := json.Unmarshal(n.Params, v); err != nil {
		m.note(levelError, n.Method+": "+err.Error())
		return false
	}
	return true
}

// entry folds one appended entry in: the transcript takes what is on screen, and the rest
// of the model takes what only it reads, the usage totals and the session's own changes.
// A model change the registry snapshot cannot explain returns the command that refreshes
// it.
func (m *Model) entry(e session.Entry) tea.Cmd {
	// An entry is folded once per session however many times the server sends it. The
	// transcript dedupes its rows by key, but the usage totals, the commit and the
	// status line have no key to dedupe by, and a log does arrive twice: a fork the
	// server attached to this connection and this client then re-attached to replays it
	// once for each attachment.
	switch e.Payload.(type) {
	case session.SessionOpened, session.ForkPoint:
		// A log starts here. The server sends one of these only at the head of a replay
		// (they are the only entries a log's first line may be), so this is where a
		// switched-to session's transcript begins.
		m.awaitLog = false
	}
	if m.awaitLog {
		// Waiting for that head. A fork a command opened is attached and replayed by the
		// server and then re-attached and replayed again by this client, and the first of
		// those two copies may already be part way past when the switch takes effect.
		// Folding from the middle of it would put an answer above its own question, so
		// the middle is dropped and the next copy taken from its head. Notifications
		// arrive in the order the server sent them, so a head is always followed by the
		// whole of its own copy.
		return nil
	}
	id := e.ID.String()
	if m.seen[id] {
		return nil
	}
	m.seen[id] = true
	added := m.tr.Apply(e)
	var cmd tea.Cmd
	switch p := e.Payload.(type) {
	case session.UserMessage:
		// A user message opens a turn, and inline rendering keeps only the newest one in
		// the live region: what came before it has rested (a turn that had not would
		// still be the one this message is being added to) and belongs in scrollback.
		// This is what commits a replayed log turn by turn as it folds.
		cmd = m.commitTurns(m.tr.Turn())
	case session.AssistantMessage:
		m.usage = m.usage.Add(p.Usage)
		m.lastPrompt = p.Usage.Input + p.Usage.CacheRead + p.Usage.CacheWrite
	case session.PermissionDecision:
		// The question has been answered, by this client or by a hook or the gate, so
		// the row standing in its place goes.
		m.answered(p.ToolUseID)
	case session.ModelChange:
		cmd = m.setModel(p.Model)
	case session.ModeChange:
		m.session.Mode = p.Mode
	case session.ThinkingChange:
		m.session.Thinking = p.Thinking
	case session.TitleChange:
		m.session.Title = p.Title
	}
	// An entry can land after its turn has already been committed to scrollback, and the
	// row it made would stand in the live region for the rest of the session.
	return tea.Batch(cmd, m.lateRows(added))
}

// callResult folds one server answer in. A call whose answer says nothing this model
// reads (session.interrupt, session.answer, session.close) has no case: a failure is
// already a notice above, and the turn's own notifications carry the rest.
func (m *Model) callResult(r CallResultMsg) tea.Cmd {
	if r.Err != nil {
		return m.callFailed(r)
	}
	switch r.Method {
	case protocol.MethodSessionAnswer:
		// The server has the decision; nothing is left to put back.
		m.turn.asked = nil
	case protocol.MethodRegistryList, protocol.MethodRegistryRefresh:
		var res protocol.RegistryListResult
		if !m.result(r, &res) {
			return nil
		}
		m.models = res.Models
		// Not setModel: the refresh is the answer to a model-not-found, and a model the
		// fresh registry still lacks must not ask for it again.
		m.model = pickModel(res.Models, m.session.Model)
		if m.pick != nil && m.pick.kind == pickerModel {
			m.pick.setRows(modelRows(m.models))
		}
	case protocol.MethodSessionSetThinking:
		// Nothing on screen carries the thinking level, so the change says so itself.
		// From the answer rather than from the thinking_change entry: a level this
		// client did not set, and a log replayed on a resume, must not post a notice
		// about a change that is not news.
		m.note(levelInfo, "thinking: "+r.Name)
	case protocol.MethodSessionList:
		var res protocol.SessionListResult
		if !m.result(r, &res) {
			return nil
		}
		m.openSessionPicker(res.Sessions)
	case protocol.MethodSessionOpen, protocol.MethodSessionResume, protocol.MethodSessionFork:
		var info protocol.SessionInfo
		if !m.result(r, &info) {
			return nil
		}
		return m.switched(info)
	case protocol.MethodSessionSubmit:
		var res protocol.SessionSubmitResult
		if !m.result(r, &res) {
			return nil
		}
		return m.started(res.TurnID)
	case protocol.MethodCommandList:
		var res protocol.CommandListResult
		if !m.result(r, &res) {
			return nil
		}
		// Registered first, the client's own under them: what a menu lists in the order it
		// lists them, and a plugin that registered exit or quit is shadowed by the client's
		// (ADR 0015 decision 3), so its row is the one nobody can reach.
		m.commands = append(slices.Clone(res.Commands), clientCommands...)
	case protocol.MethodCommandRun:
		var res protocol.CommandRunResult
		if !m.result(r, &res) {
			return nil
		}
		// The notice the answer carries is read, not drawn: the server sends the same text
		// to every attached client as a notice notification, which is what put it on
		// screen, and a second copy from the answer would draw every command's notice
		// twice. The field is for a caller that does not subscribe, which is what the
		// headless printer is.
		if res.SessionID != "" {
			// The command opened another session, which is what a fork is. The server
			// has already attached this connection to it, so the switch drops that
			// attachment and takes it again through a resume (see resumeSession).
			if cmd := m.resumeSession(res.SessionID); cmd != nil {
				return cmd
			}
			if res.SessionID != m.session.SessionID {
				// The switch was refused (another one is in flight, or the connection
				// is gone). The fork the server opened and attached is nobody's now, so
				// it is let go rather than left held for the life of the connection.
				return m.call(protocol.MethodSessionClose, protocol.SessionCloseParams{SessionID: res.SessionID})
			}
		}
		return m.started(res.TurnID)
	}
	return nil
}

// callFailed is one call that came back an error. Every failure is a notice, and the two
// that leave the client holding something have to put it back: an answer that never
// reached the server, and a session switch whose new session never opened.
func (m *Model) callFailed(r CallResultMsg) tea.Cmd {
	var cmd tea.Cmd
	switch {
	case r.Method == protocol.MethodSessionAnswer:
		// The question came down when the answer went out and the server never got it:
		// put it back rather than leave the turn waiting on a decision with nothing on
		// screen to make it with.
		m.reask()
	case switchMethod(r.Method):
		// The session that was being left is still here and still attached: nothing was
		// closed, and what arrived while the switch was in flight was its own.
		cmd = m.abortSwitch()
	}
	if r.Method == protocol.MethodCommandRun {
		// A command's failure is the server's own words, which is what the user typed
		// come back at them ("unknown command /x"). The method and the JSON-RPC code in
		// front of it would say nothing they can act on.
		m.note(levelError, serverMessage(r.Err))
		return cmd
	}
	m.note(levelError, r.Method+": "+r.Err.Error())
	return cmd
}

// serverMessage is the message a server error carries, or the whole error for one that
// never reached the server.
func serverMessage(err error) string {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return pe.Message
	}
	return err.Error()
}

// switchPending is a session switch waiting for its answer: the session being left, which
// is closed only once the new one is in hand, and the session-scoped notifications that
// arrived in the meantime.
type switchPending struct {
	from   string
	buffer []protocol.Notification
}

// switchMethod reports whether a method's answer is a session to switch to.
func switchMethod(method string) bool {
	switch method {
	case protocol.MethodSessionOpen, protocol.MethodSessionResume, protocol.MethodSessionFork:
		return true
	}
	return false
}

// sessionScoped reports whether a notification belongs to one session rather than to the
// connection. These are the ones a switch has to hold: the rest (a notice, a status line,
// a widget, a plugin's state) are the process's and are folded whatever session is on.
func sessionScoped(method string) bool {
	switch method {
	case protocol.NotifyEntryAppended, protocol.NotifyStreamDelta,
		protocol.NotifyTurnState, protocol.NotifyPermissionRequested:
		return true
	}
	return false
}

// sessionOf is the session a notification names, or "" for one that names none.
func sessionOf(n protocol.Notification) string {
	var p struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(n.Params, &p); err != nil {
		return ""
	}
	return p.SessionID
}

// newSession opens a session on the same directory this client was started in.
func (m *Model) newSession() tea.Cmd {
	return m.startSwitch(m.call(protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: m.cwd}))
}

// forkSession forks the current session at its newest entry and switches to the fork,
// which is what an empty at_entry_id asks for.
func (m *Model) forkSession() tea.Cmd {
	return m.startSwitch(m.call(protocol.MethodSessionFork, protocol.SessionForkParams{SessionID: m.session.SessionID}))
}

// listSessions asks for the sessions the resume picker is built from.
func (m *Model) listSessions() tea.Cmd {
	if m.offline() {
		return nil
	}
	return m.call(protocol.MethodSessionList, nil)
}

// resumeSession switches to id, dropping first whatever attachment this client already
// holds to it. The server attaches the fork a /fork command opened to the connection that
// ran the command and replays it there before answering, so resuming that fork without
// the close would subscribe the same connection to it twice and every later entry would
// arrive, and be counted, twice. The two calls travel on one command, which is what puts
// the close on the wire ahead of the resume; a close of a session this client does not
// hold is answered not_found and ignored.
func (m *Model) resumeSession(id string) tea.Cmd {
	if id == m.session.SessionID {
		m.note(levelInfo, "already in this session")
		return nil
	}
	c := m.cl
	return m.startSwitch(func() tea.Msg {
		ctx := context.Background()
		_ = c.Call(ctx, protocol.MethodSessionClose, protocol.SessionCloseParams{SessionID: id}, nil)
		var raw json.RawMessage
		err := c.Call(ctx, protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: id}, &raw)
		return CallResultMsg{Method: protocol.MethodSessionResume, Result: raw, Err: err}
	})
}

// startSwitch runs the call that opens the session to switch to and starts holding the
// notifications that arrive until it answers. The session being left is not closed here:
// a switch that fails leaves the client exactly where it was, which it could not do if
// the way out had already been given up.
func (m *Model) startSwitch(cmd tea.Cmd) tea.Cmd {
	if m.offline() {
		return nil
	}
	if m.switching != nil {
		// One at a time: two in flight and the buffered replays would interleave with no
		// way to tell whose was whose until both had answered.
		m.note(levelWarn, "a session switch is already in flight")
		return nil
	}
	m.switching = &switchPending{from: m.session.SessionID}
	return cmd
}

// switched takes the session a switch answered with. What the old session put on screen
// goes, what belongs to the connection rather than to the session stays: a fresh
// transcript, no turn and no usage, the registry, the plugins' status items and widgets
// kept (nothing re-sends those but a hello), the model and the workspace recomputed. The
// queue goes back into the draft: messages queued behind the old session's turn were
// meant for that session, and nothing may send them to this one.
//
// The new session's replay was held while the call was in flight and is folded in here,
// where there is a transcript for it to land in; inline rendering then commits every
// replayed turn to scrollback in one ordered print, leaving the live region to what
// happens next. The session that was left is closed last, once there is somewhere else
// to be.
func (m *Model) switched(info protocol.SessionInfo) tea.Cmd {
	sw := m.switching
	m.switching = nil
	m.session = info
	m.tr = m.newTranscript()
	m.turn = turnControl{}
	m.spinning = false
	m.usage, m.lastPrompt = session.Usage{}, 0
	m.seen = make(map[string]bool)
	m.awaitLog = true
	m.ed.Restore()
	m.model = pickModel(m.models, info.Model)
	if !m.wsFixed {
		m.workspace = detectWorkspace(info.Workspace.GitRoot, m.cwd)
	}
	m.note(levelInfo, "switched to session "+info.SessionID)
	if sw == nil {
		return nil
	}
	// The fold's own commits are suppressed and done once here instead: two prints from
	// one update are two commands, and a tea.Batch does not order them. What is left
	// live is the newest replayed turn, the same bound the launch-time replay of a
	// resumed session keeps; the next user message commits it like any other.
	m.replaying = true
	cmds := []tea.Cmd{m.replayBuffered(sw.buffer, info.SessionID)}
	m.replaying = false
	cmds = append(cmds, m.commitTurns(m.tr.Turn()))
	if sw.from != "" && sw.from != info.SessionID {
		cmds = append(cmds, m.call(protocol.MethodSessionClose, protocol.SessionCloseParams{SessionID: sw.from}))
	}
	return tea.Batch(cmds...)
}

// commitTurns prints every turn on screen but keep to the terminal's own scrollback,
// oldest first, and takes those rows out of the live region. It is inline rendering's
// only shape: altscreen keeps every row where it is and scrolls them.
//
// One print for all of them rather than one per turn, because two commands from one
// update are not ordered against each other. keep is the turn the live region is left
// with, or "" for none, which is never a turn id.
func (m *Model) commitTurns(keep string) tea.Cmd {
	if !m.inline() || m.replaying {
		return nil
	}
	var lines []string
	for _, id := range m.tr.Turns() {
		if id == keep {
			continue
		}
		lines = append(lines, m.tr.Commit(id)...)
	}
	if len(lines) == 0 {
		return nil
	}
	return tea.Println(strings.Join(lines, "\n"))
}

// abortSwitch is a switch that failed: what was held belongs to the session the client
// never left, so it is folded in now rather than dropped.
func (m *Model) abortSwitch() tea.Cmd {
	sw := m.switching
	m.switching = nil
	if sw == nil {
		return nil
	}
	return m.replayBuffered(sw.buffer, m.session.SessionID)
}

// replayBuffered folds the held notifications that belong to session sid, in the order
// they arrived. The rest named the session being left and are moot: it is closed, and its
// rows are not the ones on screen.
func (m *Model) replayBuffered(ns []protocol.Notification, sid string) tea.Cmd {
	var cmds []tea.Cmd
	for _, n := range ns {
		if sessionOf(n) != sid {
			continue
		}
		cmds = append(cmds, m.notification(n))
	}
	return tea.Batch(cmds...)
}

// setSessionModel asks the server to change the session's model. The status line does not
// move here: the model_change entry the server appends is what moves it, the same entry
// another client's change would arrive as.
func (m *Model) setSessionModel(ref session.ModelRef) tea.Cmd {
	if m.offline() {
		return nil
	}
	return m.call(protocol.MethodSessionSetModel, protocol.SessionSetModelParams{
		SessionID: m.session.SessionID,
		Model:     ref.String(),
	})
}

// cycleModel steps d models through the registry from the session's own, wrapping at both
// ends. A model the registry does not carry starts the walk at the first entry rather
// than nowhere.
func (m *Model) cycleModel(d int) tea.Cmd {
	n := len(m.models)
	if n == 0 {
		m.note(levelWarn, "the registry has no models")
		return nil
	}
	next := 0
	if i := slices.IndexFunc(m.models, func(mo provider.Model) bool { return mo.Ref == m.session.Model }); i >= 0 {
		next = ((i+d)%n + n) % n
	}
	return m.setSessionModel(m.models[next].Ref)
}

// thinkingLevels is the cycle app.thinking.cycle steps through, in the order the domain
// model lists them.
var thinkingLevels = []session.ThinkingLevel{
	session.ThinkingOff, session.ThinkingLow, session.ThinkingMedium, session.ThinkingHigh,
}

// cycleThinking asks for the next thinking level. Nothing on screen is named for the
// level, so the answer says what it set (see the session.set_thinking arm of callResult);
// the entry is what moves the session's own value.
func (m *Model) cycleThinking() tea.Cmd {
	if m.offline() {
		return nil
	}
	i := slices.Index(thinkingLevels, m.session.Thinking)
	next := thinkingLevels[(i+1)%len(thinkingLevels)]
	return m.callNamed(protocol.MethodSessionSetThinking, string(next), protocol.SessionSetThinkingParams{
		SessionID: m.session.SessionID,
		Thinking:  next,
	})
}

// toggleThinking flips whether thinking text is drawn, for this run only: it overrides
// ui.transcript.thinking in this model, never in the config, which is shared with the
// server and is read and not written. It is the client's own view of the log, not the
// session's level, so it asks the server nothing.
func (m *Model) toggleThinking() {
	m.showThinking = !m.showThinking
	m.tr.SetShowThinking(m.showThinking)
}

// offline reports that nothing can be asked of the server, and says so. Every action that
// needs a call goes through it: a picker, a cycle and a switch all do nothing while the
// connection is gone, and say why rather than looking stuck.
func (m *Model) offline() bool {
	if !m.disconnected {
		return false
	}
	m.note(levelError, "not connected")
	return true
}

// result decodes one call's answer, turning a shape this client cannot read into a notice
// the way a malformed notification becomes one.
func (m *Model) result(r CallResultMsg, v any) bool {
	if err := json.Unmarshal(r.Result, v); err != nil {
		m.note(levelError, r.Method+": "+err.Error())
		return false
	}
	return true
}

// note appends a notice, oldest falling off past maxNotices.
func (m *Model) note(level, text string) {
	m.notices = append(m.notices, notice{level: level, text: text})
	if len(m.notices) > maxNotices {
		m.notices = slices.Delete(m.notices, 0, len(m.notices)-maxNotices)
	}
}

// setStatus replaces the plugin half of the status line. status.updated carries the whole
// line, every plugin's items at once, so the map is rebuilt rather than merged: that is
// what makes a withdrawn item disappear without letting one plugin clear another's.
func (m *Model) setStatus(items []protocol.StatusItem) {
	m.status = make(map[string]protocol.StatusItem, len(items))
	for _, it := range items {
		m.status[it.Owner+":"+it.Key] = it
	}
}

// setWidget replaces one plugin's widget, keyed by owner and key: latest wins, a widget
// that changed slots moves rather than doubling, and no owner's key touches another's.
func (m *Model) setWidget(w protocol.Widget) {
	for i, old := range m.widgets {
		if old.Owner == w.Owner && old.Key == w.Key {
			m.widgets[i] = w
			return
		}
	}
	m.widgets = append(m.widgets, w)
}

// inline reports whether the client draws inline, in the terminal's own scrollback, which
// is what makes a rested turn's rows commit and leave the live region. The other mode is
// altscreen, which owns the screen and keeps every row.
func (m *Model) inline() bool { return m.cfg.UI.Render != renderAltscreen }

// resize takes the terminal's size. The transcript and the editor rewrap to it; the
// viewport's height is settled while composing, once the other slots have taken theirs.
func (m *Model) resize(w, h int) {
	m.width, m.height = w, h
	if w > 0 {
		m.tr.SetWidth(w)
		m.ed.SetWidth(w)
		m.vp.SetWidth(w)
	}
}

// key routes one key press in the order the design gives it: a picker while one is up,
// then a standing permission question, then the key table's actions, then the editor. An
// action that does not apply falls through to the next one the key is bound to, which is
// what lets ctrl+d exit on an empty editor and delete forward on a full one.
//
// A picker takes every key, a question's y, a and n included: it is what the keyboard is
// pointed at while it stands, and a question it hid is still standing when it closes.
func (m *Model) key(k tea.KeyPressMsg) tea.Cmd {
	// Somebody typing has stopped watching the wordmark arrive.
	m.settleHeader()
	if m.pick != nil {
		return m.pickerKey(k)
	}
	if cmd, answered := m.answer(k); answered {
		return cmd
	}
	// The slash menu takes the keys a list reads and leaves the rest to the editor, so the
	// draft it completes goes on being typed while it stands (ADR 0015 decision 4).
	if m.menuKey(k) {
		return nil
	}
	actions := m.keys.Match(tea.Key(k))
	if !slices.Contains(actions, keys.AppInterrupt) {
		// The double press that cancels is two Esc presses in a row: anything else in
		// between ends the pair, so a steer message typed and sent between them leaves
		// the next Esc meaning steer again rather than cancel.
		m.turn.lastEsc = time.Time{}
	}
	for _, a := range actions {
		switch a {
		case keys.AppInterrupt:
			if cmd, handled := m.interrupt(); handled {
				return cmd
			}
		case keys.TUIInputSubmit:
			return m.submit()
		case keys.AppMessageFollowUp:
			// A follow-up with nothing to follow is just a message: at rest there is no
			// turn to queue behind, and a queue nothing draws would swallow the draft.
			if m.turn.resting() {
				return m.submit()
			}
			m.ed.Enqueue()
			return nil
		case keys.AppMessageDequeue:
			m.ed.Restore()
			return nil
		case keys.AppExit:
			if m.ed.Empty() {
				return tea.Quit
			}
		case keys.AppClear:
			m.ed.Clear()
			return nil
		case keys.AppToolsExpand:
			if m.expandNewestTool() {
				return nil
			}
		case keys.AppModelSelect:
			return m.openModelPicker()
		case keys.AppModelCycleForward:
			return m.cycleModel(1)
		case keys.AppModelCycleBackward:
			return m.cycleModel(-1)
		case keys.AppThinkingCycle:
			return m.cycleThinking()
		case keys.AppThinkingToggle:
			m.toggleThinking()
			return nil
		case keys.AppSessionNew:
			return m.newSession()
		case keys.AppSessionFork:
			return m.forkSession()
		case keys.AppSessionResume:
			return m.listSessions()
		case keys.AppSuspend:
			// The program stops itself and resumes on SIGCONT; nothing here has to be put
			// away first, since the frame is redrawn when it comes back.
			return tea.Suspend
		case keys.TUIAltScreenLineUp, keys.TUIAltScreenLineDown,
			keys.TUIAltScreenPageUp, keys.TUIAltScreenPageDown,
			keys.TUIAltScreenHalfPageUp, keys.TUIAltScreenHalfPageDown,
			keys.TUIAltScreenTop, keys.TUIAltScreenBottom:
			// Only altscreen has a viewport to move. Inline rendering lives in the
			// terminal's own scrollback, so the key falls through to the editor, which is
			// where pageUp and home are bound as well.
			if m.scroll(a) {
				return nil
			}
		case keys.TUIInputNewLine:
			// The editor's own key map binds the same action, so the press falls through
			// to it below rather than being handled twice.
		default:
			// Every other action is the editor's own or nothing this client binds; the
			// key reaches the editor below, which is what an unbound key does.
		}
	}
	return m.ed.Update(k)
}

// expandNewestTool toggles the newest tool row on screen and reports whether there was
// one. Inline rendering commits a rested turn's rows to scrollback, so on screen means
// the live region; in altscreen every row is still here.
func (m *Model) expandNewestTool() bool {
	for _, r := range slices.Backward(m.tr.Rows()) {
		if r.Kind == transcript.RowTool {
			m.tr.Toggle(r.Key)
			return true
		}
	}
	return false
}

// click toggles the tool row the pointer landed on. The layout the last View drew says
// which row owns which line, so a click lands where the user aimed however many widgets,
// notices or header lines sit above the transcript.
//
// y is where the terminal saw the pointer, counted from the top of the screen. Altscreen
// owns the whole screen, so the two agree; inline anchors its frame at the bottom, so the
// frame's first line is at height minus the frame's own height and a click above that
// landed in scrollback, which this client does not own.
func (m *Model) click(y int) {
	lines, rowAt := m.compose()
	if m.inline() {
		y -= m.height - len(lines)
		if y < 0 {
			return
		}
	}
	if r := rowAt[y]; r != nil {
		// Toggle ignores anything that is not a tool row, which is every other row a
		// click can land on.
		m.tr.Toggle(r.Key)
	}
}

// wheel scrolls the transcript. Only altscreen has a viewport to scroll: inline rendering
// lives in the terminal's own scrollback, which the terminal scrolls itself.
func (m *Model) wheel(b tea.MouseButton) {
	if m.inline() {
		return
	}
	switch b {
	case tea.MouseWheelUp:
		m.vp.ScrollUp(m.vp.MouseWheelDelta)
	case tea.MouseWheelDown:
		m.vp.ScrollDown(m.vp.MouseWheelDelta)
	}
}

// scroll moves the viewport for one tui.altScreen action and reports whether it did.
// Inline is false for every one of them: the scroll there is the terminal's, and the key
// belongs to whatever else it is bound to.
//
// A scroll away from the bottom is what stops transcriptBlock following new rows, and
// bottom is what starts it again; both fall out of the viewport's own offset, so nothing
// here has to remember which it was.
func (m *Model) scroll(a keys.Action) bool {
	if m.inline() {
		return false
	}
	switch a {
	case keys.TUIAltScreenLineUp:
		m.vp.ScrollUp(1)
	case keys.TUIAltScreenLineDown:
		m.vp.ScrollDown(1)
	case keys.TUIAltScreenPageUp:
		m.vp.PageUp()
	case keys.TUIAltScreenPageDown:
		m.vp.PageDown()
	case keys.TUIAltScreenHalfPageUp:
		m.vp.HalfPageUp()
	case keys.TUIAltScreenHalfPageDown:
		m.vp.HalfPageDown()
	case keys.TUIAltScreenTop:
		m.vp.GotoTop()
	case keys.TUIAltScreenBottom:
		m.vp.GotoBottom()
	default:
		return false
	}
	return true
}

// newSpinner is the turn item's glyph: one cell, unstyled, so the item paints the glyph
// and its word in one role the way every other built-in item paints itself.
func newSpinner() spinner.Model {
	return spinner.New(spinner.WithSpinner(spinner.MiniDot))
}

// spinTick arms or disarms the turn spinner against what the turn is now doing. It returns
// the first tick when a resting client starts a turn and nothing at all otherwise: an idle
// client schedules no timer, and a turn already running does not start a second loop.
func (m *Model) spinTick() tea.Cmd {
	running := m.turn.word() != ""
	if running == m.spinning {
		return nil
	}
	m.spinning = running
	if !running {
		return nil
	}
	return m.spin.Tick
}

// spinTicked advances the spinner one frame and asks for the next tick, unless the turn
// has come to rest, in which case the loop ends here.
func (m *Model) spinTicked(msg spinner.TickMsg) tea.Cmd {
	if !m.spinning {
		return nil
	}
	var cmd tea.Cmd
	m.spin, cmd = m.spin.Update(msg)
	return cmd
}

// View draws the slots. AltScreen and the mouse mode are the view's own, so the program
// follows ui.render without a caller having to set it.
func (m *Model) View() tea.View {
	lines, _ := m.compose()
	v := tea.NewView(strings.Join(lines, "\n"))
	v.AltScreen = !m.inline()
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// compose lays the configured slots out in order and returns the lines with the
// transcript row each line came from, for the click that traces one back. It is the one
// place slots are ordered: View joins what it returns and click reads its map.
//
// above_editor and below_editor are not slots of their own in ui.layout.slots; they
// bracket the input slot, which is where the design puts them.
func (m *Model) compose() ([]string, map[int]*transcript.Row) {
	blocks := make([][]string, 0, len(m.cfg.UI.Layout.Slots)+2)
	at := -1
	for _, slot := range m.cfg.UI.Layout.Slots {
		switch slot {
		case slotHeader:
			blocks = append(blocks, m.widgetLines(protocol.SlotHeader))
		case slotTranscript:
			// Sized last: in altscreen the transcript takes the height the other slots
			// left, so its own block cannot be built until they are.
			at = len(blocks)
			blocks = append(blocks, nil)
		case slotInput:
			// The design leaves one blank line between the transcript and the composer,
			// so the input area reads as its own block and not as the next transcript
			// row. Only when something is above it: input as the first slot opens the
			// frame, and a frame does not open on a blank line.
			if len(blocks) > 0 {
				blocks = append(blocks, []string{""})
			}
			// A picker stands where the editor is, not over it: it is what the keyboard
			// is pointed at, and the draft it hides is still there when it closes.
			editor := strings.Split(m.ed.View(), "\n")
			if m.pick != nil {
				editor = m.pickerView()
			}
			blocks = append(blocks,
				m.widgetLines(protocol.SlotAboveEditor),
				m.menuLines(),
				editor,
				m.widgetLines(protocol.SlotBelowEditor),
			)
		case slotStatus:
			if s := m.statusLine(); s != "" {
				blocks = append(blocks, []string{s})
			}
		}
	}
	var rows []*transcript.Row
	if at >= 0 {
		used := 0
		for _, b := range blocks {
			used += len(b)
		}
		blocks[at], rows = m.transcriptBlock(m.height - used)
	}
	total := 0
	for _, b := range blocks {
		total += len(b)
	}
	out := make([]string, 0, total)
	rowAt := make(map[int]*transcript.Row)
	for i, b := range blocks {
		if i == at {
			for j := range min(len(b), len(rows)) {
				if rows[j] != nil {
					rowAt[len(out)+j] = rows[j]
				}
			}
		}
		out = append(out, b...)
	}
	return out, rowAt
}

// transcriptBlock is the transcript slot: the rows, then the notices under them. Inline
// draws the rows as they are, since a rested turn's rows have already been committed to
// scrollback and what is left is the live region. Altscreen keeps every row and scrolls
// them through a viewport h lines tall.
//
// The second result is the row each line came from, nil where a line belongs to none.
func (m *Model) transcriptBlock(h int) ([]string, []*transcript.Row) {
	laid := m.tr.Layout()
	notices := m.noticeLines()
	// The header is the top of the transcript, above the first row, and the conversation
	// scrolls it away as it grows. Inline never has one here: it was printed into the
	// terminal's own scrollback when the first size arrived (header.go).
	head := m.headerLines()
	if m.inline() {
		// The live region is what the frame has left once the other slots have taken
		// theirs. A turn that outgrew it keeps its newest lines: the oldest have already
		// been read, and the editor and the status line must stay on screen.
		if keep := max(h-len(notices), 0); len(laid) > keep {
			laid = laid[len(laid)-keep:]
		}
		lines := make([]string, 0, len(laid)+len(notices))
		rows := make([]*transcript.Row, 0, len(laid))
		for _, l := range laid {
			lines = append(lines, l.Text)
			rows = append(rows, l.Row)
		}
		return append(lines, notices...), rows
	}
	content := make([]string, 0, len(head)+len(laid))
	content = append(content, head...)
	for _, l := range laid {
		content = append(content, l.Text)
	}
	m.vp.SetHeight(max(h-len(notices), 1))
	// A viewport sitting at the bottom follows the rows that land under it; one the user
	// scrolled up stays where they left it. Read before the content changes: afterwards
	// every offset short of the new bottom looks scrolled.
	follow := m.vp.AtBottom()
	m.vp.SetContentLines(content)
	if follow {
		m.vp.GotoBottom()
	}
	view := strings.Split(m.vp.View(), "\n")
	rows := make([]*transcript.Row, len(view))
	for i := range view {
		// The header's own lines belong to no row, so the click that traces a line back to
		// one counts from where the rows start rather than from the top of the viewport.
		if j := m.vp.YOffset() + i - len(head); j >= 0 && j < len(laid) {
			rows[i] = laid[j].Row
		}
	}
	return append(view, notices...), rows
}

// noticeRoles map the notice level vocabulary onto the theme's. A level the contract does
// not define reads as text rather than being dropped.
var noticeRoles = map[string]theme.Role{
	levelInfo:  theme.RoleText,
	levelWarn:  theme.RoleWarning,
	levelError: theme.RoleError,
}

// noticeLines are the notices, drawn under the transcript in the region the client owns,
// the newest ui.notices.max lines of them. They are display-only: a notice never reaches
// the log, so it is held here rather than folded into the transcript as a row that a
// commit would print into scrollback, and it is bounded here so a run of them can never
// push the editor off the frame.
func (m *Model) noticeLines() []string {
	limit := m.cfg.UI.Notices.Max
	if limit <= 0 {
		return nil
	}
	var out []string
	for _, n := range m.notices {
		role, ok := noticeRoles[n.level]
		if !ok {
			role = theme.RoleText
		}
		out = append(out, m.tr.Wrap(role, n.text)...)
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// widgetLines is one line per widget in a slot, in the order the widgets arrived. A
// plugin owns its own widget and can neither clear nor reorder another's.
func (m *Model) widgetLines(slot protocol.WidgetSlot) []string {
	var out []string
	for _, w := range m.widgets {
		if w.Slot != slot {
			continue
		}
		// A widget with nothing to say takes no line, the way a status item with nothing
		// to say takes no cell: a plugin clears its widget by emptying it, and a blank
		// line in a slot is not what clearing looks like.
		if s := renderSpans(m.th, w.Content); s != "" {
			out = append(out, m.clamp(strings.Repeat(" ", transcript.Gutter)+s))
		}
	}
	return out
}

// renderSpans paints one plugin's spans through the theme. It is the only place a span
// reaches the screen, for a status item and a widget alike.
func renderSpans(th theme.Theme, spans []protocol.Span) string {
	var b strings.Builder
	for _, s := range spans {
		b.WriteString(th.StyleFor(s.Role).Render(spanText(s.Text)))
	}
	return b.String()
}

// spanText is one span's text, safe to draw. A span is data a plugin sent, and the design
// allows it no colors, no layout and no escape codes; one embedded sequence or newline
// would corrupt the region the client owns, so every control character goes, tab and
// newline included, since a span is one run on one line.
func spanText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, ansi.Strip(s))
}

// clamp truncates one already styled line to the terminal's width, so nothing a plugin
// sends can push the client's region wider than the screen.
func (m *Model) clamp(s string) string {
	if m.width <= 0 {
		return s
	}
	return ansi.Truncate(s, m.width, "")
}
