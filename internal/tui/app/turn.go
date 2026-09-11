package app

import (
	"slices"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tui/input"
	"github.com/guygrigsby/rudy/internal/tui/keys"
)

// The turn states a turn.state notification carries (docs/specs/rudy-contracts.md,
// TurnState). The client mirrors the server's vocabulary rather than keeping a second
// state machine beside it: running, steering and resting, the three states the
// TurnControl table decides by, are read off this one value.
const (
	stateStreaming          = "streaming"
	stateRunningTool        = "running_tool"
	stateAwaitingPermission = "awaiting_permission"
	stateSteering           = "steering"
	stateCompleted          = "completed"
	stateFailed             = "failed"
	stateIdle               = "idle"
)

// answerReason is what a decision this client made records as its reason. The server
// stamps decided_by asker on it; the reason says the same thing to a human reading the
// log.
const answerReason = "asker"

// turnControl is what the client knows about the turn: the state the server last
// reported and the turn it belongs to, when the last Esc the table acted on landed, every
// permission question standing on screen and the ones this client has answered but not yet
// heard back about.
type turnControl struct {
	state   string
	turnID  string
	lastEsc time.Time
	// prompts are every standing question this client knows about, keyed the same way the
	// server keys its own standing set (session_live.go): session id and tool_use id. One
	// slot stopped being enough once tool calls in an assistant message started running
	// concurrently (ADR 0028 wired the runner for it; task 7 is what makes a subagent's own
	// questions reach this client too), since more than one can be outstanding on this
	// session alone, before a subagent adds its own on top (rudy-9nc).
	prompts map[promptKey]*protocol.PermissionRequested
	// order is prompts' keys in arrival order. A map has none of its own, and a question with
	// no row on screen has no place in the transcript's own order either, which is what
	// newest falls back to.
	order []promptKey
	// asked is the same shape as prompts, for a question whose answer is in flight, kept so
	// a call that never reached the server can put it back.
	asked map[promptKey]*protocol.PermissionRequested
	// streamed is set once text has streamed in this turn. The server reports one
	// streaming state for both halves of it, and what the turn status item says while it
	// runs is thinking until an answer begins and streaming after (ADR 0013 decision 3).
	streamed bool
}

// promptKey names one standing question exactly the way the server's own standing set does:
// which session asked, and which tool_use it is for. Two sessions can mint the same
// tool_use id shape (ADR 0028), so the session id is part of the key, not an afterthought.
type promptKey struct {
	sessionID string
	toolUseID string
}

func keyOf(p *protocol.PermissionRequested) promptKey {
	return promptKey{sessionID: p.SessionID, toolUseID: p.ToolUseID}
}

// promptName encodes a key into CallResultMsg.Name, so callResult and callFailed know which
// of possibly several outstanding session.answer calls a given response is for. sid is a
// ULID and so never contains "/", which is what makes splitting on the first one exact.
func promptName(sid, toolUseID string) string { return sid + "/" + toolUseID }

func parsePromptName(name string) (sid, toolUseID string) {
	sid, toolUseID, _ = strings.Cut(name, "/")
	return sid, toolUseID
}

// focused is the question the keyboard answers: the one whose row is nearest the input, which
// is the row the transcript draws the keys on (Transcript.FocusedPrompt). The two read the same
// row on purpose. Arrival order is not row order, so answering the newest question would answer
// whichever one arrived last wherever it happens to be drawn: a subagent's question is inserted
// under the agent call that opened it, so a child's later question lands above a parent's
// earlier one, and the operator reading the bottom row and pressing y would have consented to
// arguments they never saw (rudy-omc).
//
// A question with no row falls back to arrival order: nothing is on screen for the keyboard to
// be nearest to, and the row can genuinely be gone while the question stands (a session switch
// rebuilds the transcript and leaves the tracking, which takeDownPrompt already allows for).
func (m *Model) focused() *protocol.PermissionRequested {
	if p := m.tr.FocusedPrompt(); p != nil {
		if tracked, ok := m.turn.prompts[keyOf(p)]; ok {
			return tracked
		}
	}
	return m.turn.newest()
}

// newest is the most recently arrived standing question, or nil when none stands.
func (t turnControl) newest() *protocol.PermissionRequested {
	if len(t.order) == 0 {
		return nil
	}
	return t.prompts[t.order[len(t.order)-1]]
}

