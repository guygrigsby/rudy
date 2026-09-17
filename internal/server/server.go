// SPDX-License-Identifier: AGPL-3.0-or-later

// Package server is the protocol server: it dispatches JSON-RPC requests over one or more
// connections, holds every open session live for the process, runs turns through turn.Runner,
// routes permission questions to asker connections and fans notifications out in order.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/agentdef"
	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
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
	Socket   string             // the socket a locked session's unavailable error names
	// Prompt is the system prompt template every session renders. Empty is the built-in
	// one; a file the operator wrote replaces it (ADR 0024). It is read once, when the
	// server is built, so a turn never waits on a disk read for it.
	Prompt string
	// Home is the home directory of the process running this server, answered in the hello.
	Home string
}

// EntryIDResult answers session.set_model, set_mode, set_thinking, set_title and
// session.compact.
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

	instanceID        ulid.ULID
	state             protocol.ServerState
	shutdownRequested chan struct{}
	shutdownComplete  chan struct{}
	shutdownFailed    chan struct{}
	shutdownOnce      sync.Once
	completeOnce      sync.Once
	failOnce          sync.Once

	wgMu              sync.Mutex // guards wg.Add against Shutdown's wg.Wait; see spawnTurn
	wg                sync.WaitGroup
	shuttingDown      bool
	shutdownClaimed   bool
	shutdownCommitted bool

	// pluginState orders what clients are told about plugin load state: one connection's
	// connect snapshot against every broadcast. It is taken on its own, never while mu or a
	// conn's lock is held, and only enqueues happen under it (see broadcastPluginState).
	pluginState sync.Mutex

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
	return &Server{
		d: d, ctx: ctx, cancel: cancel,
		instanceID: ulid.Make(), state: protocol.ServerStateRunning,
		shutdownRequested: make(chan struct{}), shutdownComplete: make(chan struct{}), shutdownFailed: make(chan struct{}),
		live: map[ulid.ULID]*liveSession{}, conns: map[int]*conn{},
	}
}

// Serve runs one connection until it closes. client.hello must be its first request.
// Notifications for a session reach every connection that opened, resumed or forked it, in the
// order they were produced. A permission question goes to every such connection whose hello
// declared asker, and to an asker that attaches while it stands; the first answer decides and
// every later one is conflict. None at the moment the question is asked means no asker, and a
// turn that started with none attached asks nobody for its whole life (rudy-xz9).
func (s *Server) Serve(ctx context.Context, c protocol.Conn) error {
	return s.serveConn(ctx, c, "", nil)
}

// servePlugin runs one connection whose caller class is plugin: it needs no hello (one
// arriving is answered normally) and may append notes and name a parent session. Only the
// server hands these out, through Host.Connect.
func (s *Server) servePlugin(ctx context.Context, c protocol.Conn, name string) error {
	return s.serveConn(ctx, c, name, nil)
}

// ServePlugin runs a spawned plugin's connection: servePlugin plus the adapter its
// registrations and notifications are applied to. The plugin adapter calls it with the peer
// it holds over the child's stdio, so a spawned plugin reaches the server through exactly the
// requests a linked plugin's Host.Connect client sends, and registers through the same
// registry. The name is the manifest's, taken from the Registrar rather than from anything
// the child says.
func (s *Server) ServePlugin(ctx context.Context, c protocol.Conn, reg plugin.Registrar) error {
	// The loop ends with the caller's context or with the server's, whichever comes first.
	// The server's matters: Shutdown waits for every serve loop to return before it closes a
	// session, and the context a plugin adapter loads under outlives the server (it is the
	// process's), so without this a spawned plugin's loop would keep Shutdown waiting for a
	// child that is only asked to exit afterwards.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(s.ctx, cancel)()
	return s.serveConn(ctx, c, reg.Name(), reg)
}

