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

// Client multiplexes calls and notifications over one Conn. Notifications are queued
// in memory without bound between the reader and Notifications, so a slow or absent
// consumer never stalls a pending Call; it only grows the queue.
type Client struct {
	conn    Conn
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan Response

	notes chan Notification // 256-buffered; fed by drain, exposed by Notifications

	qmu      sync.Mutex
	qcond    *sync.Cond
	queue    []Notification // unbounded, in arrival order
	readDone bool           // set once read has returned; queue is final from then on

	done chan struct{} // closed once read has returned
	err  error
	once sync.Once
}

// NewClient starts the reader and the notification drain goroutines.
func NewClient(conn Conn) *Client {
	c := &Client{
		conn:    conn,
		pending: make(map[int64]chan Response),
		notes:   make(chan Notification, 256),
		done:    make(chan struct{}),
	}
	c.qcond = sync.NewCond(&c.qmu)
	go c.read()
	go c.drain()
	return c
}

// Notifications yields server notifications in arrival order. The channel closes once
// the connection has ended and every notification already received has drained into it;
// nothing queued is ever dropped.
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

	if err := c.conn.Send(ctx, req); err != nil {
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

// Close ends the connection. The reader then stops, and the drain goroutine follows it
// once every already-queued notification has been delivered.
func (c *Client) Close() error {
	var err error
	c.once.Do(func() { err = c.conn.Close() })
	return err
}

type incoming struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
}

// read only ever appends to the notification queue or a buffered per-call channel; it
// never blocks on a slow Notifications consumer, so one pending Call always gets routed.
func (c *Client) read() {
	defer c.finishReading()
	ctx := context.Background()
	for {
		raw, err := c.conn.Recv(ctx)
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
			// A server-to-client request. Nothing in this plan handles one; answer so the
			// server does not hang. The error is discarded: a broken conn surfaces on the
			// next Recv above and stops the reader there.
			resp := NewErrorResponse(m.ID, NewError(CodeMethodNotFound, "client handles no requests", nil))
			_ = c.conn.Send(ctx, resp)
		default:
			id, err := strconv.ParseInt(string(m.ID), 10, 64)
			if err != nil {
				continue
			}
			c.mu.Lock()
			ch, ok := c.pending[id]
			c.mu.Unlock()
			if ok {
				ch <- Response{JSONRPC: Version, ID: m.ID, Result: m.Result, Error: m.Error}
			}
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

// finishReading marks the reader done and wakes drain so it can notice the queue is
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
// closes notes once the reader has finished and the queue is empty.
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
		c.notes <- n
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
