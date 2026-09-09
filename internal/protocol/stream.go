package protocol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// maxLineBytes caps one message. A spawned plugin that writes a line longer than this has
// gone wrong in a way no amount of buffering fixes, and the alternative is letting a child
// grow the parent's heap without bound.
const maxLineBytes = 16 << 20

// readBufBytes is the reader's buffer. Big enough that an ordinary message is one read and
// small enough that a plugin doing nothing costs 64KB.
const readBufBytes = 64 << 10

// errLineTooLong ends the connection: a message that will not fit cannot be skipped either,
// since the rest of its bytes are not a message boundary the parser can trust.
var errLineTooLong = fmt.Errorf("protocol: message line too long (over %d bytes)", maxLineBytes)

// streamConn is newline-delimited JSON over a reader and a writer, the transport a spawned
// plugin speaks on its stdin and stdout. One reader goroutine feeds a channel so Recv can
// honor its context: an io.Reader has no way to be interrupted, so the read itself is never
// what a cancelled Recv waits on.
type streamConn struct {
	w      io.Writer
	closer io.Closer
	wmu    sync.Mutex

	lines chan json.RawMessage

	errMu   sync.Mutex
	readErr error // why the reader stopped; nil means end of input

	closed chan struct{}
	once   sync.Once
}

// NewStreamConn returns a Conn that reads newline-delimited JSON from r, writes it to w and
// closes closer on Close. closer may be nil.
func NewStreamConn(r io.Reader, w io.Writer, closer io.Closer) Conn {
	c := &streamConn{
		w:      w,
		closer: closer,
		lines:  make(chan json.RawMessage),
		closed: make(chan struct{}),
	}
	go c.read(r)
	return c
}

func (c *streamConn) read(r io.Reader) {
	defer close(c.lines)
	br := bufio.NewReaderSize(r, readBufBytes)
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			select {
			case c.lines <- line:
			case <-c.closed:
				return
			}
		}
		if err != nil {
			c.setReadErr(err)
			return
		}
	}
}

// readLine returns one newline-terminated line with the terminator and any surrounding
// space trimmed, or an error. A line over the cap ends the connection.
func readLine(br *bufio.Reader) (json.RawMessage, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(buf)+len(chunk) > maxLineBytes {
			return nil, errLineTooLong
		}
		buf = append(buf, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimSpace(buf), err
	}
}

func (c *streamConn) setReadErr(err error) {
	c.errMu.Lock()
	c.readErr = err
	c.errMu.Unlock()
}

// endErr is what Recv returns once the reader has stopped: io.EOF for an orderly end of
// input, the reader's own error otherwise.
func (c *streamConn) endErr() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if c.readErr == nil || errors.Is(c.readErr, io.EOF) {
		return io.EOF
	}
	return c.readErr
}

func (c *streamConn) Recv(ctx context.Context) (json.RawMessage, error) {
	select {
	case <-c.closed:
		return nil, ErrConnClosed
	default:
	}
	select {
	case line, ok := <-c.lines:
		if !ok {
			return nil, c.endErr()
		}
		return line, nil
	case <-c.closed:
		return nil, ErrConnClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *streamConn) Send(ctx context.Context, msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	select {
	case <-c.closed:
		return ErrConnClosed
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// One Write per message, under the mutex: a partial interleaving of two messages is not
	// something the peer's parser can recover from.
	_, err = c.w.Write(b)
	return err
}

func (c *streamConn) Close() error {
	var err error
	c.once.Do(func() {
		close(c.closed)
		if c.closer != nil {
			err = c.closer.Close()
		}
	})
	return err
}