// serveConn sets a connection up, runs its loop and tears it down. name is the caller class:
// empty for a client, the plugin's name for a plugin connection.
func (s *Server) serveConn(ctx context.Context, c protocol.Conn, name string, reg plugin.Registrar) error {
	// Tracked in the same WaitGroup as running turns, and registered under wgMu against
	// shuttingDown for the same reason spawnTurn is (see spawnTurn): a serve loop can be
	// dispatching a request against a live session at any moment, so Shutdown must not
	// close a session until every loop has returned. The Done below happens after detach but
	// before a shutdown control connection waits for full process cleanup, so the request
	// that initiated shutdown cannot make Shutdown wait on itself.
	s.wgMu.Lock()
	if s.shuttingDown || s.shutdownClaimed {
		s.wgMu.Unlock()
		return ErrShuttingDown
	}
	s.wg.Add(1)
	s.wgMu.Unlock()
	s.mu.Lock()
	s.nextID++
	cn := newConn(s.nextID, c)
	cn.sameUser = protocol.IsSameUser(c)
	cn.plugin = name
	cn.reg = reg
	cn.hello = name != ""
	s.conns[cn.id] = cn
	s.mu.Unlock()
	slog.Info("server: conn open", "conn", cn.id, "plugin", name)

	pumpCtx, stopPump := context.WithCancel(context.Background())
	cn.abortPump = stopPump
	go cn.pump(pumpCtx)
	err := s.serve(ctx, cn)
	shutdownControl := errors.Is(err, ErrShutdownRequested)
	s.mu.Lock()
	delete(s.conns, cn.id)
	s.mu.Unlock()
	s.detachAll(cn)
	if !shutdownControl {
		stopPump()
		<-cn.pumpDone
		s.wg.Done()
		slog.Info("server: conn closed", "conn", cn.id, "plugin", name)
		return err
	}

	// The control loop no longer owns session state, so Shutdown must not wait on it. Its
	// writer stays alive until the process owner reports success or failure.
	s.wg.Done()
	select {
	case <-s.shutdownComplete:
		note, noteErr := protocol.NewNotification(protocol.NotifyServerStopped, protocol.ServerStoppedParams{
			InstanceID: s.instanceID.String(), State: protocol.ServerStateStopped,
		})
		if noteErr == nil {
			flushCtx, cancel := context.WithTimeout(context.Background(), shutdownResponseBudget)
			noteErr = cn.sendAndWait(flushCtx, note)
			cancel()
		}
		err = errors.Join(err, noteErr)
	case <-s.shutdownFailed:
	}
	stopPump()
	<-cn.pumpDone
	slog.Info("server: conn closed", "conn", cn.id, "plugin", name)
	return err
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
			// A spawned plugin's notifications (tool.progress, provider.delta) belong to the
			// adapter that owns its registrations. A client's are not part of this plan.
			if cn.reg != nil {
				cn.reg.Deliver(req.Method, req.Params)
			}
			continue
		}
		result, rerr := s.dispatch(ctx, cn, req)
		if rerr != nil {
			cn.send(protocol.NewErrorResponse(req.ID, rerr))
			continue
		}
		resp, merr := protocol.NewResponse(req.ID, result)
		if merr != nil {
			if req.Method == protocol.MethodServerShutdown {
				s.releaseShutdownClaim()
			}
			cn.send(protocol.NewErrorResponse(req.ID, perr(protocol.CodeInternal, merr.Error())))
			continue
		}
		if req.Method == protocol.MethodServerShutdown {
			flushCtx, cancel := context.WithTimeout(ctx, shutdownResponseBudget)
			err := cn.sendAndWait(flushCtx, resp)
			cancel()
			if err != nil {
				s.releaseShutdownClaim()
				return err
			}
			s.requestShutdown()
			return ErrShutdownRequested
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

// shutdownResponseBudget bounds the physical response flush. A requester that stops reading
// cannot hold the daemon open, and the process owner is not notified unless acceptance reached
// the peer.
const shutdownResponseBudget = 2 * time.Second

// ErrShutdownRequested tells the connection owner to keep this connection open until the
// process owner calls CompleteShutdown. It is a clean lifecycle transition, not a diagnostic.
var ErrShutdownRequested = errors.New("server: shutdown requested")

// ShutdownRequested closes when this Server begins its terminal transition. The process
// owner selects on it beside signals and parent context cancellation.
func (s *Server) ShutdownRequested() <-chan struct{} { return s.shutdownRequested }

// InstanceID is this process-lifetime Server identity.
func (s *Server) InstanceID() string { return s.instanceID.String() }

// State reports the Server lifecycle state.
func (s *Server) State() protocol.ServerState {
	s.wgMu.Lock()
	defer s.wgMu.Unlock()
	return s.state
}

// CompleteShutdown lets the acknowledged control connection send terminal proof after the
// process owner has closed every runtime resource, including its listener and plugins.
func (s *Server) CompleteShutdown() {
	s.wgMu.Lock()
	if s.state != protocol.ServerStateShuttingDown {
		s.wgMu.Unlock()
		return
	}
	s.wgMu.Unlock()
	s.completeOnce.Do(func() {
		s.wgMu.Lock()
		s.state = protocol.ServerStateStopped
		s.wgMu.Unlock()
		close(s.shutdownComplete)
	})
}

// FailShutdown releases a held shutdown control connection without the terminal success
// notification. Its resulting bare EOF is deliberately not proof to a client.
func (s *Server) FailShutdown() {
	s.failOnce.Do(func() { close(s.shutdownFailed) })
}

func (s *Server) claimShutdown() bool {
	s.wgMu.Lock()
	defer s.wgMu.Unlock()
	if s.shuttingDown || s.shutdownClaimed {
		return false
	}
	s.shutdownClaimed = true
	return true
}

func (s *Server) releaseShutdownClaim() {
	s.wgMu.Lock()
	if !s.shutdownCommitted {
		s.shutdownClaimed = false
	}
	s.wgMu.Unlock()
}

func (s *Server) requestShutdown() {
	s.wgMu.Lock()
	s.shuttingDown = true
	s.shutdownClaimed = true
	s.shutdownCommitted = true
	s.state = protocol.ServerStateShuttingDown
	s.wgMu.Unlock()
	s.shutdownOnce.Do(func() { close(s.shutdownRequested) })
}

func (s *Server) isShuttingDown() bool {
	s.wgMu.Lock()
	defer s.wgMu.Unlock()
	return s.shuttingDown || s.shutdownClaimed
}

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
	s.requestShutdown()

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
//
// ErrLocked gets one thing the generic mapping does not: the socket a second process should
// attach through instead of opening its own store on the same session. Every other
// CodeUnavailable stays data-less.
func loadErr(err error, socket string) *protocol.Error {
	if errors.Is(err, session.ErrLocked) {
		return protocol.NewError(protocol.CodeUnavailable, err.Error(), map[string]string{"socket": socket})
	}
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
	if s.isShuttingDown() {
		return nil, perr(protocol.CodeRefusedByInvariant, "server is shutting down")
	}
	if req.Method == protocol.MethodClientHello {
		return s.handleHello(cn, req.Params)
	}
	if !cn.hello {
		if req.Method == protocol.MethodServerShutdown {
			return nil, perr(protocol.CodeRefusedByInvariant, "client.hello must precede server.shutdown")
		}
		return nil, perr(protocol.CodeUnauthorized, "client.hello must be the first request")
	}
	if e := s.authorizePlugin(cn, req); e != nil {
		return nil, e
	}
	switch req.Method {
	case protocol.MethodServerShutdown:
		return s.handleServerShutdown(cn, req.Params)
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
		return s.handleInterrupt(cn, req.Params)
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
	case protocol.MethodSessionCompact:
		return s.handleCompact(ctx, req.Params)
	case protocol.MethodRegistryList:
		return protocol.RegistryListResult{Models: s.d.Registry.Models()}, nil
	case protocol.MethodRegistryRefresh:
		if err := s.d.Registry.Refresh(ctx); err != nil {
			cn.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "warn", Text: "registry refresh: " + err.Error()})
		}
		return protocol.RegistryListResult{Models: s.d.Registry.Models()}, nil
	case protocol.MethodSessionShell:
		return s.handleShell(ctx, req.Params)
	case protocol.MethodCommandList:
		return commandList(s.d.Plugins.Commands()), nil
	case protocol.MethodCommandRun:
		return s.handleCommandRun(ctx, cn, req.Params)
	case protocol.MethodPluginAppendNote:
		return s.handleAppendNote(cn, req.Params)
	case protocol.MethodPluginRegisterTool, protocol.MethodPluginRegisterCommand,
		protocol.MethodPluginRegisterHook, protocol.MethodPluginRegisterProvider,
		protocol.MethodPluginRegisterAgent, protocol.MethodPluginRegisterWidget,
		protocol.MethodPluginSetStatus:
		return s.handleRegister(cn, req)
	default:
		return nil, perr(protocol.CodeNotFound, "unknown method "+req.Method)
	}
}

// pluginMethods is the caller class table for the plugin class: everything a plugin may ask
// for. It is an allowlist rather than a list of refusals so that a method added later is
// refused until somebody decides a plugin may call it, instead of being granted by having
// been forgotten.
var pluginMethods = map[string]bool{
	protocol.MethodClientHello:            true,
	protocol.MethodSessionOpen:            true,
	protocol.MethodSessionSubmit:          true, // own sessions; handleSubmit checks, after its lookup
	protocol.MethodSessionInterrupt:       true,
	protocol.MethodSessionClose:           true,
	protocol.MethodSessionSetModel:        true,
	protocol.MethodSessionSetMode:         true,
	protocol.MethodSessionSetThinking:     true,
	protocol.MethodSessionSetTitle:        true,
	protocol.MethodSessionCompact:         true,
	protocol.MethodCommandRun:             true,
	protocol.MethodRegistryList:           true,
	protocol.MethodPluginAppendNote:       true,
	protocol.MethodPluginRegisterTool:     true,
	protocol.MethodPluginRegisterCommand:  true,
	protocol.MethodPluginRegisterHook:     true,
	protocol.MethodPluginRegisterWidget:   true,
	protocol.MethodPluginRegisterProvider: true,
	protocol.MethodPluginRegisterAgent:    true,
	protocol.MethodPluginSetStatus:        true,
}

// pluginOwnSession is the subset of pluginMethods a plugin may only aim at a session it
// opened. session.submit belongs here too by rule, but its check stays in handleSubmit, which
// answers not_found for a session that does not exist at all rather than unauthorized.
var pluginOwnSession = map[string]bool{
	protocol.MethodSessionInterrupt:   true,
	protocol.MethodSessionClose:       true,
	protocol.MethodSessionSetModel:    true,
	protocol.MethodSessionSetMode:     true,
	protocol.MethodSessionSetThinking: true,
	protocol.MethodSessionSetTitle:    true,
	protocol.MethodSessionCompact:     true,
	protocol.MethodCommandRun:         true,
}

// authorizePlugin applies that table. A plugin asserts things about itself and about the
// sessions it opened: it never lists, resumes or forks somebody else's session, never answers
// a permission question, and never refreshes the registry the whole process shares.
func (s *Server) authorizePlugin(cn *conn, req protocol.Request) *protocol.Error {
	if cn.plugin == "" {
		return nil
	}
	if !pluginMethods[req.Method] {
		return perr(protocol.CodeUnauthorized, "a plugin may not call "+req.Method)
	}
	if pluginOwnSession[req.Method] {
		return s.ownSession(cn, req)
	}
	return nil
}

// ownSession refuses a plugin naming a session it does not hold. The subscription is the
// proof: a plugin holds exactly the sessions it opened on its own connection.
func (s *Server) ownSession(cn *conn, req protocol.Request) *protocol.Error {
	var p struct {
		SessionID string `json:"session_id"`
	}
	if e := decode(req.Params, &p); e != nil {
		return e
	}
	sid, err := ulid.Parse(p.SessionID)
	if err != nil {
		return perr(protocol.CodeInvalidArgument, "bad session id")
	}
	if !cn.subscribed(sid) {
		return perr(protocol.CodeUnauthorized, "a plugin may only "+req.Method+" a session it opened")
	}
	return nil
}

