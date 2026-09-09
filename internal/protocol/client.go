package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

// Notification is a server-to-client message without an id.
type Notification struct {
	Method string
	Params json.RawMessage
}

// Client multiplexes calls and notifications over one transport. Notifications are queued
// in memory without bound between the reader and Notifications, so a slow or absent
// consumer never stalls a pending Call; it only grows the queue, until Close, which
// drops whatever is still queued rather than waiting the consumer out.
type Client struct {
	// send and closeFn are the transport. They are functions rather than a Conn because a
	// Peer feeds one Client while also serving the peer's own requests off the same Conn:
	// the reader lives there, and this Client only ever sends and closes.
	send    func(ctx context.Context, msg any) error
	closeFn func() error

	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan Response

	notes chan Notification // 256-buffered; fed by drain, exposed by Notifications

	qmu      sync.Mutex
	qcond    *sync.Cond
	queue    []Notification // unbounded, in arrival order
	readDone bool           // set once read has returned; queue is final from then on

	done   chan struct{} // closed once read has returned
	closed chan struct{} // closed exactly once, by Close
	err    error
	once   sync.Once
}

// responseBuffer is how many responses the reader may run ahead of the routing goroutine
// before it waits. Routing never blocks, so this only absorbs scheduling jitter.
const responseBuffer = 64

// NewClient starts the reader, the routing and the notification drain goroutines.
func NewClient(conn Conn) *Client {
	responses := make(chan Response, responseBuffer)
	c := newClientOn(conn.Send, conn.Close, responses)
	go c.read(conn, responses)
	return c
}

// newClientOn builds a Client over a transport somebody else reads: send writes one message,
// closeFn ends the connection and responses is fed with every response that arrives, closed
// when the feeder stops. It is what Peer uses; NewClient is this plus its own reader.
func newClientOn(send func(ctx context.Context, msg any) error, closeFn func() error, responses <-chan Response) *Client {
	c := &Client{
		send:    send,
		closeFn: closeFn,
		pending: make(map[int64]chan Response),
		notes:   make(chan Notification, 256),
		done:    make(chan struct{}),
		closed:  make(chan struct{}),
	}
	c.qcond = sync.NewCond(&c.qmu)
	go c.route(responses)
	go c.drain()
	return c
}

// Notifications yields server notifications in arrival order. The channel closes once
// the connection has ended and every notification already received has drained into it,
// or once Close runs, whichever comes first: a notification still queued when Close is
// called may never reach this channel. See Close.
func (c *Client) Notifications() <-chan Notification { return c.notes }

// Call sends a request and waits for its response. A JSON-RPC error is returned as
// *Error. result may be nil.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	id := c.nextID.Add(1)
	req, err := NewRequest(id, method, params)
	if err != nil {
		return err
	}
	ch := make(chan Response, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return c.err
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.send(ctx, req); err != nil {
		return err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.err != nil {
			return c.err
		}
		return io.EOF
	}
}

// Close ends the connection. The reader then stops, and the drain goroutine follows: it
// delivers whatever it can hand off promptly, but a notification not yet consumed when
// Close is called may be dropped rather than delivered. That is fine: a client that
// closes is done with the session and does not need them.
func (c *Client) Close() error {
	var err error
	c.once.Do(func() {
		err = c.closeFn()
		close(c.closed)
	})
	return err
}

type incoming struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
}

// read is the reader for a Client that owns its Conn: it appends notifications to the queue
// itself and hands responses to route. It never blocks on a slow Notifications consumer, so
// one pending Call always gets routed.
func (c *Client) read(conn Conn, responses chan<- Response) {
	defer close(responses)
	ctx := context.Background()
	for {
		raw, err := conn.Recv(ctx)
		if err != nil {
			c.fail(err)
			return
		}
		var m incoming
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) == 0:
			c.enqueueNotification(Notification{Method: m.Method, Params: m.Params})
		case m.Method != "":
			// A server-to-client request. A plain Client handles none; answer so the server
			// does not hang. The error is discarded: a broken conn surfaces on the next Recv
			// above and stops the reader there. A Peer's Client never sees these: the Peer
			// routes anything with a method to its Incoming queue instead.
			resp := NewErrorResponse(m.ID, NewError(CodeMethodNotFound, "client handles no requests", nil))
			_ = conn.Send(ctx, resp)
		default:
			responses <- Response{JSONRPC: Version, ID: m.ID, Result: m.Result, Error: m.Error}
		}
	}
}

// route hands each response to the Call waiting for it. The per-call channel is buffered, so
// this never blocks; a response for a call that has already given up, or a second response
// for one id, is dropped. When the feeder closes responses the client is finished: pending
// calls are released by finishReading closing done.
func (c *Client) route(responses <-chan Response) {
	defer c.finishReading()
	for resp := range responses {
		id, err := strconv.ParseInt(string(resp.ID), 10, 64)
		if err != nil {
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[id]
		c.mu.Unlock()
		if !ok {
			continue
		}
		select {
		case ch <- resp:
		default:
		}
	}
}

// enqueueNotification appends to the unbounded queue and wakes drain. It never blocks.
func (c *Client) enqueueNotification(n Notification) {
	c.qmu.Lock()
	c.queue = append(c.queue, n)
	c.qmu.Unlock()
	c.qcond.Signal()
}

// finishReading marks the client finished and wakes drain so it can notice the queue is
// final and, once empty, exit.
func (c *Client) finishReading() {
	close(c.done)
	c.qmu.Lock()
	c.readDone = true
	c.qmu.Unlock()
	c.qcond.Broadcast()
}

// drain moves queued notifications into notes one at a time, in order. Sending on notes
// may block on a slow consumer, but that only blocks drain, never read. drain exits and
// closes notes once the reader has finished and the queue is empty, or as soon as Close
// fires: a slow or absent consumer must never leak drain parked on a full notes channel,
// so Close discards whatever remains queued instead of waiting the consumer out.
func (c *Client) drain() {
	defer close(c.notes)
	for {
		c.qmu.Lock()
		for len(c.queue) == 0 && !c.readDone {
			c.qcond.Wait()
		}
		if len(c.queue) == 0 {
			c.qmu.Unlock()
			return
		}
		n := c.queue[0]
		c.queue = c.queue[1:]
		c.qmu.Unlock()
		select {
		case c.notes <- n:
		case <-c.closed:
			return
		}
	}
}

func (c *Client) fail(err error) {
	if errors.Is(err, ErrConnClosed) {
		err = io.EOF
	}
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
}
