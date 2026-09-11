package server

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/turn"
)

// errNoAsker is returned by liveAsker.Ask when the session has no subscriber whose hello
// declared asker, and when the last one detaches while a question stands. It wraps
// turn.ErrNoAsker, which is what makes the runner record the fixed no_asker reason for both
// rather than an asker failure.
var errNoAsker = fmt.Errorf("server: %w", turn.ErrNoAsker)

// liveSession is one open session: the real, single-owner *session.Session plus everything
// the server tracks about it while it is live.
//
// It carries two locks with a strict, never-reversed nesting order (mu outer, obsMu inner,
// each also independently self-locking) chosen specifically to avoid an AB-BA deadlock against
// turn.Runner's own mutex:
//
//   - mu guards sess, model, runner, pending and closed: the fields that decide whether a turn
//     may start and that only the server ever mutates (never the running turn itself). Nothing
//     that holds mu may call any turn.Runner method (State, TurnID, Interrupt, Run): the
//     runner calls its Observer's methods while holding its own mutex, and if a handler here
//     held mu while blocking on a Runner call, a concurrent Observer callback wanting mu (or
//     obsMu, see below) would deadlock against it. A handler that needs "is a turn active"
//     reads the mirror in obsMu instead of asking the runner directly.
//
//   - obsMu guards entries (the log mirror), conns (subscribers), state/turnID (a mirror of
//     the runner's State/TurnID, updated from the Observer callbacks) and the standing and
//     answered permission questions. Those last two sit here rather than beside pending
//     because subscribeLocked reads them under the Server.mu it already holds, and waiting
//     there on mu, which a compaction owns for the length of a provider request, would stall
//     every session lookup in the process (see claimCloseIfIdle). obsMu is the ONLY lock a
//     turn.Observer callback (fanout's EntryAppended/Delta/StateChanged) may take, together
//     with each connection's own outbox lock (conn.mu, unrelated to either lock here): those
//     callbacks run while the turn.Runner holds its own mutex, so taking mu, or calling any
//     Runner method, from inside one would risk the same deadlock mu's own rule guards
//     against. Every read of the runner's current state or turn id anywhere in this package,
//     even from a handler that also holds mu, goes through this mirror instead of the runner,
//     except where no liveSession lock is held at all (session.interrupt, and the two lines in
//     startTurn and runTurn noted there) - calling the runner directly is fine, and fresher,
//     when neither lock is held.
//
// Because the mirror is only ever updated by the Observer callback for the state or entry that
// already happened, a handler's active-turn check can observe it one callback late: between
// the runner appending its first entry (which unblocks startTurn's caller, see
// firstAppendSignal) and that first StateChanged callback landing, a concurrent handler could
// still see the pre-turn mirrored state. This is a deliberately accepted, narrow lag rather
// than something eliminated by holding a lock across both changes; it exists because closing
// it would mean re-introducing exactly the AB-BA risk mu's own doc above rules out.
type liveSession struct {
	mu          sync.Mutex
	sess        *session.Session
	model       provider.Model
	runner      *turn.Runner
	pending     map[string]chan turn.Answer
	closed      bool     // sess has been closed and removed from Server.live; never touch sess again
	hookContext []string // what session_opened handlers added to this session's system prompt

	// asking is the questions currently in front of the operator, keyed by what they ask
	// rather than by which call asked. Tool calls run concurrently, so several calls can want
	// the same permission at once; they share one question and its answer instead of
	// prompting the operator once per call (ADR 0028).
	asking map[askKey]*standingAsk

	// overrides is what after_tool handlers replaced, by tool_use id, for as long as this
	// session is live. The map reference is set once in newLive and never replaced, so it
	// needs no lock; its contents belong to whichever turn.Runner is currently running and
	// are only touched on that runner's own goroutine (see turn.Config.Overrides).
	overrides map[string][]session.Block

	// The agent definition's effect on every turn this session runs, and the parent that
	// opened it. All four are written by open (or coldLoadOne) before the session is
	// shared, and never again, so startTurn reads them under the mu it already holds
	// without any further synchronization.
	tools    turn.Tools   // nil means every registered tool
	system   string       // the agent definition's body; "" means the default base prompt
	maxSteps int          // the definition's max_turns; zero means unlimited
	parent   *liveSession // the session whose tool call opened this one; nil for a root

	// children is the tool_use ids of this session that have already opened a child, so a
	// second open for the same call is refused rather than silently spawning a second
	// subagent nobody will read. Guarded by mu, unlike the four fields above: it is written
	// while the session is live, from whichever connection is opening the child.
	children map[string]bool

	obsMu   sync.Mutex
	entries []session.Entry
	conns   []*conn
	state   turn.State
	turnID  string

	// standing is the permission questions the askers have been asked and none has
	// answered yet, by tool_use id: what an asker attaching while one stands is owed, and
	// what detach abandons when the last asker leaves. answered is the ids one of them has
	// already decided, kept until the turn rests (see fanout.StateChanged): it is what
	// tells a second answer, which is a conflict, from an answer to a question nobody is
	// asking, which is not_found.
	standing map[string]standingQuestion
	answered map[string]bool
}

