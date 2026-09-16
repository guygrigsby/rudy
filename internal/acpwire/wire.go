package acpwire

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"

	"github.com/guygrigsby/rudy/internal/strictjson"
)

// Options hooks run synchronously before bytes reach the SDK. They must not
// block on handler work. Validator errors and transport errors never escape raw.
type Options struct {
	Methods              map[string]Kind
	ValidateNotification func(string, json.RawMessage) error
	BeforeDispatch       func(Dispatch)
	RequestGate          func(string) Admission
	// Required for SDK adapters: claim in the callback before any handler work.
	RequireRequestClaims bool
}
type Admission uint8

const (
	DispatchRequest Admission = iota
	NotReady
	MethodUnavailable
)

type Usage struct{ Requests, Notifications, Responses, InboundBytes, OutboundRequests, OutboundBytes, QueuedFrames, QueuedBytes int }
type Wire struct {
	mu             sync.Mutex
	source         io.ReadCloser
	sink           io.Writer
	reader         *bufio.Reader
	inputMu        sync.Mutex
	pending        []byte
	output         *frameWriter
	gate           chan struct{}
	openOnce       sync.Once
	done           chan struct{}
	failed         chan error
	closeOnce      sync.Once
	ctx            context.Context
	cancel         context.CancelFunc
	options        Options
	methods        map[string]Kind
	inbound        map[ID]*Request
	outbound       map[ID]*Outbound
	notifications  []int
	responseBytes  map[ID]int
	usage          Usage
	high           int64
	queue          chan *carrier
	constructions  *ConstructionPool
	initiation     chan struct{}
	sdkPending     *Outbound
	sdkHigh        int64
	unclaimed      *Request
	ingress        []ingressFrame
	ingressChanged chan struct{}
	inputDone      bool
}

func New(source io.ReadCloser, sink io.Writer, opts Options) *Wire {
	ctx, cancel := context.WithCancel(context.Background())
	m := MethodKinds()
	for name, k := range opts.Methods {
		m[name] = k
	}
	w := &Wire{source: source, sink: sink, reader: bufio.NewReaderSize(source, 32<<10), gate: make(chan struct{}), done: make(chan struct{}), failed: make(chan error, 1), ctx: ctx, cancel: cancel, options: opts, methods: m, inbound: make(map[ID]*Request), outbound: make(map[ID]*Outbound), responseBytes: make(map[ID]int), queue: make(chan *carrier, MaxItems), constructions: NewConstructionPool()}
	w.output = &frameWriter{wire: w}
	w.initiation = make(chan struct{}, 1)
	w.initiation <- struct{}{}
	w.ingressChanged = make(chan struct{}, 1)
	go w.writeLoop()
	if opts.RequireRequestClaims {
		go w.scanLoop()
	}
	return w
}
func (w *Wire) Input() io.Reader { return w }

