// Package server is the protocol server: it dispatches JSON-RPC requests over one or more
// connections, holds every open session live for the process, runs turns through turn.Runner,
// routes permission questions to asker connections and fans notifications out in order.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/turn"
	"github.com/guygrigsby/rudy/internal/workspace"
)

// Deps wires a Server to the rest of the kernel.
type Deps struct {
	Version  string
	Config   *config.Config
	Store    *session.Store
	Registry *provider.Registry
	Plugins  *plugin.Registry
	Gate     *gate.Gate
	Hooks    *plugin.HookRunner // nil means no hooks fire
}

// EntryIDResult answers session.set_model, set_mode, set_thinking and set_title.
type EntryIDResult struct {
	EntryID string `json:"entry_id"`
}

// InterruptResult answers session.interrupt.
type InterruptResult struct {
	TurnID string `json:"turn_id"`
	State  string `json:"state"`
}

// Server dispatches JSON-RPC requests, holds live sessions, runs turns through turn.Runner and
// fans notifications out to every connection subscribed to a session, in order.
//
// Lock order, strict and never reversed, across Server and liveSession: mu > (a
// liveSession's) mu > (that liveSession's) obsMu. wgMu is an independent leaf, only ever
// nested inside a liveSession's mu (see spawnTurn); cn.mu (per connection) is never nested
// with any of the others. See liveSession's own doc for why mu/obsMu are split at all.
type Server struct {
	d      Deps
	ctx    context.Context
	cancel context.CancelFunc

	wgMu         sync.Mutex // guards wg.Add against Shutdown's wg.Wait; see spawnTurn
	wg           sync.WaitGroup
	shuttingDown bool

	mu      sync.Mutex
	live    map[ulid.ULID]*liveSession
	loading map[ulid.ULID]chan struct{} // sids with a cold load in flight; see loadCold
	closing map[ulid.ULID]chan struct{} // sids detach is closing; see detach, loadCold
	conns   map[int]*conn               // every live connection, for the status and widget broadcasts
	nextID  int
}

// New wires a Server. Deps must already be fully populated.
func New(d Deps) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{d: d, ctx: ctx, cancel: cancel, live: map[ulid.ULID]*liveSession{}, conns: map[int]*conn{}}
}

// Serve runs one connection until it closes. client.hello must be its first request.
// Notifications for a session reach every connection that opened, resumed or forked it, in the
// order they were produced. Permission questions go to the first such connection whose hello
// declared asker; none means no asker.
func (s *Server) Serve(ctx context.Context, c protocol.Conn) error {
	return s.serveConn(ctx, c, "")
}

// servePlugin runs one connection whose caller class is plugin: it needs no hello (one
// arriving is answered normally) and may append notes and name a parent session. Only the
// server hands these out, through Host.Connect.
func (s *Server) servePlugin(ctx context.Context, c protocol.Conn, name string) error {
	return s.serveConn(ctx, c, name)
}

// serveConn sets a connection up, runs its loop and tears it down. name is the caller class:
// empty for a client, the plugin's name for a plugin connection.
func (s *Server) serveConn(ctx context.Context, c protocol.Conn, name string) error {
	// Tracked in the same WaitGroup as running turns, and registered under wgMu against
	// shuttingDown for the same reason spawnTurn is (see spawnTurn): a serve loop can be
	// dispatching a request against a live session at any moment, so Shutdown must not
	// close a session until every loop has returned. The Done below is deferred before the
	// detach, so it fires only after this connection has released its sessions.
	s.wgMu.Lock()
	if s.shuttingDown {
		s.wgMu.Unlock()
		return ErrShuttingDown
	}
	s.wg.Add(1)
	s.wgMu.Unlock()
	defer s.wg.Done()

	s.mu.Lock()
	s.nextID++
	cn := newConn(s.nextID, c)
	cn.plugin = name
	cn.hello = name != ""
	s.conns[cn.id] = cn
	s.mu.Unlock()

	pumpCtx, stopPump := context.WithCancel(ctx)
	go cn.pump(pumpCtx)
	defer func() {
		s.mu.Lock()
		delete(s.conns, cn.id)
		s.mu.Unlock()
		s.detachAll(cn)
		stopPump()
	}()
	return s.serve(ctx, cn)
}