// standingQuestion is one question currently put to the askers: the payload to re-send to an
// asker attaching while it stands, and the channel closed when the last asker detaches, which
// is what turns the wait in liveAsker.Ask into the errNoAsker the runner already records as a
// no_asker denial. There is no second way to write that decision.
type standingQuestion struct {
	req     protocol.PermissionRequested
	abandon chan struct{}
}

// newLive wraps a freshly opened, loaded or forked session. It must be called before the
// session is shared with any other goroutine: reading sess.Entries() here is what seeds the
// mirror, and that read is only safe while this caller is still the sole owner.
func newLive(sess *session.Session, m provider.Model) *liveSession {
	return &liveSession{
		sess:      sess,
		model:     m,
		entries:   append([]session.Entry(nil), sess.Entries()...),
		pending:   map[string]chan turn.Answer{},
		asking:    map[askKey]*standingAsk{},
		overrides: map[string][]session.Block{},
		children:  map[string]bool{},
		standing:  map[string]standingQuestion{},
		answered:  map[string]bool{},
	}
}

// mirroredState returns the runner's last-observed state and turn id. Self-locking (takes
// obsMu); safe to call whether or not the caller already holds mu (obsMu nests inside mu, never
// the reverse - see the liveSession doc).
func (ls *liveSession) mirroredState() (turn.State, string) {
	ls.obsMu.Lock()
	defer ls.obsMu.Unlock()
	return ls.state, ls.turnID
}

// markStarting sets the mirrored state to Streaming, and the mirrored turn id when it is
// already known, before the runner has run far enough to report either itself through
// StateChanged. Caller holds mu and is about to install (or has just installed) ls.runner and
// call spawnTurn; call this in that same critical section, before releasing mu, so that once
// mu is released, "ls.runner != nil" and "isActive(mirroredState)" are true atomically from
// any concurrent reader's point of view. Without it, a handler landing in the gap between mu
// being released and the runner's own first StateChanged callback could see ls.runner != nil
// but an unrelated, stale mirrored state, pass the active-turn check, and append to the
// session concurrently with the runner. The runner's first real StateChanged overwrites both
// fields moments later; turnID is the id when the caller already knows it (a steer resume,
// which keeps the turn id it read from this same mirror) and empty for a fresh turn, whose id
// is not known until its first append (see firstAppendSignal). Empty clears the mirror rather
// than leaving the last turn's id behind: an attach landing in this window would otherwise be
// handed a turn.state naming a turn that has already finished (see subscribeLocked).
func (ls *liveSession) markStarting(turnID string) {
	ls.obsMu.Lock()
	ls.state = turn.Streaming
	ls.turnID = turnID
	ls.obsMu.Unlock()
}

// claimChild records that toolUseID is opening a child session and reports whether this
// caller is the first to do so: at most one child per tool call. Self-locking (takes mu, which
// nests outside obsMu; nothing else is taken here).
func (ls *liveSession) claimChild(toolUseID string) bool {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.children[toolUseID] {
		return false
	}
	ls.children[toolUseID] = true
	return true
}

