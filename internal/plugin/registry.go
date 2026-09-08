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

// Load initializes each plugin in order, once at boot before any turn runs.
// Each plugin's registrations are staged in a private view while its Init
// runs and are committed to the shared registry atomically only when Init
// returns nil; an error or a panic discards the stage instead, so a
// concurrent reader can never observe a half-loaded plugin's capabilities.
// Load itself never returns an error: a failure becomes a failed status and
// a notice, and the next plugin still loads. Every accessor (Tools, Tool,
// Commands, Command, Providers, Statuses) is safe to call concurrently at
// any time, including while Load is still running.
func (r *Registry) Load(ctx context.Context, plugins ...Plugin) {
	for _, p := range plugins {
		name := p.Name()
		idx := r.setStatus(Status{Name: name, State: StateLoading})
		h := newHost(r, name)
		if err := safeInit(ctx, p, h); err != nil {
			r.statusAt(idx, Status{Name: name, State: StateFailed, Reason: err.Error()})
			r.notice(fmt.Sprintf("plugin %s: failed: %s", name, err.Error()))
			continue
		}
		r.commit(h)
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

// commit moves everything a plugin staged during a successful Init into the
// shared registry. Called only after Init has returned nil, so every name in
// the stage already cleared the duplicate check against the committed table.
func (r *Registry) commit(h *host) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range h.toolOrder {
		r.tools[n] = owned[tool.Tool]{owner: h.name, value: h.tools[n]}
		r.toolOrder = append(r.toolOrder, n)
	}
	for _, n := range h.cmdOrder {
		r.commands[n] = owned[Command]{owner: h.name, value: h.commands[n]}
		r.cmdOrder = append(r.cmdOrder, n)
	}
	for _, n := range h.provOrder {
		r.providers[n] = owned[provider.Provider]{owner: h.name, value: h.providers[n]}
		r.provOrder = append(r.provOrder, n)
	}
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

// host is the per-plugin view of the registry handed to Init. It stages
// registrations locally; nothing reaches the shared registry until Load
// commits it after a successful Init.
type host struct {
	r    *Registry
	name string

	mu        sync.Mutex
	tools     map[string]tool.Tool
	toolOrder []string
	commands  map[string]Command
	cmdOrder  []string
	providers map[string]provider.Provider
	provOrder []string
}

func newHost(r *Registry, name string) *host {
	return &host{
		r:         r,
		name:      name,
		tools:     map[string]tool.Tool{},
		commands:  map[string]Command{},
		providers: map[string]provider.Provider{},
	}
}

func (h *host) RegisterTool(t tool.Tool) error {
	return stageRegister(h, "tool", t.Name, t, h.r.tools, h.tools, &h.toolOrder)
}

func (h *host) RegisterCommand(c Command) error {
	return stageRegister(h, "command", c.Name, c, h.r.commands, h.commands, &h.cmdOrder)
}

func (h *host) RegisterProvider(p provider.Provider) error {
	if p == nil {
		return errors.New("plugin: nil provider")
	}
	return stageRegister(h, "provider", p.Name(), p, h.r.providers, h.providers, &h.provOrder)
}

// stageRegister checks name against both the committed table and this
// plugin's own stage, then either records the duplicate or stages the value.
// The registry lock, when taken at all, is always released before notice is
// called: a notice sink is free to read the registry (Tools, Statuses, ...)
// without deadlocking.
func stageRegister[T any](h *host, kind, name string, value T, committed map[string]owned[T], staged map[string]T, order *[]string) error {
	if name == "" {
		return fmt.Errorf("plugin %s: %s name empty", h.name, kind)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := staged[name]; ok {
		h.r.notice(fmt.Sprintf("plugin %s: %s %s already registered by %s", h.name, kind, name, h.name))
		return fmt.Errorf("%w: %s %q owned by %s", ErrDuplicate, kind, name, h.name)
	}
	h.r.mu.RLock()
	existing, ok := committed[name]
	h.r.mu.RUnlock()
	if ok {
		h.r.notice(fmt.Sprintf("plugin %s: %s %s already registered by %s", h.name, kind, name, existing.owner))
		return fmt.Errorf("%w: %s %q owned by %s", ErrDuplicate, kind, name, existing.owner)
	}
	staged[name] = value
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