func (s *Server) serve(ctx context.Context, cn *conn) error {
	for {
		raw, err := cn.c.Recv(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		var req protocol.Request
		if err := json.Unmarshal(raw, &req); err != nil || req.JSONRPC != protocol.Version || req.Method == "" {
			cn.send(protocol.NewErrorResponse(req.ID, perr(protocol.CodeInvalidArgument, "malformed request")))
			continue
		}
		if req.IsNotification() {
			continue // client-to-server notifications are not part of this plan
		}
		result, rerr := s.dispatch(ctx, cn, req)
		if rerr != nil {
			cn.send(protocol.NewErrorResponse(req.ID, rerr))
			continue
		}
		resp, merr := protocol.NewResponse(req.ID, result)
		if merr != nil {
			cn.send(protocol.NewErrorResponse(req.ID, perr(protocol.CodeInternal, merr.Error())))
			continue
		}
		cn.send(resp)
		if req.Method == protocol.MethodClientHello {
			// After the response is queued, never before: a client learns the server is
			// there and then, in the same ordered outbox, what the plugins are showing.
			s.sendConnectState(cn)
		}
	}
}

// ErrShuttingDown is returned by Serve for a connection offered after Shutdown has begun.
var ErrShuttingDown = errors.New("server: shutting down")

// Shutdown cancels every running turn and waits for it to record its turn_interrupted entry
// (or run to completion), and for every Serve loop to return, before closing every live
// session, or until ctx ends, whichever comes first. Both waits matter: a turn still writing
// and a connection still dispatching requests can each be using a session, and closing one out
// from under either loses entries or fails an in-flight call. A Serve loop ends when its own
// context ends or its client disconnects, neither of which Shutdown controls, so ctx is the
// caller's bound on how long that is worth waiting for.
func (s *Server) Shutdown(ctx context.Context) error {
	// Set shuttingDown before touching wg.Wait below: spawnTurn checks it and calls wg.Add
	// together under the same wgMu, so any spawnTurn call that could still race this is
	// guaranteed to either see shuttingDown already true (and refuse) or have its Add counted
	// before the Wait below runs. See spawnTurn.
	s.wgMu.Lock()
	s.shuttingDown = true
	s.wgMu.Unlock()

	s.mu.Lock()
	lives := make([]*liveSession, 0, len(s.live))
	for _, ls := range s.live {
		lives = append(lives, ls)
	}
	s.live = map[ulid.ULID]*liveSession{}
	s.mu.Unlock()

	s.cancel() // unwinds every in-flight turn: see runTurn and turn.Runner.Run's ctx handling

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}

	var errs []error
	for _, ls := range lives {
		// A fresh context for the closing hooks rather than ctx: ctx is the caller's bound
		// on waiting for turns and Serve loops above and may already be done, and an expired
		// context would skip every handler instead of running it.
		hookCtx, cancelHooks := s.hookContext()
		err := ls.closeIfOpen(func() { s.fireSessionClosed(hookCtx, ls) })
		cancelHooks()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func perr(code int, msg string) *protocol.Error {
	return protocol.NewError(code, msg, nil)
}

// loadErr maps a session.Load failure to the protocol taxonomy through protocol.ErrorFrom,
// which is what gives session.ErrLocked (another process holds this session) CodeUnavailable.
// Load's other realistic failure, a session id with no log at all, does not wrap any sentinel
// ErrorFrom recognizes and falls through to its generic CodeInternal; that case is remapped
// here to CodeNotFound, since a resume or fork of a session that never existed is exactly that.
func loadErr(err error) *protocol.Error {
	pe := protocol.ErrorFrom(err)
	if pe.Code == protocol.CodeInternal {
		return perr(protocol.CodeNotFound, err.Error())
	}
	return pe
}

func decode(raw json.RawMessage, into any) *protocol.Error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return perr(protocol.CodeInvalidArgument, err.Error())
	}
	return nil
}

func (s *Server) dispatch(ctx context.Context, cn *conn, req protocol.Request) (any, *protocol.Error) {
	if req.Method == protocol.MethodClientHello {
		return s.handleHello(cn, req.Params)
	}
	if !cn.hello {
		return nil, perr(protocol.CodeUnauthorized, "client.hello must be the first request")
	}
	switch req.Method {
	case protocol.MethodSessionOpen:
		var p protocol.SessionOpenParams
		if e := decode(req.Params, &p); e != nil {
			return nil, e
		}
		return s.open(cn, p)
	case protocol.MethodSessionResume:
		var p protocol.SessionResumeParams
		if e := decode(req.Params, &p); e != nil {
			return nil, e
		}
		return s.resume(cn, p)
	case protocol.MethodSessionFork:
		var p protocol.SessionForkParams
		if e := decode(req.Params, &p); e != nil {
			return nil, e
		}
		return s.fork(cn, p)
	case protocol.MethodSessionList:
		sums, err := s.d.Store.List()
		if err != nil {
			return nil, protocol.ErrorFrom(err)
		}
		return protocol.SessionListResult{Sessions: sums}, nil
	case protocol.MethodSessionClose:
		return s.handleClose(cn, req.Params)
	case protocol.MethodSessionSubmit:
		return s.handleSubmit(cn, req.Params)
	case protocol.MethodSessionInterrupt:
		return s.handleInterrupt(req.Params)
	case protocol.MethodSessionAnswer:
		return s.handleAnswer(cn, req.Params)
	case protocol.MethodSessionSetModel:
		return s.handleSetModel(req.Params)
	case protocol.MethodSessionSetMode:
		return s.handleSetMode(req.Params)
	case protocol.MethodSessionSetThinking:
		return s.handleSetThinking(req.Params)
	case protocol.MethodSessionSetTitle:
		return s.handleSetTitle(req.Params)
	case protocol.MethodRegistryList:
		return protocol.RegistryListResult{Models: s.d.Registry.Models()}, nil
	case protocol.MethodRegistryRefresh:
		if err := s.d.Registry.Refresh(ctx); err != nil {
			cn.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "warn", Text: "registry refresh: " + err.Error()})
		}
		return protocol.RegistryListResult{Models: s.d.Registry.Models()}, nil
	case protocol.MethodCommandRun:
		return s.handleCommandRun(ctx, cn, req.Params)
	case protocol.MethodPluginAppendNote:
		return s.handleAppendNote(cn, req.Params)
	default:
		return nil, perr(protocol.CodeNotFound, "unknown method "+req.Method)
	}
}

