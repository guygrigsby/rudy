// SPDX-License-Identifier: AGPL-3.0-or-later

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
	"time"
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
	return writeContext(ctx, c.w, b)
}

type writeDeadliner interface {
	SetWriteDeadline(time.Time) error
}

// writeContext makes a blocked socket write answer cancellation. Writers without deadline
// support retain the synchronous behavior they had before; daemon and dialed Unix sockets
// implement writeDeadliner, which is the trust boundary where bounded shutdown matters.
func writeContext(ctx context.Context, w io.Writer, b []byte) error {
	d, ok := w.(writeDeadliner)
	if !ok {
		n, err := w.Write(b)
		if n != len(b) && err == nil {
			err = io.ErrShortWrite
		}
		return err
	}
	deadline, hasDeadline := ctx.Deadline()
	writeDeadline := time.Time{}
	if hasDeadline {
		writeDeadline = deadline
	}
	if err := d.SetWriteDeadline(writeDeadline); err != nil {
		n, writeErr := w.Write(b)
		if n != len(b) && writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return writeErr
	}
	fired := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = d.SetWriteDeadline(time.Now())
		close(fired)
	})
	n, writeErr := w.Write(b)
	if n != len(b) && writeErr == nil {
		writeErr = io.ErrShortWrite
	}
	if !stop() {
		<-fired
	}
	clearErr := d.SetWriteDeadline(time.Time{})
	if writeErr == nil {
		// The complete message reached the transport. A simultaneous cancellation cannot
		// turn that physical fact back into failure; shutdown commits from this result.
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(writeErr, ctxErr)
	}
	if hasDeadline && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return errors.Join(writeErr, clearErr)
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

// TailBytes is how much of a stdio child's stderr is worth keeping: enough for the notice it
// printed on its way out, and bounded because the other end is a process that may decide to
// talk for as long as it likes.
const TailBytes = 4096

// Tail keeps the last max bytes written to it. Every child process this package carries a
// stream for points its stderr here: a spawned plugin, ssh on the way to a host. The end of
// a child's stderr is where it says why it went, and it is the only account of a failure
// that never reached the protocol.
type Tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func NewTail(max int) *Tail { return &Tail{max: max} }

func (t *Tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *Tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
