package acpwire

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"sync"

	"github.com/guygrigsby/rudy/internal/strictjson"
)

// Receipt acknowledges a complete frame and its LF reaching the physical writer.
// Tag belongs to the caller, for event sequences and terminal shutdown carriers.
type Receipt struct {
	Tag  uint64
	done chan struct{}
	once sync.Once
	err  error
}

func newReceipt(tag uint64) *Receipt { return &Receipt{Tag: tag, done: make(chan struct{})} }
func (r *Receipt) finish(err error)  { r.once.Do(func() { r.err = err; close(r.done) }) }
func (r *Receipt) Wait(ctx context.Context) error {
	select {
	case <-r.done:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type carrier struct {
	raw     []byte
	ack     *Receipt
	request *Request
}
type frameWriter struct {
	mu      sync.Mutex
	wire    *Wire
	partial []byte
}

func (f *frameWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.wire.closed() {
		return 0, ErrClosed
	}
	total := len(p)
	for len(p) > 0 {
		index := bytes.IndexByte(p, '\n')
		size := len(p)
		if index >= 0 {
			size = index
		}
		f.wire.mu.Lock()
		if f.wire.closed() {
			f.wire.mu.Unlock()
			return 0, ErrClosed
		}
		if len(f.partial)+size > MaxFrame {
			f.partial = nil
			f.wire.mu.Unlock()
			f.wire.fail(ErrFrame)
			return 0, ErrFrame
		}
		f.partial = append(f.partial, p[:size]...)
		if index < 0 {
			f.wire.mu.Unlock()
			return total, nil
		}
		raw := f.partial
		f.partial = nil
		f.wire.mu.Unlock()
		if err := f.wire.sendSDK(raw); err != nil {
			return 0, err
		}
		p = p[index+1:]
	}
	return total, nil
}

func (w *Wire) sendSDK(raw []byte) error {
	if len(raw) > MaxFrame || strictjson.Scan(raw, strictjson.Outer) != nil {
		w.fail(ErrEnvelope)
		return ErrEnvelope
	}
	var routing struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if json.Unmarshal(raw, &routing) != nil {
		w.fail(ErrEnvelope)
		return ErrEnvelope
	}
	// Invoke emits its own wire-id cancellation before cancelling the SDK
	// context. Discard the SDK's asynchronously queued duplicate (its id is local).
	if routing.Method == "$/cancel_request" {
		return nil
	}
	if routing.Method == "" && bytes.Equal(bytes.TrimSpace(routing.ID), []byte(sdkNullID)) {
		raw = replaceID(raw, []byte("null"))
	}
	if routing.Method != "" && len(routing.ID) > 0 {
		sdkID, err := ParseID(routing.ID)
		n, nerr := strconv.ParseInt(sdkID.Value, 10, 64)
		w.mu.Lock()
		o := w.sdkPending
		if err != nil || nerr != nil || sdkID.Kind != NumberID || n <= w.sdkHigh || n > MaxOutboundID || o == nil || o.method != routing.Method {
			w.mu.Unlock()
			w.fail(ErrEnvelope)
			return ErrEnvelope
		}
		w.sdkHigh = n
		o.sdkID = &sdkID
		if w.outbound[o.ID] != o {
			w.releaseInitiation(o)
			w.mu.Unlock()
			return nil
		}
		raw = replaceID(raw, o.ID.Raw())
		w.releaseInitiation(o)
		w.mu.Unlock()
	}
	_, err := w.sendFrame(raw, 0, false, true)
	return err
}
func (w *Wire) Send(raw []byte, tag uint64) (*Receipt, error) { return w.send(raw, tag, false) }
func responseKind(raw []byte) ResponseKind {
	var f struct {
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(raw, &f)
	if len(f.Error) == 0 {
		return ResultResponse
	}
	var e struct {
		Code json.RawMessage `json:"code"`
	}
	_ = json.Unmarshal(f.Error, &e)
	id, _ := ParseID(e.Code)
	if id.Kind == NumberID && id.Value == "-32800" {
		return CancelResponse
	}
	return ErrorResponse
}
func (w *Wire) send(raw []byte, tag uint64, readerError bool) (*Receipt, error) {
	return w.sendFrame(raw, tag, readerError, false)
}
func (w *Wire) sendFrame(raw []byte, tag uint64, readerError, allowRetired bool) (*Receipt, error) {
	if w.closed() {
		return nil, ErrClosed
	}
	e, err := parseEnvelope(raw, w.methods)
	if err != nil {
		w.fail(ErrEnvelope)
		return nil, ErrEnvelope
	}
	w.mu.Lock()
	if w.closed() {
		w.mu.Unlock()
		return nil, ErrClosed
	}
	if w.usage.QueuedFrames >= MaxItems || len(raw) > MaxBytes-w.usage.QueuedBytes {
		w.mu.Unlock()
		w.fail(ErrOverload)
		return nil, ErrOverload
	}
	var req *Request
	if e.kind == RequestFrame {
		out := w.outbound[e.id]
		if out == nil && allowRetired {
			ack := newReceipt(tag)
			ack.finish(ErrClosed)
			w.mu.Unlock()
			return ack, nil
		}
		if out == nil || len(raw) > len(out.Frame) {
			w.mu.Unlock()
			w.fail(ErrEnvelope)
			return nil, ErrEnvelope
		}
	}
	if e.kind == ResponseFrame && !readerError {
		req = w.inbound[e.id]
		kind := responseKind(raw)
		if req == nil || req.enqueued || (!req.reserved && !req.reserve(kind)) || (req.reserved && req.response != kind) {
			w.mu.Unlock()
			w.fail(ErrEnvelope)
			return nil, ErrEnvelope
		}
		req.enqueued = true
		w.claimRequest(req)
	}
	ack := newReceipt(tag)
	if e.kind == RequestFrame {
		out := w.outbound[e.id]
		out.enqueued = true
		ack = out.written
		if tag != 0 {
			ack.Tag = tag
		}
	}
	item := &carrier{raw: bytes.Clone(raw), ack: ack, request: req}
	w.usage.QueuedFrames++
	w.usage.QueuedBytes += len(raw)
	w.queue <- item
	w.mu.Unlock()
	return ack, nil
}
func (w *Wire) writeLoop() {
	defer func() {
		for {
			select {
			case item := <-w.queue:
				item.ack.finish(ErrClosed)
			default:
				return
			}
		}
	}()
	for {
		select {
		case <-w.done:
			return
		case item := <-w.queue:
			if w.closed() {
				item.ack.finish(ErrClosed)
				return
			}
			// One writer owns both writes, so the separate LF cannot interleave.
			n, err := w.sink.Write(item.raw)
			if err == nil && n != len(item.raw) {
				err = io.ErrShortWrite
			}
			if err == nil {
				n, err = w.sink.Write([]byte{'\n'})
				if err == nil && n != 1 {
					err = io.ErrShortWrite
				}
			}
			if err != nil {
				if w.closed() {
					item.ack.finish(ErrClosed)
					return
				}
				item.ack.finish(ErrWrite)
				w.fail(ErrWrite)
				return
			}
			w.mu.Lock()
			if !w.closed() {
				w.usage.QueuedFrames--
				w.usage.QueuedBytes -= len(item.raw)
				if r := item.request; r != nil && w.inbound[r.id] == r {
					delete(w.inbound, r.id)
					r.retired = true
					w.pruneCancels()
					w.usage.Requests--
					w.usage.InboundBytes -= r.bytes
					if r.reservation != nil {
						r.reservation.Release()
					}
					r.written.finish(nil)
				}
			}
			w.mu.Unlock()
			item.ack.finish(nil)
		}
	}
}
