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
	"encoding/json"
	"slices"
	"strings"
	"time"

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

	// status holds the plugin half of the status line, keyed "<owner>:<key>", which is
	// also how ui.status.items names one.
	status map[string]protocol.StatusItem
	// widgets are every plugin's widget in arrival order, one entry per owner and key.
	widgets []protocol.Widget
	notices []notice

	// turn is what the client knows about the turn in flight: the server's own state
	// mirrored, the question standing on screen and what Esc does next (turn.go).
	turn turnControl

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
	width     int
	height    int

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
		cfg:       cfg,
		th:        o.Theme,
		keys:      table,
		cl:        o.Client,
		cwd:       o.Cwd,
		session:   o.Session,
		models:    o.Models,
		status:    make(map[string]protocol.StatusItem),
		workspace: o.Workspace,
		width:     defaultWidth,
		height:    defaultHeight,
	}
	m.tr = transcript.New(transcript.Options{
		Width:            defaultWidth,
		ToolCollapsed:    cfg.UI.Transcript.ToolCollapsed,
		ToolPreviewLines: cfg.UI.Transcript.ToolPreviewLines,
		ShowThinking:     cfg.UI.Transcript.Thinking == thinkingShown,
		UserPrefix:       cfg.UI.Transcript.UserPrefix,
		BlockGap:         cfg.UI.Transcript.BlockGap,
		DiffBackground:   cfg.UI.Diff.Style == diffBackground,
	}, o.Theme)
	m.ed = input.New(cfg.UI.Vim, o.Theme, defaultWidth, table)
	m.vp = viewport.New(viewport.WithWidth(defaultWidth), viewport.WithHeight(defaultHeight))
	m.model = pickModel(m.models, m.session.Model)
	if m.workspace == "" {
		m.workspace = detectWorkspace(m.session.Workspace.GitRoot, m.cwd)
	}
	return m
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
	return tea.Batch(pump(m.cl), m.ed.Focus(), m.setModel(m.session.Model))
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
		return m, nil
	case tea.KeyPressMsg:
		return m, m.key(msg)
	case tea.MouseClickMsg:
		m.click(msg.Y)
		return m, nil
	case tea.MouseWheelMsg:
		m.wheel(msg.Button)
		return m, nil
	}
	// Everything else, a cursor blink among it, belongs to the editor.
	return m, m.ed.Update(msg)
}

// notification dispatches one server notification by method. It returns a command for the
// methods that need one (the inline commit hangs off turn.state); the folding itself
// happens here, on the update loop.
func (m *Model) notification(n protocol.Notification) tea.Cmd {
	switch n.Method {
	case protocol.NotifyEntryAppended:
		var p protocol.EntryAppended
		if m.decode(n, &p) {
			return m.entry(p.Entry)
		}
	case protocol.NotifyStreamDelta:
		var p protocol.StreamDelta
		if m.decode(n, &p) {
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
	added := m.tr.Apply(e)
	var cmd tea.Cmd
	switch p := e.Payload.(type) {
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
// reads (session.interrupt, session.answer) has no case: a failure is already a notice
// above, and the turn's own notifications carry the rest. Task 8 adds the pickers'
// methods beside these.
func (m *Model) callResult(r CallResultMsg) tea.Cmd {
	if r.Err != nil {
		if r.Method == protocol.MethodSessionAnswer {
			// The question came down when the answer went out and the server never got
			// it: put it back rather than leave the turn waiting on a decision with
			// nothing on screen to make it with.
			m.reask()
		}
		m.note(levelError, r.Method+": "+r.Err.Error())
		return nil
	}
	switch r.Method {
	case protocol.MethodSessionAnswer:
		// The server has the decision; nothing is left to put back.
		m.turn.asked = nil
	case protocol.MethodRegistryList:
		var res protocol.RegistryListResult
		if !m.result(r, &res) {
			return nil
		}
		m.models = res.Models
		// Not setModel: the refresh is the answer to a model-not-found, and a model the
		// fresh registry still lacks must not ask for it again.
		m.model = pickModel(res.Models, m.session.Model)
	case protocol.MethodSessionSubmit:
		var res protocol.SessionSubmitResult
		if !m.result(r, &res) {
			return nil
		}
		m.started(res.TurnID)
	case protocol.MethodCommandRun:
		var res protocol.CommandRunResult
		if !m.result(r, &res) {
			return nil
		}
		if res.Notice != "" {
			m.note(levelInfo, res.Notice)
		}
		// A command that opened another session (a fork) answers with its id; switching
		// to it is Task 8's, with the rest of the session actions.
		m.started(res.TurnID)
	}
	return nil
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

// key routes one key press in the order the design gives it: a standing permission
// question first, then a picker (Task 8), then the key table's actions, then the editor.
// An action that does not apply falls through to the next one the key is bound to, which
// is what lets ctrl+d exit on an empty editor and delete forward on a full one.
func (m *Model) key(k tea.KeyPressMsg) tea.Cmd {
	if cmd, answered := m.answer(k); answered {
		return cmd
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
		case keys.TUIInputNewLine:
			// The editor's own key map binds the same action, so the press falls through
			// to it below rather than being handled twice.
		default:
			// Every other action belongs to Task 8: the pickers and the model, thinking
			// and session actions. Until they land the key reaches the editor, which is
			// what an unbound key does.
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
			blocks = append(blocks,
				m.widgetLines(protocol.SlotAboveEditor),
				strings.Split(m.ed.View(), "\n"),
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
	content := make([]string, len(laid))
	for i, l := range laid {
		content[i] = l.Text
	}
	m.vp.SetHeight(max(h-len(notices), 1))
	m.vp.SetContentLines(content)
	view := strings.Split(m.vp.View(), "\n")
	rows := make([]*transcript.Row, len(view))
	for i := range view {
		if j := m.vp.YOffset() + i; j < len(laid) {
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
		if s := m.clamp(renderSpans(m.th, w.Content)); s != "" {
			out = append(out, s)
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