func (s *Server) handleHello(cn *conn, raw json.RawMessage) (any, *protocol.Error) {
	var p protocol.ClientHelloParams
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	if cn.greeted {
		return nil, perr(protocol.CodeRefusedByInvariant, "hello already received")
	}
	cn.hello = true
	cn.greeted = true
	cn.asker = p.Asker
	return protocol.ClientHelloResult{Server: "rudy", Version: s.d.Version}, nil
}

func (s *Server) handleClose(cn *conn, raw json.RawMessage) (any, *protocol.Error) {
	var p protocol.SessionCloseParams
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	ls, e := s.lookup(p.SessionID)
	if e != nil {
		return nil, e
	}
	s.detach(cn, ls)
	return struct{}{}, nil
}

// handleSubmit starts a turn. A plugin connection may only submit to a session it opened or
// attached itself: driving somebody else's session is not what the plugin caller class is
// for, and a child session (Task 5) is exactly a session the plugin does hold.
func (s *Server) handleSubmit(cn *conn, raw json.RawMessage) (any, *protocol.Error) {
	var p protocol.SessionSubmitParams
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	ls, e := s.lookup(p.SessionID)
	if e != nil {
		return nil, e
	}
	if cn.plugin != "" && !cn.subscribed(ls.sess.ID()) {
		return nil, perr(protocol.CodeUnauthorized, "plugin may only submit to sessions it opened")
	}
	if len(p.Content) == 0 {
		return nil, perr(protocol.CodeInvalidArgument, "empty content")
	}
	if p.Source == "" {
		p.Source = session.SourceTyped
	}
	if p.Source != session.SourceTyped && p.Source != session.SourceSteer {
		return nil, perr(protocol.CodeInvalidArgument, "source must be typed or steer")
	}
	msg := session.UserMessage{Source: p.Source, Content: p.Content}
	// Validate here rather than letting the runner discover it: a message the log refuses
	// makes the runner's first append fail, and it cannot record a turn_failed for a turn
	// whose id that append was going to assign. The caller gets the refusal instead.
	if err := session.Validate(msg); err != nil {
		return nil, perr(protocol.CodeInvalidArgument, err.Error())
	}
	tid, e := s.startTurn(ls, msg)
	if e != nil {
		return nil, e
	}
	return protocol.SessionSubmitResult{TurnID: tid}, nil
}

// handleInterrupt never holds ls.mu or ls.obsMu while calling into r: it reads ls.runner once
// under mu, releases it, and only then calls State/Interrupt/TurnID directly on the runner
// (safe and freshest, since no liveSession lock is held during those calls - see liveSession's
// doc).
func (s *Server) handleInterrupt(raw json.RawMessage) (any, *protocol.Error) {
	var p protocol.SessionInterruptParams
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	ls, e := s.lookup(p.SessionID)
	if e != nil {
		return nil, e
	}
	if !p.How.Valid() {
		return nil, perr(protocol.CodeInvalidArgument, "how must be steer or cancel")
	}
	ls.mu.Lock()
	r := ls.runner
	ls.mu.Unlock()
	if r == nil || !isActive(r.State()) {
		return nil, perr(protocol.CodeRefusedByInvariant, "no active turn")
	}
	r.Interrupt(p.How)
	return InterruptResult{TurnID: r.TurnID(), State: string(r.State())}, nil
}

func (s *Server) handleAnswer(cn *conn, raw json.RawMessage) (any, *protocol.Error) {
	var p protocol.SessionAnswerParams
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	if cn.plugin != "" {
		return nil, perr(protocol.CodeUnauthorized, "plugins cannot answer permission questions")
	}
	if !cn.asker {
		return nil, perr(protocol.CodeUnauthorized, "connection did not declare asker")
	}
	ls, e := s.lookup(p.SessionID)
	if e != nil {
		return nil, e
	}
	if !p.Decision.Valid() || !p.Scope.Valid() {
		return nil, perr(protocol.CodeInvalidArgument, "invalid decision or scope")
	}
	ls.mu.Lock()
	ch, ok := ls.pending[p.ToolUseID]
	delete(ls.pending, p.ToolUseID)
	ls.mu.Unlock()
	if !ok {
		return nil, perr(protocol.CodeNotFound, "no pending question for "+p.ToolUseID)
	}
	ch <- turn.Answer{Decision: p.Decision, Scope: p.Scope, Reason: p.Reason}
	return struct{}{}, nil
}