// addPrompt records p as standing, appending its key to order the first time it is seen so
// a replay or a resend of the same question does not move it out of arrival order.
func (t *turnControl) addPrompt(p *protocol.PermissionRequested) {
	k := keyOf(p)
	if t.prompts == nil {
		t.prompts = map[promptKey]*protocol.PermissionRequested{}
	}
	if _, seen := t.prompts[k]; !seen {
		t.order = append(t.order, k)
	}
	t.prompts[k] = p
}

// removePrompt takes k down, wherever it stood in arrival order.
func (t *turnControl) removePrompt(k promptKey) {
	if _, ok := t.prompts[k]; !ok {
		return
	}
	delete(t.prompts, k)
	t.order = slices.DeleteFunc(t.order, func(x promptKey) bool { return x == k })
}

// clearAsked drops the in-flight answer for sid/toolUseID, if there is one.
func (t *turnControl) clearAsked(sid, toolUseID string) {
	delete(t.asked, promptKey{sessionID: sid, toolUseID: toolUseID})
}

// clearPrompts drops every standing and in-flight question: what a turn coming to rest does
// (turnChanged), since an interrupt denies whatever it was waiting on and the decision is
// already in the log, so every question the turn was carrying goes with it.
func (t *turnControl) clearPrompts() {
	t.prompts, t.order, t.asked = nil, nil, nil
}

// turnWords name what the turn item says for each state the server reports that is not
// rest. streaming is not here: it is the one state that reads two ways, see word.
var turnWords = map[string]string{
	stateRunningTool:        "tool",
	stateAwaitingPermission: "waiting",
	stateSteering:           "steering",
}

// word is what the turn is doing, in one word, or "" for a turn at rest. It is the whole
// of what says whether the turn item draws anything and whether the spinner is turning.
func (t turnControl) word() string {
	if w, ok := turnWords[t.state]; ok {
		return w
	}
	if t.state != stateStreaming {
		return ""
	}
	if t.streamed {
		return "streaming"
	}
	return "thinking"
}

// set moves the mirror to one state of one turn. A turn id this client has not seen
// before is a new turn, and what the turn before it streamed is not its.
func (t *turnControl) set(state, turnID string) {
	if turnID != t.turnID {
		t.streamed = false
	}
	t.state, t.turnID = state, turnID
}

// running is a turn the server is working on: streaming, running a tool, or waiting for
// an answer to a permission question.
func (t turnControl) running() bool {
	switch t.state {
	case stateStreaming, stateRunningTool, stateAwaitingPermission:
		return true
	}
	return false
}

// steering is a turn parked waiting for the message that continues it.
func (t turnControl) steering() bool { return t.state == stateSteering }

// resting is a turn that is over, or one that never started. completed, failed and idle
// all rest: what separates them is what the log recorded, not what the client may do
// next.
func (t turnControl) resting() bool { return !t.running() && !t.steering() }

// turnChanged mirrors one turn.state notification. A turn that comes to rest is
// committed: inline rendering prints its rows into the terminal's own scrollback through
// one tea.Println, leaving the live region to the editor and the status line, and then
// sends whatever was queued behind it. Altscreen keeps every row where it is, so it only
// sends the queue.
func (m *Model) turnChanged(p protocol.TurnStateChanged) tea.Cmd {
	m.turn.set(p.State, p.TurnID)
	spin := m.spinTick()
	if !m.turn.resting() {
		return spin
	}
	// A turn that ended answers no question: an interrupt denies what it was waiting on
	// and the decision is already in the log, so the question goes with the turn.
	m.turn.clearPrompts()
	var commit tea.Cmd
	// The same guard commitTurns keeps: a turn.state buffered during a session switch is
	// folded while the switch replays, and the one ordered print at the end of that replay
	// is what puts those rows in scrollback. A second print from here would batch against
	// it, and a tea.Batch does not order its commands.
	if m.inline() && !m.replaying {
		// One Println for the whole turn: the program writes each one above the frame as
		// its own block, and a turn's rows are one block, in order.
		if lines := m.tr.Commit(p.TurnID); len(lines) > 0 {
			commit = tea.Println(strings.Join(lines, "\n"))
		}
	}
	return tea.Batch(spin, commit, m.sendQueued())
}

// sendQueued sends the oldest message waiting behind the turn that just rested, down the
// same path Enter would have taken it: what was held back is a message the user typed, so
// a queued slash word is still a command.
func (m *Model) sendQueued() tea.Cmd {
	if m.disconnected {
		return nil
	}
	head, ok := m.ed.Dequeue()
	if !ok {
		return nil
	}
	return m.sendTyped(head)
}

