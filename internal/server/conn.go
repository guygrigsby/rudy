package server

import (
	"context"
	"sync"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/protocol"
)

// conn is one client connection. Everything sent to the client goes through the outbox so
// notifications and responses leave in the order they were produced, and a slow client never
// blocks whoever is producing them (the turn.Runner's Observer callbacks, in particular).
type conn struct {
	id    int
	c     protocol.Conn
	hello bool
	asker bool

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
// is dropped rather than returned: every params type this plan sends is known to marshal.
func (cn *conn) notify(method string, params any) {
	req, err := protocol.NewNotification(method, params)
	if err != nil {
		return
	}
	cn.send(req)
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
