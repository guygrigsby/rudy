package server

import (
	"context"
	"errors"
	"slices"
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
// session.Session carries no lock of its own; only one goroutine may touch it at a time. While
// a turn is running, that goroutine belongs to the turn.Runner the session was handed to in
// Config.Session, and the server must not call any *session.Session method concurrently with
// it. So every read the server needs while a turn might be active (replaying entries to a
// resuming connection, answering what session.set_model/set_mode/set_thinking/set_title need
// to know about the session's current title, model, mode, thinking and workspace) is served
// from entries, a mirror kept in step with the real log instead: seeded once from
// sess.Entries() at construction, before the liveSession is visible to anyone else, and from
// then on appended to only inside the fanout Observer (while a turn owns sess) or inside
// appendAndBroadcastLocked (when the server appends directly, which it only does once it has
// confirmed under mu that no turn is active). Every access to entries, conns, pending, runner
// and closed goes through mu.
type liveSession struct {
	mu      sync.Mutex
	sess    *session.Session
	model   provider.Model
	entries []session.Entry
	runner  *turn.Runner
	conns   []*conn
	pending map[string]chan turn.Answer
	closed  bool // sess has been closed and removed from Server.live; never touch sess again
}

// newLive wraps a freshly opened, loaded or forked session. It must be called before the
// session is shared with any other goroutine: reading sess.Entries() here is what seeds the
// mirror, and that read is only safe while this caller is still the sole owner.
func newLive(sess *session.Session, m provider.Model) *liveSession {
	return &liveSession{
		sess:    sess,
		model:   m,
		entries: append([]session.Entry(nil), sess.Entries()...),
		pending: map[string]chan turn.Answer{},
	}
}

// firstAskerLocked is the connection a turn's permission questions route to: the first
// subscriber whose hello declared asker, or nil when none has. Caller holds mu.
func (ls *liveSession) firstAskerLocked() *conn {
	for _, c := range ls.conns {
		if c.asker {
			return c
		}
	}
	return nil
}

// broadcastLocked sends one notification to every current subscriber. Caller holds mu.
// conn.notify never blocks (it only enqueues), so this is safe to call from inside the
// turn.Runner's Observer callbacks, which run under the runner's own mutex and must never
// block or call back into the runner.
func (ls *liveSession) broadcastLocked(method string, params any) {
	for _, c := range ls.conns {
		c.notify(method, params)
	}
}

// latestEntryIDLocked is the id of the newest mirrored entry of any of the given kinds, or ""
// when none matches. Caller holds mu.
func (ls *liveSession) latestEntryIDLocked(kinds ...session.Kind) string {
	for _, e := range slices.Backward(ls.entries) {
		if slices.Contains(kinds, e.Kind) {
			return e.ID.String()
		}
	}
	return ""
}

// appendAndBroadcastLocked appends an entry the server produces directly. The caller must
// already have confirmed, under mu, that no turn is active: this is the one place outside a
// turn that touches sess, and it is only safe because of that. It keeps the mirror in step and
// broadcasts the new entry. Caller holds mu.
func (ls *liveSession) appendAndBroadcastLocked(p session.Payload) (session.Entry, error) {
	e, err := ls.sess.Append(p)
	if err != nil {
		return session.Entry{}, err
	}
	ls.entries = append(ls.entries, e)
	ls.broadcastLocked(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: ls.sess.ID().String(), Entry: e})
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
// live session): both go through this, and whichever sets closed first is the one that
// actually calls sess.Close; the other is a no-op. Without that, Shutdown running concurrently
// with a connection's own detach (its Serve loop returning and unsubscribing the last
// connection) could call sess.Close twice on the same *session.Session.
func (ls *liveSession) closeIfOpen() error {
	ls.mu.Lock()
	already := ls.closed
	ls.closed = true
	ls.mu.Unlock()
	if already {
		return nil
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

// fanout is the turn.Observer wired into every turn: it keeps a liveSession's entries mirror
// in step with the session's real log and forwards every runner event to its subscribers. Its
// methods run while the Runner holds its own mutex (see turn.Runner.EntryAppended and
// StateChanged), so they must never block and must never call back into the runner; they only
// take ls.mu and enqueue onto each connection's own outbox.
type fanout struct {
	ls  *liveSession
	sid string
}

func (f *fanout) EntryAppended(e session.Entry) {
	f.ls.mu.Lock()
	f.ls.entries = append(f.ls.entries, e)
	f.ls.broadcastLocked(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: f.sid, Entry: e})
	f.ls.mu.Unlock()
}

func (f *fanout) Delta(turnID string, p provider.Part) {
	f.ls.mu.Lock()
	f.ls.broadcastLocked(protocol.NotifyStreamDelta, protocol.StreamDelta{SessionID: f.sid, TurnID: turnID, Part: p})
	f.ls.mu.Unlock()
}

func (f *fanout) StateChanged(turnID string, s turn.State) {
	f.ls.mu.Lock()
	f.ls.broadcastLocked(protocol.NotifyTurnState, protocol.TurnStateChanged{SessionID: f.sid, TurnID: turnID, State: string(s)})
	f.ls.mu.Unlock()
}

// firstAppendSignal wraps an Observer so the first EntryAppended call also sends the entry,
// once, on started before delegating. turn.Runner.Run appends the user_message as its very
// first action, synchronously and before any provider call (see turn.Runner.Run), so this is
// how startTurn learns a fresh turn's id, the id of that entry, without waiting for the turn
// itself, which can run for as long as the provider and any tool calls take. started is
// buffered by one so the send here never blocks on nobody reading it yet.
type firstAppendSignal struct {
	turn.Observer
	once    sync.Once
	started chan session.Entry
}

func (o *firstAppendSignal) EntryAppended(e session.Entry) {
	o.once.Do(func() { o.started <- e })
	o.Observer.EntryAppended(e)
}

// liveAsker routes one turn's permission questions to the session's first asker connection
// (the binding: "the asker is the first subscribed connection whose hello declared asker") and
// waits for session.answer to resolve them by tool_use id.
type liveAsker struct {
	ls     *liveSession
	sid    string
	runner *turn.Runner
}

func (a *liveAsker) Ask(ctx context.Context, q turn.Question) (turn.Answer, error) {
	ch := make(chan turn.Answer, 1)
	a.ls.mu.Lock()
	first := a.ls.firstAskerLocked()
	if first != nil {
		a.ls.pending[q.ToolUseID] = ch
	}
	a.ls.mu.Unlock()
	if first == nil {
		return turn.Answer{}, errNoAsker
	}
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