// sendTyped starts a turn with text, or runs it as a command when it opens with a slash.
// It is the one path a typed message takes, whether Enter sent it now or the queue sent it
// when the turn rested.
func (m *Model) sendTyped(text string) tea.Cmd {
	if name, args, ok := commandIn(text); ok {
		return m.runCommand(name, args, text)
	}
	return m.submitText(text, session.SourceTyped)
}

// runCommand runs one command and remembers the draft it was typed as, so a refusal can
// put it back where a person can press Enter on it again. The client's own never reach the
// wire: what they do is the client's business and no session hears about them (ADR 0015
// decision 3, ADR 0020).
func (m *Model) runCommand(name, args, draft string) tea.Cmd {
	if localCommand(name) {
		return m.runLocal(name, args)
	}
	m.pendingCommand = draft
	return m.callNamed(protocol.MethodCommandRun, name, protocol.CommandRunParams{
		SessionID: m.session.SessionID,
		Name:      name,
		Args:      args,
	})
}

// lateRows commits a row that arrived after its turn had already gone to scrollback: a
// note appended between turns carries the committed turn's id, and nothing else would
// ever print it. Altscreen keeps every row, so it commits nothing here either.
func (m *Model) lateRows(added []string) tea.Cmd {
	if len(added) == 0 || !m.inline() {
		return nil
	}
	if lines := m.tr.CommitLate(); len(lines) > 0 {
		return tea.Println(strings.Join(lines, "\n"))
	}
	return nil
}

// requested puts a permission question on screen and adds it to the standing set (rudy-9nc):
// concurrent tool calls mean it may not be the only one, so this never assumes it replaces
// whatever was there before.
func (m *Model) requested(p protocol.PermissionRequested) {
	m.turn.addPrompt(&p)
	m.tr.Prompt(p)
}

// requestedFrom is requested for a subagent's own permission question (rudy-ssz): a child
// has no asker of its own, so its question reaches this client borrowing the parent's, and
// this client is entitled to answer it (contracts: session.answer checks cn.asker only, not
// subscription). Rendered under the agent call that opened the child (Transcript.PromptFrom)
// rather than in place of a tool row this session's own turn owns.
func (m *Model) requestedFrom(sid, callToolUseID string, p protocol.PermissionRequested) {
	m.turn.addPrompt(&p)
	m.tr.PromptFrom(sid, callToolUseID, p)
}

// clearPrompt takes down the standing or in-flight question sid asked for toolUseID,
// whichever of the two it is, wherever the decision came from: this client's own answer, a
// hook, the gate, or another asker.
func (m *Model) clearPrompt(sid, toolUseID string) {
	k := promptKey{sessionID: sid, toolUseID: toolUseID}
	m.turn.removePrompt(k)
	m.turn.clearAsked(sid, toolUseID)
}

// answered takes this session's own question for toolUseID down.
func (m *Model) answered(toolUseID string) {
	m.clearPrompt(m.session.SessionID, toolUseID)
	m.tr.Answered(toolUseID)
}

// answeredFrom is answered for a subagent's own question.
func (m *Model) answeredFrom(sid, callToolUseID, toolUseID string) {
	m.clearPrompt(sid, toolUseID)
	m.tr.AnsweredFrom(sid, callToolUseID, toolUseID)
}

// takeDownPrompt removes p's own row and the tracking that held it, wherever it came from:
// this session's own tool row, or a subagent's under the call that opened it. The mapping
// being gone (a stale prompt surviving a session switch) still clears the tracking, so the
// keyboard does not keep answering a question that no longer has anywhere to land.
func (m *Model) takeDownPrompt(p *protocol.PermissionRequested) {
	if p.SessionID == m.session.SessionID {
		m.answered(p.ToolUseID)
		return
	}
	if call, ok := m.tr.AgentCallFor(p.SessionID); ok {
		m.answeredFrom(p.SessionID, call, p.ToolUseID)
		return
	}
	m.clearPrompt(p.SessionID, p.ToolUseID)
}

