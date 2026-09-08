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
type Server struct {
	d      Deps
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup // running turns; Shutdown waits on this before closing sessions

	mu           sync.Mutex
	live         map[ulid.ULID]*liveSession
	nextID       int
	shuttingDown bool
}

// New wires a Server. Deps must already be fully populated.
func New(d Deps) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{d: d, ctx: ctx, cancel: cancel, live: map[ulid.ULID]*liveSession{}}
}

// Serve runs one connection until it closes. client.hello must be its first request.
// Notifications for a session reach every connection that opened, resumed or forked it, in the
// order they were produced. Permission questions go to the first such connection whose hello
// declared asker; none means no asker.
func (s *Server) Serve(ctx context.Context, c protocol.Conn) error {
	s.mu.Lock()
	s.nextID++
	cn := newConn(s.nextID, c)
	s.mu.Unlock()

	pumpCtx, stopPump := context.WithCancel(ctx)
	go cn.pump(pumpCtx)
	defer func() {
		s.detachAll(cn)
		stopPump()
	}()

	for {
		raw, err := c.Recv(ctx)
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
	}
}

// Shutdown cancels every running turn and waits for it to record its turn_interrupted entry
// (or run to completion) before closing every live session, or until ctx ends, whichever comes
// first.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.shuttingDown = true
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
		if err := ls.closeIfOpen(); err != nil {
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
		return s.handleSubmit(req.Params)
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
	default:
		return nil, perr(protocol.CodeNotFound, "unknown method "+req.Method)
	}
}

func (s *Server) handleHello(cn *conn, raw json.RawMessage) (any, *protocol.Error) {
	var p protocol.ClientHelloParams
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	if cn.hello {
		return nil, perr(protocol.CodeRefusedByInvariant, "hello already received")
	}
	cn.hello = true
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

func (s *Server) handleSubmit(raw json.RawMessage) (any, *protocol.Error) {
	var p protocol.SessionSubmitParams
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	ls, e := s.lookup(p.SessionID)
	if e != nil {
		return nil, e
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
	tid, e := s.startTurn(ls, session.UserMessage{Source: p.Source, Content: p.Content})
	if e != nil {
		return nil, e
	}
	return protocol.SessionSubmitResult{TurnID: tid}, nil
}

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
	if ls.runner != nil && isActive(ls.runner.State()) {
		return nil, perr(protocol.CodeConflict, "a turn is active")
	}
	view := deriveInfo(ls.sess.ID(), ls.entries)
	if m.Ref == view.Model {
		return EntryIDResult{EntryID: ls.latestEntryIDLocked(session.KindModelChange, session.KindSessionOpened)}, nil
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
	ls.mu.Lock()
	call := plugin.CommandCall{SessionID: ls.sess.ID(), Workspace: deriveInfo(ls.sess.ID(), ls.entries).Workspace, Args: p.Args}
	ls.mu.Unlock()
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
	if ls.runner != nil && isActive(ls.runner.State()) {
		return nil, perr(protocol.CodeConflict, "a turn is active")
	}
	view := deriveInfo(ls.sess.ID(), ls.entries)
	same, p := f(view)
	if same {
		return EntryIDResult{EntryID: ls.latestEntryIDLocked(kind, session.KindSessionOpened)}, nil
	}
	e2, err := ls.appendAndBroadcastLocked(p)
	if err != nil {
		return nil, protocol.ErrorFrom(err)
	}
	return EntryIDResult{EntryID: e2.ID.String()}, nil
}

func (s *Server) open(cn *conn, p protocol.SessionOpenParams) (any, *protocol.Error) {
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
	s.mu.Lock()
	s.live[sess.ID()] = ls
	s.mu.Unlock()
	return s.attach(cn, ls), nil
}

func (s *Server) resume(cn *conn, p protocol.SessionResumeParams) (any, *protocol.Error) {
	sid, err := ulid.Parse(p.SessionID)
	if err != nil {
		return nil, perr(protocol.CodeInvalidArgument, "bad session id")
	}
	s.mu.Lock()
	ls, ok := s.live[sid]
	s.mu.Unlock()
	if !ok {
		sess, lerr := session.Load(s.d.Store, sid)
		if lerr != nil {
			return nil, loadErr(lerr)
		}
		m, rerr := s.d.Registry.Resolve(sess.Model().String())
		if rerr != nil {
			m = provider.Model{Ref: sess.Model()}
			cn.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "warn", Text: "model not in registry: " + sess.Model().String()})
		}
		ls = newLive(sess, m)
		s.mu.Lock()
		if existing, raced := s.live[sid]; raced {
			ls = existing
			_ = sess.Close()
		} else {
			s.live[sid] = ls
		}
		s.mu.Unlock()
	}
	return s.attach(cn, ls), nil
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
	s.mu.Lock()
	parent, live := s.live[sid]
	s.mu.Unlock()

	var child *session.Session
	if live {
		parent.mu.Lock()
		switch {
		case parent.closed:
			parent.mu.Unlock()
			live = false
		case parent.runner != nil && isActive(parent.runner.State()):
			parent.mu.Unlock()
			return nil, perr(protocol.CodeConflict, "a turn is active")
		default:
			child, err = parent.sess.Fork(s.d.Store, at)
			parent.mu.Unlock()
		}
	}
	if !live {
		loaded, lerr := session.Load(s.d.Store, sid)
		if lerr != nil {
			return nil, loadErr(lerr)
		}
		child, err = loaded.Fork(s.d.Store, at)
		_ = loaded.Close()
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
	ls := newLive(child, m)
	s.mu.Lock()
	s.live[child.ID()] = ls
	s.mu.Unlock()
	return s.attach(cn, ls), nil
}

// attach subscribes cn to ls and replays every entry known so far. It registers cn before
// releasing ls.mu, in the same critical section that snapshots the entries to replay, so no
// entry appended concurrently (by a running turn or another connection's mutation) is ever
// missed or delivered twice: anything appended before the snapshot is in it, and anything
// appended after cn was registered reaches cn through the normal broadcast path.
func (s *Server) attach(cn *conn, ls *liveSession) protocol.SessionInfo {
	ls.mu.Lock()
	ls.conns = append(ls.conns, cn)
	entries := append([]session.Entry(nil), ls.entries...)
	info := deriveInfo(ls.sess.ID(), ls.entries)
	ls.mu.Unlock()

	cn.mu.Lock()
	cn.subs[ls.sess.ID()] = ls
	cn.mu.Unlock()

	sid := ls.sess.ID().String()
	for _, e := range entries {
		cn.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: sid, Entry: e})
	}
	return info
}

