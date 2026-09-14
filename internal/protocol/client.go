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

// NewClient starts the reader and the notification drain goroutine.
func NewClient(conn Conn) *Client {
	c := newClientOn(conn.Send, conn.Close)
	go c.read(conn)
	return c
}

// newClientOn builds a Client over a transport somebody else reads: send writes one message,
// closeFn ends the connection, and whoever reads hands every response to deliver and calls
// finishReading when it stops. It is what Peer uses; NewClient is this plus its own reader.
func newClientOn(send func(ctx context.Context, msg any) error, closeFn func() error) *Client {
	c := &Client{
		send:    send,
		closeFn: closeFn,
		pending: make(map[int64]chan Response),
		notes:   make(chan Notification, 256),
		done:    make(chan struct{}),
		closed:  make(chan struct{}),
	}
	c.qcond = sync.NewCond(&c.qmu)
	go c.drain()
	return c
}

// Notifications yields server notifications in arrival order. The channel closes once
// the connection has ended and every notification already received has drained into it,
// or once Close runs, whichever comes first: a notification still queued when Close is
// called may never reach this channel. See Close.
func (c *Client) Notifications() <-chan Notification { return c.notes }

// Wait blocks until the peer ends the connection or ctx ends. An orderly peer close is EOF;
// a transport failure is returned unchanged. Connection completion already observed wins a
// simultaneous context cancellation, matching Call's response-first rule.
func (c *Client) Wait(ctx context.Context) error {
	select {
	case <-c.done:
		return c.endErr()
	default:
	}
	select {
	case <-c.done:
		return c.endErr()
	case <-ctx.Done():
		select {
		case <-c.done:
			return c.endErr()
		default:
			return ctx.Err()
		}
	}
}

func (c *Client) endErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return io.EOF
}

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
	// Registered first, so LIFO runs it last: after the c.done branch below has released mu
	// with its own defer. The order is load-bearing, not incidental. Moving this below that
	// branch's `defer c.mu.Unlock()`, or turning either into an inline call, deadlocks on a
	// mutex that is not reentrant.
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
		return unpack(resp, result)
	case <-ctx.Done():
		// An answer that already arrived beats the reason for giving up on it. Both cases are
		// ready whenever a call is cancelled just as its response lands, and a select between
		// ready cases picks at random, so without this second look a call that in fact
		// succeeded reports context.Canceled about half the time and its result is discarded.
		// That is what the agent tool hit: a cancelled call's session.submit came back with
		// the child's turn id, reported itself cancelled, and so never interrupted the child
		// it had just started.
		if resp, ok := delivered(ch); ok {
			return unpack(resp, result)
		}
		return ctx.Err()
	case <-c.done:
		if resp, ok := delivered(ch); ok {
			return unpack(resp, result)
		}
		// Unlocked by this defer before the pending-map cleanup registered above runs, which
		// is the whole reason that one is a defer and not a line at the end of this branch.
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.err != nil {
			return c.err
		}
		return io.EOF
	}
}

// delivered takes the response already sitting in ch, if there is one. The channel holds one
// and only this call reads it, so nothing else can take it in between.
func delivered(ch chan Response) (Response, bool) {
	select {
	case resp := <-ch:
		return resp, true
	default:
		return Response{}, false
	}
}

// unpack is one response as the call's return: its error, or its result decoded into out.
func unpack(resp Response, out any) error {
	if resp.Error != nil {
		return resp.Error
	}
	if out != nil && len(resp.Result) > 0 {
		return json.Unmarshal(resp.Result, out)
	}
	return nil
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
// itself and hands responses straight to deliver. Neither blocks on a slow Notifications
// consumer, so one pending Call always gets its answer.
func (c *Client) read(conn Conn) {
	defer c.finishReading()
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
			c.deliver(Response{JSONRPC: Version, ID: m.ID, Result: m.Result, Error: m.Error})
		}
	}
}

// deliver hands one response to the Call waiting for it. The per-call channel is buffered, so
// this never blocks; a response for a call that has already given up, or a second response for
// one id, is dropped. It runs on the reader rather than on a goroutine of its own, so a reader
// that has taken a message has already made every response before it visible to its Call: with
// a queue in between, a call cancelled at that moment could not see an answer that had in fact
// arrived, and would report itself cancelled instead.
func (c *Client) deliver(resp Response) {
	id, err := strconv.ParseInt(string(resp.ID), 10, 64)
	if err != nil {
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[id]
	c.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- resp:
	default:
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