func (s *Server) handleSetModel(raw json.RawMessage) (any, *protocol.Error) {
	var p protocol.SessionSetModelParams
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	ls, e := s.lookup(p.SessionID)
	if e != nil {
		return nil, e
	}
	m, err := s.d.Registry.Resolve(p.Model)
	if err != nil {
		return nil, perr(protocol.CodeNotFound, err.Error())
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.closed {
		return nil, perr(protocol.CodeNotFound, "session closed")
	}
	st, _ := ls.mirroredState()
	if ls.runner != nil && isActive(st) {
		return nil, perr(protocol.CodeConflict, "a turn is active")
	}
	view := deriveInfo(ls.sess.ID(), ls.snapshotEntries())
	if m.Ref == view.Model {
		return EntryIDResult{EntryID: ls.latestEntryID(session.KindModelChange, session.KindSessionOpened)}, nil
	}
	e2, err := ls.appendAndBroadcastLocked(session.ModelChange{Model: m.Ref})
	if err != nil {
		return nil, protocol.ErrorFrom(err)
	}
	ls.model = m
	return EntryIDResult{EntryID: e2.ID.String()}, nil
}

func (s *Server) handleSetMode(raw json.RawMessage) (any, *protocol.Error) {
	var p protocol.SessionSetModeParams
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	if !p.Mode.Valid() {
		return nil, perr(protocol.CodeInvalidArgument, "invalid mode")
	}
	return s.setEntry(p.SessionID, session.KindModeChange, func(view protocol.SessionInfo) (bool, session.Payload) {
		return view.Mode == p.Mode, session.ModeChange{Mode: p.Mode}
	})
}

func (s *Server) handleSetThinking(raw json.RawMessage) (any, *protocol.Error) {
	var p protocol.SessionSetThinkingParams
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	if !p.Thinking.Valid() {
		return nil, perr(protocol.CodeInvalidArgument, "invalid thinking level")
	}
	return s.setEntry(p.SessionID, session.KindThinkingChange, func(view protocol.SessionInfo) (bool, session.Payload) {
		return view.Thinking == p.Thinking, session.ThinkingChange{Thinking: p.Thinking}
	})
}

func (s *Server) handleSetTitle(raw json.RawMessage) (any, *protocol.Error) {
	var p protocol.SessionSetTitleParams
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	if p.Title == "" {
		return nil, perr(protocol.CodeInvalidArgument, "empty title")
	}
	return s.setEntry(p.SessionID, session.KindTitleChange, func(view protocol.SessionInfo) (bool, session.Payload) {
		return view.Title == p.Title, session.TitleChange{Title: p.Title}
	})
}

// handleAppendNote is the plugin caller class's one write into a session log. A client
// connection has no business asserting a note came from a plugin, so it is refused before
// the params are even decoded.
func (s *Server) handleAppendNote(cn *conn, raw json.RawMessage) (any, *protocol.Error) {
	if cn.plugin == "" {
		return nil, perr(protocol.CodeUnauthorized, "plugin.append_note is for plugins")
	}
	var p protocol.PluginAppendNoteParams
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	sid, err := ulid.Parse(p.SessionID)
	if err != nil {
		return nil, perr(protocol.CodeInvalidArgument, "bad session id")
	}
	e, aerr := s.appendNote(sid, cn.plugin, p.Text, p.Role)
	switch {
	case errors.Is(aerr, errSessionNotOpen):
		return nil, perr(protocol.CodeNotFound, aerr.Error())
	case aerr != nil:
		return nil, protocol.ErrorFrom(aerr)
	}
	return EntryIDResult{EntryID: e.ID.String()}, nil
}

func (s *Server) handleCommandRun(ctx context.Context, cn *conn, raw json.RawMessage) (any, *protocol.Error) {
	var p protocol.CommandRunParams
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	ls, e := s.lookup(p.SessionID)
	if e != nil {
		return nil, e
	}
	cmd, ok := s.d.Plugins.Command(p.Name)
	if !ok {
		return nil, perr(protocol.CodeNotFound, "unknown command /"+p.Name)
	}
	call := plugin.CommandCall{SessionID: ls.sess.ID(), Workspace: deriveInfo(ls.sess.ID(), ls.snapshotEntries()).Workspace, Args: p.Args}
	act, err := cmd.Run(ctx, call)
	if err != nil {
		return nil, perr(protocol.CodePluginError, err.Error())
	}
	switch a := act.(type) {
	case plugin.SubmitPrompt:
		tid, e := s.startTurn(ls, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock(a.Text)}})
		if e != nil {
			return nil, e
		}
		return protocol.CommandRunResult{TurnID: tid}, nil
	case plugin.Notice:
		cn.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "info", Text: a.Text})
		return protocol.CommandRunResult{Notice: a.Text}, nil
	default:
		return protocol.CommandRunResult{}, nil
	}
}

func (s *Server) lookup(id string) (*liveSession, *protocol.Error) {
	sid, err := ulid.Parse(id)
	if err != nil {
		return nil, perr(protocol.CodeInvalidArgument, "bad session id")
	}
	s.mu.Lock()
	ls, ok := s.live[sid]
	s.mu.Unlock()
	if !ok {
		return nil, perr(protocol.CodeNotFound, "session not open: "+id)
	}
	return ls, nil
}