// reaskOne puts back the question sid/toolUseID named when its answer never reached the
// server. Without it the turn waits on a decision that will never come, with nothing on
// screen to answer it with.
func (m *Model) reaskOne(sid, toolUseID string) {
	k := promptKey{sessionID: sid, toolUseID: toolUseID}
	p, ok := m.turn.asked[k]
	if !ok {
		return
	}
	delete(m.turn.asked, k)
	if _, exists := m.turn.prompts[k]; exists {
		// A later question already stands in this same slot; the failed answer's own
		// question is moot, since nothing asks twice for the same call.
		return
	}
	m.turn.addPrompt(p)
	if sid != m.session.SessionID {
		if call, ok := m.tr.AgentCallFor(sid); ok {
			m.tr.PromptFrom(sid, call, *p)
			return
		}
	}
	m.tr.Prompt(*p)
}

// answer is the standing question's own key handling, and it runs before anything else:
// the turn is waiting on it, so nothing may take y, a, n or Esc from it. Any other key
// falls through to the rest of the keyboard, which is what keeps ctrl+c and ctrl+d
// working while a question stands. It answers the focused question, the one nearest the input
// (rudy-9nc, rudy-omc) — concurrent tool calls mean more than one can be waiting, and the
// keyboard has no way to name one but the nearest until a picker gives it one, which is why
// that row is also the only one that draws the keys.
func (m *Model) answer(k tea.KeyPressMsg) (tea.Cmd, bool) {
	p := m.focused()
	if p == nil {
		return nil, false
	}
	decision, scope, ok := answerKey(tea.Key(k))
	if !ok {
		return nil, false
	}
	// The question goes now rather than when the decision entry arrives: the server has
	// the answer, and a question that stays on screen after it was answered reads as one
	// that was not. It is kept in hand until the call comes back, since an answer that
	// never arrived has to go back up.
	m.takeDownPrompt(p)
	if m.turn.asked == nil {
		m.turn.asked = map[promptKey]*protocol.PermissionRequested{}
	}
	m.turn.asked[keyOf(p)] = p
	return m.callNamed(protocol.MethodSessionAnswer, promptName(p.SessionID, p.ToolUseID), protocol.SessionAnswerParams{
		// The session the prompt actually named, not this client's own: a subagent's
		// question carries the child's session id, and session.answer is deliberately not
		// subscription gated (contracts), so this is the mechanism working as designed
		// (rudy-ssz), not this session answering for another it does not own.
		SessionID: p.SessionID,
		ToolUseID: p.ToolUseID,
		Decision:  decision,
		Scope:     scope,
		Reason:    answerReason,
	}), true
}

// answerKey maps a key to the decision it makes: the three the question offers, plus Esc,
// which denies the way a client that goes away denies. These are not [keys] actions: a
// question is answered by the letters it draws, and rebinding them would leave the row on
// screen lying about what to press.
func answerKey(k tea.Key) (session.Decision, session.Scope, bool) {
	n := keys.Normalize(k)
	if n.Mod != 0 {
		return "", "", false
	}
	switch n.Code {
	case 'y':
		return session.Allow, session.ScopeOnce, true
	case 'a':
		return session.Allow, session.ScopeSession, true
	case 'n':
		return session.Deny, session.ScopeOnce, true
	case tea.KeyEscape:
		return session.Deny, session.ScopeOnce, true
	}
	return "", "", false
}

// interrupt runs the TurnControl table for one Esc (docs/specs/rudy-domain-model.md,
// TurnControl; ADR 0006). handled false means the key was not the table's and belongs to
// whatever comes next, which is the editor.
func (m *Model) interrupt() (cmd tea.Cmd, handled bool) {
	// The editor's mode is read first: pi's Esc layering leaves insert and visual before
	// the turn ever hears about the key.
	if mode := m.ed.Mode(); mode == input.ModeInsert || mode == input.ModeVisual {
		return nil, false
	}
	now := time.Now()
	double := !m.turn.lastEsc.IsZero() && now.Sub(m.turn.lastEsc) < m.doublePress()
	switch {
	case m.turn.running():
		m.turn.lastEsc = now
		if double {
			return m.cancelTurn(), true
		}
		return m.interruptTurn(session.InterruptSteer), true
	case m.turn.steering():
		// A steering turn with a draft continues on Enter, so Esc does nothing but arm
		// the second press that cancels it: this is the one row of the table that arms
		// the window without issuing an interrupt, and without it the row's own second
		// press could never be a double. With nothing to continue the turn with, one Esc
		// is the cancel.
		m.turn.lastEsc = now
		if double || m.ed.Empty() {
			return m.cancelTurn(), true
		}
		return nil, true
	}
	// Idle: a picker takes Esc before any of this (Model.key), so what reaches here with
	// no turn to interrupt is the editor's own. It arms nothing, or a turn that starts by
	// itself (the queue draining into a new turn) would read the next Esc as a double
	// press and cancel it.
	return nil, false
}

