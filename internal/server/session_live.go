package server

import (
	"context"
	"errors"
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
// declared asker.
var errNoAsker = errors.New("server: no asker attached")

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
//   - obsMu guards entries (the log mirror), conns (subscribers) and state/turnID (a mirror of
//     the runner's State/TurnID, updated from the Observer callbacks). It is the ONLY lock a
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
		overrides: map[string][]session.Block{},
		children:  map[string]bool{},
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
// fields moments later; turnID here matters only for a steer resume (where it is already known
// and unchanged) - pass "" for a fresh turn, whose id is not known until its first append (see
// firstAppendSignal).
func (ls *liveSession) markStarting(turnID string) {
	ls.obsMu.Lock()
	ls.state = turn.Streaming
	if turnID != "" {
		ls.turnID = turnID
	}
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

// firstAsker is the connection a turn's permission questions route to: the first subscriber
// whose hello declared asker, or, for a child session (whose only subscriber is the plugin
// that opened it), its parent's. That is the binding: a subagent's unsafe tool is answered by
// the human who is already answering for the session that spawned it. Self-locking (takes
// obsMu); the walk up the parent chain takes each ancestor's obsMu in turn, never holding two
// at once, and the chain is only ever one link long (a child has no agent tool to open a
// grandchild with).
func (ls *liveSession) firstAsker() *conn {
	ls.obsMu.Lock()
	for _, c := range ls.conns {
		if c.asker {
			ls.obsMu.Unlock()
			return c
		}
	}
	parent := ls.parent
	ls.obsMu.Unlock()
	if parent != nil {
		return parent.firstAsker()
	}
	return nil
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

// subscribeLocked registers cn as a subscriber and returns a snapshot of entries to replay
// plus the info to reply with. Caller holds mu (from installAndAttach or attachIfLive, both of
// which hold Server.mu across the whole lookup-then-subscribe, which is what keeps this from
// ever racing detach's decide-and-remove into subscribing to a session that is concurrently
// being closed - see detach).
func (ls *liveSession) subscribeLocked(cn *conn) ([]session.Entry, protocol.SessionInfo) {
	ls.obsMu.Lock()
	ls.conns = append(ls.conns, cn)
	entries := append([]session.Entry(nil), ls.entries...)
	info := deriveInfo(ls.sess.ID(), ls.entries)
	ls.obsMu.Unlock()

	cn.mu.Lock()
	cn.subs[ls.sess.ID()] = ls
	cn.mu.Unlock()

	return entries, info
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
func (ls *liveSession) claimCloseIfIdle() bool {
	ls.mu.Lock()
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

// liveAsker routes one turn's permission questions to the session's first asker connection
// (the binding: "the asker is the first subscribed connection whose hello declared asker") and
// waits for session.answer to resolve them by tool_use id.
type liveAsker struct {
	ls     *liveSession
	sid    string
	runner *turn.Runner
}

// Ask runs on the turn.Runner's own goroutine, between steps, with no turn.Runner or
// liveSession lock held (see turn.Runner.runTool): calling a.runner.TurnID() here is safe and
// gives the freshest value, unlike a handler that already holds mu or obsMu, which must use
// the mirror instead.
func (a *liveAsker) Ask(ctx context.Context, q turn.Question) (turn.Answer, error) {
	first := a.ls.firstAsker()
	if first == nil {
		return turn.Answer{}, errNoAsker
	}
	ch := make(chan turn.Answer, 1)
	a.ls.mu.Lock()
	a.ls.pending[q.ToolUseID] = ch
	a.ls.mu.Unlock()
	first.notify(protocol.NotifyPermissionRequested, protocol.PermissionRequested{
		SessionID: a.sid, TurnID: a.runner.TurnID(), ToolUseID: q.ToolUseID, Tool: q.Tool, Input: q.Input, Matcher: q.Matcher,
	})
	select {
	case ans := <-ch:
		return ans, nil
	case <-ctx.Done():
		a.ls.mu.Lock()
		delete(a.ls.pending, q.ToolUseID)
		a.ls.mu.Unlock()
		return turn.Answer{}, ctx.Err()
	}
}