// detach drops one subscription and, once nothing else holds the session and no turn is
// running on it, removes it from the server and closes it.
func (s *Server) detach(cn *conn, ls *liveSession) {
	cn.mu.Lock()
	delete(cn.subs, ls.sess.ID())
	cn.mu.Unlock()

	ls.mu.Lock()
	kept := ls.conns[:0]
	for _, c := range ls.conns {
		if c != cn {
			kept = append(kept, c)
		}
	}
	ls.conns = kept
	active := ls.runner != nil && isActive(ls.runner.State())
	empty := len(ls.conns) == 0
	closed := ls.closed
	ls.mu.Unlock()
	if !empty || active || closed {
		return
	}

	s.mu.Lock()
	delete(s.live, ls.sess.ID())
	s.mu.Unlock()
	_ = ls.closeIfOpen()
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
// runner is Steering; otherwise it starts or resumes one in its own goroutine.
func (s *Server) startTurn(ls *liveSession, msg session.UserMessage) (string, *protocol.Error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.closed {
		return "", perr(protocol.CodeNotFound, "session closed")
	}
	if ls.runner != nil {
		st := ls.runner.State()
		if msg.Source == session.SourceSteer {
			if st != turn.Steering {
				return "", perr(protocol.CodeRefusedByInvariant, "session is not steering")
			}
			r := ls.runner
			if e := s.spawnTurn(ls, r, msg); e != nil {
				return "", e
			}
			return r.TurnID(), nil
		}
		if isActive(st) {
			return "", perr(protocol.CodeConflict, "a turn is active")
		}
	} else if msg.Source == session.SourceSteer {
		return "", perr(protocol.CodeRefusedByInvariant, "session is not steering")
	}
	prov, ok := s.d.Registry.Provider(ls.model.Ref.Provider)
	if !ok {
		return "", perr(protocol.CodeUnavailable, "provider not loaded: "+ls.model.Ref.Provider)
	}
	sid := ls.sess.ID().String()
	la := &liveAsker{ls: ls, sid: sid}
	var asker turn.Asker
	if ls.firstAskerLocked() != nil {
		asker = la
	}
	view := deriveInfo(ls.sess.ID(), ls.entries)
	first := &firstAppendSignal{Observer: &fanout{ls: ls, sid: sid}, started: make(chan session.Entry, 1)}
	r := turn.NewRunner(turn.Config{
		Session:   ls.sess,
		Provider:  prov,
		Model:     ls.model,
		Tools:     s.d.Plugins,
		Gate:      s.d.Gate,
		Asker:     asker,
		Observer:  first,
		System:    turn.SystemPrompt(view.Workspace, s.d.Version),
		MaxTokens: s.d.Config.MaxTokens,
	})
	la.runner = r
	ls.runner = r
	if e := s.spawnTurn(ls, r, msg); e != nil {
		ls.runner = nil // nobody is running it; undo the assignment above
		return "", e
	}
	// The turn id is the id of the user_message entry Run appends as its first action (see
	// turn.Runner.TurnID); wait for that append, not the whole turn, so this returns quickly
	// and correctly instead of racing runTurn's goroutine for r.TurnID().
	select {
	case e := <-first.started:
		return e.ID.String(), nil
	case <-s.ctx.Done():
		return "", perr(protocol.CodeUnavailable, "server is shutting down")
	}
}

// spawnTurn starts the goroutine that drives r.Run, tracked in s.wg so Shutdown can wait for
// it. Caller holds ls.mu. Checking shuttingDown and calling wg.Add together under s.mu is what
// keeps this from ever racing Shutdown's wg.Wait: Shutdown sets shuttingDown under s.mu before
// it can reach Wait, so any spawnTurn that observes shuttingDown false here is guaranteed to
// have its Add counted before that Wait runs.
func (s *Server) spawnTurn(ls *liveSession, r *turn.Runner, msg session.UserMessage) *protocol.Error {
	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		return perr(protocol.CodeUnavailable, "server is shutting down")
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go s.runTurn(ls, r, msg)
	return nil
}

func (s *Server) runTurn(ls *liveSession, r *turn.Runner, msg session.UserMessage) {
	defer s.wg.Done()
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	_ = r.Run(ctx, msg) // failures are already recorded as turn_failed entries by the runner
	ls.mu.Lock()
	if ls.runner == r && r.State() != turn.Steering {
		ls.runner = nil
	}
	ls.mu.Unlock()
}
