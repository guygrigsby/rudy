package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/guygrigsby/rudy/internal/session"
)

var (
	ErrUnknownModel = errors.New("provider: unknown model")
	ErrAmbiguous    = errors.New("provider: ambiguous model id")
)

// Registry is the discovered model list across every configured provider, with a snapshot
// on disk so a session can open before the first refresh completes.
type Registry struct {
	mu        sync.Mutex
	snapshot  string
	order     []string
	providers map[string]Provider
	models    map[string][]Model // by provider name
}

type snapshotFile struct {
	FetchedAt time.Time `json:"fetched_at"`
	Models    []Model   `json:"models"`
}

func NewRegistry(snapshotPath string, providers ...Provider) *Registry {
	r := &Registry{snapshot: snapshotPath, providers: map[string]Provider{}, models: map[string][]Model{}}
	for _, p := range providers {
		r.order = append(r.order, p.Name())
		r.providers[p.Name()] = p
	}
	return r
}

// SetProviders replaces the provider set and keeps whatever models are already known: the
// snapshot on disk was written by an earlier run and is still the best answer until the
// first Refresh. Call it before the first Refresh, which is where wire.go calls it, once
// plugin.Load has committed the provider plugins.
func (r *Registry) SetProviders(ps ...Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = r.order[:0]
	r.providers = map[string]Provider{}
	for _, p := range ps {
		r.order = append(r.order, p.Name())
		r.providers[p.Name()] = p
	}
}

// Refresh lists models on every provider concurrently. A provider that fails keeps its
// previous models; every failure is joined into the returned error. The snapshot is
// rewritten when at least one provider answered.
func (r *Registry) Refresh(ctx context.Context) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	results := map[string][]Model{}
	for _, p := range r.snapshotProviders() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ms, err := p.ListModels(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("provider %s: %w", p.Name(), err))
				return
			}
			results[p.Name()] = ms
		}()
	}
	wg.Wait()
	r.mu.Lock()
	for name, ms := range results {
		r.models[name] = ms
	}
	r.mu.Unlock()
	if len(results) > 0 {
		if err := r.writeSnapshot(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *Registry) writeSnapshot() error {
	if r.snapshot == "" {
		return nil
	}
	data, err := json.MarshalIndent(snapshotFile{FetchedAt: time.Now(), Models: r.Models()}, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.snapshot), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.snapshot), ".registry-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), r.snapshot)
}

// LoadSnapshot fills the registry from the snapshot file. A missing file is not an error.
func (r *Registry) LoadSnapshot() error {
	data, err := os.ReadFile(r.snapshot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("provider: read snapshot: %w", err)
	}
	var s snapshotFile
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("provider: decode snapshot %s: %w", r.snapshot, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.models = map[string][]Model{}
	for _, m := range s.Models {
		r.models[m.Ref.Provider] = append(r.models[m.Ref.Provider], m)
	}
	return nil
}

// Models returns every model sorted by provider then id, one entry per ref.
func (r *Registry) Models() []Model {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Model
	for _, ms := range r.models {
		out = append(out, ms...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ref.Provider != out[j].Ref.Provider {
			return out[i].Ref.Provider < out[j].Ref.Provider
		}
		return out[i].Ref.Model < out[j].Ref.Model
	})
	return collapse(out)
}

// upstreamSep joins the upstreams of a model served by more than one.
const upstreamSep = ", "

// collapse folds the entries that share a ref, which the sort has already put side by
// side. An endpoint that fronts several serves some ids from more than one of them and
// lists the pair each time, but (provider, id) is what everything downstream addresses a
// model by, so two entries the ref cannot tell apart are one model (rudy-aol). The first
// wins every field but the upstream: the survivor names each one it came from, since a
// person filtering by either has to find it.
func collapse(sorted []Model) []Model {
	out := make([]Model, 0, len(sorted))
	for _, m := range sorted {
		if n := len(out); n > 0 && out[n-1].Ref == m.Ref {
			out[n-1].Upstream = withUpstream(out[n-1].Upstream, m.Upstream)
			continue
		}
		out = append(out, m)
	}
	return out
}

// withUpstream adds one name to what a merged model carries, in the order they arrived and
// each said once.
func withUpstream(have, add string) string {
	switch {
	case add == "":
		return have
	case have == "":
		return add
	}
	for _, s := range strings.Split(have, upstreamSep) {
		if s == add {
			return have
		}
	}
	return have + upstreamSep + add
}

// Resolve finds a model by "provider:id" or by a bare id that is unique across providers.
func (r *Registry) Resolve(spec string) (Model, error) { return ResolveIn(r.Models(), spec) }

// ResolveIn is Resolve's rule over a plain slice, for a caller holding a registry listing
// rather than a registry: a client attached to rudy serve reads the models over registry.list
// and has to spell "provider:id" and a unique bare id the same way the server does.
func ResolveIn(models []Model, spec string) (Model, error) {
	if ref, ok := session.ParseModelRef(spec); ok {
		for _, m := range models {
			if m.Ref == ref {
				return m, nil
			}
		}
		return Model{}, fmt.Errorf("%w: %s", ErrUnknownModel, spec)
	}
	var matches []Model
	for _, m := range models {
		if m.Ref.Model == spec {
			matches = append(matches, m)
		}
	}
	switch len(matches) {
	case 0:
		return Model{}, fmt.Errorf("%w: %s", ErrUnknownModel, spec)
	case 1:
		return matches[0], nil
	}
	var names []string
	for _, m := range matches {
		names = append(names, m.Ref.String())
	}
	return Model{}, fmt.Errorf("%w: %s matches %s", ErrAmbiguous, spec, strings.Join(names, ", "))
}

// Provider returns a configured provider by name. Locked, like every other reader: the set
// is replaced wholesale by SetProviders once plugin.Load has committed.
func (r *Registry) Provider(name string) (Provider, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.providers[name]
	return p, ok
}

// snapshotProviders is the provider set in order, for a caller that then works without the
// lock (Refresh, which waits on the network).
func (r *Registry) snapshotProviders() []Provider {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Provider, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.providers[name])
	}
	return out
}