// handleRegister applies one plugin.register_* or plugin.set_status. The name it registers
// under is the connection's, never anything in the params.
func (s *Server) handleRegister(cn *conn, req protocol.Request) (any, *protocol.Error) {
	if cn.plugin == "" {
		return nil, perr(protocol.CodeUnauthorized, req.Method+" is for plugins")
	}
	if cn.reg == nil {
		return nil, perr(protocol.CodeUnauthorized, "linked plugins register through the Host")
	}
	res, err := plugin.Register(cn.reg, req.Method, req.Params)
	if err != nil {
		return nil, registerErr(err)
	}
	return res, nil
}

// registerErr maps a registration failure: a name another plugin already owns is a conflict,
// everything else goes through the usual taxonomy.
func registerErr(err error) *protocol.Error {
	if errors.Is(err, plugin.ErrDuplicate) {
		return perr(protocol.CodeConflict, err.Error())
	}
	return protocol.ErrorFrom(err)
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
	// A plugin connection is never an asker, whatever it claims: the plugin that opens a
	// child session is the one waiting on that child's tool call, so routing that child's
	// permission question back to it would park the question behind its own answer. The
	// human's connection is the only one that can answer. Take the claim, then let the one
	// predicate decide whether it holds, so the door and every routing walk say the same
	// thing about who is an asker.
	cn.asker = p.Asker
	cn.asker = cn.isAsker()
	return protocol.ClientHelloResult{Server: "rudy", Version: s.d.Version, InstanceID: s.instanceID.String(), Home: s.d.Home}, nil
}

