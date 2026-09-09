package plugin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

var ErrDuplicate = errors.New("plugin: already registered")

type owned[T any] struct {
	owner string
	value T
}

// ownerKey identifies one status item or widget. Owner is part of the key, which is what
// keeps a plugin from clearing another plugin's item.
type ownerKey struct {
	owner string
	key   string
}

type Registry struct {
	mu          sync.RWMutex
	config      map[string]map[string]any
	notice      func(string)
	services    Services
	disabled    map[string]bool
	failed      map[string]bool // Fail has withdrawn this plugin; later writes are ignored
	tools       map[string]owned[tool.Tool]
	toolOrder   []string
	commands    map[string]owned[Command]
	cmdOrder    []string
	providers   map[string]owned[provider.Provider]
	provOrder   []string
	hooks       []OwnedHook // load order; Hooks sorts a point's handlers by priority
	statuses    []Status
	status      map[ownerKey]StatusItem
	statusOrder []ownerKey // first-set order
	widgets     map[ownerKey]Widget
	widgetOrder []ownerKey // first-set order
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
		disabled:  map[string]bool{},
		failed:    map[string]bool{},
		status:    map[ownerKey]StatusItem{},
		widgets:   map[ownerKey]Widget{},
	}
}

// SetServices attaches the server. Call it once, after server.New and before Load: a
// plugin's Init may already want to Connect or append a note.
func (r *Registry) SetServices(s Services) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.services = s
}

// Disable marks plugins Load must skip. Call it before Load. A disabled plugin gets no
// status row at all: it was never asked to load, so it is neither ready nor failed.
func (r *Registry) Disable(names ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range names {
		r.disabled[n] = true
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
		if r.isDisabled(name) {
			continue
		}
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

func (r *Registry) isDisabled(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.disabled[name]
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
	// h.mu is held across the registry write, not just the copy: it is what flips the host
	// from staging to writing through, and a SetStatus from a goroutine the plugin started
	// during Init must land either wholly before the stage (and be overwritten by it) or
	// wholly after (and overwrite it), never in between. h.mu outside r.mu is the order
	// stageRegister already uses.
	h.mu.Lock()
	defer h.mu.Unlock()
	h.live = true
	statuses := make([]StatusItem, 0, len(h.statusOrder))
	for _, k := range h.statusOrder {
		statuses = append(statuses, h.status[k])
	}
	widgets := make([]Widget, 0, len(h.widgetOrder))
	for _, k := range h.widgetOrder {
		widgets = append(widgets, h.widgets[k])
	}

	r.mu.Lock()
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
	for _, hh := range h.hooks {
		r.hooks = append(r.hooks, OwnedHook{Owner: h.name, HookHandler: hh})
	}
	for _, it := range statuses {
		r.putStatusLocked(it)
	}
	for _, w := range widgets {
		r.putWidgetLocked(w)
	}
	services := r.services
	r.mu.Unlock()

	// Outside the lock: a sink is free to read the registry back (the server's does, to
	// broadcast the whole status line).
	if len(statuses) > 0 && services.StatusChanged != nil {
		services.StatusChanged()
	}
	if services.WidgetChanged != nil {
		for _, w := range widgets {
			services.WidgetChanged(w)
		}
	}
}

// putStatusLocked stores or clears one item. Empty content is the deletion marker: it
// removes a previous item of the same owner and key rather than showing an empty cell.
// Caller holds mu.
func (r *Registry) putStatusLocked(it StatusItem) {
	k := ownerKey{owner: it.Owner, key: it.Key}
	if len(it.Content) == 0 {
		if _, ok := r.status[k]; ok {
			delete(r.status, k)
			r.statusOrder = slices.DeleteFunc(r.statusOrder, func(o ownerKey) bool { return o == k })
		}
		return
	}
	if _, ok := r.status[k]; !ok {
		r.statusOrder = append(r.statusOrder, k)
	}
	r.status[k] = it
}

// putWidgetLocked stores one widget, replacing whatever that owner had under that key.
// Caller holds mu.
func (r *Registry) putWidgetLocked(w Widget) {
	k := ownerKey{owner: w.Owner, key: w.Key}
	if _, ok := r.widgets[k]; !ok {
		r.widgetOrder = append(r.widgetOrder, k)
	}
	r.widgets[k] = w
}

// setStatusItem is the write-through path, for a host whose Init has already been
// committed. A plugin Fail has withdrawn writes nothing.
func (r *Registry) setStatusItem(it StatusItem) {
	r.mu.Lock()
	if r.failed[it.Owner] {
		r.mu.Unlock()
		return
	}
	r.putStatusLocked(it)
	f := r.services.StatusChanged
	r.mu.Unlock()
	if f != nil {
		f()
	}
}

func (r *Registry) setWidget(w Widget) {
	r.mu.Lock()
	if r.failed[w.Owner] {
		r.mu.Unlock()
		return
	}
	r.putWidgetLocked(w)
	f := r.services.WidgetChanged
	r.mu.Unlock()
	if f != nil {
		f(w)
	}
}

// StatusItems is the committed status line in first-set order, cleared items omitted.
func (r *Registry) StatusItems() []StatusItem {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]StatusItem, 0, len(r.statusOrder))
	for _, k := range r.statusOrder {
		out = append(out, r.status[k])
	}
	return out
}