// cancelTurn ends the turn and puts the queue back where it can be edited: a user who
// cancelled a turn did not mean to send the messages waiting behind it either.
func (m *Model) cancelTurn() tea.Cmd {
	m.ed.Restore()
	return m.interruptTurn(session.InterruptCancel)
}

func (m *Model) interruptTurn(how session.Interrupt) tea.Cmd {
	return m.call(protocol.MethodSessionInterrupt, protocol.SessionInterruptParams{
		SessionID: m.session.SessionID,
		How:       how,
	})
}

// doublePress is how close two Esc presses have to be to mean cancel.
func (m *Model) doublePress() time.Duration {
	return time.Duration(m.cfg.Permissions.DoublePressMS) * time.Millisecond
}

// submit sends what the editor holds, by what the turn is doing: a running turn takes it
// as a follow-up in the queue, a steering turn as the steer message that continues it,
// and a resting one as a new turn, or as a command when the draft opens with a slash. The
// editor is cleared by everything that leaves with the message.
func (m *Model) submit() tea.Cmd {
	if m.ed.Empty() {
		return nil
	}
	text := m.ed.Text()
	name, args, isCommand := commandIn(text)
	// The client's own commands answer with no server at all: /exit is the one thing a
	// client whose server has gone away still has to be able to do.
	if isCommand && localCommand(name) {
		m.ed.Clear()
		return m.runLocal(name, args)
	}
	if m.disconnected {
		m.note(levelError, "not connected: nothing was sent, the draft is still here")
		return nil
	}
	// A command is not a message and does not wait behind the turn: somebody who types one
	// while the model is thinking means now (ADR 0026). The server refuses the few that
	// need a resting session in its own words, and the draft comes back to the editor.
	if isCommand {
		m.ed.Clear()
		return m.runCommand(name, args, text)
	}
	if m.turn.running() {
		m.ed.Enqueue()
		return nil
	}
	// A shell command is not a message and never steers a turn: it runs, it is recorded,
	// and the model reads it next time something is sent (ADR 0023).
	if command, ok := shellIn(text); ok {
		m.ed.Clear()
		return m.call(protocol.MethodSessionShell, protocol.SessionShellParams{
			SessionID: m.session.SessionID,
			Command:   command,
		})
	}
	m.ed.Clear()
	if m.turn.steering() {
		return m.submitText(text, session.SourceSteer)
	}
	return m.sendTyped(text)
}

// submitText sends one message as a turn's content.
func (m *Model) submitText(text string, source session.Source) tea.Cmd {
	return m.call(protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: m.session.SessionID,
		Content:   []session.Block{session.TextBlock(text)},
		Source:    source,
	})
}

// shellIn reads a draft that opens with a bang as a shell command, and reports whether it
// is one. Everything after the bang is the command, the whole draft, so a multi-line script
// is one command the way a person would paste it.
func shellIn(draft string) (string, bool) {
	rest, ok := strings.CutPrefix(draft, shellPrefix)
	if !ok {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	return rest, rest != ""
}

// commandIn reads a draft that opens with a slash as a command and its arguments: the
// first word is the name, the rest is the argument string the command parses itself.
func commandIn(s string) (name, args string, ok bool) {
	if !strings.HasPrefix(s, "/") {
		return "", "", false
	}
	rest := strings.TrimPrefix(s, "/")
	if i := strings.IndexFunc(rest, unicode.IsSpace); i >= 0 {
		name, args = rest[:i], strings.TrimSpace(rest[i:])
	} else {
		name = rest
	}
	return name, args, name != ""
}

// started takes the turn id a submit or a command answered with. The turn's own
// notifications are what drive the mirror; this covers only the gap before the first of
// them lands, so a second Enter in that window is queued rather than refused by the
// server. A turn the mirror has already heard about is left alone, which is what keeps an
// answer that arrives after its turn already finished from reviving it.
func (m *Model) started(turnID string) tea.Cmd {
	if turnID == "" || m.turn.turnID == turnID {
		return nil
	}
	m.turn.set(stateStreaming, turnID)
	return m.spinTick()
}