// inputEnd is what a closed wire owes its reader. A connection the peer ended cleanly reports
// io.EOF for the rest of its life, so a reader that comes back after the end still sees an
// ordinary end of stream rather than a failure it would log.
func (w *Wire) inputEnd() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inputDone {
		return io.EOF
	}
	return ErrClosed
}
func (w *Wire) Output() io.Writer    { return w.output }
func (w *Wire) Open()                { w.openOnce.Do(func() { close(w.gate) }) }
func (w *Wire) Failed() <-chan error { return w.failed }
func (w *Wire) Usage() Usage         { w.mu.Lock(); defer w.mu.Unlock(); return w.usage }
func (w *Wire) Close() error {
	w.closeWithCause(nil)
	return nil
}
func (w *Wire) closeWithCause(cause error) {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		if cause != nil {
			w.failed <- cause
		}
		close(w.done)
		w.cancel()
		for _, r := range w.inbound {
			r.retired = true
			r.written.finish(ErrClosed)
			if r.reservation != nil {
				r.reservation.Release()
			}
		}
		w.inbound = make(map[ID]*Request)
		for _, o := range w.outbound {
			o.Frame = nil
			o.written.finish(ErrClosed)
		}
		w.outbound = make(map[ID]*Outbound)
		w.responseBytes = make(map[ID]int)
		w.notifications = nil
		w.pending = nil
		w.output.partial = nil
		w.ingress = nil
		w.claimRequest(w.unclaimed)
		w.usage = Usage{}
		w.mu.Unlock()
		_ = w.source.Close()
		if c, ok := w.sink.(io.Closer); ok {
			_ = c.Close()
		}
	})
}
func (w *Wire) fail(cause error) { w.closeWithCause(cause) }
func (w *Wire) closed() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}
func (w *Wire) scanFrame() (ingressFrame, error) {
	for {
		if w.closed() {
			return ingressFrame{}, ErrClosed
		}
		raw, err := readFrame(w.reader)
		if err != nil {
			// A peer that closed its write side after a complete frame ended the input;
			// it did not truncate one. That is not a supervisor failure, so nothing is
			// reported on failed. The frames already admitted are still owed to the SDK,
			// so the wire outlives the input exactly that long: whoever hands over the
			// last of the ingress releases the connection. Anything less leaks the write
			// loop, the source and every admitted charge until the process ends.
			if errors.Is(err, io.EOF) {
				w.mu.Lock()
				w.inputDone = true
				w.signalIngress()
				queued := len(w.ingress)
				w.mu.Unlock()
				if queued == 0 {
					_ = w.Close()
				}
				return ingressFrame{}, io.EOF
			}
			w.fail(ErrFrame)
			return ingressFrame{}, ErrFrame
		}
		e, err := parseEnvelope(raw, w.methods)
		if err != nil {
			if errors.Is(err, strictjson.ErrLimit) {
				w.fail(ErrEnvelope)
				return ingressFrame{}, ErrEnvelope
			}
			code := -32600
			if errors.Is(err, strictjson.ErrInvalid) {
				code = -32700
			}
			a, sendErr := w.send(fixedError(code), 0, true)
			if sendErr == nil {
				_ = a.Wait(w.ctx)
			}
			w.fail(ErrEnvelope)
			return ingressFrame{}, ErrEnvelope
		}
		if e.kind == NotificationFrame {
			if _, known := w.methods[e.method]; !known {
				continue
			}
			validate := w.options.ValidateNotification
			if validate == nil {
				validate = defaultNotification
			}
			if validate(e.method, e.params) != nil {
				w.fail(ErrEnvelope)
				return ingressFrame{}, ErrEnvelope
			}
		}
		if e.kind != RequestFrame {
			err = validateRouting(&e)
		}
		if err != nil {
			if e.kind == RequestFrame {
				a, se := w.send(fixedError(-32600), 0, true)
				if se == nil {
					_ = a.Wait(w.ctx)
				}
			}
			w.fail(ErrEnvelope)
			return ingressFrame{}, ErrEnvelope
		}
		dispatch, drop, err := w.admit(e, len(raw))
		if err != nil {
			w.fail(err)
			return ingressFrame{}, err
		}
		if drop {
			continue
		}
		if e.kind == RequestFrame {
			if _, known := w.methods[e.method]; known && w.options.RequestGate != nil {
				decision := w.options.RequestGate(e.method)
				if decision != DispatchRequest {
					code := -32601
					message := "Method not found"
					if decision == NotReady {
						code = -32014
						message = "Agent not initialized"
					}
					reply := errorResponse(e.id, code, message)
					if _, sendErr := w.Send(reply, 0); sendErr != nil {
						return ingressFrame{}, sendErr
					}
					continue
				}
			}
			if validateRouting(&e) != nil {
				a, se := w.send(fixedError(-32600), 0, true)
				if se == nil {
					_ = a.Wait(w.ctx)
				}
				w.fail(ErrEnvelope)
				return ingressFrame{}, ErrEnvelope
			}
			w.mu.Lock()
			dispatch.Request.session = e.session
			dispatch.Session = e.session
			w.mu.Unlock()
		}
		if w.options.BeforeDispatch != nil {
			w.options.BeforeDispatch(dispatch)
		}
		if e.method == "$/cancel_request" {
			// Retain only the exact validated request handle. Encoding waits for
			// SDK exposure and duplicate intents are coalesced during admission.
			return ingressFrame{dispatch: dispatch}, nil
		}
		if e.method == "session/cancel" {
			raw = cancelFrame(e)
		}
		if e.kind == RequestFrame && e.id.Kind == NullID {
			raw = replaceID(raw, []byte(sdkNullID))
		}
		if e.kind == ResponseFrame && dispatch.sdkID != nil {
			raw = replaceID(raw, dispatch.sdkID.Raw())
		}
		return ingressFrame{raw: append(raw, '\n'), dispatch: dispatch}, nil
	}
}
func (w *Wire) admit(e envelope, size int) (Dispatch, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	d := Dispatch{Kind: e.kind, Method: e.method, Session: e.session}
	if w.closed() {
		return d, false, ErrClosed
	}
	if e.kind == NotificationFrame && e.method == "$/cancel_request" {
		r := w.inbound[e.cancelID]
		if r == nil || r.reserved {
			return d, true, nil
		}
		d.Request = r
		d.Intent = RequestCancel
		if !r.addIntent(RequestCancel) {
			return d, true, nil
		}
		return d, false, nil
	}
	if e.kind == ResponseFrame {
		if e.id.Kind == NullID {
			if e.nullError {
				return d, true, nil
			}
			return d, false, ErrEnvelope
		}
		if e.id.Kind != NumberID {
			return d, false, ErrEnvelope
		}
		n, err := strconv.ParseInt(e.id.Value, 10, 64)
		if err != nil || n <= 0 || n > w.high || n > MaxOutboundID {
			return d, false, ErrEnvelope
		}
		if w.outbound[e.id] == nil {
			return d, true, nil
		}
		sdkID := w.outbound[e.id].sdkID
		if sdkID == nil {
			return d, false, ErrEnvelope
		}
		id := *sdkID
		d.sdkID = &id
		if _, exists := w.responseBytes[e.id]; exists {
			return d, false, ErrEnvelope
		}
	}
	if size > MaxBytes-w.usage.InboundBytes {
		return d, false, ErrOverload
	}
	switch e.kind {
	case RequestFrame:
		if len(w.inbound) >= MaxItems || w.inbound[e.id] != nil {
			return d, false, ErrOverload
		}
		r := &Request{wire: w, id: e.id, method: e.method, session: e.session, bytes: size, written: newReceipt(0)}
		w.inbound[e.id] = r
		d.Request = r
		w.usage.Requests++
	case NotificationFrame:
		if len(w.notifications) >= MaxItems {
			return d, false, ErrOverload
		}
		w.notifications = append(w.notifications, size)
		w.usage.Notifications++
		if e.method == "session/cancel" {
			d.Intent = SemanticCancel
		}
	case ResponseFrame:
		if len(w.responseBytes) >= MaxItems {
			return d, false, ErrOverload
		}
		w.responseBytes[e.id] = size
		w.usage.Responses++
	}
	w.usage.InboundBytes += size
	return d, false, nil
}

