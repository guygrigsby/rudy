package acpwire

// Intent records routing classification in ingress order, before SDK dispatch.
type Intent uint8

const (
	NoIntent Intent = iota
	SemanticCancel
	RequestCancel
)

type Dispatch struct {
	Kind            Kind
	Method, Session string
	Request         *Request
	Intent          Intent
	sdkID           *ID
}
type ResponseKind uint8

const (
	ResultResponse ResponseKind = iota + 1
	ErrorResponse
	CancelResponse
)

// Request identity is its pointer, not just its reusable wire id. Adapters bind
// generations only after their own Session admission, using this exact handle.
type Request struct {
	wire                        *Wire
	id                          ID
	method, session             string
	bytes                       int
	reserved, retired, enqueued bool
	response                    ResponseKind
	intents                     [2]Intent
	intentCount                 int
	written                     *Receipt
	reservation                 *Reservation
	exposed                     bool
}

func (r *Request) ID() ID            { return r.id }
func (r *Request) Session() string   { r.wire.mu.Lock(); defer r.wire.mu.Unlock(); return r.session }
func (r *Request) Written() *Receipt { return r.written }
func (r *Request) addIntent(intent Intent) bool {
	for _, v := range r.intents[:r.intentCount] {
		if v == intent {
			return false
		}
	}
	if r.intentCount < len(r.intents) {
		r.intents[r.intentCount] = intent
		r.intentCount++
		return true
	}
	return false
}

// MarkSemantic lets the pre-dispatch hook mark only the Session's admitted
// standing prompt. The wire never chooses a generation or a Turn on its own.
func (r *Request) MarkSemantic() bool {
	w := r.wire
	w.mu.Lock()
	defer w.mu.Unlock()
	if r.retired || w.inbound[r.id] != r {
		return false
	}
	r.addIntent(SemanticCancel)
	return true
}
func (r *Request) Intents() []Intent {
	r.wire.mu.Lock()
	defer r.wire.mu.Unlock()
	return append([]Intent(nil), r.intents[:r.intentCount]...)
}
func (r *Request) reserve(kind ResponseKind) bool {
	if r.retired || r.reserved || r.wire.inbound[r.id] != r {
		return false
	}
	if kind != ResultResponse && kind != ErrorResponse && kind != CancelResponse {
		return false
	}
	cancelled := false
	for _, v := range r.intents[:r.intentCount] {
		if v == RequestCancel {
			cancelled = true
		}
	}
	if cancelled && kind != CancelResponse {
		return false
	}
	if kind == CancelResponse && !cancelled {
		return false
	}
	r.reserved = true
	r.response = kind
	return true
}
func (r *Request) Reserve(kind ResponseKind) bool {
	r.wire.mu.Lock()
	defer r.wire.mu.Unlock()
	return r.reserve(kind)
}
