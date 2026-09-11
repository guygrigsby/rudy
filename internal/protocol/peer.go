package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Peer splits one Conn into our outgoing requests (Client) and their incoming requests and
// notifications (Incoming). One reader goroutine routes each message by shape: a response
// (an id, no method) goes to the Client's pending call, anything with a method goes to the
// Incoming queue. Incoming.Send writes our responses and notifications back on the same
// Conn, so both directions share one transport and one reader.
//
// It is what the server and a spawned plugin each hold: the server calls the plugin
// (plugin.init, tool.invoke, hook.fire) through Client while serving the plugin's own
// requests (plugin.register_tool, plugin.append_note) off Incoming.
type Peer struct {
	c    Conn
	cl   *Client
	inc  *queueConn
	once sync.Once
}

// NewPeer starts the reader. The Peer owns c: closing the Peer, or its Client, closes it.
func NewPeer(c Conn) *Peer {
	p := &Peer{c: c}
	p.inc = &queueConn{
		out:      c,
		wake:     make(chan struct{}, 1),
		finished: make(chan struct{}),
		closed:   make(chan struct{}),
	}
	p.inc.closeFn = p.Close
	p.cl = newClientOn(c.Send, p.Close)
	go p.read()
	return p
}

// Client is our side: the requests we make of them.
func (p *Peer) Client() *Client { return p.cl }

// Incoming is their side: Recv pops their requests and notifications in arrival order, Send
// writes our responses and notifications back. The server's serve loop runs over it
// unchanged.
func (p *Peer) Incoming() Conn { return p.inc }

// Sync returns once the Incoming consumer has taken every message that arrived before this
// call, or when ctx ends. It is the ordering barrier between the two halves: a notification
// the peer sent before a response has been handed to the consumer by the time a caller that
// has the response calls this. Without it the two halves race, and a provider's last deltas
// could be delivered after the completion they belong to has already ended.
func (p *Peer) Sync(ctx context.Context) error { return p.inc.sync(ctx) }

// Close closes the underlying Conn once. The reader then ends, pending calls fail and
// Incoming.Recv returns.
func (p *Peer) Close() error {
	var err error
	p.once.Do(func() {
		err = p.c.Close()
		p.inc.shutdown()
	})
	return err
}

// read is the one reader. Responses go straight to the Client's pending call, everything with
// a method to the Incoming queue.
func (p *Peer) read() {
	defer p.cl.finishReading()
	ctx := context.Background()
	for {
		raw, err := p.c.Recv(ctx)
		if err != nil {
			p.end(err)
			return
		}
		var m incoming
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		if m.Method != "" {
			if err := p.inc.push(raw); err != nil {
				p.end(err)
				return
			}
			continue
		}
		p.cl.deliver(Response{JSONRPC: Version, ID: m.ID, Result: m.Result, Error: m.Error})
	}
}

// maxIncoming caps the Incoming queue. The queue is what lets the reader run ahead of a busy
// consumer; without a cap it is also what lets a hostile or broken child turn the parent's
// memory into its own buffer, which is exactly the backpressure the stream transport has and
// this would throw away. A peer that gets this far ahead is not one worth keeping.
const maxIncoming = 4096

// ErrIncomingOverflow ends a connection whose peer sent more unread messages than the queue
// holds.
var ErrIncomingOverflow = fmt.Errorf("protocol: peer sent more than %d unread messages", maxIncoming)

// end stops the peer: pending calls fail, the consumer drains what is queued and then sees
// err, and the transport is closed. Closing it is what tells the other side to stop, which
// matters most when the transport is a child's stdio and the child is still running: without
// it a peer whose reader has given up leaves the child talking to nobody.
func (p *Peer) end(err error) {
	p.cl.fail(err)
	p.inc.stop(err)
	_ = p.c.Close()
}

// queued is one message waiting for the Incoming consumer, or a barrier: a marker with no
// message whose channel is closed when the consumer reaches it (see Peer.Sync).
type queued struct {
	msg     json.RawMessage
	barrier chan struct{}
}

// queueConn is the Incoming half of a Peer: a Conn whose Recv pops the queue the reader
// fills and whose Send writes to the shared Conn. The queue is in arrival order and the
// reader never blocks behind a slow consumer; it is capped at maxIncoming, past which the
// reader ends the connection rather than buffering a peer's whole output.
type queueConn struct {
	out     Conn
	closeFn func() error

	mu   sync.Mutex
	q    []queued
	err  error // why the reader stopped
	done bool  // the reader has stopped; the queue is final

	wake     chan struct{} // 1-buffered: something was queued
	finished chan struct{} // closed by stop
	closed   chan struct{} // closed by shutdown
	stopOnce sync.Once
	downOnce sync.Once
}

// push queues one message for the consumer. It never blocks; past the cap it refuses, and the
// reader ends the connection.
func (q *queueConn) push(msg json.RawMessage) error {
	q.mu.Lock()
	if len(q.q) >= maxIncoming {
		q.mu.Unlock()
		return ErrIncomingOverflow
	}
	q.q = append(q.q, queued{msg: msg})
	q.mu.Unlock()
	q.signal()
	return nil
}

func (q *queueConn) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// stop marks the queue final. Whatever is still queued is still delivered; err is what Recv
// returns once it is empty.
func (q *queueConn) stop(err error) {
	q.mu.Lock()
	q.err = err
	q.done = true
	q.mu.Unlock()
	q.stopOnce.Do(func() { close(q.finished) })
}

func (q *queueConn) shutdown() {
	q.downOnce.Do(func() { close(q.closed) })
}

func (q *queueConn) Recv(ctx context.Context) (json.RawMessage, error) {
	for {
		q.mu.Lock()
		for len(q.q) > 0 {
			it := q.q[0]
			q.q = q.q[1:]
			if it.barrier != nil {
				close(it.barrier)
				continue
			}
			q.mu.Unlock()
			return it.msg, nil
		}
		err, done := q.err, q.done
		q.mu.Unlock()
		if done {
			if err == nil || errors.Is(err, ErrConnClosed) {
				err = io.EOF
			}
			return nil, err
		}
		select {
		case <-q.wake:
		case <-q.finished:
		case <-q.closed:
			return nil, ErrConnClosed
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Send writes on the shared Conn: our responses to their requests, and our notifications.
func (q *queueConn) Send(ctx context.Context, msg any) error {
	select {
	case <-q.closed:
		return ErrConnClosed
	default:
	}
	return q.out.Send(ctx, msg)
}

// Close closes the whole Peer: the Incoming half has no transport of its own.
func (q *queueConn) Close() error { return q.closeFn() }

// sync queues a barrier and waits for the consumer to reach it.
func (q *queueConn) sync(ctx context.Context) error {
	b := make(chan struct{})
	q.mu.Lock()
	if q.done {
		q.mu.Unlock()
		return nil
	}
	q.q = append(q.q, queued{barrier: b})
	q.mu.Unlock()
	q.signal()
	select {
	case <-b:
		return nil
	case <-q.finished:
		return nil
	case <-q.closed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
