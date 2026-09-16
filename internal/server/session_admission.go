package server

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/turn"
)

// sessionAdmission is one Session's admission fence: the single boundary every immediate read,
// mutation, append, attachment and detachment commit of that Session crosses, and the place its
// first durability cause is retained (ADR 0037).
//
// A separate availability check is not enough on its own. An attach, a plugin's Host.Note or a
// Turn append can pass such a check, pause, and commit after another path has already recorded
// a durability failure. Linearizing the commit with the cause is what makes the outcome one of
// two things and never a third: the operation commits before quarantine and is included in the
// cancellation and subscriber snapshot that quarantine takes, or it observes the cause and
// changes nothing.
//
// The fence is the innermost server lock but one. The order is Server.mu > liveSession.mu >
// this > liveSession.obsMu > conn.mu, so a fenced operation may take obsMu (the mirror and the
// broadcast) but must never reach for Server.mu or a liveSession's mu: an operation that needs
// either takes it first and enters the fence for its commit alone. Provider requests, hook
// handlers and transport waits stay outside entirely, and each later commit reenters.
type sessionAdmission struct {
	mu    sync.Mutex
	cause error // first durability cause; nil until quarantine

	// cancels is the work registered against this Session, by registration id: the context
	// of every turn running on it. Quarantine cancels each one. A turn registering after the
	// cause is installed is cancelled on the spot rather than recorded.
	cancels map[int]context.CancelFunc
	next    int
}

// installLocked records cause as the first one and, for the caller that installed it, returns
// the work and subscribers that transition owns. A later cause never replaces the first: the
// first is what the log's prefix actually became uncertain from, and the ones after it are its
// consequences. Caller holds mu, which is what puts the cause and the snapshot on the same side
// of every racing commit.
func (a *sessionAdmission) installLocked(ls *liveSession, cause error) (fired bool, conns []*conn, cancels []context.CancelFunc) {
	if a.cause != nil {
		return false, nil, nil
	}
	a.cause = cause
	for _, cancel := range a.cancels {
		cancels = append(cancels, cancel)
	}
	a.cancels = nil
	ls.obsMu.Lock()
	conns = append(conns, ls.conns...)
	ls.obsMu.Unlock()
	return true, conns, cancels
}

// admission returns sid's fence, creating it on first use. The map is never pruned: a Session's
// quarantine outlives its runtime, so detach, unload and reload cannot clear it and only a new
// Server process starts with an empty map (ADR 0037).
//
// admMu is an independent leaf. It guards this map alone, nothing is taken while it is held, and
// so it may be reached for under Server.mu, under a liveSession's mu or under no lock at all.
func (s *Server) admission(sid ulid.ULID) *sessionAdmission {
	s.admMu.Lock()
	defer s.admMu.Unlock()
	a, ok := s.admissions[sid]
	if !ok {
		a = &sessionAdmission{}
		s.admissions[sid] = a
	}
	return a
}

// admissionIfAny returns sid's fence only when one already exists, so a request naming a session
// id nobody has ever opened leaves nothing behind. The quarantine guards read through this;
// only a commit on a real Session creates a fence.
func (s *Server) admissionIfAny(sid ulid.ULID) *sessionAdmission {
	s.admMu.Lock()
	defer s.admMu.Unlock()
	return s.admissions[sid]
}

// withSessionOperation runs one immediate Session read, mutation, append or attachment commit
// under that Session's fence. A quarantined Session refuses before f runs at all; otherwise f
// commits, and a durability cause it returns is installed before the fence is released, so the
// next operation in line observes it rather than committing behind it.
//
// f does its own work and nothing else: no provider request, no hook, no wait on a transport,
// and nothing that reaches for Server.mu or a liveSession's mu (see sessionAdmission).
func (s *Server) withSessionOperation(ls *liveSession, f func() error) error {
	a := s.admission(ls.sess.ID())
	a.mu.Lock()
	if a.cause != nil {
		a.mu.Unlock()
		return turn.ErrTerminalDurability
	}
	err := f()
	var (
		fired   bool
		conns   []*conn
		cancels []context.CancelFunc
	)
	if err != nil && errors.Is(err, turn.ErrTerminalDurability) {
		fired, conns, cancels = a.installLocked(ls, err)
	}
	a.mu.Unlock()
	if fired {
		s.enforceQuarantine(ls, err, conns, cancels)
	}
	return err
}

// withSessionTeardown runs one detachment commit under the fence. It runs whatever the cause:
// a connection leaving is not work the Session grants, it is the record of a connection that is
// already gone, and refusing it would leave a quarantined session holding subscribers nobody is
// on the other end of. Taking the fence is still what orders it against the subscriber snapshot
// quarantine takes.
func (s *Server) withSessionTeardown(ls *liveSession, f func()) {
	a := s.admission(ls.sess.ID())
	a.mu.Lock()
	defer a.mu.Unlock()
	f()
}

