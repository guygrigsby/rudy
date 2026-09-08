package plugin

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/tool"
)

var ErrDuplicate = errors.New("plugin: already registered")

type owned[T any] struct {
	owner string
	value T
}

type Registry struct {
	mu        sync.RWMutex
	config    map[string]map[string]any
	notice    func(string)
	tools     map[string]owned[tool.Tool]
	toolOrder []string
	commands  map[string]owned[Command]
	cmdOrder  []string
	providers map[string]owned[provider.Provider]
	provOrder []string
	statuses  []Status
}

func NewRegistry(config map[string]map[string]any, notice func(string)) *Registry {
	if notice == nil {
		notice = func(string) {}
	}
	return &Registry{
		config:    config,
		notice:    notice,
		tools:     map[string]owned[tool.Tool]{},
		commands:  map[string]owned[Command]{},
		providers: map[string]owned[provider.Provider]{},
	}
}

// Load initializes each plugin in order. A failure never aborts the load: the
// plugin is marked failed, its registrations are rolled back and a notice is
// recorded.
func (r *Registry) Load(ctx context.Context, plugins ...Plugin) {
	for _, p := range plugins {
		name := p.Name()
		idx := r.setStatus(Status{Name: name, State: StateLoading})
		h := &host{r: r, name: name}
		if err := safeInit(ctx, p, h); err != nil {
			r.rollback(name)
			r.statusAt(idx, Status{Name: name, State: StateFailed, Reason: err.Error()})
			r.notice(fmt.Sprintf("plugin %s: failed: %s", name, err.Error()))
			continue
		}
		r.statusAt(idx, Status{Name: name, State: StateReady})
	}
}

func safeInit(ctx context.Context, p Plugin, h Host) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic: %v", rec)
		}
	}()
	return p.Init(ctx, h)
}

func (r *Registry) setStatus(s Status) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses = append(r.statuses, s)
	return len(r.statuses) - 1
}

func (r *Registry) statusAt(i int, s Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses[i] = s
}

func (r *Registry) rollback(owner string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.toolOrder = dropOwned(r.toolOrder, r.tools, owner)
	r.cmdOrder = dropOwned(r.cmdOrder, r.commands, owner)
	r.provOrder = dropOwned(r.provOrder, r.providers, owner)
}

func dropOwned[T any](order []string, m map[string]owned[T], owner string) []string {
	kept := order[:0]
	for _, name := range order {
		if m[name].owner == owner {
			delete(m, name)
			continue
		}
		kept = append(kept, name)
	}
	return kept
}

func (r *Registry) Tools() []tool.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]tool.Tool, 0, len(r.toolOrder))
	for _, n := range r.toolOrder {
		out = append(out, r.tools[n].value)
	}
	return out
}

func (r *Registry) Tool(name string) (tool.Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	o, ok := r.tools[name]
	return o.value, ok
}

func (r *Registry) Commands() []Command {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Command, 0, len(r.cmdOrder))
	for _, n := range r.cmdOrder {
		out = append(out, r.commands[n].value)
	}
	return out
}

func (r *Registry) Command(name string) (Command, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	o, ok := r.commands[name]
	return o.value, ok
}

func (r *Registry) Providers() []provider.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]provider.Provider, 0, len(r.provOrder))
	for _, n := range r.provOrder {
		out = append(out, r.providers[n].value)
	}
	return out
}

func (r *Registry) Statuses() []Status {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Status(nil), r.statuses...)
}

// host is the per-plugin view of the registry handed to Init.
type host struct {
	r    *Registry
	name string
}

func (h *host) RegisterTool(t tool.Tool) error {
	return register(h, "tool", t.Name, t, h.r.tools, &h.r.toolOrder)
}

func (h *host) RegisterCommand(c Command) error {
	return register(h, "command", c.Name, c, h.r.commands, &h.r.cmdOrder)
}

func (h *host) RegisterProvider(p provider.Provider) error {
	if p == nil {
		return errors.New("plugin: nil provider")
	}
	return register(h, "provider", p.Name(), p, h.r.providers, &h.r.provOrder)
}

func register[T any](h *host, kind, name string, value T, m map[string]owned[T], order *[]string) error {
	if name == "" {
		return fmt.Errorf("plugin %s: %s name empty", h.name, kind)
	}
	h.r.mu.Lock()
	defer h.r.mu.Unlock()
	if existing, ok := m[name]; ok {
		h.r.notice(fmt.Sprintf("plugin %s: %s %s already registered by %s", h.name, kind, name, existing.owner))
		return fmt.Errorf("%w: %s %q owned by %s", ErrDuplicate, kind, name, existing.owner)
	}
	m[name] = owned[T]{owner: h.name, value: value}
	*order = append(*order, name)
	return nil
}

func (h *host) Config() map[string]any {
	if c, ok := h.r.config[h.name]; ok && c != nil {
		return c
	}
	return map[string]any{}
}

func (h *host) Notice(text string) {
	h.r.notice(fmt.Sprintf("plugin %s: %s", h.name, text))
}