// HandleNotification wraps each known sequential SDK notification callback.
// The FIFO charge survives the entire callback, including a blocking handler.
func (w *Wire) HandleNotification(handle func()) {
	defer func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if len(w.notifications) > 0 {
			size := w.notifications[0]
			w.notifications = w.notifications[1:]
			w.usage.Notifications--
			w.usage.InboundBytes -= size
		}
	}()
	handle()
}

// PrepareOutbound must precede SDK request creation and event sequencing.
// The adapter must correlate this id with the SDK's request id before sending.
func (w *Wire) PrepareOutbound(method string, params json.RawMessage) (*Outbound, error) {
	w.mu.Lock()
	if w.closed() {
		w.mu.Unlock()
		return nil, ErrClosed
	}
	if w.high == MaxOutboundID || len(w.outbound) >= MaxItems {
		w.mu.Unlock()
		w.fail(ErrOverload)
		return nil, ErrOverload
	}
	id := numericID(w.high + 1)
	if len(params) > MaxFrame || len(method) > 4096 || (len(params) > 0 && strictjson.Scan(params, strictjson.Outer) != nil) {
		w.mu.Unlock()
		w.fail(ErrEnvelope)
		return nil, ErrEnvelope
	}
	raw, err := encodeOutbound(id, method, params, MaxBytes-w.usage.OutboundBytes)
	if err != nil {
		w.mu.Unlock()
		w.fail(ErrOverload)
		return nil, ErrOverload
	}
	e, err := parseEnvelope(raw, w.methods)
	if err != nil || e.kind != RequestFrame {
		w.mu.Unlock()
		w.fail(ErrEnvelope)
		return nil, ErrEnvelope
	}
	w.high++
	o := &Outbound{wire: w, ID: id, Frame: raw, method: method, written: newReceipt(0)}
	w.outbound[id] = o
	w.usage.OutboundRequests++
	w.usage.OutboundBytes += len(raw)
	w.mu.Unlock()
	return o, nil
}