// setEntry is the common shape of session.set_mode, session.set_thinking and
// session.set_title: refuse while closed or while a turn is active, otherwise let f decide
// from the current view whether anything changes and, when it does, append and broadcast it.
func (s *Server) setEntry(id string, kind session.Kind, f func(view protocol.SessionInfo) (same bool, p session.Payload)) (any, *protocol.Error) {
	ls, e := s.lookup(id)
	if e != nil {
		return nil, e
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.closed {
		return nil, perr(protocol.CodeNotFound, "session closed")
	}
	st, _ := ls.mirroredState()
	if ls.runner != nil && isActive(st) {
		return nil, perr(protocol.CodeConflict, "a turn is active")
	}
	view := deriveInfo(ls.sess.ID(), ls.snapshotEntries())
	same, p := f(view)
	if same {
		return EntryIDResult{EntryID: ls.latestEntryID(kind, session.KindSessionOpened)}, nil
	}
	e2, err := ls.appendAndBroadcastLocked(p)
	if err != nil {
		return nil, protocol.ErrorFrom(err)
	}
	return EntryIDResult{EntryID: e2.ID.String()}, nil
}

func (s *Server) open(cn *conn, p protocol.SessionOpenParams) (any, *protocol.Error) {
	if p.Parent != nil {
		if cn.plugin == "" {
			return nil, perr(protocol.CodeInvalidArgument, "parent is for plugins")
		}
		return nil, perr(protocol.CodeNotFound, "child sessions arrive in Task 5")
	}
	ws, err := workspace.Detect(p.Cwd)
	if err != nil {
		return nil, perr(protocol.CodeInvalidArgument, err.Error())
	}
	spec := p.Model
	if spec == "" {
		spec = s.d.Config.Default.Provider + ":" + s.d.Config.Default.Model
	}
	m, err := s.d.Registry.Resolve(spec)
	if err != nil {
		return nil, perr(protocol.CodeNotFound, err.Error())
	}
	mode := session.Mode(p.Mode)
	if mode == "" {
		mode = session.Mode(s.d.Config.Permissions.Mode)
	}
	thinking := session.ThinkingLevel(p.Thinking)
	if thinking == "" {
		thinking = session.ThinkingLevel(s.d.Config.Default.Thinking)
	}
	if !mode.Valid() || !thinking.Valid() {
		return nil, perr(protocol.CodeInvalidArgument, "invalid mode or thinking level")
	}
	agent := p.Agent
	if agent == "" {
		agent = "default"
	}
	sess, err := session.Open(s.d.Store, session.SessionOpened{
		SchemaVersion: 1, RudyVersion: s.d.Version, Workspace: ws, Model: m.Ref, Thinking: thinking, Mode: mode, Agent: agent,
	})
	if err != nil {
		return nil, protocol.ErrorFrom(err)
	}
	ls := newLive(sess, m)
	s.fireSessionOpened(s.ctx, ls, false)
	return s.installAndAttach(cn, ls), nil
}

// resume attaches cn to sid, loading it from disk first when it is not already live. loadCold
// single-flights concurrent cold loads of the same id (see loadCold); attachIfLive then
// subscribes atomically with the s.live lookup (see detach for why that matters).
func (s *Server) resume(cn *conn, p protocol.SessionResumeParams) (any, *protocol.Error) {
	sid, err := ulid.Parse(p.SessionID)
	if err != nil {
		return nil, perr(protocol.CodeInvalidArgument, "bad session id")
	}
	if _, lerr := s.loadCold(cn, sid); lerr != nil {
		return nil, lerr
	}
	info, ok := s.attachIfLive(cn, sid)
	if !ok {
		// Vanishingly rare: the session was detached and closed by someone else between
		// loadCold returning and this attach (or the server is shutting down). Ask the client
		// to retry rather than looping here.
		return nil, perr(protocol.CodeUnavailable, "session unavailable, retry")
	}
	return info, nil
}

func (s *Server) fork(cn *conn, p protocol.SessionForkParams) (any, *protocol.Error) {
	sid, err := ulid.Parse(p.SessionID)
	if err != nil {
		return nil, perr(protocol.CodeInvalidArgument, "bad session id")
	}
	at, err := ulid.Parse(p.AtEntryID)
	if err != nil {
		return nil, perr(protocol.CodeInvalidArgument, "bad entry id")
	}
	parent, lerr := s.loadCold(cn, sid)
	if lerr != nil {
		return nil, lerr
	}

	parent.mu.Lock()
	st, _ := parent.mirroredState()
	active := parent.runner != nil && isActive(st)
	closed := parent.closed
	var child *session.Session
	switch {
	case closed:
		parent.mu.Unlock()
		return nil, perr(protocol.CodeUnavailable, "session unavailable, retry")
	case active:
		parent.mu.Unlock()
		return nil, perr(protocol.CodeConflict, "a turn is active")
	default:
		child, err = parent.sess.Fork(s.d.Store, at)
		parent.mu.Unlock()
	}
	if err != nil {
		if errors.Is(err, session.ErrInvariant) {
			return nil, perr(protocol.CodeNotFound, err.Error())
		}
		return nil, protocol.ErrorFrom(err)
	}
	m, rerr := s.d.Registry.Resolve(child.Model().String())
	if rerr != nil {
		m = provider.Model{Ref: child.Model()}
	}
	return s.installAndAttach(cn, newLive(child, m)), nil
}

// loadCold returns the live session for sid, loading it from disk first if it is not already
// live. Concurrent cold loads for the same id single-flight through s.loading: session.Store's
// flock is exclusive across file descriptors within one process, not just across processes, so
// a second concurrent session.Load for the same id fails with ErrLocked instead of blocking.
// Without single-flighting, two connections resuming (or forking from) the same cold session at
// once would race, and the loser would see a spurious "locked" error instead of the same live
// session the winner produced.
//
// It also waits out a concurrent detach that is still closing this same id (see s.closing,
// detach): detach removes a session from s.live before its own sess.Close has actually
// released the flock, so a cold load that only checked s.live could still lose to that flock,
// seeing session.ErrLocked for a session nobody has held live for a while.
func (s *Server) loadCold(cn *conn, sid ulid.ULID) (*liveSession, *protocol.Error) {
	for {
		s.mu.Lock()
		if ls, ok := s.live[sid]; ok {
			s.mu.Unlock()
			return ls, nil
		}
		if ch, ok := s.loading[sid]; ok {
			s.mu.Unlock()
			<-ch
			continue // the winner has installed it into s.live, or failed; check s.live again
		}
		if ch, ok := s.closing[sid]; ok {
			s.mu.Unlock()
			<-ch
			continue // detach's Close has released the flock now; re-check from the top, since
			// the session may have become live again through another path in the meantime
		}
		ch := make(chan struct{})
		if s.loading == nil {
			s.loading = map[ulid.ULID]chan struct{}{}
		}
		s.loading[sid] = ch
		s.mu.Unlock()

		ls, lerr := s.coldLoadOne(cn, sid)

		s.mu.Lock()
		delete(s.loading, sid)
		s.mu.Unlock()
		close(ch)
		return ls, lerr
	}
}

// coldLoadOne does the actual disk load and installs the result into s.live. Called with no
// lock held. Only the single goroutine loadCold lets through for a given sid at a time ever
// calls this for that sid, so the final install needs no raced-insert check.
func (s *Server) coldLoadOne(cn *conn, sid ulid.ULID) (*liveSession, *protocol.Error) {
	sess, err := session.Load(s.d.Store, sid)
	if err != nil {
		return nil, loadErr(err)
	}
	m, rerr := s.d.Registry.Resolve(sess.Model().String())
	if rerr != nil {
		m = provider.Model{Ref: sess.Model()}
		cn.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "warn", Text: "model not in registry: " + sess.Model().String()})
	}
	ls := newLive(sess, m)
	// Every path that brings a session back from disk goes through here, so this is where a
	// resumed session gets its session_opened, once, whether the caller was resume or fork.
	// Before the install, not after: loadCold checks s.live before s.loading, so a session
	// already in s.live is one another connection can attach to and submit a turn on, and
	// that turn would assemble its system prompt without a context still being computed.
	s.fireSessionOpened(s.ctx, ls, true)
	s.mu.Lock()
	s.live[sid] = ls
	s.mu.Unlock()
	return ls, nil
}