func (s *Server) handleServerShutdown(cn *conn, raw json.RawMessage) (any, *protocol.Error) {
	var p struct{}
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	if cn.plugin != "" || !cn.sameUser {
		return nil, perr(protocol.CodeUnauthorized, "server.shutdown requires a same-user unix socket connection")
	}
	if !s.claimShutdown() {
		return nil, perr(protocol.CodeRefusedByInvariant, "server shutdown already requested")
	}
	return protocol.ServerShutdownResult{InstanceID: s.instanceID.String(), State: protocol.ServerStateShuttingDown}, nil
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

// childRefusesUnsubscribed reports whether ls is a child session and cn, a plain client, has
// not itself subscribed to it: the gate session.submit and session.interrupt share. Watching a
// child through its parent's forwarded notifications (ADR 0028 decision 6) is deliberately not
// the same thing as being subscribed to it, so a parent's client that never resumed the child
// directly has no more standing to drive it than a stranger would. This uses openedAsChild on
// the log rather than ls.parent, the live link open sets and nothing else ever does: a child
// resumed cold, after a restart or after the session that spawned it has closed, has a nil
// parent and is still a child (rudy-review round 1 on task 6). A plugin connection is exempted
// here because its own submit and interrupt authority is already the stricter "only what it
// subscribed to", enforced separately (handleSubmit's own check; ownSession for interrupt via
// pluginOwnSession).
func childRefusesUnsubscribed(cn *conn, ls *liveSession) bool {
	return cn.plugin == "" && !cn.subscribed(ls.sess.ID()) && openedAsChild(ls.snapshotEntries())
}

// handleSubmit starts a turn. A plugin connection may only submit to a session it opened or
// attached itself: driving somebody else's session is not what the plugin caller class is
// for, and a child session is exactly a session the plugin does hold. An ordinary session
// outside a parent-child relationship keeps the looser rule it always had: any client that
// knows its id may submit to it, the same as session.interrupt (the broader gap that leaves is
// rudy-jkz, not this one).
func (s *Server) handleSubmit(cn *conn, raw json.RawMessage) (any, *protocol.Error) {
	var p protocol.SessionSubmitParams
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	ls, e := s.lookup(p.SessionID)
	if e != nil {
		return nil, e
	}
	switch {
	case cn.plugin != "" && !cn.subscribed(ls.sess.ID()):
		return nil, perr(protocol.CodeUnauthorized, "plugin may only submit to sessions it opened")
	case childRefusesUnsubscribed(cn, ls):
		return nil, perr(protocol.CodeUnauthorized, "only a session's own subscriber may submit to a child session")
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
// doc). A child session refuses a plain client that has not subscribed to it, the same gate
// session.submit applies: the routing this wave added is what makes a running child's id
// reachable by a client that only watches the parent, and watching must not double as standing
// to cancel it.
func (s *Server) handleInterrupt(cn *conn, raw json.RawMessage) (any, *protocol.Error) {
	var p protocol.SessionInterruptParams
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	ls, e := s.lookup(p.SessionID)
	if e != nil {
		return nil, e
	}
	if childRefusesUnsubscribed(cn, ls) {
		return nil, perr(protocol.CodeUnauthorized, "only a session's own subscriber may interrupt a child session")
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
	// The turn the question was asked in, as the notification carried it. Consent is given
	// for one call of one turn, and a tool_use id can come round again: without this, an
	// answer aimed at a question that has since been cut could decide a later question
	// that reuses the id (rudy-rn7, and the answer row of the contracts).
	if p.TurnID == "" {
		return nil, perr(protocol.CodeInvalidArgument, "answer names no turn")
	}
	if _, active := ls.mirroredState(); active != p.TurnID {
		return nil, perr(protocol.CodeConflict, "that question belongs to another turn")
	}
	// The first answer decides. resolveAnswer claims the pending channel and marks the id
	// answered in one critical section, which is what makes that race-free between two askers
	// holding the same question: exactly one of them finds the channel, and the loser is told
	// which of the two refusals applies.
	ans := turn.Answer{Decision: p.Decision, Scope: p.Scope, Reason: p.Reason}
	ch, settle, ok, already := ls.resolveAnswer(p.ToolUseID, ans)
	if !ok {
		if already {
			return nil, perr(protocol.CodeConflict, "another asker already answered "+p.ToolUseID)
		}
		return nil, perr(protocol.CodeNotFound, "no pending question for "+p.ToolUseID)
	}
	ch <- ans
	for _, sch := range settle {
		sch <- ans
	}
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
	_, res, serr := s.setModel(ls, p.Model)
	if serr != nil {
		return nil, serr
	}
	return res, nil
}

// setModel is the body of session.set_model, factored out so command.run's /model can share
// it: resolve spec against the registry, refuse a closed session or one with an active turn,
// and append a model_change only when the value actually differs. It also returns the resolved
// model, so a caller building a message for a person (a /model notice) can name it without
// resolving spec a second time.
func (s *Server) setModel(ls *liveSession, spec string) (provider.Model, EntryIDResult, *protocol.Error) {
	m, err := s.d.Registry.Resolve(spec)
	if err != nil {
		return provider.Model{}, EntryIDResult{}, perr(protocol.CodeNotFound, err.Error())
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.closed {
		return provider.Model{}, EntryIDResult{}, perr(protocol.CodeNotFound, "session closed")
	}
	st, _ := ls.mirroredState()
	if ls.runner != nil && isActive(st) {
		return provider.Model{}, EntryIDResult{}, perr(protocol.CodeConflict, "a turn is active")
	}
	view := deriveInfo(ls.sess.ID(), ls.snapshotEntries())
	if m.Ref == view.Model {
		return m, EntryIDResult{EntryID: ls.latestEntryID(session.KindModelChange, session.KindSessionOpened)}, nil
	}
	e2, err := ls.appendAndBroadcastLocked(session.ModelChange{Model: m.Ref})
	if err != nil {
		return provider.Model{}, EntryIDResult{}, protocol.ErrorFrom(err)
	}
	ls.model = m
	return m, EntryIDResult{EntryID: e2.ID.String()}, nil
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
	return s.setTitle(p.SessionID, p.Title)
}

// setTitle names a session, the body session.set_title and the /rename command share: the
// same refusal of an empty title and the same title_change entry, so a command cannot name
// a session something the method would not.
func (s *Server) setTitle(sessionID, title string) (any, *protocol.Error) {
	if title == "" {
		return nil, perr(protocol.CodeInvalidArgument, "empty title")
	}
	return s.setEntry(sessionID, session.KindTitleChange, func(view protocol.SessionInfo) (bool, session.Payload) {
		return view.Title == title, session.TitleChange{Title: title}
	})
}

func (s *Server) handleCompact(ctx context.Context, raw json.RawMessage) (any, *protocol.Error) {
	var p protocol.SessionCompactParams
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	ls, e := s.lookup(p.SessionID)
	if e != nil {
		return nil, e
	}
	entry, _, cerr := s.compact(ctx, ls, p.Instructions)
	if cerr != nil {
		return nil, cerr
	}
	// The zero entry is not a failure: it is a session with fewer than two entries to cover,
	// and an empty entry_id is how the contract says so.
	if entry.ID.IsZero() {
		return EntryIDResult{}, nil
	}
	return EntryIDResult{EntryID: entry.ID.String()}, nil
}

// compact summarizes the session now, through the Compactor a turn uses, and returns the
// compaction entry (the zero Entry when there was nothing to cover) with the number of
// conversation entries it covered. session.compact and /compact are both this call, so an
// explicit compaction covers what an automatic one would and fires the same hook.
//
// ls.mu is held across the whole Compact call, deliberately, and this is the one long call
// under it: the summary is a provider request and the entries it covers are decided from the
// log, so nothing may append between the decision and the compaction that records it. Holding
// mu is exactly what stops a turn from starting underneath. No Runner method is called while
// it is held, and the active-turn check reads the mirrored state, as liveSession's lock
// discipline requires.
//
// A detach landing during that call finds ls.mu held and leaves the session alone rather than
// waiting on it (see claimCloseIfIdle), so the close check runs again here once the lock is
// free: otherwise a session whose last subscriber left mid-compaction would stay live, and
// hold its flock, for the rest of the process.
func (s *Server) compact(ctx context.Context, ls *liveSession, instructions string) (session.Entry, int, *protocol.Error) {
	e, n, cerr := s.compactHoldingSession(ctx, ls, instructions)
	s.mu.Lock()
	s.closeIfUnusedLocked(ls)
	return e, n, cerr
}

// compactHoldingSession is compact's body, under ls.mu from the first check to the mirrored
// entry.
func (s *Server) compactHoldingSession(ctx context.Context, ls *liveSession, instructions string) (session.Entry, int, *protocol.Error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.closed {
		return session.Entry{}, 0, perr(protocol.CodeNotFound, "session closed")
	}
	st, _ := ls.mirroredState()
	if ls.runner != nil && isActive(st) {
		return session.Entry{}, 0, perr(protocol.CodeConflict, "a turn is active")
	}
	prov, ok := s.d.Registry.Provider(ls.model.Ref.Provider)
	if !ok {
		return session.Entry{}, 0, perr(protocol.CodeUnavailable, "provider not loaded: "+ls.model.Ref.Provider)
	}
	// The summary request answers to the server's own shutdown as well as to the caller: it
	// runs under ls.mu, and Shutdown closes every live session, which needs that lock. Without
	// this the caller's context is the only way out, and a shutdown would wait on a provider
	// nobody is going to answer, long past the budget its own caller gave it.
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(s.ctx, cancel)()
	n := len(turn.Cover(ls.sess, ulid.ULID{}))
	e, err := s.compactorLocked(prov, ls).Compact(cctx, ls.sess, ulid.ULID{}, instructions)
	if err != nil {
		return session.Entry{}, 0, protocol.ErrorFrom(err)
	}
	if e.ID.IsZero() {
		return session.Entry{}, 0, nil
	}
	// Synced here, unlike the other entries the server appends: no turn is coming to rest
	// behind this one, and a compaction is a provider call already paid for and already
	// broadcast. A crash before the next turn would charge for it twice. A sync that fails
	// can only be logged, since the compaction itself succeeded.
	if err := ls.sess.Sync(); err != nil {
		slog.Error("server: sync session log at compaction", "err", err)
	}
	ls.mirror(e)
	return e, n, nil
}

// compactorLocked is the Compactor the turn loop and session.compact share. Caller holds
// ls.mu, which guards ls.model; ls.overrides is the session-lived map after_tool handlers
// write, and reading it here is safe on both paths: the turn loop's compactor runs on the same
// goroutine that owns it, and session.compact has already refused an active turn.
func (s *Server) compactorLocked(prov provider.Provider, ls *liveSession) *turn.ModelCompactor {
	return &turn.ModelCompactor{
		Provider:  prov,
		Model:     ls.model,
		Hooks:     s.hookFirer(),
		MaxTokens: ls.model.OutputBudget(s.d.Config.MaxTokens),
		Overrides: ls.overrides,
	}
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

// renderPrompt is the system prompt for one turn: the operator's template when there is
// one, the built-in otherwise, with the session's own values in it. An agent definition's
// body takes the place of the built-in opening paragraph and nothing else.
//
// A template that fails to render is reported as a notice and the built-in is used: a bad
// prompt file must not take a session down, and a session that silently lost its prompt
// would be worse than one that says so.
func (s *Server) renderPrompt(ls *liveSession, ws session.Workspace, tools []tool.Tool) string {
	base := ls.system
	if base == "" {
		base = turn.BaseParagraph(ws, s.d.Version)
	}
	if s.d.Prompt == "" {
		if ls.system == "" {
			return turn.SystemPrompt(ws, s.d.Version, tools...)
		}
		return turn.SystemPromptWith(ls.system, ws, tools...)
	}
	out, err := turn.Build(s.d.Prompt, turn.Vars{
		Base:      strings.TrimRight(base, "\n"),
		Tools:     turn.ToolList(tools),
		Agents:    turn.AgentsSections(ws),
		Version:   s.d.Version,
		Workspace: ws.Root,
		Project:   ws.ProjectID,
		Model:     ls.model.Ref.String(),
		Date:      time.Now().Format("2006-01-02"),
		OS:        runtime.GOOS,
	})
	if err != nil {
		// Said to whoever is attached to this session, since it is their prompt file and
		// their session that is running without it.
		ls.obsMu.Lock()
		ls.broadcastObsLocked(protocol.NotifyNotice, protocol.NoticeParams{
			Level: "warn", Text: err.Error() + "; using the built-in prompt",
		})
		ls.obsMu.Unlock()
		return turn.SystemPromptWith(base, ws, tools...)
	}
	return out
}

// shellTool is the tool `!` runs through. The operator's shell command is the same tool the
// model calls, invoked by hand: the kernel has no private path to a shell either.
const shellTool = "bash"

// handleShell runs a shell command the operator typed with `!` and records it with its
// output as one user_message, starting no turn. ADR 0023.
//
// The gate does not run: the gate exists so a human answers for what the model wants to do,
// and here the human is the one who typed it. A session with no bash tool in its view (an
// agent definition that took it away) has nowhere to run it and says so.
func (s *Server) handleShell(ctx context.Context, raw json.RawMessage) (any, *protocol.Error) {
	var p protocol.SessionShellParams
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	// Trimmed once, here: what runs and what is recorded have to be the same command, and
	// the shell has no use for the spaces around it either.
	command := strings.TrimSpace(p.Command)
	if command == "" {
		return nil, perr(protocol.CodeInvalidArgument, "empty command")
	}
	ls, e := s.lookup(p.SessionID)
	if e != nil {
		return nil, e
	}
	// What the run needs is read under the lock; the run itself is not, since a command can
	// take as long as the tool timeout and nothing else could touch the session meanwhile.
	ls.mu.Lock()
	if ls.closed {
		ls.mu.Unlock()
		return nil, perr(protocol.CodeNotFound, "session closed")
	}
	if st, _ := ls.mirroredState(); ls.runner != nil && isActive(st) {
		ls.mu.Unlock()
		return nil, perr(protocol.CodeConflict, "a turn is active")
	}
	var tools turn.Tools = s.d.Plugins
	if ls.tools != nil {
		tools = ls.tools
	}
	ws := deriveInfo(ls.sess.ID(), ls.snapshotEntries()).Workspace
	sid := ls.sess.ID()
	ls.mu.Unlock()

	bt, ok := tools.Tool(shellTool)
	if !ok {
		return nil, perr(protocol.CodeNotFound, "this session has no "+shellTool+" tool to run a command with")
	}
	input, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		return nil, perr(protocol.CodeInternal, err.Error())
	}
	runCtx := ctx
	if d := time.Duration(s.d.Config.ToolTimeoutMS) * time.Millisecond; d > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	res, err := bt.Invoke(runCtx, tool.Call{ID: session.NewID().String(), Name: shellTool, Input: input, Workspace: ws, SessionID: sid})
	if err != nil {
		// The tool could not run at all, which is not something to write into the log as
		// though the command had answered.
		return nil, perr(protocol.CodeInternal, err.Error())
	}
	msg := session.UserMessage{
		Source:  session.SourceShell,
		Content: []session.Block{session.TextBlock(shellTranscript(command, res.Content))},
	}
	if err := session.Validate(msg); err != nil {
		return nil, perr(protocol.CodeInvalidArgument, err.Error())
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.closed {
		return nil, perr(protocol.CodeNotFound, "session closed")
	}
	entry, err := ls.appendAndBroadcastLocked(msg)
	if err != nil {
		return nil, protocol.ErrorFrom(err)
	}
	return protocol.SessionShellResult{EntryID: entry.ID.String(), IsError: res.IsError}, nil
}

// shellTranscript is what the log records and the model reads: the command as a person
// would write it at a prompt, then whatever it printed. A command that printed nothing is
// still worth recording, since the operator saw that too.
func shellTranscript(command string, out []session.Block) string {
	var b strings.Builder
	b.WriteString("$ " + command)
	for _, blk := range out {
		if text := strings.TrimRight(blk.Text, "\n"); text != "" {
			b.WriteString("\n" + text)
		}
	}
	return b.String()
}

// commandList is the registered commands as a client reads them. Name and description and
// nothing else: a Command's Run is the server's, and its arguments have no completion
// source yet (docs/specs/rudy-contracts.md, plugin.register_command).
func commandList(cmds []plugin.Command) protocol.CommandListResult {
	out := make([]protocol.CommandInfo, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, protocol.CommandInfo{Name: c.Name, Description: c.Description})
	}
	return protocol.CommandListResult{Commands: out}
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
	view := deriveInfo(ls.sess.ID(), ls.snapshotEntries())
	call := plugin.CommandCall{
		SessionID: ls.sess.ID(), Workspace: view.Workspace, Args: p.Args,
		Mode: view.Mode, Model: view.Model, Thinking: view.Thinking,
	}
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
	case plugin.Compact:
		e2, n, cerr := s.compact(ctx, ls, a.Instructions)
		if cerr != nil {
			return nil, cerr
		}
		notice := "nothing to compact"
		if !e2.ID.IsZero() {
			notice = fmt.Sprintf("compacted %d entries", n)
		}
		cn.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "info", Text: notice})
		return protocol.CommandRunResult{Notice: notice}, nil
	case plugin.SetModel:
		m, _, serr := s.setModel(ls, a.Model)
		if serr != nil {
			return nil, serr
		}
		notice := "model set to " + m.Ref.String()
		cn.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "info", Text: notice})
		return protocol.CommandRunResult{Notice: notice}, nil
	case plugin.SetMode:
		if !a.Mode.Valid() {
			return nil, perr(protocol.CodeInvalidArgument, "invalid mode")
		}
		if _, e := s.setEntry(p.SessionID, session.KindModeChange, func(view protocol.SessionInfo) (bool, session.Payload) {
			return view.Mode == a.Mode, session.ModeChange{Mode: a.Mode}
		}); e != nil {
			return nil, e
		}
		notice := "permissions: " + string(a.Mode)
		cn.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "info", Text: notice})
		return protocol.CommandRunResult{Notice: notice}, nil
	case plugin.SetTitle:
		if _, e := s.setTitle(p.SessionID, a.Title); e != nil {
			return nil, e
		}
		notice := "session named " + a.Title
		cn.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "info", Text: notice})
		return protocol.CommandRunResult{Notice: notice}, nil
	case plugin.Fork:
		info, ferr := s.forkAt(cn, ls, a.AtEntryID)
		if ferr != nil {
			return nil, ferr
		}
		notice := "forked to " + info.SessionID
		cn.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "info", Text: notice})
		return protocol.CommandRunResult{SessionID: info.SessionID, Notice: notice}, nil
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
	ws, err := workspace.Detect(p.Cwd)
	if err != nil {
		return nil, perr(protocol.CodeInvalidArgument, err.Error())
	}
	// The parent is resolved first: it is what says whether this caller may open a child at
	// all, and a refusal there should not depend on anything the request asked for. It also
	// decides what an unset model, mode or thinking level inherits.
	parent, perror := s.parentOf(cn, p.Parent, ws)
	if perror != nil {
		return nil, perror
	}
	agentName := p.Agent
	if agentName == "" {
		agentName = s.d.Config.Agent
	}
	def, ok := s.resolveAgent(cn, ws, agentName)
	if !ok {
		return nil, perr(protocol.CodeNotFound, "unknown agent "+agentName)
	}
	// What a child inherits from its parent: the parent's current values, not the ones it
	// opened with. A root session inherits nothing, so these stay empty for it.
	var fromParent struct{ model, mode, thinking string }
	if parent != nil {
		v := deriveInfo(parent.sess.ID(), parent.snapshotEntries())
		fromParent.model, fromParent.mode, fromParent.thinking = v.Model.String(), string(v.Mode), string(v.Thinking)
	}

	spec := firstNonEmpty(p.Model, def.Model, fromParent.model, s.d.Config.Default.Provider+":"+s.d.Config.Default.Model)
	m, err := s.d.Registry.Resolve(spec)
	if err != nil {
		// A model the caller named is a request this server cannot honour, and saying so is
		// the useful answer. A model that came from config, an agent definition or a parent
		// is different: the provider dropped an id the operator wrote down months ago, and
		// refusing to open means the harness cannot be started by the person who needs it
		// to change the setting. So the session opens on the ref as written, with a notice,
		// the way a resume already does for a session whose recorded model has gone (see
		// coldLoadOne). The picker is one keystroke away and set_model still refuses an id
		// the registry does not have.
		ref, parsed := session.ParseModelRef(spec)
		if p.Model != "" || !parsed {
			return nil, perr(protocol.CodeNotFound, err.Error())
		}
		m = provider.Model{Ref: ref}
		cn.notify(protocol.NotifyNotice, protocol.NoticeParams{
			Level: "warn",
			Text:  "model not in registry: " + spec + "; pick another with the model picker",
		})
	}
	mode := session.Mode(firstNonEmpty(p.Mode, fromParent.mode, s.d.Config.Permissions.Mode))
	thinking := session.ThinkingLevel(firstNonEmpty(p.Thinking, string(def.Thinking), fromParent.thinking, s.d.Config.Default.Thinking))
	if !mode.Valid() || !thinking.Valid() {
		return nil, perr(protocol.CodeInvalidArgument, "invalid mode or thinking level")
	}
	// Resolved now, while the parent (if any) is still live, and persisted on the entry below:
	// a resume or a fork brings this session back long after that parent may be gone, and
	// applyAgentFromLog reads this recorded value rather than trying to recompute it (ADR
	// 0028, rudy-ef4). SchemaVersion 2 is what tells that recorded value apart from a version 1
	// log that has no tools key at all: both decode Tools() to nil, and reading a version 1
	// log's absent key as "every tool" would be the same escalation one field earlier.
	allow := resolveTools(parent, def, registeredToolNames(s.d.Plugins, p.Tools))
	opened := session.SessionOpened{
		SchemaVersion: 2, RudyVersion: s.d.Version, Workspace: ws, Model: m.Ref,
		Thinking: thinking, Mode: mode, Agent: def.Name, Tools: allow,
	}
	if parent != nil {
		opened.ParentSessionID, opened.ParentToolUseID = parent.sess.ID().String(), p.Parent.ToolUseID
	}
	sess, err := session.Open(s.d.Store, opened)
	if err != nil {
		return nil, protocol.ErrorFrom(err)
	}
	ls := newLive(sess, m)
	ls.parent = parent
	if parent != nil && len(ls.entries) > 0 {
		// session.Open appends session_opened inside itself, before this liveSession
		// exists, so the ordinary mirror/fanout broadcast (session_live.go) never runs for
		// it: the caller who opened it only ever sees it by direct replay, in
		// installAndAttach below, and the parent's own watchers see nothing at all. Without
		// this, AgentCallFor and its equivalents never learn the mapping a child's own
		// notifications are routed under, and a subagent never renders (rudy-contracts.md,
		// entry.appended: a child's entries go to its parent's subscribers too). Sent now,
		// after ls.parent is set, since watchers() has nowhere to look before that.
		//
		// The length guard is defensive: session.Open guarantees session_opened is the
		// log's first entry, so ls.entries is never empty here in practice, but indexing
		// it bare is a panic waiting for whatever future path seeds a liveSession
		// differently.
		ls.notifyWatchers(protocol.NotifyEntryAppended, protocol.EntryAppended{
			SessionID: sess.ID().String(), Entry: ls.entries[0],
		})
	}
	s.applyAgent(ls, def, allow)
	s.fireSessionOpened(s.ctx, ls, false)
	return s.installAndAttach(cn, ls), nil
}