// quarantine installs cause as this Session's first durability cause and, for the caller that
// installed it, cancels the Session's registered work and closes the subscribers captured with
// it. Every Server-owned path that learns of a durability failure outside a fenced commit,
// including the permission decision batch, comes through here; a fenced commit installs its own
// cause through withSessionOperation instead.
func (s *Server) quarantine(ls *liveSession, cause error) {
	a := s.admission(ls.sess.ID())
	a.mu.Lock()
	fired, conns, cancels := a.installLocked(ls, cause)
	a.mu.Unlock()
	if fired {
		s.enforceQuarantine(ls, cause, conns, cancels)
	}
}

// enforceQuarantine is what the transition owes once the cause is retained: the Session's
// remaining work is cancelled and every connection that held it at that instant is closed. The
// cause itself is logged here and never sent: a client is told the fixed text and nothing about
// the log underneath it.
func (s *Server) enforceQuarantine(ls *liveSession, cause error, conns []*conn, cancels []context.CancelFunc) {
	slog.Error("server: session quarantined", "session", ls.sess.ID(), "err", cause, "subscribers", len(conns), "cancelled", len(cancels))
	for _, cancel := range cancels {
		cancel()
	}
	for _, cn := range conns {
		slog.Warn("server: closing a quarantined session's subscriber", "session", ls.sess.ID(), "conn", cn.id)
		cn.closeNow()
	}
}

// sessionQuarantined is the refusal every later operation on a quarantined Session gets, and
// nil for every other session. The text is turn.ErrTerminalDurability's fixed one: the cause
// stays in the Server's log, where a client cannot read the path, the errno or the state of the
// file that failed.
func (s *Server) sessionQuarantined(sid ulid.ULID) *protocol.Error {
	a := s.admissionIfAny(sid)
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cause == nil {
		return nil
	}
	return perr(protocol.CodeUnavailable, turn.ErrTerminalDurability.Error())
}

// registerSessionWork registers cancel as work quarantine must end, and returns the release to
// call when that work is over. Work offered to an already quarantined Session is cancelled
// immediately: the cancellation snapshot has already been taken, and nothing may keep running
// on a log whose prefix is uncertain.
func (s *Server) registerSessionWork(ls *liveSession, cancel context.CancelFunc) (release func()) {
	a := s.admission(ls.sess.ID())
	a.mu.Lock()
	if a.cause != nil {
		a.mu.Unlock()
		cancel()
		return func() {}
	}
	a.next++
	id := a.next
	if a.cancels == nil {
		a.cancels = map[int]context.CancelFunc{}
	}
	a.cancels[id] = cancel
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		delete(a.cancels, id)
		a.mu.Unlock()
	}
}

// sessionCommit is the fenced commit a turn.Runner and its Compactor are given, so a Turn or
// Gate append lands through the same boundary a protocol call does (turn.Config.Commit).
func (s *Server) sessionCommit(ls *liveSession) func(func() error) error {
	return func(f func() error) error { return s.withSessionOperation(ls, f) }
}

// appendEntry is one server-produced append through the fence: the commit every protocol
// mutation of a live session ends in. The caller has already confirmed under ls.mu that no
// turn is active, exactly as liveSession.appendAndBroadcastLocked requires.
func (s *Server) appendEntry(ls *liveSession, p session.Payload) (session.Entry, error) {
	var e session.Entry
	err := s.withSessionOperation(ls, func() error {
		var aerr error
		e, aerr = ls.appendAndBroadcastLocked(p)
		return aerr
	})
	return e, err
}

// sessionErr maps what a fenced Session operation returned to the protocol error a client
// gets. A durability refusal is unavailable carrying the fixed text and nothing else;
// every other failure keeps the mapping it already had.
func sessionErr(err error) *protocol.Error {
	if errors.Is(err, turn.ErrTerminalDurability) {
		return perr(protocol.CodeUnavailable, turn.ErrTerminalDurability.Error())
	}
	return protocol.ErrorFrom(err)
}

// terminalTurnDurability reports whether every hook the terminal_turn_durability_v1 guarantee
// names is installed on this Server: the store its terminal Entry syncs to, and the admission
// fence that retains the first cause, cancels the Session's work and closes its subscribers.
// The other two halves of the guarantee, the sync before a terminal state publishes and the
// error that propagates from it, are the Runner's and are covered by the tests that hold this
// name to its meaning. A Server missing any of it advertises nothing rather than a promise an
// edge adapter would then rely on (ADR 0033, ADR 0037).
func (s *Server) terminalTurnDurability() bool {
	return s.d.Store != nil && s.admissions != nil
}

// capabilities is the closed internal vocabulary this Server implements, sorted and
// duplicate-free, as client.hello answers it. A name appears only once the whole invariant
// behind it is installed; a version string never implies one.
func (s *Server) capabilities() []string {
	out := []string{}
	if s.terminalTurnDurability() {
		out = append(out, protocol.CapabilityTerminalTurnDurabilityV1)
	}
	slices.Sort(out)
	return out
}
