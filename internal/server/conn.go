package server

import (
	"context"
	"log"
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
	hello   bool
	greeted bool
	asker   bool
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

	mu    sync.Mutex
	queue []any
	wake  chan struct{}
	subs  map[ulid.ULID]*liveSession
}

func newConn(id int, c protocol.Conn) *conn {
	return &conn{id: id, c: c, wake: make(chan struct{}, 1), subs: map[ulid.ULID]*liveSession{}}
}

// send enqueues msg without blocking.
func (cn *conn) send(msg any) {
	cn.mu.Lock()
	cn.queue = append(cn.queue, msg)
	cn.mu.Unlock()
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
		log.Printf("server: conn %d: marshal %s notification: %v", cn.id, method, err)
		return
	}
	cn.send(req)
}

// isAsker reports whether this connection is one a permission question may be put to: its
// hello declared asker, and it is a client rather than a plugin. handleHello refuses a
// plugin's claim at the door and every routing walk asks this again, so the two can never
// disagree about who counts (see liveSession.askers for why a plugin never does).
func (cn *conn) isAsker() bool { return cn.asker && cn.plugin == "" }

// subscribed reports whether this connection holds sid. Self-locking (takes cn.mu, which
// is never nested inside any of the server's locks).
func (cn *conn) subscribed(sid ulid.ULID) bool {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	_, ok := cn.subs[sid]
	return ok
}

// pump drains the outbox to the transport, in order, until ctx ends or a send fails.
func (cn *conn) pump(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-cn.wake:
		}
		for {
			cn.mu.Lock()
			if len(cn.queue) == 0 {
				cn.mu.Unlock()
				break
			}
			msg := cn.queue[0]
			cn.queue = cn.queue[1:]
			cn.mu.Unlock()
			if err := cn.c.Send(ctx, msg); err != nil {
				return
			}
		}
	}
}