// firstNonEmpty is the first non-empty of vs, or "" when there is none: the resolution order
// for a session's model, mode and thinking level (the request, then the agent definition,
// then the parent, then config) written once instead of four times.
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// resolveAgent reads the definitions visible to a session in ws and resolves one by name,
// telling cn about any file that would not parse. The user's root, then the workspace's, then
// whatever plugins registered, first name winning: an operator's file always beats a plugin's
// registration, so installing a plugin cannot take a name that is already in use and cannot
// change what an existing session resolves to (ADR 0028). Read per session rather than cached,
// so a definition edited between two sessions takes effect on the second without a restart. ok
// is false only for a name nothing defines; "" and "default" always resolve.
func (s *Server) resolveAgent(cn *conn, ws session.Workspace, name string) (agentdef.Definition, bool) {
	defs, errs := agentdef.Load([]string{
		filepath.Join(s.d.Config.ConfigDir, "agents"),
		filepath.Join(ws.Root, ".rudy", "agents"),
	})
	for _, e := range errs {
		cn.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "warn", Text: e.Error()})
	}
	for n, d := range s.d.Plugins.AgentDefs() {
		if _, seen := defs[n]; !seen {
			defs[n] = d
		}
	}
	return agentdef.Resolve(defs, name)
}

// applyAgent stamps a definition and an already-resolved tool list onto a session that is not
// yet shared: its tool view, its system prompt and its step limit. A child never sees the agent
// tool, even when allow lists it, which is what keeps subagent depth at one: with no tool to
// call, a child cannot open a grandchild.
//
// allow is never computed here. At open time it is resolveTools's intersection against the
// live parent; on a reload it is whatever session_opened recorded. applyAgent only stamps, so
// it needs nothing about the parent's liveness (ADR 0028, rudy-ef4).
func (s *Server) applyAgent(ls *liveSession, def agentdef.Definition, allow []string) {
	var deny []string
	if ls.parent != nil || openedAsChild(ls.entries) {
		deny = []string{"agent"}
	}
	if allow != nil || deny != nil {
		ls.tools = plugin.NewToolView(s.d.Plugins, allow, deny)
	}
	ls.system = def.Prompt
	ls.maxSteps = def.MaxTurns
}