// hookContextSuffixLocked is what the session_opened hooks added to this session's system
// prompt, ready to append to it: one blank line, then the contexts a blank line apart, or
// nothing at all when no handler returned any. Caller holds mu. hookContext itself needs no
// lock: it is written before the session is shared (see fireSessionOpened) and only read
// afterwards; the Locked name marks where startTurn reads it, in the critical section it
// already holds.
func (ls *liveSession) hookContextSuffixLocked() string {
	if len(ls.hookContext) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(ls.hookContext, "\n\n")
}

// askers is every connection a turn's permission questions go to: each subscriber whose hello
// declared asker, or, for a child session (whose only subscriber is the plugin that opened
// it), its parent's. That is the binding: a subagent's unsafe tool is answered by the humans
// already answering for the session that spawned it. Every one of them is asked and the first
// answer decides (ADR 0014). Self-locking (takes obsMu); the walk up the parent chain takes
// the ancestor's obsMu only after releasing this session's, never holding two at once, and
// the chain is only ever one link long (a child has no agent tool to open a grandchild with).
//
// A plugin connection is never an asker, whatever its hello said: the plugin that opened a
// child session is the one waiting on that child's answer, so routing the child's question
// back to it would park the question behind the tool call it is the answer to. handleHello
// refuses the claim at the door too; this is the second half of the same rule.
func (ls *liveSession) askers() []*conn {
	ls.obsMu.Lock()
	out := ls.askersObsLocked()
	parent := ls.parent
	ls.obsMu.Unlock()
	if len(out) == 0 && parent != nil {
		return parent.askers()
	}
	return out
}

// askersObsLocked is askers for this session's own subscribers, without the walk up to the
// parent. Caller holds obsMu.
func (ls *liveSession) askersObsLocked() []*conn {
	var out []*conn
	for _, c := range ls.conns {
		if c.isAsker() {
			out = append(out, c)
		}
	}
	return out
}

// stand publishes q as one of this session's standing questions and returns the askers to put
// it to; none of them means nobody can answer and the caller denies instead. Recording it and
// snapshotting the subscribers in one obsMu section is what makes delivery exactly once
// against a concurrent attach: either the newcomer is already in conns and is notified below,
// or it subscribes afterwards and reads the question out of standing (see subscribeLocked).
// Self-locking (takes obsMu, then the parent's askers after releasing it, never both at once).
func (ls *liveSession) stand(q standingQuestion) []*conn {
	ls.obsMu.Lock()
	targets := ls.askersObsLocked()
	if len(targets) > 0 {
		ls.standing[q.req.ToolUseID] = q
	}
	parent := ls.parent
	ls.obsMu.Unlock()
	if len(targets) > 0 || parent == nil {
		return targets
	}
	// A child's question is answered by the parent's askers, but it stands on the child: an
	// asker attaching to the child itself is owed it too.
	targets = parent.askers()
	if len(targets) == 0 {
		return nil
	}
	ls.obsMu.Lock()
	ls.standing[q.req.ToolUseID] = q
	ls.obsMu.Unlock()
	return targets
}

// forget drops a question that is no longer standing, whether it was answered, abandoned or
// interrupted. Self-locking (takes mu, then obsMu inside it, the order the type documents).
func (ls *liveSession) forget(toolUseID string) {
	ls.mu.Lock()
	delete(ls.pending, toolUseID)
	ls.obsMu.Lock()
	delete(ls.standing, toolUseID)
	ls.obsMu.Unlock()
	ls.mu.Unlock()
}

// abandonStanding denies every question this session has standing once nobody is left to
// answer it, by closing the channel liveAsker.Ask waits on: the runner then records the one
// no_asker denial it already knows how to write. Called by detach with neither Server.mu nor
// ls.mu held, because the runner goroutine it wakes goes straight for both.
//
// Who could still answer is asked before obsMu is taken, since that walk takes the parent's
// obsMu and this must never hold two at once. An asker attaching in the gap is handed a
// question that is then denied: it sees the permission_decision moments later, which is the
// same thing it sees when the answer it was about to send loses the race to another asker.
// The parent's last asker leaving does not reach a child's standing question, which stays
// until the turn is interrupted; a child has no back pointer from the parent to walk down.
func (ls *liveSession) abandonStanding() {
	if len(ls.askers()) > 0 {
		return
	}
	ls.obsMu.Lock()
	abandoned := make([]chan struct{}, 0, len(ls.standing))
	for id, q := range ls.standing {
		abandoned = append(abandoned, q.abandon)
		delete(ls.standing, id)
	}
	ls.obsMu.Unlock()
	for _, ch := range abandoned {
		close(ch)
	}
}