// hookContext bounds one set of hook handlers for a caller that has no request context to
// use. Every handler is bounded again, individually, by the HookRunner's own timeout.
func (s *Server) hookContext() (context.Context, context.CancelFunc) {
	d := time.Duration(s.d.Config.HookTimeoutMS) * time.Millisecond
	if d <= 0 {
		d = plugin.DefaultHookTimeout
	}
	return context.WithTimeout(context.Background(), d)
}

// hookFirer is s.d.Hooks as the interface the turn loop takes, or nil when no runner is
// wired. The explicit nil matters: a nil *plugin.HookRunner assigned straight into the
// interface field would be non-nil to the runner's own "are there hooks" check and panic on
// the first fire.
func (s *Server) hookFirer() turn.HookFirer {
	if s.d.Hooks == nil {
		return nil
	}
	return s.d.Hooks
}

// fireSessionOpened runs the session_opened hook for a session that is about to go live and
// keeps every context it returns for that session's system prompt. Callers must not have
// shared ls yet (open before installAndAttach, coldLoadOne before the s.live install), which
// is what makes the unlocked write to hookContext safe: nothing else can reach ls until the
// install publishes it under Server.mu, and every later reader takes that lock to find it.
// Firing first is also the point, not an optimization: a session already visible is one
// another connection can submit a turn on, and that turn would build its system prompt while
// the handlers deciding what belongs in it are still running.
func (s *Server) fireSessionOpened(ctx context.Context, ls *liveSession, resumed bool) {
	if s.d.Hooks == nil {
		return
	}
	view := deriveInfo(ls.sess.ID(), ls.entries)
	p := &plugin.SessionOpenedPayload{
		SessionID: view.SessionID,
		Workspace: view.Workspace,
		Model:     view.Model,
		Mode:      view.Mode,
		Thinking:  view.Thinking,
		Resumed:   resumed,
	}
	for _, res := range s.d.Hooks.Fire(ctx, plugin.HookCall{Point: plugin.HookSessionOpened, SessionID: p.SessionID, Payload: p}) {
		if so, ok := res.(*plugin.SessionOpenedResult); ok && so.Context != "" {
			ls.hookContext = append(ls.hookContext, so.Context)
		}
	}
}