// resolveTools is the intersection a fresh session.open needs: the definition's list narrowed
// by the caller's own tools (when given), narrowed again by the live parent's own effective set
// (when there is one). It runs once, at open time, and its result is persisted on session_opened
// rather than recomputed on a later resume or fork, since the parent may be gone by then and
// recomputing from the definition alone would hand back exactly what the intersection removed
// (ADR 0028, rudy-ef4). The caller term is applied before the parent term rather than after: the
// two commute mathematically, but doing it this way makes the parent bound visibly the outer
// limit, the one term nothing else in this chain can widen past.
func resolveTools(parent *liveSession, def agentdef.Definition, tools []string) []string {
	allow := intersectTools(def.Tools, tools)
	if parent == nil {
		return allow
	}
	return intersectTools(allow, parentToolNames(parent))
}

// registeredToolNames drops any name in names that no plugin has actually registered, before it
// can reach session_opened. Left unfiltered, a name nothing answers to at open time still
// persists verbatim: today's ToolView already keeps it from being offered, so nothing looks
// wrong, but a plugin that registers that exact name later would silently grant it to a session
// that had already resumed once (rudy-review round 1 on task 5). A nil names is left nil: that
// means the caller applied no narrowing at all, not a narrowing to an empty registry.
func registeredToolNames(reg *plugin.Registry, names []string) []string {
	if names == nil {
		return nil
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if _, ok := reg.Tool(n); ok {
			out = append(out, n)
		}
	}
	return out
}

// intersectTools narrows want by have. A nil want means every tool, so the result is have; a
// nil have means the parent is unrestricted, so the result is want. Only ever removes.
func intersectTools(want, have []string) []string {
	switch {
	case have == nil:
		return want
	case want == nil:
		return slices.Clone(have)
	}
	out := make([]string, 0, min(len(want), len(have)))
	for _, n := range want {
		if slices.Contains(have, n) {
			out = append(out, n)
		}
	}
	return out
}

// parentToolNames is parent's effective list, or nil when it holds every tool.
func parentToolNames(parent *liveSession) []string {
	if parent.tools == nil {
		return nil
	}
	all := parent.tools.Tools()
	out := make([]string, 0, len(all))
	for _, t := range all {
		out = append(out, t.Name)
	}
	return out
}

// applyAgentFromLog stamps the agent definition and tool set a session already carries in its
// log onto a liveSession that is not yet shared: the cold-load and fork paths, where both come
// from the log rather than the request. The definition is a file and not part of the log, so it
// is re-read here; one that has since been deleted falls back to the default rather than
// refusing to bring the session back, since its entries are still perfectly readable and a lost
// file is not the user's fault. The tool set is not re-read from that file: from SchemaVersion 2
// on it is whatever session_opened recorded, what this session actually ran under, and it does
// not depend on a parent that may no longer exist (ADR 0028, rudy-ef4).
func (s *Server) applyAgentFromLog(cn *conn, ls *liveSession) {
	name := ls.sess.Agent()
	ws := deriveInfo(ls.sess.ID(), ls.entries).Workspace
	def, ok := s.resolveAgent(cn, ws, name)
	if !ok {
		cn.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "warn", Text: "agent " + name + " is no longer defined; continuing under the default agent"})
		def, _ = s.resolveAgent(cn, ws, "")
	}
	s.applyAgent(ls, def, loggedTools(ls.sess, def))
}

// loggedTools is the tool set applyAgentFromLog stamps: the recorded value whenever there is
// one, the definition's own list only when there is not. The fallback is not a bare version
// check: commit d2a6da8 added the tools field while still writing SchemaVersion 1, so a log
// from that window can already carry a correct, intersected value at version 1. Falling back on
// version alone would discard a bound that was genuinely applied, resuming wider than the
// session ran live, which is this task's whole subject.
//
// A nil Tools is safe to treat as absent even inside that window: intersectTools returns nil
// only when the parent held every tool and the definition itself named none, in which case
// def.Tools is independently nil too, so recomputing it changes nothing. A non-nil Tools can
// only have come from a round 2 (or later) writer and is never the one discarded here.
func loggedTools(sess *session.Session, def agentdef.Definition) []string {
	if sess.SchemaVersion() < 2 && sess.Tools() == nil {
		return def.Tools
	}
	return sess.Tools()
}

// parentSessionIDOf reads the parent a session was opened under off the log rather than off
// the live parent, which a resumed session no longer has: a child resumed long after the
// session that spawned it is gone is still a child.
//
// It is the first session_opened in the entries, not the first entry: a fork begins with a
// fork_point and carries the entries it inherited behind it, session_opened included, so a
// fork of a child is a child too. It has the same parent, the same agent deny and the same
// standing with a plugin folding memory over root sessions; reading entry zero made it look
// like a root.
func parentSessionIDOf(entries []session.Entry) string {
	for _, e := range entries {
		if o, ok := e.Payload.(session.SessionOpened); ok {
			return o.ParentSessionID
		}
	}
	return ""
}