// snapshotEntries returns a copy of the entries mirror. Self-locking (takes obsMu).
func (ls *liveSession) snapshotEntries() []session.Entry {
	ls.obsMu.Lock()
	defer ls.obsMu.Unlock()
	return append([]session.Entry(nil), ls.entries...)
}

// latestEntryID is the id of the newest mirrored entry of any of the given kinds, or "" when
// none matches. No kinds at all means match any kind: the newest entry of the session, the
// default a bare /fork forks at. Self-locking (takes obsMu).
func (ls *liveSession) latestEntryID(kinds ...session.Kind) string {
	ls.obsMu.Lock()
	defer ls.obsMu.Unlock()
	for _, e := range slices.Backward(ls.entries) {
		if len(kinds) == 0 || slices.Contains(kinds, e.Kind) {
			return e.ID.String()
		}
	}
	return ""
}

// broadcastObsLocked sends one notification to every current subscriber. Caller holds obsMu.
// conn.notify never blocks (it only enqueues), so this is safe to call from inside the
// turn.Runner's Observer callbacks, which run under the runner's own mutex and must never
// block or call back into the runner or take any lock but obsMu and a conn's own mu.
func (ls *liveSession) broadcastObsLocked(method string, params any) {
	for _, c := range ls.conns {
		c.notify(method, params)
	}
}

// attachment is everything a newly subscribed connection is owed, in the order the contract
// hands it over: every entry as entry.appended, then the current turn.state when a turn is
// active, then each standing permission.requested when the newcomer is an asker, and last the
// response carrying info.
type attachment struct {
	entries  []session.Entry
	state    *protocol.TurnStateChanged
	standing []protocol.PermissionRequested
	info     protocol.SessionInfo
}

// subscribeLocked registers cn as a subscriber and returns what it is owed: the entries to
// replay, the turn's current state and any standing question, and the info to reply with.
// Caller holds mu (from installAndAttach or attachIfLive, both of which hold Server.mu across
// the whole lookup-then-subscribe, which is what keeps this from ever racing detach's
// decide-and-remove into subscribing to a session that is concurrently being closed - see
// detach). Registering and reading standing in one obsMu section is the other half of stand's
// exactly-once: a question published before this runs is read out of standing here, and one
// published after finds cn already in conns and notifies it directly.
func (ls *liveSession) subscribeLocked(cn *conn) attachment {
	sid := ls.sess.ID()
	ls.obsMu.Lock()
	ls.conns = append(ls.conns, cn)
	at := attachment{
		entries: append([]session.Entry(nil), ls.entries...),
		info:    deriveInfo(sid, ls.entries),
	}
	// No turn id yet means the turn is between markStarting and the runner's first append,
	// so there is nothing truthful to name: the newcomer gets that first StateChanged as a
	// live notification a moment later instead.
	if isActive(ls.state) && ls.turnID != "" {
		at.state = &protocol.TurnStateChanged{SessionID: sid.String(), TurnID: ls.turnID, State: string(ls.state)}
	}
	if cn.isAsker() {
		for _, q := range ls.standing {
			at.standing = append(at.standing, q.req)
		}
		// Map order is not an order. Sorting by tool_use id makes what a newcomer hears
		// the same on every attach, which is what the order test can pin.
		slices.SortFunc(at.standing, func(a, b protocol.PermissionRequested) int {
			return strings.Compare(a.ToolUseID, b.ToolUseID)
		})
	}
	ls.obsMu.Unlock()

	cn.mu.Lock()
	cn.subs[sid] = ls
	cn.mu.Unlock()

	return at
}