// Widgets is the committed widget set in first-set order.
func (r *Registry) Widgets() []Widget {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Widget, 0, len(r.widgetOrder))
	for _, k := range r.widgetOrder {
		out = append(out, r.widgets[k])
	}
	return out
}

// Fail marks a plugin failed and withdraws every tool, command, hook, provider, status item
// and widget it owns, so the next request assembles without them. Used when a spawned
// plugin's process exits. A withdrawn plugin's host can no longer write anything back.
func (r *Registry) Fail(name, reason string) {
	r.mu.Lock()
	r.failed[name] = true
	r.toolOrder = withdraw(r.tools, r.toolOrder, name)
	r.cmdOrder = withdraw(r.commands, r.cmdOrder, name)
	r.provOrder = withdraw(r.providers, r.provOrder, name)
	r.hooks = slices.DeleteFunc(r.hooks, func(h OwnedHook) bool { return h.Owner == name })
	hadStatus := false
	for _, k := range r.statusOrder {
		if k.owner == name {
			hadStatus = true
			delete(r.status, k)
		}
	}
	r.statusOrder = slices.DeleteFunc(r.statusOrder, func(k ownerKey) bool { return k.owner == name })
	for _, k := range r.widgetOrder {
		if k.owner == name {
			delete(r.widgets, k)
		}
	}
	r.widgetOrder = slices.DeleteFunc(r.widgetOrder, func(k ownerKey) bool { return k.owner == name })
	for i, st := range r.statuses {
		if st.Name == name {
			r.statuses[i] = Status{Name: name, State: StateFailed, Reason: reason}
		}
	}
	statusChanged := r.services.StatusChanged
	providersChanged := r.services.ProvidersChanged
	remaining := r.providersLocked()
	r.mu.Unlock()
	if hadStatus && statusChanged != nil {
		statusChanged()
	}
	// The provider registry took its own copy of the set at boot and never revisits it, so
	// without this the withdrawn plugin's provider still answers the next turn.
	if providersChanged != nil {
		providersChanged(remaining)
	}
}

