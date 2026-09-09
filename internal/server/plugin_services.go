package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/session"
)

// errSessionNotOpen is what a note for a session nobody holds live comes back as; the
// dispatch path turns it into not_found.
var errSessionNotOpen = errors.New("session not open")

// PluginServices is what the registry's hosts call. Connect returns the client end of a pipe
// whose server end is served on s.ctx as caller class plugin; Note appends a note entry to a
// live session; StatusChanged and WidgetChanged broadcast to every connection. It is the
// whole of a plugin's private access to the server: everything else it wants, it asks for
// over the protocol like any other caller.
func (s *Server) PluginServices() plugin.Services {
	return plugin.Services{
		Note: func(sid ulid.ULID, name, text string, role session.NoteRole) error {
			_, err := s.appendNote(sid, name, text, role)
			return err
		},
		Connect:       s.connectPlugin,
		StatusChanged: s.broadcastStatus,
		WidgetChanged: s.broadcastWidget,
		OnStatus:      s.broadcastPluginState,
	}
}

// connectPlugin gives a plugin the client end of an in-memory pipe. The server end runs on
// s.ctx, not the caller's: a plugin's connection belongs to the server's lifetime, and
// serveConn registers it in the same WaitGroup Shutdown waits on, so Shutdown still ends it.
func (s *Server) connectPlugin(_ context.Context, name string) (protocol.Conn, error) {
	clientEnd, serverEnd := protocol.Pipe()
	s.wgMu.Lock()
	if s.shuttingDown {
		s.wgMu.Unlock()
		// Close both ends rather than leaking a pipe nobody serves: a plugin handed the
		// client end of an unserved pipe would block on its first Call until its own
		// context expired instead of failing here.
		_ = serverEnd.Close()
		_ = clientEnd.Close()
		return nil, ErrShuttingDown
	}
	s.wgMu.Unlock()
	go func() { _ = s.servePlugin(s.ctx, serverEnd, name) }()
	return clientEnd, nil
}

// appendNote appends outside any turn's goroutine. Session.Append is safe for that since it
// carries its own mutex; the mirror is updated under obsMu and the entry broadcast like any
// other. It takes no ls.mu: a note is display only, never sent to a model, so unlike every
// other server-side append it does not have to wait for the turn to give the session back.
func (s *Server) appendNote(sid ulid.ULID, owner, text string, role session.NoteRole) (session.Entry, error) {
	s.mu.Lock()
	ls, ok := s.live[sid]
	s.mu.Unlock()
	if !ok {
		return session.Entry{}, fmt.Errorf("%w: %s", errSessionNotOpen, sid)
	}
	return ls.appendNote(owner, text, role)
}

// broadcastStatus sends the whole status line to every client connection. The conns snapshot
// is taken under mu and the notifies happen after releasing it: conn.mu is never nested
// inside any of the server's locks (see the Server doc), and notify only enqueues anyway.
func (s *Server) broadcastStatus() {
	items := s.d.Plugins.StatusItems()
	for _, cn := range s.clientConns() {
		cn.notify(protocol.NotifyStatusUpdated, protocol.StatusUpdated{Items: items})
	}
}

// broadcastPluginState tells every client what one plugin's load state has become: loading
// and ready as the registry loads it, failed when its process dies under a live session.
func (s *Server) broadcastPluginState(st plugin.Status) {
	ps := protocol.PluginState{Name: st.Name, Origin: stateOrigin(st), State: string(st.State), Reason: st.Reason}
	for _, cn := range s.clientConns() {
		cn.notify(protocol.NotifyPluginState, ps)
	}
}

// stateOrigin is where a plugin came from, for the wire. A status that does not say is a
// linked plugin: that is what being compiled in looks like.
func stateOrigin(st plugin.Status) string {
	if st.Origin == "" {
		return plugin.OriginLinked
	}
	return st.Origin
}

func (s *Server) broadcastWidget(w plugin.Widget) {
	for _, cn := range s.clientConns() {
		cn.notify(protocol.NotifyWidgetUpdated, w)
	}
}

// sendConnectState tells one connection what every plugin is currently showing, right after
// its hello response. A client that connects late renders the same status line, widgets and
// plugin states as one that was there when they were set.
func (s *Server) sendConnectState(cn *conn) {
	cn.notify(protocol.NotifyStatusUpdated, protocol.StatusUpdated{Items: s.d.Plugins.StatusItems()})
	for _, w := range s.d.Plugins.Widgets() {
		cn.notify(protocol.NotifyWidgetUpdated, w)
	}
	for _, st := range s.d.Plugins.Statuses() {
		cn.notify(protocol.NotifyPluginState, protocol.PluginState{
			Name: st.Name, Origin: stateOrigin(st), State: string(st.State), Reason: st.Reason,
		})
	}
}

// clientConns is every connection a render notification is worth sending to: the clients.
// Plugin connections are skipped. A protocol.Client queues notifications without bound and
// nothing obliges a plugin to drain them, so a plugin that only ever makes calls would grow
// that queue for the life of the process on every status or widget change. A plugin that
// does want the state asks for it with a hello (see sendConnectState).
func (s *Server) clientConns() []*conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*conn, 0, len(s.conns))
	for _, cn := range s.conns {
		if cn.plugin != "" {
			continue
		}
		out = append(out, cn)
	}
	return out
}