// mirror records an entry appended to the session by something other than a turn and sends it
// to the subscribers, which is what the runner's Observer does for the entries a turn appends.
// Self-locking (it takes obsMu); safe whether or not the caller holds mu, which nests outside
// it. Every server-side append ends here, so the mirror and the broadcast can never disagree
// about what landed.
func (ls *liveSession) mirror(e session.Entry) {
	ls.obsMu.Lock()
	ls.entries = append(ls.entries, e)
	ls.broadcastObsLocked(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: ls.sess.ID().String(), Entry: e})
	ls.obsMu.Unlock()
}

// appendAndBroadcastLocked appends an entry the server produces directly. The caller must
// already have confirmed, under mu, that no turn is active: this is the one place outside a
// turn that calls sess.Append. Caller holds mu; mirror takes obsMu itself, consistent with the
// mu-outer, obsMu-inner order everywhere else.
func (ls *liveSession) appendAndBroadcastLocked(p session.Payload) (session.Entry, error) {
	e, err := ls.sess.Append(p)
	if err != nil {
		return session.Entry{}, err
	}
	ls.mirror(e)
	return e, nil
}

// appendNote appends a plugin's note and broadcasts it. Unlike appendAndBroadcastLocked it
// takes no mu and makes no active-turn check: a note is display only and never reaches a
// model, so it may land in the middle of a turn, which is exactly when a plugin has
// something to say. Session.Append is what makes that concurrent append safe.
func (ls *liveSession) appendNote(owner, text string, role session.NoteRole) (session.Entry, error) {
	e, err := ls.sess.Append(session.Note{Plugin: owner, Text: text, Role: role})
	if err != nil {
		return session.Entry{}, err
	}
	ls.mirror(e)
	return e, nil
}

// deriveInfo computes a SessionInfo by scanning entries the same way *session.Session's own
// accessors scan its log, so the two never disagree.
func deriveInfo(id ulid.ULID, entries []session.Entry) protocol.SessionInfo {
	info := protocol.SessionInfo{SessionID: id.String()}
	for _, e := range entries {
		switch p := e.Payload.(type) {
		case session.SessionOpened:
			info.Workspace = p.Workspace
			info.Model = p.Model
			info.Mode = p.Mode
			info.Thinking = p.Thinking
		case session.ModelChange:
			info.Model = p.Model
		case session.ModeChange:
			info.Mode = p.Mode
		case session.ThinkingChange:
			info.Thinking = p.Thinking
		case session.TitleChange:
			info.Title = p.Title
		}
	}
	return info
}

// closeIfOpen closes sess exactly once. It races safely against every other path that can
// also decide to close this session (detach reaching zero subscribers, Shutdown closing every
// live session): both go through the same claim, and whichever sets closed first is the one
// that actually fires before and calls sess.Close; the other is a no-op. Without that,
// Shutdown running concurrently with a connection's own detach (its Serve loop returning and
// unsubscribing the last connection) could call sess.Close twice on the same *session.Session,
// and would fire session_closed twice for one session.
func (ls *liveSession) closeIfOpen(before func()) error {
	ls.mu.Lock()
	already := ls.closed
	ls.closed = true
	ls.mu.Unlock()
	if already {
		return nil
	}
	return ls.closeClaimed(before)
}

// claimCloseIfIdle marks the session closed and reports whether this caller now owns closing
// it, refusing while a turn still owns the session or somebody else has already claimed it.
// detach uses it instead of closeIfOpen because it has to make that decision while still
// holding Server.mu, and do the closing itself after releasing it (see detach).
//
// The lock is tried, not taken: the caller holds Server.mu, and session.compact holds ls.mu
// across a whole provider request, so waiting here would stall every session lookup in the
// process for the length of a summary. A held ls.mu is read as busy, which is what it means:
// a compaction owns the session the way a turn does, and compact runs this again once it has
// let go, so a session whose last subscriber left mid-compaction still closes.
func (ls *liveSession) claimCloseIfIdle() bool {
	if !ls.mu.TryLock() {
		return false
	}
	defer ls.mu.Unlock()
	st, _ := ls.mirroredState()
	if ls.closed || (ls.runner != nil && isActive(st)) {
		return false
	}
	ls.closed = true
	return true
}