// openedAsChild is parentSessionIDOf as the predicate the open path gates on: a child has no
// business opening one of its own.
func openedAsChild(entries []session.Entry) bool { return parentSessionIDOf(entries) != "" }

// parentOf resolves the parent a child session hangs off, and is the whole of the authority
// to open one. A plugin may name a parent only through a tool call it is itself running: the
// named tool_use must be pending in a live session and must be a tool this very plugin
// registered, which is the binding between a child and the tool whose answer it is. On top of
// that the parent must not itself be a child (depth is one), the tool_use may open a child
// only once, and the child runs in the parent's workspace and nowhere else.
func (s *Server) parentOf(cn *conn, ref *protocol.ParentRef, ws session.Workspace) (*liveSession, *protocol.Error) {
	if ref == nil {
		return nil, nil
	}
	if cn.plugin == "" {
		return nil, perr(protocol.CodeInvalidArgument, "parent is for plugins")
	}
	parent, e := s.lookup(ref.SessionID)
	if e != nil {
		return nil, e
	}
	entries := parent.snapshotEntries()
	toolName, pending := "", false
	for _, b := range session.PendingToolUsesIn(entries) {
		if b.ID == ref.ToolUseID {
			toolName, pending = b.Name, true
			break
		}
	}
	if !pending {
		return nil, perr(protocol.CodeNotFound, "no pending tool_use "+ref.ToolUseID+" in "+ref.SessionID)
	}
	if owner, ok := s.d.Plugins.ToolOwner(toolName); !ok || owner != cn.plugin {
		return nil, perr(protocol.CodeUnauthorized, "tool_use "+ref.ToolUseID+" is not a tool of plugin "+cn.plugin)
	}
	if openedAsChild(entries) {
		return nil, perr(protocol.CodeRefusedByInvariant, "depth is one: a child session cannot open another")
	}
	if root := deriveInfo(parent.sess.ID(), entries).Workspace.Root; ws.Root != root {
		return nil, perr(protocol.CodeInvalidArgument, "child cwd must be the parent's workspace")
	}
	// Claimed last, so a refusal above never spends the one child this tool_use may open.
	if !parent.claimChild(ref.ToolUseID) {
		return nil, perr(protocol.CodeConflict, "tool_use "+ref.ToolUseID+" already opened a child session")
	}
	return parent, nil
}

// resumeAttempts bounds how many times resume will re-open a session that was closed out
// from under it between the load and the attach. Each pass waits for the close in flight and
// re-opens, so one retry is enough for a single racing close and three covers a pile-up; a
// session that keeps being closed as fast as this loads it is not a session to attach to.
const resumeAttempts = 3

// beforeAttachHook runs inside resume between loading the session and attaching to it: the
// one window a close can take the session away in, a few instructions wide and reachable in
// practice only on a loaded machine. Nil in every build but the test that closes the session
// inside it on purpose (export_test.go). Atomic because a server from a finished test can
// still be reading it while the next test writes it.
var beforeAttachHook atomic.Pointer[func()]

// resume attaches cn to sid, loading it from disk first when it is not already live. loadCold
// single-flights concurrent cold loads of the same id (see loadCold); attachIfLive then
// subscribes atomically with the s.live lookup (see detach for why that matters).
func (s *Server) resume(cn *conn, p protocol.SessionResumeParams) (any, *protocol.Error) {
	sid, err := ulid.Parse(p.SessionID)
	if err != nil {
		return nil, perr(protocol.CodeInvalidArgument, "bad session id")
	}
	// loadCold and attachIfLive are two critical sections, and between them the last
	// subscriber's close can take the session away: nothing is attached to it yet, so it is
	// unused and closeIfUnusedLocked is entitled to close it. Loading it again is this
	// server's job rather than the client's, and the contract says so: session.resume's
	// unavailable means a session another process holds, named by data.socket, not a race
	// this process had with itself. loadCold waits out the close that is in flight and
	// re-opens from disk, so every pass makes progress (rudy-anw).
	for range resumeAttempts {
		if _, lerr := s.loadCold(cn, sid); lerr != nil {
			return nil, lerr
		}
		if f := beforeAttachHook.Load(); f != nil {
			(*f)()
		}
		if info, ok := s.attachIfLive(cn, sid); ok {
			return info, nil
		}
	}
	// Every attempt lost the same race, or the server is shutting down and s.live is being
	// emptied under us. Nothing here can attach a client to a session that keeps going away.
	return nil, perr(protocol.CodeUnavailable, "session unavailable, retry")
}

func (s *Server) fork(cn *conn, p protocol.SessionForkParams) (any, *protocol.Error) {
	sid, err := ulid.Parse(p.SessionID)
	if err != nil {
		return nil, perr(protocol.CodeInvalidArgument, "bad session id")
	}
	parent, lerr := s.loadCold(cn, sid)
	if lerr != nil {
		return nil, lerr
	}
	info, ferr := s.forkAt(cn, parent, p.AtEntryID)
	if ferr != nil {
		return nil, ferr
	}
	return info, nil
}

// forkAt is the body of session.fork once the parent is already resolved to a live session:
// shared by fork, which loads it cold from a session id, and command.run's /fork, which already
// has it live from the command's own session_id. at is the entry id to fork at, as a string so
// a caller can pass an unparsed one straight through and get invalid_argument back rather than
// having to parse it itself; empty means the newest entry of any kind. That default is resolved
// here, after parent.mu is taken, not by a caller reading it beforehand: a command racing on
// the same session (another /fork, a /model, a set_title) could append between such a read and
// this lock, landing the fork one entry stale. obsMu, which latestEntryID takes, nests inside mu
// (see liveSession's doc), so taking it here while already holding parent.mu is safe.
func (s *Server) forkAt(cn *conn, parent *liveSession, at string) (protocol.SessionInfo, *protocol.Error) {
	parent.mu.Lock()
	if at == "" {
		at = parent.latestEntryID()
	}
	atID, err := ulid.Parse(at)
	if err != nil {
		parent.mu.Unlock()
		return protocol.SessionInfo{}, perr(protocol.CodeInvalidArgument, "bad entry id")
	}
	st, _ := parent.mirroredState()
	active := parent.runner != nil && isActive(st)
	closed := parent.closed
	var child *session.Session
	switch {
	case closed:
		parent.mu.Unlock()
		return protocol.SessionInfo{}, perr(protocol.CodeUnavailable, "session unavailable, retry")
	case active:
		parent.mu.Unlock()
		return protocol.SessionInfo{}, perr(protocol.CodeConflict, "a turn is active")
	default:
		child, err = parent.sess.Fork(s.d.Store, atID)
		parent.mu.Unlock()
	}
	if err != nil {
		if errors.Is(err, session.ErrInvariant) {
			return protocol.SessionInfo{}, perr(protocol.CodeNotFound, err.Error())
		}
		return protocol.SessionInfo{}, protocol.ErrorFrom(err)
	}
	m, rerr := s.d.Registry.Resolve(child.Model().String())
	if rerr != nil {
		m = provider.Model{Ref: child.Model()}
	}
	// A fork inherits its parent's entries by reference, so its log answers "which agent"
	// and "is this a child" exactly as the session it came from does. Without this a fork of
	// a restricted subagent session would come back with every tool, the default prompt and
	// no step limit.
	ls := newLive(child, m)
	s.applyAgentFromLog(cn, ls)
	return s.installAndAttach(cn, ls), nil
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
		return nil, loadErr(err, s.d.Socket)
	}
	m, rerr := s.d.Registry.Resolve(sess.Model().String())
	if rerr != nil {
		m = provider.Model{Ref: sess.Model()}
		cn.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "warn", Text: "model not in registry: " + sess.Model().String()})
	}
	ls := newLive(sess, m)
	s.applyAgentFromLog(cn, ls)
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
		SessionID:       view.SessionID,
		Workspace:       view.Workspace,
		Model:           view.Model,
		Mode:            view.Mode,
		Thinking:        view.Thinking,
		Resumed:         resumed,
		ParentSessionID: parentSessionIDOf(ls.entries),
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
	at := ls.subscribeLocked(cn)
	at.children = s.childAttachmentsLocked(cn, ls)
	s.mu.Unlock()
	deliverAttach(cn, ls.sess.ID(), at)
	slog.Info("session: open", "session", ls.sess.ID(), "agent", ls.sess.Agent(), "conn", cn.id)
	return at.info
}