type Outbound struct {
	wire      *Wire
	ID        ID
	Frame     []byte
	method    string
	sdkID     *ID
	invoked   bool
	sdkCancel context.CancelFunc
	written   *Receipt
	enqueued  bool
}

// Written exposes SDK request physical completion. Set its Tag after admission
// and before Invoke when the request carries an allocated event sequence.
func (o *Outbound) Written() *Receipt { return o.written }

// Invoke serializes only SDK initiation through its first Output write. The
// callback and response remain concurrent. Its context advances cancellation
// classification before the SDK can delete its callback.
func (o *Outbound) Invoke(ctx context.Context, call func(context.Context) error) error {
	w := o.wire
	select {
	case <-w.initiation:
	case <-ctx.Done():
		o.Cancel()
		return ErrClosed
	case <-w.done:
		return ErrClosed
	}
	w.mu.Lock()
	if w.closed() || w.outbound[o.ID] != o || o.invoked || w.sdkHigh == MaxOutboundID {
		w.mu.Unlock()
		w.initiation <- struct{}{}
		w.fail(ErrClosed)
		return ErrClosed
	}
	o.invoked = true
	w.sdkPending = o
	w.mu.Unlock()
	callCtx, cancel := context.WithCancel(w.ctx)
	w.mu.Lock()
	o.sdkCancel = cancel
	w.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { o.Cancel(); cancel() })
	defer func() {
		stop()
		cancel()
		w.mu.Lock()
		w.releaseInitiation(o)
		w.mu.Unlock()
		o.Complete()
	}()
	if ctx.Err() != nil {
		o.Cancel()
		return ErrClosed
	}
	if err := call(callCtx); err != nil {
		return ErrClosed
	}
	return nil
}
func (w *Wire) releaseInitiation(o *Outbound) {
	if w.sdkPending == o {
		w.sdkPending = nil
		w.initiation <- struct{}{}
	}
}

func (o *Outbound) Complete() { o.retire(true) }

// Cancel advances the wire ledger before the adapter removes the SDK callback.
// Supersession uses the same retirement operation. IDs are never reused.
func (o *Outbound) Cancel() {
	w := o.wire
	w.mu.Lock()
	notify := w.outbound[o.ID] == o && o.sdkID != nil
	cancel := o.sdkCancel
	w.mu.Unlock()
	o.retire(false)
	if notify {
		_, _ = w.Send(cancelFrame(envelope{method: "$/cancel_request", cancelID: o.ID}), 0)
	}
	if cancel != nil {
		cancel()
	}
}

// Supersede retires a request without signalling semantic Session cancellation.
func (o *Outbound) Supersede() { o.Cancel() }
func (o *Outbound) retire(callbackReturned bool) {
	w := o.wire
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.outbound[o.ID] == o {
		delete(w.outbound, o.ID)
		w.usage.OutboundRequests--
		w.usage.OutboundBytes -= len(o.Frame)
		o.Frame = nil
		if !o.enqueued {
			o.written.finish(ErrClosed)
		}
	}
	if size, ok := w.responseBytes[o.ID]; ok && callbackReturned {
		delete(w.responseBytes, o.ID)
		w.usage.Responses--
		w.usage.InboundBytes -= size
	}
}

// Request returns only an active directional request, for callback binding.
func (w *Wire) Request(id ID) *Request { w.mu.Lock(); defer w.mu.Unlock(); return w.inbound[id] }

// ClaimRequest binds the single SDK callback being released to its exact
// admitted request. Call it before acquiring a construction reservation or
// starting handler work. SDK-generated errors claim through the writer instead.
func (w *Wire) ClaimRequest() *Request {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := w.unclaimed
	w.claimRequest(r)
	return r
}
func (w *Wire) claimRequest(r *Request) {
	if r != nil && w.unclaimed == r {
		w.unclaimed = nil
		w.signalIngress()
	}
}