// fireSessionClosed runs the session_closed hook for a session about to be closed. It runs
// from inside closeClaimed, so it fires exactly once per session no matter which of detach and
// Shutdown won the claim, and with no lock held: it waits on handlers.
func (s *Server) fireSessionClosed(ctx context.Context, ls *liveSession) {
	if s.d.Hooks == nil {
		return
	}
	sid := ls.sess.ID().String()
	s.d.Hooks.Fire(ctx, plugin.HookCall{Point: plugin.HookSessionClosed, SessionID: sid, Payload: &plugin.SessionClosedPayload{SessionID: sid}})
}

// installAndAttach installs a freshly created (never-before-shared) liveSession into s.live and
// subscribes cn to it in one critical section. Used by open, fork's child and resume's cold
// load. Safe unconditionally: the session is not yet visible to any other goroutine before this
// call, so there is no id to race.
func (s *Server) installAndAttach(cn *conn, ls *liveSession) protocol.SessionInfo {
	s.mu.Lock()
	s.live[ls.sess.ID()] = ls
	entries, info := ls.subscribeLocked(cn)
	s.mu.Unlock()
	replay(cn, ls.sess.ID(), entries)
	return info
}

// attachIfLive subscribes cn to sid's live session, atomically with the s.live lookup: holding
// Server.mu across both is what keeps this from ever racing detach's decide-and-remove into
// subscribing to a session that is concurrently being closed (see detach). ok is false when
// sid is not currently live.
func (s *Server) attachIfLive(cn *conn, sid ulid.ULID) (protocol.SessionInfo, bool) {
	s.mu.Lock()
	ls, ok := s.live[sid]
	if !ok {
		s.mu.Unlock()
		return protocol.SessionInfo{}, false
	}
	entries, info := ls.subscribeLocked(cn)
	s.mu.Unlock()
	replay(cn, sid, entries)
	return info, true
}

func replay(cn *conn, sid ulid.ULID, entries []session.Entry) {
	sidStr := sid.String()
	for _, e := range entries {
		cn.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: sidStr, Entry: e})
	}
}

// detach drops one subscription and, once nothing else holds the session and no turn is
// running on it, removes it from the server and closes it. It holds Server.mu across the whole
// decide-then-remove sequence, the same lock attachIfLive and installAndAttach hold across
// their lookup-then-subscribe, so the two can never interleave: either a concurrent attach
// registers its connection before this reads conns (so this correctly sees "not empty" and
// leaves the session alone), or this removes the session from s.live before that attach's
// lookup can find it (so that attach correctly falls through to a cold reload instead of
// subscribing to a session about to be closed out from under it).
func (s *Server) detach(cn *conn, ls *liveSession) {
	cn.mu.Lock()
	delete(cn.subs, ls.sess.ID())
	cn.mu.Unlock()

	s.mu.Lock()

	ls.obsMu.Lock()
	kept := ls.conns[:0]
	for _, c := range ls.conns {
		if c != cn {
			kept = append(kept, c)
		}
	}
	ls.conns = kept
	empty := len(ls.conns) == 0
	ls.obsMu.Unlock()
	if !empty {
		s.mu.Unlock()
		return
	}

	if !ls.claimCloseIfIdle() {
		s.mu.Unlock()
		return
	}

	// The delete is what has to happen before releasing mu: it is what stops a concurrent
	// attachIfLive from finding this session again. The actual Close, once that is done, no
	// longer needs mu - nothing can reach ls through s.live to race it, and the claim above
	// (ls.closed, set under ls.mu by claimCloseIfIdle) is what stops any other path
	// (Shutdown's closeIfOpen) from also closing it, or from firing session_closed a second
	// time for the same session. But sess.Close still has to run (releasing the store's flock) before a cold
	// load for this same id can safely call session.Load again, so register it in s.closing in
	// this same critical section, before releasing mu: a concurrent loadCold checks s.closing
	// right alongside s.live and s.loading (see loadCold) and waits instead of racing the
	// flock.
	id := ls.sess.ID()
	closingCh := make(chan struct{})
	if s.closing == nil {
		s.closing = map[ulid.ULID]chan struct{}{}
	}
	s.closing[id] = closingCh
	delete(s.live, id)
	s.mu.Unlock()

	_ = ls.closeClaimed(func() { s.fireSessionClosed(s.ctx, ls) })

	s.mu.Lock()
	delete(s.closing, id)
	close(closingCh)
	s.mu.Unlock()
}

func (s *Server) detachAll(cn *conn) {
	cn.mu.Lock()
	subs := make([]*liveSession, 0, len(cn.subs))
	for _, ls := range cn.subs {
		subs = append(subs, ls)
	}
	cn.mu.Unlock()
	for _, ls := range subs {
		s.detach(cn, ls)
	}
}