// closeClaimed runs before, then closes sess. The caller has already won the claim, so this
// body runs exactly once per session and before is exactly the once-per-session hook point.
func (ls *liveSession) closeClaimed(before func()) error {
	if before != nil {
		before()
	}
	return ls.sess.Close()
}

// isActive reports whether a turn.State currently owns the session.
func isActive(s turn.State) bool {
	switch s {
	case turn.Streaming, turn.RunningTool, turn.AwaitingPermission, turn.Steering:
		return true
	}
	return false
}

// fanout is the turn.Observer wired into every turn: it keeps a liveSession's entries and
// state mirrors in step with the runner and forwards every event to its subscribers. Its
// methods run while the Runner holds its own mutex (see turn.Runner.EntryAppended,
// StateChanged), so per the liveSession doc they take only obsMu, never mu, and never call
// back into the runner.
type fanout struct {
	ls  *liveSession
	sid string
}

func (f *fanout) EntryAppended(e session.Entry) {
	f.ls.obsMu.Lock()
	f.ls.entries = append(f.ls.entries, e)
	f.ls.broadcastObsLocked(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: f.sid, Entry: e})
	f.ls.obsMu.Unlock()
}

func (f *fanout) Delta(turnID string, p provider.Part) {
	f.ls.obsMu.Lock()
	f.ls.broadcastObsLocked(protocol.NotifyStreamDelta, protocol.StreamDelta{SessionID: f.sid, TurnID: turnID, Part: p})
	f.ls.obsMu.Unlock()
}

func (f *fanout) StateChanged(turnID string, s turn.State) {
	f.ls.obsMu.Lock()
	f.ls.state = s
	f.ls.turnID = turnID
	if !isActive(s) {
		// The answered set is per turn: once the turn is over, an answer naming one of its
		// tool_use ids is not a second answer to a live question, it is an answer to a
		// question nobody is asking, which is not_found rather than conflict.
		clear(f.ls.answered)
	}
	f.ls.broadcastObsLocked(protocol.NotifyTurnState, protocol.TurnStateChanged{SessionID: f.sid, TurnID: turnID, State: string(s)})
	f.ls.obsMu.Unlock()
}

// firstAppendSignal wraps an Observer so the first EntryAppended call also sends the entry,
// once, on started before delegating. turn.Runner.Run appends the user_message as its very
// first action, synchronously and before any provider call (see turn.Runner.Run), so this is
// how startTurn learns a fresh turn's id, the id of that entry, without waiting for the turn
// itself, which can run for as long as the provider and any tool calls take. started is
// buffered by one so the send here never blocks on nobody reading it yet. This is deliberately
// separate from the state mirror above (which StateChanged keeps current): it exists only to
// answer the one RPC call that started this turn, synchronously, with the id that call needs.
//
// A turn that reaches Failed without ever appending closes failed instead: that first append
// is exactly what can fail (a user_message the log refuses), and the runner cannot record a
// turn_failed for a turn whose id that same append was going to assign, so no entry would
// ever arrive and startTurn would wait forever. One sync.Once covers both channels, so a
// turn that started normally and failed later never also reports itself as never started.
type firstAppendSignal struct {
	turn.Observer
	once    sync.Once
	started chan session.Entry
	failed  chan struct{}
}

func (o *firstAppendSignal) EntryAppended(e session.Entry) {
	o.once.Do(func() { o.started <- e })
	o.Observer.EntryAppended(e)
}

func (o *firstAppendSignal) StateChanged(turnID string, s turn.State) {
	if s == turn.Failed {
		o.once.Do(func() { close(o.failed) })
	}
	o.Observer.StateChanged(turnID, s)
}

// askKey is the question a call would put to the operator. Two calls with the same matcher ask
// the same thing, whatever their tool_use ids are.
type askKey struct{ tool, prefix string }

