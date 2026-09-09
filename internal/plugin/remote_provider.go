package plugin

import (
	"context"
	"errors"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

// remoteProvider is a wire: custom provider: the plugin does the streaming and the server
// only carries it. Complete sends provider.complete and turns the provider.delta
// notifications that answer it into parts, in order, before the usage and stop the response
// itself carries.
type remoteProvider struct {
	name string
	sp   *Spawned
}

var _ provider.Provider = (*remoteProvider)(nil)

func (p *remoteProvider) Name() string { return p.name }

func (p *remoteProvider) ListModels(ctx context.Context) ([]provider.Model, error) {
	var out protocol.ProviderListModelsResult
	if err := p.sp.peer.Client().Call(ctx, protocol.MethodProviderListModels, struct{}{}, &out); err != nil {
		return nil, providerErr(err)
	}
	return out.Models, nil
}

// completion is what the provider.complete call returned, carried off its goroutine so the
// deltas that arrive while it is in flight can be emitted as they land.
type completion struct {
	res protocol.ProviderCompleteResult
	err error
}

func (p *remoteProvider) Complete(ctx context.Context, req provider.Request, emit func(provider.Part) error) error {
	// The request id is what the plugin tags its deltas with: one completion's stream is
	// never another's, even when a plugin runs several at once.
	id := ulid.Make().String()
	sub := p.sp.subscribe(id)
	defer p.sp.unsubscribe(id)

	done := make(chan completion, 1)
	go func() {
		var res protocol.ProviderCompleteResult
		err := p.sp.peer.Client().Call(ctx, protocol.MethodProviderComplete, protocol.ProviderCompleteParams{
			RequestID: id,
			Model:     req.Model,
			System:    req.System,
			Messages:  req.Messages,
			Tools:     req.Tools,
			Thinking:  req.Thinking,
			MaxTokens: req.MaxTokens,
		}, &res)
		done <- completion{res: res, err: err}
	}()

	for {
		select {
		case part := <-sub.parts:
			if err := emit(part); err != nil {
				return err
			}
		case c := <-done:
			if c.err != nil {
				return providerErr(c.err)
			}
			return p.finish(sub, c.res, emit)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// finish emits whatever the plugin streamed before its response and then the usage and stop
// parts that response carries. The barrier is what makes that ordering real: a delta and the
// response travel the same connection but reach this process down two different paths, and
// without waiting for the incoming side to catch up the last deltas of a completion would be
// emitted after its stop, or not at all.
func (p *remoteProvider) finish(sub *deltaSub, res protocol.ProviderCompleteResult, emit func(provider.Part) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()
	synced := make(chan struct{})
	go func() { defer close(synced); _ = p.sp.peer.Sync(ctx) }()
	for waiting := true; waiting; {
		select {
		case part := <-sub.parts:
			if err := emit(part); err != nil {
				return err
			}
		case <-synced:
			waiting = false
		}
	}
	if err := drainParts(sub.parts, emit); err != nil {
		return err
	}
	if err := emit(provider.Part{Type: provider.PartUsage, Usage: res.Usage}); err != nil {
		return err
	}
	return emit(provider.Part{
		Type:          provider.PartStop,
		StopReason:    res.StopReason,
		StopReasonRaw: res.StopReasonRaw,
	})
}

// providerErr gives a plugin's failure the class the turn records. Without it every failure
// from a wire: custom provider would land as an internal fault, which is what a rudy bug
// looks like, rather than the provider error it is. A cancelled turn stays a cancellation.
func providerErr(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var pe *provider.Error
	if errors.As(err, &pe) {
		return err
	}
	return &provider.Error{Class: session.ErrProvider, Message: err.Error(), Attempts: 1}
}

func drainParts(parts <-chan provider.Part, emit func(provider.Part) error) error {
	for {
		select {
		case part := <-parts:
			if err := emit(part); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}
