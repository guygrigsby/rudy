package acpwire

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/guygrigsby/rudy/internal/strictjson"

	"golang.org/x/sync/semaphore"
)

type Class int64

const (
	Small Class = 1 << 20
	Large Class = 8 << 20
)

var ErrConstruction = errors.New("ACP response construction limit")

type ConstructionPool struct{ sem *semaphore.Weighted }
type Reservation struct {
	pool  *ConstructionPool
	class Class
	once  sync.Once
}

func NewConstructionPool() *ConstructionPool {
	return &ConstructionPool{sem: semaphore.NewWeighted(MaxBytes)}
}
func (p *ConstructionPool) Acquire(ctx context.Context, class Class) (*Reservation, error) {
	if class != Small && class != Large {
		return nil, ErrConstruction
	}
	if err := p.sem.Acquire(ctx, int64(class)); err != nil {
		return nil, ErrClosed
	}
	return &Reservation{pool: p, class: class}, nil
}
func (r *Reservation) Release() { r.once.Do(func() { r.pool.sem.Release(int64(r.class)) }) }

// Check preflights encoded result size while the complete class is held. Callers
// must abort construction before retaining output beyond the reservation.
func (r *Reservation) Check(size int) error {
	if size < 0 || size > int(r.class) || size > 7<<20 {
		return ErrConstruction
	}
	return nil
}
func classFor(method string) Class {
	switch method {
	case "initialize", "session/new", "session/load", "session/resume", "session/fork", "session/list", "session/set_config_option", "_rudy/session/fork", "_rudy/command/list", "_rudy/registry/list", "_rudy/registry/refresh":
		return Large
	default:
		return Small
	}
}

// AcquireConstruction runs inside the admitted handler, away from the reader.
// Close wakes waiters and releases reservations held through response writes.
func (r *Request) AcquireConstruction(ctx context.Context) (*Reservation, error) {
	w := r.wire
	wait, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(w.ctx, cancel)
	defer stop()
	defer cancel()
	reservation, err := w.constructions.Acquire(wait, classFor(r.method))
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if r.retired || w.closed() || r.reservation != nil {
		reservation.Release()
		return nil, ErrClosed
	}
	r.reservation = reservation
	return reservation, nil
}

// SendResult preflights a bounded encoded result and its complete envelope.
// Call AcquireConstruction before producing result. Refusal reserves the fixed
// internal error without passing the rejected bytes to the encoded writer.
func (r *Request) SendResult(result []byte, tag uint64) (*Receipt, error) {
	w := r.wire
	w.mu.Lock()
	reservation := r.reservation
	w.mu.Unlock()
	if reservation == nil {
		return nil, ErrConstruction
	}
	var frame []byte
	kind := ResultResponse
	id := r.id.Raw()
	size := len(`{"jsonrpc":"2.0","id":`) + len(id) + len(`,"result":`) + len(result) + 1
	if reservation.Check(size) == nil && len(result) <= 7<<20 && strictjson.Scan(result, strictjson.Outer) == nil {
		frame = make([]byte, 0, size)
		frame = append(frame, `{"jsonrpc":"2.0","id":`...)
		frame = append(frame, id...)
		frame = append(frame, `,"result":`...)
		frame = append(frame, result...)
		frame = append(frame, '}')
	}
	if frame == nil || strictjson.Scan(frame, strictjson.Outer) != nil {
		kind = ErrorResponse
		frame = errorResponse(r.id, -32603, "Internal error")
	}
	if !r.Reserve(kind) {
		return nil, ErrClosed
	}
	return w.Send(frame, tag)
}
func errorResponse(id ID, code int, message string) []byte {
	type detail struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	raw, _ := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   detail          `json:"error"`
	}{"2.0", id.Raw(), detail{code, message}})
	return raw
}
