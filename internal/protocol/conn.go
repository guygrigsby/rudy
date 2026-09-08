package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// Conn carries one JSON message at a time in either direction.
type Conn interface {
	Send(ctx context.Context, msg any) error
	Recv(ctx context.Context) (json.RawMessage, error)
	Close() error
}

// ErrConnClosed is returned by Send and Recv on a Conn the caller already closed.
var ErrConnClosed = errors.New("protocol: connection closed")

type pipeConn struct {
	out        chan<- json.RawMessage
	in         <-chan json.RawMessage
	closed     chan struct{}
	peerClosed chan struct{}
	once       sync.Once
}

// Pipe returns two connected in-memory Conns. Messages are unbuffered, so Send returns
// only once the peer has received. Closing one end makes the peer's Recv return io.EOF.
func Pipe() (client Conn, server Conn) {
	c2s := make(chan json.RawMessage)
	s2c := make(chan json.RawMessage)
	cClosed := make(chan struct{})
	sClosed := make(chan struct{})
	client = &pipeConn{out: c2s, in: s2c, closed: cClosed, peerClosed: sClosed}
	server = &pipeConn{out: s2c, in: c2s, closed: sClosed, peerClosed: cClosed}
	return client, server
}

func (p *pipeConn) Send(ctx context.Context, msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	select {
	case <-p.closed:
		return ErrConnClosed
	case <-p.peerClosed:
		return io.ErrClosedPipe
	default:
	}
	select {
	case p.out <- b:
		return nil
	case <-p.closed:
		return ErrConnClosed
	case <-p.peerClosed:
		return io.ErrClosedPipe
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *pipeConn) Recv(ctx context.Context) (json.RawMessage, error) {
	select {
	case b := <-p.in:
		return b, nil
	case <-p.closed:
		return nil, ErrConnClosed
	case <-p.peerClosed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *pipeConn) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}
