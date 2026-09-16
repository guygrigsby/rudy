package acpwire

// ingressFrame owns already admitted original bytes. Request-scoped cancels
// retain only a validated handle and are encoded at SDK exposure.
type ingressFrame struct {
	raw      []byte
	dispatch Dispatch
}

func (w *Wire) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	select {
	case <-w.gate:
	case <-w.done:
		return 0, ErrClosed
	}
	w.inputMu.Lock()
	defer w.inputMu.Unlock()
	for {
		w.mu.Lock()
		if w.closed() {
			w.mu.Unlock()
			return 0, ErrClosed
		}
		if len(w.pending) > 0 {
			n := copy(p, w.pending)
			w.pending = w.pending[n:]
			if len(w.pending) == 0 {
				w.pending = nil
			}
			w.mu.Unlock()
			return n, nil
		}
		w.mu.Unlock()
		var frame ingressFrame
		var err error
		if w.options.RequireRequestClaims {
			frame, err = w.nextIngress()
		} else {
			frame, err = w.scanFrame()
		}
		if err != nil {
			return 0, err
		}
		w.mu.Lock()
		if w.closed() {
			w.mu.Unlock()
			return 0, ErrClosed
		}
		raw := frame.raw
		if frame.dispatch.Intent == RequestCancel {
			r := frame.dispatch.Request
			if r.retired || w.inbound[r.id] != r {
				w.mu.Unlock()
				continue
			}
			id := r.id
			if id.Kind == NullID {
				id = ID{NumberID, sdkNullID}
			}
			raw = append(cancelFrame(envelope{method: "$/cancel_request", cancelID: id}), '\n')
		}
		if frame.dispatch.Kind == RequestFrame {
			frame.dispatch.Request.exposed = true
		}
		w.pending = raw
		w.mu.Unlock()
	}
}

func (w *Wire) scanLoop() {
	select {
	case <-w.gate:
	case <-w.done:
		return
	}
	for {
		frame, err := w.scanFrame()
		if err != nil {
			return
		}
		w.mu.Lock()
		if w.closed() {
			w.mu.Unlock()
			return
		}
		w.pruneCancels()
		w.ingress = append(w.ingress, frame)
		w.signalIngress()
		w.mu.Unlock()
	}
}
func (w *Wire) signalIngress() {
	select {
	case w.ingressChanged <- struct{}{}:
	default:
	}
}

// Called with mu held. Coalesced cancels are bounded by the 64 active request
// handles. Removing retired handles prevents both accumulation and id rebinding.
func (w *Wire) pruneCancels() {
	keep := w.ingress[:0]
	for _, frame := range w.ingress {
		r := frame.dispatch.Request
		if frame.dispatch.Intent == RequestCancel && (r.retired || w.inbound[r.id] != r) {
			continue
		}
		keep = append(keep, frame)
	}
	clear(w.ingress[len(keep):])
	w.ingress = keep
}
func (w *Wire) nextIngress() (ingressFrame, error) {
	for {
		w.mu.Lock()
		if w.closed() {
			w.mu.Unlock()
			return ingressFrame{}, ErrClosed
		}
		w.pruneCancels()
		for i, frame := range w.ingress {
			if frame.dispatch.Kind == RequestFrame && w.unclaimed != nil {
				continue
			}
			// A cancellation for a not-yet-exposed request must follow that request.
			if frame.dispatch.Intent == RequestCancel && !frame.dispatch.Request.exposed {
				continue
			}
			if frame.dispatch.Kind == RequestFrame {
				w.unclaimed = frame.dispatch.Request
			}
			copy(w.ingress[i:], w.ingress[i+1:])
			w.ingress[len(w.ingress)-1] = ingressFrame{}
			w.ingress = w.ingress[:len(w.ingress)-1]
			w.mu.Unlock()
			return frame, nil
		}
		w.mu.Unlock()
		select {
		case <-w.ingressChanged:
		case <-w.done:
			return ingressFrame{}, ErrClosed
		}
	}
}
