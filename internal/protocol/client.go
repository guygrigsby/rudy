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

// Client multiplexes calls and notifications over one Conn. Callers must drain
// Notifications; the channel holds 256 before the reader blocks.
type Client struct {
	conn    Conn
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan Response
	notes   chan Notification
	done    chan struct{}
	err     error
	once    sync.Once
}

// NewClient starts the reader goroutine.
func NewClient(conn Conn) *Client {
	c := &Client{
		conn:    conn,
		pending: make(map[int64]chan Response),
		notes:   make(chan Notification, 256),
		done:    make(chan struct{}),
	}
	go c.read()
	return c
}

// Notifications yields server notifications in arrival order. It is closed when the
// connection ends.
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

// Close ends the connection and the reader.
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

func (c *Client) read() {
	defer close(c.notes)
	defer close(c.done)
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
			c.notes <- Notification{Method: m.Method, Params: m.Params}
		case m.Method != "":
			// A server-to-client request. Nothing in this plan handles one; answer so the
			// server does not hang.
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

func (c *Client) fail(err error) {
	if errors.Is(err, ErrConnClosed) {
		err = io.EOF
	}
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
}
