package server

import (
	"context"
	"log/slog"
	"sync"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/protocol"
)

// conn is one client connection. Everything sent to the client goes through the outbox so
// notifications and responses leave in the order they were produced, and a slow client never
// blocks whoever is producing them (the turn.Runner's Observer callbacks, in particular).
type conn struct {
	id int
	c  protocol.Conn
	// hello is whether this connection may dispatch anything but client.hello. A plugin
	// connection starts true: the server handed it out itself, so there is nothing to
	// introduce. greeted is whether a client.hello has actually been answered, which is
	// what makes a second one a refusal.
	hello    bool
	greeted  bool
	asker    bool
	sameUser bool
	// plugin is the caller class: the plugin's name on a connection the server itself
	// handed out through Host.Connect, or a spawned plugin's stdio peer, empty on every
	// client connection. Only a plugin may append a note or name a parent session.
	// Written once before the serve loop starts and read only from that loop.
	plugin string
	// reg is the spawned plugin's adapter: what plugin.register_* and the plugin's own
	// notifications are applied to. Nil on a client connection and on a linked plugin's
	// Host.Connect connection, which registers through the Host instead. Written once
	// before the serve loop starts.
	reg plugin.Registrar

	mu        sync.Mutex
	queue     []outbound
	wake      chan struct{}
	pumpDone  chan struct{}
	abortPump context.CancelFunc
	pumpErr   error
	stopped   bool
	// shutdownProof is set on the connection whose server.shutdown was accepted: its writer
	// stays open past its serve loop to send server.stopped, so nothing else may close it.
	shutdownProof bool
	subs          map[ulid.ULID]*liveSession
}

type outbound struct {
	msg  any
	sent chan error
}

func newConn(id int, c protocol.Conn) *conn {
	return &conn{id: id, c: c, wake: make(chan struct{}, 1), pumpDone: make(chan struct{}), subs: map[ulid.ULID]*liveSession{}}
}

// send enqueues msg without blocking.
func (cn *conn) send(msg any) {
	cn.mu.Lock()
	if cn.stopped {
		cn.mu.Unlock()
		return
	}
	cn.queue = append(cn.queue, outbound{msg: msg})
	cn.mu.Unlock()
	cn.wakePump()
}

// sendAndWait enqueues msg in order with every other response and notification, then waits
// until the transport has physically accepted it. Shutdown uses this barrier before it
// cancels anything that could stop the pump.
func (cn *conn) sendAndWait(ctx context.Context, msg any) error {
	sent := make(chan error, 1)
	cn.mu.Lock()
	if cn.stopped {
		err := cn.pumpErr
		cn.mu.Unlock()
		return err
	}
	cn.queue = append(cn.queue, outbound{msg: msg, sent: sent})
	cn.mu.Unlock()
	cn.wakePump()
	stopAbort := context.AfterFunc(ctx, func() {
		if cn.abortPump != nil {
			cn.abortPump()
		}
	})
	defer stopAbort()
	return <-sent
}

func (cn *conn) wakePump() {
	select {
	case cn.wake <- struct{}{}:
	default:
	}
}

// notify enqueues a server-to-client notification (a request with no id). A marshal failure
// is logged and the notification dropped rather than returned: every params type this plan
// sends is known to marshal, so this should never fire, but silently losing a notification the
// client is waiting on (a permission question, a state change) is worse than a stderr line.
func (cn *conn) notify(method string, params any) {
	req, err := protocol.NewNotification(method, params)
	if err != nil {
		slog.Error("server: marshal notification", "conn", cn.id, "method", method, "err", err)
		return
	}
	cn.send(req)
}

// isAsker reports whether this connection is one a permission question may be put to: its
// hello declared asker, and it is a client rather than a plugin. handleHello refuses a
// plugin's claim at the door and every routing walk asks this again, so the two can never
// disagree about who counts (see liveSession.askers for why a plugin never does).
func (cn *conn) isAsker() bool { return cn.asker && cn.plugin == "" }

// subscribed reports whether this connection holds sid. Self-locking (takes cn.mu, which is the
// innermost lock in the process: every server lock may nest it, and nothing is ever taken while
// it is held). Server.mu nests it in subscribeLocked and childAttachmentsLocked, and a
// liveSession's obsMu nests it in broadcastObsLocked (through notify) and in watchers, which
// asks this very question about each of a parent's connections. Reversing that, holding cn.mu
// and then reaching for a session's lock, is what would deadlock.
func (cn *conn) subscribed(sid ulid.ULID) bool {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	_, ok := cn.subs[sid]
	return ok
}

// claimShutdownProof marks this connection as the one that asked for shutdown and is holding
// its writer open for server.stopped. Set once the shutdown claim is won, before the response
// is written, which is the moment from which the proof is owed (ADR 0031).
func (cn *conn) claimShutdownProof() {
	cn.mu.Lock()
	cn.shutdownProof = true
	cn.mu.Unlock()
}

// keepsShutdownProof reports whether this connection owes the process its shutdown proof.
func (cn *conn) keepsShutdownProof() bool {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	return cn.shutdownProof
}

// pump drains the outbox to the transport, in order, until ctx ends or a send fails.
func (cn *conn) pump(ctx context.Context) {
	var stopErr error
	defer func() { cn.stopPump(stopErr) }()
	for {
		select {
		case <-ctx.Done():
			stopErr = ctx.Err()
			return
		case <-cn.wake:
		}
		for {
			cn.mu.Lock()
			if len(cn.queue) == 0 {
				cn.mu.Unlock()
				break
			}
			item := cn.queue[0]
			cn.queue = cn.queue[1:]
			cn.mu.Unlock()
			err := cn.c.Send(ctx, item.msg)
			if item.sent != nil {
				item.sent <- err
			}
			if err != nil {
				stopErr = err
				return
			}
		}
	}
}

// closeNow tears this connection down: the pump stops and the transport closes, so whatever
// the client had in flight fails and its next read is EOF. It is what a Session quarantine
// does to every connection that held the Session (ADR 0037), and it says nothing about why:
// the cause is the Server log's. abortPump is written before the connection is reachable from
// s.conns or any subscriber list (see serveConn), which is what makes reading it here safe.
func (cn *conn) closeNow() {
	if cn.abortPump != nil {
		cn.abortPump()
	}
	_ = cn.c.Close()
}

func (cn *conn) stopPump(err error) {
	cn.mu.Lock()
	cn.stopped = true
	cn.pumpErr = err
	pending := cn.queue
	cn.queue = nil
	cn.mu.Unlock()
	for _, item := range pending {
		if item.sent != nil {
			item.sent <- err
		}
	}
	close(cn.pumpDone)
}
