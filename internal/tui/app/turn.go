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
// reported and the turn it belongs to, when the last Esc the table acted on landed, the
// permission question standing on screen and the one this client has answered but not yet
// heard back about.
type turnControl struct {
	state   string
	turnID  string
	lastEsc time.Time
	prompt  *protocol.PermissionRequested
	// asked is the question whose answer is in flight, kept so a call that never reached
	// the server can put the question back.
	asked *protocol.PermissionRequested
	// streamed is set once text has streamed in this turn. The server reports one
	// streaming state for both halves of it, and what the turn status item says while it
	// runs is thinking until an answer begins and streaming after (ADR 0013 decision 3).
	streamed bool
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
	m.turn.prompt, m.turn.asked = nil, nil
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
		return m.callNamed(protocol.MethodCommandRun, name, protocol.CommandRunParams{
			SessionID: m.session.SessionID,
			Name:      name,
			Args:      args,
		})
	}
	return m.submitText(text, session.SourceTyped)
}

// helpCommand is the command whose notice lists the others.
const helpCommand = "help"

// complete finishes the command name a draft opens with, and reports whether it did. The
// protocol gives a client no list of commands (there is no command.list, and command.run
// takes a name rather than answering with one), so /help's notice is the discovery
// surface: what it named last is what completes here, and a client that has never run it
// completes nothing. A draft that is not a lone slash word is not a command name yet and
// is left to the editor.
func (m *Model) complete() bool {
	name, args, ok := commandIn(m.ed.Text())
	if !ok || args != "" || strings.ContainsFunc(m.ed.Text(), unicode.IsSpace) {
		return false
	}
	var match []string
	for _, c := range m.commands {
		if strings.HasPrefix(c, name) {
			match = append(match, c)
		}
	}
	// The longest prefix every candidate shares: one candidate completes the whole name,
	// several complete as far as they agree, which is what a shell does and what lets a
	// second tab, after another letter, get further.
	grown := commonPrefix(match)
	if len(grown) <= len(name) {
		return false
	}
	m.ed.SetText("/" + grown)
	return true
}

// commonPrefix is the longest prefix all of ss share, "" for none.
func commonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	out := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, out) {
			out = out[:len(out)-1]
		}
	}
	return out
}

// helpCommands are the names a /help notice listed: every line it draws opens with the
// command's own "/name". A line that does not is not a command and is skipped, so a
// notice that grows a header or a footer costs nothing.
func helpCommands(notice string) []string {
	var out []string
	for _, line := range strings.Split(notice, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasPrefix(f[0], "/") {
			continue
		}
		if name := strings.TrimPrefix(f[0], "/"); name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
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

// requested puts a permission question on screen and makes it the one the keyboard
// answers.
func (m *Model) requested(p protocol.PermissionRequested) {
	m.turn.prompt = &p
	m.tr.Prompt(p)
}

// answered takes the question for toolUseID down, wherever the decision came from: this
// client's own answer, a hook, the gate, or another asker.
func (m *Model) answered(toolUseID string) {
	if m.turn.prompt != nil && m.turn.prompt.ToolUseID == toolUseID {
		m.turn.prompt = nil
	}
	if m.turn.asked != nil && m.turn.asked.ToolUseID == toolUseID {
		m.turn.asked = nil
	}
	m.tr.Answered(toolUseID)
}

// reask puts back the question this client answered when the answer never reached the
// server. Without it the turn waits on a decision that will never come, with nothing on
// screen to answer it with.
func (m *Model) reask() {
	// A question standing now is a later one the server is waiting on, and it keeps the
	// keyboard: the failed answer's own question is moot, since nothing asks twice.
	if m.turn.asked == nil || m.turn.prompt != nil {
		return
	}
	m.turn.prompt = m.turn.asked
	m.tr.Prompt(*m.turn.asked)
}

// answer is the standing question's own key handling, and it runs before anything else:
// the turn is waiting on it, so nothing may take y, a, n or Esc from it. Any other key
// falls through to the rest of the keyboard, which is what keeps ctrl+c and ctrl+d
// working while a question stands.
func (m *Model) answer(k tea.KeyPressMsg) (tea.Cmd, bool) {
	p := m.turn.prompt
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
	m.answered(p.ToolUseID)
	m.turn.asked = p
	return m.call(protocol.MethodSessionAnswer, protocol.SessionAnswerParams{
		SessionID: m.session.SessionID,
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
	if m.disconnected {
		m.note(levelError, "not connected: nothing was sent, the draft is still here")
		return nil
	}
	if m.turn.running() {
		m.ed.Enqueue()
		return nil
	}
	text := m.ed.Text()
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