// childAttachmentsLocked is what cn is owed about the children of ls that are live right now
// (see childAttachment): the walk down that mirrors the one notifyWatchers makes on every
// broadcast, made once here because the attach itself is the moment a client learns what it
// missed. The exclusions are watchers' own, for the same reasons: a plugin connection is the one
// waiting on the tool call and has no use for its own echo, and a connection already subscribed
// to the child gets that child's own replay directly.
//
// Depth is one, so there is no recursion: a child can never have a child (open refuses a parent
// that is already one). Caller holds Server.mu, which is what makes reading s.live safe here and
// what makes the answer consistent with the subscribeLocked that ran in the same critical
// section; ls.parent on each candidate is written before its session is shared and never again
// (see liveSession), so it needs no lock of its own. cn.subscribed takes cn.mu under Server.mu,
// the nesting subscribeLocked already uses.
func (s *Server) childAttachmentsLocked(cn *conn, ls *liveSession) []childAttachment {
	if cn.plugin != "" {
		return nil
	}
	var out []childAttachment
	for sid, c := range s.live {
		if c.parent != ls || cn.subscribed(sid) {
			continue
		}
		if at, ok := c.watchAttachment(cn.isAsker()); ok {
			out = append(out, at)
		}
	}
	// Map order is not an order: sorting by session id makes what a newcomer hears the same on
	// every attach, and a session id is a ULID, so that is also the order the children opened in.
	slices.SortFunc(out, func(a, b childAttachment) int { return strings.Compare(a.sid, b.sid) })
	return out
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
	at := ls.subscribeLocked(cn)
	at.children = s.childAttachmentsLocked(cn, ls)
	s.mu.Unlock()
	deliverAttach(cn, sid, at)
	slog.Info("session: resume", "session", sid, "conn", cn.id)
	return at.info, true
}

func replay(cn *conn, sid ulid.ULID, entries []session.Entry) {
	sidStr := sid.String()
	for _, e := range entries {
		cn.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: sidStr, Entry: e})
	}
}

// deliverAttach sends a new subscriber everything it is owed, in the contract's order: the
// replay, then the turn's current state when one is running, then a tool.state for every call
// still in flight, then each question standing for an asker, then each live child's own
// session_opened and standing questions. The response the caller returns leaves after all of
// them, on the same ordered outbox (see conn.pump), which is what "then this response" in the
// session.resume row means.
//
// The children come last, after the whole of this session's replay: the entry naming a child is
// only useful once the agent call it names is on screen, and that call arrived in the replay
// above.
func deliverAttach(cn *conn, sid ulid.ULID, at attachment) {
	replay(cn, sid, at.entries)
	if at.state != nil {
		cn.notify(protocol.NotifyTurnState, *at.state)
	}
	for _, ts := range at.toolStates {
		cn.notify(protocol.NotifyToolState, ts)
	}
	for _, q := range at.standing {
		cn.notify(protocol.NotifyPermissionRequested, q)
	}
	for _, c := range at.children {
		cn.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: c.sid, Entry: c.opened})
		for _, q := range c.standing {
			cn.notify(protocol.NotifyPermissionRequested, q)
		}
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
	slog.Info("session: detach", "session", ls.sess.ID(), "conn", cn.id)

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
	steering := ls.state == turn.Steering
	standing := len(ls.standing) > 0
	ls.obsMu.Unlock()

	s.closeIfUnusedLocked(ls)
	if standing {
		// A question this session is holding may have just lost the last connection that
		// could answer it. Both locks are released by now, which is what abandonStanding
		// needs: it wakes the runner's own goroutine, and that goroutine goes straight for
		// ls.mu.
		ls.abandonStanding()
	}
	if !empty || !steering {
		return
	}
	// A steering turn owns the session but nothing is running: it is parked waiting for a
	// steer message that the connection which would have sent it has just gone. Cancel it so
	// the turn records its interruption and the session can close, rather than staying live
	// for the rest of the process. Both locks are released first: nothing holding ls.mu or
	// Server.mu may call a Runner method (see liveSession).
	ls.mu.Lock()
	r := ls.runner
	ls.mu.Unlock()
	if r == nil {
		return
	}
	r.Interrupt(session.InterruptCancel)
	s.mu.Lock()
	s.closeIfUnusedLocked(ls)
}

// closeIfUnusedLocked closes and removes ls when no connection holds it any more and no turn
// still owns it, and releases Server.mu whatever it decides. The caller holds Server.mu across
// its own change to ls.conns (detach) or across nothing at all (runTurn, whose session's last
// connection may have detached mid-turn), because holding it across the whole decide-then-
// remove is what keeps this from racing attachIfLive into subscribing to a session that is
// concurrently being closed (see detach).
//
// The delete is what has to happen before releasing mu: it is what stops a concurrent
// attachIfLive from finding this session again. The actual Close, once that is done, no longer
// needs mu - nothing can reach ls through s.live to race it, and the claim (ls.closed, set
// under ls.mu by claimCloseIfIdle) is what stops any other path (Shutdown's closeIfOpen) from
// also closing it, or from firing session_closed a second time for the same session. But
// sess.Close still has to run (releasing the store's flock) before a cold load for this same id
// can safely call session.Load again, so register it in s.closing in this same critical
// section, before releasing mu: a concurrent loadCold checks s.closing right alongside s.live
// and s.loading (see loadCold) and waits instead of racing the flock.
func (s *Server) closeIfUnusedLocked(ls *liveSession) {
	ls.obsMu.Lock()
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
			// A steer resume keeps the turn id it already has, read back from the mirror
			// rather than from the runner, which nothing holding mu may call.
			_, tid := ls.mirroredState()
			ls.markStarting(tid)
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
	if len(ls.askers()) > 0 {
		asker = la
	}
	view := deriveInfo(ls.sess.ID(), ls.snapshotEntries())
	first := &firstAppendSignal{Observer: &fanout{ls: ls, sid: sid}, started: make(chan session.Entry, 1), failed: make(chan struct{})}
	// The agent definition's tool view, prompt and step limit, all written before this
	// session was shared and read here under the mu this still holds, alongside hookContext
	// (which is written the same way, see fireSessionOpened).
	var tools turn.Tools = s.d.Plugins
	if ls.tools != nil {
		tools = ls.tools
	}
	// An agent definition's body replaces the base prompt; the workspace's AGENTS.md
	// sections still follow it, which is what SystemPromptWith is for. Only the prompt in
	// force is built: each of these reads the workspace's AGENTS.md and the global one from
	// disk, and an agent session would otherwise pay for both every turn.
	// The tool list is the view's own, so an agent definition's narrower set is what its
	// prompt describes, and the template is whatever the operator's prompt file said.
	base := s.renderPrompt(ls, view.Workspace, tools.Tools())
	r := turn.NewRunner(turn.Config{
		Session:     ls.sess,
		Provider:    prov,
		Model:       ls.model,
		Tools:       tools,
		Gate:        s.d.Gate,
		Asker:       asker,
		Observer:    first,
		System:      base + ls.hookContextSuffixLocked(),
		MaxSteps:    ls.maxSteps,
		MaxTokens:   ls.model.OutputBudget(s.d.Config.MaxTokens),
		Hooks:       s.hookFirer(),
		Compactor:   s.compactorLocked(prov, ls),
		CompactAt:   s.d.Config.Sessions.CompactAt,
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
	// A session whose last connection detached while this turn was running is nobody's now:
	// detach could not close it then (a turn still owned it), and nothing else will come
	// back for it. That is the plugin whose agent tool was interrupted and closed its child
	// while the child was still streaming, and equally a client that disconnected mid-turn.
	s.mu.Lock()
	s.closeIfUnusedLocked(ls)
}