// withdraw deletes every entry owned by name from a committed table and returns its order
// list without those names. Caller holds mu.
func withdraw[T any](table map[string]owned[T], order []string, name string) []string {
	kept := order[:0]
	for _, n := range order {
		if table[n].owner == name {
			delete(table, n)
			continue
		}
		kept = append(kept, n)
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

// ToolView is a filtered view of a registry's tools: what one session may call. A session
// opened under an agent definition sees the definition's list; a child session never sees the
// agent tool, which is what keeps subagent depth at one. It reads the registry live, so a
// provider or tool withdrawn after the session opened disappears from the view too.
type ToolView struct {
	reg   *Registry
	allow []string // nil means every tool
	deny  []string
}

// NewToolView returns a view of reg keeping only allow (nil means all) and never deny.
func NewToolView(reg *Registry, allow, deny []string) *ToolView {
	return &ToolView{reg: reg, allow: allow, deny: deny}
}

func (v *ToolView) permits(name string) bool {
	if slices.Contains(v.deny, name) {
		return false
	}
	return v.allow == nil || slices.Contains(v.allow, name)
}

func (v *ToolView) Tools() []tool.Tool {
	all := v.reg.Tools()
	out := make([]tool.Tool, 0, len(all))
	for _, t := range all {
		if v.permits(t.Name) {
			out = append(out, t)
		}
	}
	return out
}

func (v *ToolView) Tool(name string) (tool.Tool, bool) {
	if !v.permits(name) {
		return tool.Tool{}, false
	}
	return v.reg.Tool(name)
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
	return r.providersLocked()
}

// providersLocked is Providers for a caller that already holds mu. Caller holds mu.
func (r *Registry) providersLocked() []provider.Provider {
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

	mu          sync.Mutex
	live        bool // commit has run; status and widget writes go straight to the registry
	tools       map[string]tool.Tool
	toolOrder   []string
	commands    map[string]Command
	cmdOrder    []string
	providers   map[string]provider.Provider
	provOrder   []string
	hooks       []HookHandler
	status      map[string]StatusItem
	statusOrder []string
	widgets     map[string]Widget
	widgetOrder []string
}

func newHost(r *Registry, name string) *host {
	return &host{
		r:         r,
		name:      name,
		tools:     map[string]tool.Tool{},
		commands:  map[string]Command{},
		providers: map[string]provider.Provider{},
		status:    map[string]StatusItem{},
		widgets:   map[string]Widget{},
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

// SetStatus stages the item while Init runs and writes it through afterwards. StatusChanged
// fires either way: during Init that is harmless, since the server reads StatusItems, which
// shows committed items only, and commit fires it again once the stage lands.
func (h *host) SetStatus(key string, content []Span) {
	it := StatusItem{Owner: h.name, Key: key, Content: content}
	h.mu.Lock()
	live := h.live
	if !live {
		if _, ok := h.status[key]; !ok {
			h.statusOrder = append(h.statusOrder, key)
		}
		h.status[key] = it
	}
	h.mu.Unlock()
	if live {
		h.r.setStatusItem(it)
		return
	}
	h.r.statusChanged()
}

// statusChanged fires the sink from stage time, when nothing has been committed yet.
func (r *Registry) statusChanged() {
	r.mu.RLock()
	f := r.services.StatusChanged
	r.mu.RUnlock()
	if f != nil {
		f()
	}
}

func (h *host) SetWidget(key string, slot WidgetSlot, content []Span) error {
	if !slot.Valid() {
		return fmt.Errorf("plugin %s: unknown widget slot %q", h.name, slot)
	}
	w := Widget{Owner: h.name, Key: key, Slot: slot, Content: content}
	h.mu.Lock()
	live := h.live
	if !live {
		if _, ok := h.widgets[key]; !ok {
			h.widgetOrder = append(h.widgetOrder, key)
		}
		h.widgets[key] = w
	}
	h.mu.Unlock()
	if live {
		h.r.setWidget(w)
	}
	return nil
}

func (h *host) Note(sessionID ulid.ULID, text string, role session.NoteRole) error {
	h.r.mu.RLock()
	f := h.r.services.Note
	h.r.mu.RUnlock()
	if f == nil {
		return ErrNoServer
	}
	return f(sessionID, h.name, text, role)
}

func (h *host) Connect(ctx context.Context) (*protocol.Client, error) {
	h.r.mu.RLock()
	f := h.r.services.Connect
	h.r.mu.RUnlock()
	if f == nil {
		return nil, ErrNoServer
	}
	conn, err := f(ctx, h.name)
	if err != nil {
		return nil, err
	}
	return protocol.NewClient(conn), nil
}

// Commands, Tools and Statuses are the committed registry, not this host's stage: a plugin
// asking what else is loaded wants what the kernel will actually run.
func (h *host) Commands() []Command { return h.r.Commands() }
func (h *host) Tools() []tool.Tool  { return h.r.Tools() }
func (h *host) Statuses() []Status  { return h.r.Statuses() }