// standingAsk is one outstanding question and the answer the calls waiting on it will take.
// done is closed once ans and err are final; nothing reads them before that. waiting is how many
// calls are currently parked on it, the raiser included: it is what lets a caller tell that every
// concurrent call asking the same thing has arrived, before any of them has been answered.
type standingAsk struct {
	done    chan struct{}
	ans     turn.Answer
	err     error
	waiting int
}

// liveAsker puts one turn's permission questions to every asker connection the session has
// (see askers for the binding, the child sessions included) and waits for the first
// session.answer to resolve each of them by tool_use id.
type liveAsker struct {
	ls     *liveSession
	sid    string
	runner *turn.Runner
}

// Ask is called from every goroutine running a tool call in the turn, so it may be re-entered
// while an earlier question stands. Calls that ask the same thing share one question: the
// operator sees one prompt and every call waiting on it when it is answered takes that answer.
// A call that arrives after a question resolves asks again, unless the answer was session
// scope, in which case the allowance exists and the Gate never asks.
func (a *liveAsker) Ask(ctx context.Context, q turn.Question) (turn.Answer, error) {
	key := askKey{tool: q.Matcher.Tool, prefix: q.Matcher.Prefix}
	a.ls.mu.Lock()
	if st, ok := a.ls.asking[key]; ok {
		st.waiting++
		a.ls.mu.Unlock()
		select {
		case <-st.done:
			return st.ans, st.err
		case <-ctx.Done():
			// This call is going away; the question stands for whoever else is waiting.
			a.ls.mu.Lock()
			st.waiting--
			a.ls.mu.Unlock()
			return turn.Answer{}, ctx.Err()
		}
	}
	st := &standingAsk{done: make(chan struct{}), waiting: 1}
	a.ls.asking[key] = st
	a.ls.mu.Unlock()

	ans, err := a.ask(ctx, q)

	a.ls.mu.Lock()
	delete(a.ls.asking, key)
	a.ls.mu.Unlock()
	st.ans, st.err = ans, err
	close(st.done)
	return ans, err
}

// ask puts one question to the operator and waits for its answer: the whole of what Ask did
// before calls could coalesce, unchanged, and now the path only the call that raises a question
// takes. It runs on the caller's own goroutine, with no turn.Runner or liveSession lock held
// (see turn.Runner.runTool): calling a.runner.TurnID() here is safe and gives the freshest
// value, unlike a handler that already holds mu or obsMu, which must use the mirror instead.
func (a *liveAsker) ask(ctx context.Context, q turn.Question) (turn.Answer, error) {
	// TurnID before any lock is taken: calling a Runner method under one is what the
	// liveSession doc rules out, and here no lock is held, so it is also the freshest value.
	req := protocol.PermissionRequested{
		SessionID: a.sid, TurnID: a.runner.TurnID(), ToolUseID: q.ToolUseID, Tool: q.Tool, Input: q.Input, Matcher: q.Matcher,
	}
	ch := make(chan turn.Answer, 1)
	abandon := make(chan struct{})
	a.ls.mu.Lock()
	a.ls.pending[q.ToolUseID] = ch
	a.ls.mu.Unlock()
	askers := a.ls.stand(standingQuestion{req: req, abandon: abandon})
	if len(askers) == 0 {
		a.ls.forget(q.ToolUseID)
		return turn.Answer{}, errNoAsker
	}
	for _, c := range askers {
		c.notify(protocol.NotifyPermissionRequested, req)
	}
	select {
	case ans := <-ch:
		// Drop it here too, not only in the handler that sent the answer: that handler can
		// run between the pending channel being published above and stand publishing the
		// question, in which case it deleted a standing entry that did not exist yet, and
		// the question would stand, already decided, for the rest of the turn.
		a.ls.forget(q.ToolUseID)
		return ans, nil
	case <-abandon:
		// The last asker detached while this stood. Same return as never having had one,
		// so the runner records the same no_asker denial rather than the server growing a
		// second path that writes a permission_decision of its own.
		a.ls.forget(q.ToolUseID)
		return turn.Answer{}, errNoAsker
	case <-ctx.Done():
		a.ls.forget(q.ToolUseID)
		return turn.Answer{}, ctx.Err()
	}
}