// startTurn refuses a typed submit while a turn is active and a steer submit unless the
// runner is Steering; otherwise it starts or resumes one in its own goroutine. It never holds
// ls.mu while calling a turn.Runner method or while waiting on first.started: both would risk
// the AB-BA deadlock documented on liveSession.
func (s *Server) startTurn(ls *liveSession, msg session.UserMessage) (string, *protocol.Error) {
	ls.mu.Lock()
	if ls.closed {
		ls.mu.Unlock()
		return "", perr(protocol.CodeNotFound, "session closed")
	}
	if ls.runner != nil {
		st, _ := ls.mirroredState()
		if msg.Source == session.SourceSteer {
			if st != turn.Steering {
				ls.mu.Unlock()
				return "", perr(protocol.CodeRefusedByInvariant, "session is not steering")
			}
			r := ls.runner
			// state is already Steering (isActive) here, but stamp it explicitly anyway,
			// same as the fresh-turn path below: it is what makes "ls.runner != nil and
			// isActive(mirror)" hold atomically the instant mu is released, rather than
			// relying on the reader also knowing Steering was never touched in between.
			// turnID is already correct (a steer resume keeps the same turn id), so leave it.
			ls.markStarting("")
			ls.mu.Unlock()
			if e := s.spawnTurn(ls, r, msg); e != nil {
				return "", e
			}
			return r.TurnID(), nil // no lock held here; direct call is fine and freshest
		}
		if isActive(st) {
			ls.mu.Unlock()
			return "", perr(protocol.CodeConflict, "a turn is active")
		}
	} else if msg.Source == session.SourceSteer {
		ls.mu.Unlock()
		return "", perr(protocol.CodeRefusedByInvariant, "session is not steering")
	}
	prov, ok := s.d.Registry.Provider(ls.model.Ref.Provider)
	if !ok {
		ls.mu.Unlock()
		return "", perr(protocol.CodeUnavailable, "provider not loaded: "+ls.model.Ref.Provider)
	}
	sid := ls.sess.ID().String()
	la := &liveAsker{ls: ls, sid: sid}
	var asker turn.Asker
	if ls.firstAsker() != nil {
		asker = la
	}
	view := deriveInfo(ls.sess.ID(), ls.snapshotEntries())
	first := &firstAppendSignal{Observer: &fanout{ls: ls, sid: sid}, started: make(chan session.Entry, 1), failed: make(chan struct{})}
	r := turn.NewRunner(turn.Config{
		Session:  ls.sess,
		Provider: prov,
		Model:    ls.model,
		Tools:    s.d.Plugins,
		Gate:     s.d.Gate,
		Asker:    asker,
		Observer: first,
		// hookContext is guarded by ls.mu, which this still holds.
		System:      turn.SystemPrompt(view.Workspace, s.d.Version) + ls.hookContextSuffixLocked(),
		MaxTokens:   s.d.Config.MaxTokens,
		Hooks:       s.hookFirer(),
		ToolTimeout: time.Duration(s.d.Config.ToolTimeoutMS) * time.Millisecond,
		Overrides:   ls.overrides,
	})
	la.runner = r
	ls.runner = r
	// Mark active before releasing mu: the turn id isn't known yet (Run hasn't appended the
	// user_message that defines it - see firstAppendSignal below), but the state must be, or a
	// set_title/set_mode/steer submit landing between here and the runner's first
	// StateChanged callback would see ls.runner != nil and a stale, inactive mirrored state,
	// pass the active-turn check, and append to the session concurrently with the runner.
	ls.markStarting("")
	ls.mu.Unlock()

	if e := s.spawnTurn(ls, r, msg); e != nil {
		ls.mu.Lock()
		if ls.runner == r {
			ls.runner = nil // nobody is running it; undo the assignment above
		}
		ls.mu.Unlock()
		return "", e
	}
	// The turn id is the id of the user_message entry Run appends as its first action (see
	// turn.Runner.TurnID); wait for that append, not the whole turn, so this returns quickly
	// and correctly instead of racing runTurn's goroutine for r.TurnID(). ls.mu is not held
	// here: nothing in this wait needs it, and holding it would block every other handler for
	// this session for as long as the wait takes.
	select {
	case e := <-first.started:
		return e.ID.String(), nil
	case <-first.failed:
		// The runner failed before it appended anything, so there is no turn id to report
		// and no turn_failed entry naming one either (see firstAppendSignal). Answering
		// the caller is what matters; the runner has already logged what went wrong.
		return "", perr(protocol.CodeInternal, "the turn failed before it started")
	case <-s.ctx.Done():
		return "", perr(protocol.CodeUnavailable, "server is shutting down")
	}
}

// spawnTurn starts the goroutine that drives r.Run, tracked in s.wg so Shutdown can wait for
// it. Does not touch ls.mu or Server.mu, only wgMu: it may be called with ls.mu held (from
// startTurn) or not (it does not matter either way), and never nests under Server.mu, keeping
// wgMu out of the mu > ls.mu > ls.obsMu order entirely.
func (s *Server) spawnTurn(ls *liveSession, r *turn.Runner, msg session.UserMessage) *protocol.Error {
	s.wgMu.Lock()
	if s.shuttingDown {
		s.wgMu.Unlock()
		return perr(protocol.CodeUnavailable, "server is shutting down")
	}
	s.wg.Add(1)
	s.wgMu.Unlock()
	go s.runTurn(ls, r, msg)
	return nil
}

func (s *Server) runTurn(ls *liveSession, r *turn.Runner, msg session.UserMessage) {
	defer s.wg.Done()
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	_ = r.Run(ctx, msg) // failures are already recorded as turn_failed entries by the runner
	final := r.State()  // read before taking ls.mu: never call a Runner method while holding it
	ls.mu.Lock()
	if ls.runner == r && final != turn.Steering {
		ls.runner = nil
	}
	ls.mu.Unlock()
}
