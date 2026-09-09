package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

type fakePlugin struct {
	name string
	init func(ctx context.Context, h Host) error
}

func (f fakePlugin) Name() string                           { return f.name }
func (f fakePlugin) Init(ctx context.Context, h Host) error { return f.init(ctx, h) }

func namedTool(name string) tool.Tool {
	return tool.Tool{Name: name, Description: name, Safety: tool.Safe}
}

type fakeProvider struct{ name string }

func (p fakeProvider) Name() string { return p.name }
func (p fakeProvider) Complete(context.Context, provider.Request, func(provider.Part) error) error {
	return nil
}
func (p fakeProvider) ListModels(context.Context) ([]provider.Model, error) { return nil, nil }

func newTestRegistry(config map[string]map[string]any) (*Registry, *[]string) {
	var notices []string
	r := NewRegistry(config, func(s string) { notices = append(notices, s) })
	return r, &notices
}

func TestLoadRegistersInOrder(t *testing.T) {
	r, notices := newTestRegistry(nil)
	r.Load(context.Background(),
		fakePlugin{"a", func(_ context.Context, h Host) error {
			if err := h.RegisterTool(namedTool("read")); err != nil {
				return err
			}
			return h.RegisterCommand(Command{Name: "init", Run: func(context.Context, CommandCall) (Action, error) { return NoAction{}, nil }})
		}},
		fakePlugin{"b", func(_ context.Context, h Host) error {
			if err := h.RegisterTool(namedTool("bash")); err != nil {
				return err
			}
			return h.RegisterProvider(fakeProvider{"aperture"})
		}},
	)
	tools := r.Tools()
	if len(tools) != 2 || tools[0].Name != "read" || tools[1].Name != "bash" {
		t.Fatalf("tools = %+v", tools)
	}
	if _, ok := r.Tool("bash"); !ok {
		t.Fatal("bash missing")
	}
	if _, ok := r.Tool("nope"); ok {
		t.Fatal("nope should be absent")
	}
	if cmds := r.Commands(); len(cmds) != 1 || cmds[0].Name != "init" {
		t.Fatalf("commands = %+v", cmds)
	}
	if _, ok := r.Command("init"); !ok {
		t.Fatal("init missing")
	}
	if provs := r.Providers(); len(provs) != 1 || provs[0].Name() != "aperture" {
		t.Fatalf("providers = %+v", provs)
	}
	st := r.Statuses()
	if len(st) != 2 || st[0].Name != "a" || st[0].State != StateReady || st[1].Name != "b" || st[1].State != StateReady {
		t.Fatalf("statuses = %+v", st)
	}
	if len(*notices) != 0 {
		t.Fatalf("unexpected notices %v", *notices)
	}
}

func TestDuplicateToolKeepsFirstAndNotices(t *testing.T) {
	r, notices := newTestRegistry(nil)
	var second error
	r.Load(context.Background(),
		fakePlugin{"first", func(_ context.Context, h Host) error { return h.RegisterTool(namedTool("read")) }},
		fakePlugin{"second", func(_ context.Context, h Host) error {
			second = h.RegisterTool(tool.Tool{Name: "read", Description: "impostor", Safety: tool.Unsafe})
			return nil // the plugin chooses to carry on
		}},
	)
	if !errors.Is(second, ErrDuplicate) {
		t.Fatalf("second registration error = %v", second)
	}
	got, _ := r.Tool("read")
	if got.Description != "read" {
		t.Fatalf("first registration was replaced: %+v", got)
	}
	if len(r.Tools()) != 1 {
		t.Fatalf("tools = %+v", r.Tools())
	}
	if len(*notices) != 1 || (*notices)[0] != "plugin second: tool read already registered by first" {
		t.Fatalf("notices = %v", *notices)
	}
	st := r.Statuses()
	if st[1].State != StateReady {
		t.Fatalf("second should still be ready: %+v", st[1])
	}
}

func TestDuplicateCommandAndProvider(t *testing.T) {
	r, notices := newTestRegistry(nil)
	r.Load(context.Background(),
		fakePlugin{"one", func(_ context.Context, h Host) error {
			_ = h.RegisterCommand(Command{Name: "x", Run: func(context.Context, CommandCall) (Action, error) { return NoAction{}, nil }})
			return h.RegisterProvider(fakeProvider{"p"})
		}},
		fakePlugin{"two", func(_ context.Context, h Host) error {
			_ = h.RegisterCommand(Command{Name: "x", Run: func(context.Context, CommandCall) (Action, error) { return NoAction{}, nil }})
			_ = h.RegisterProvider(fakeProvider{"p"})
			return nil
		}},
	)
	want := []string{
		"plugin two: command x already registered by one",
		"plugin two: provider p already registered by one",
	}
	if strings.Join(*notices, "|") != strings.Join(want, "|") {
		t.Fatalf("notices = %v", *notices)
	}
	if len(r.Commands()) != 1 || len(r.Providers()) != 1 {
		t.Fatalf("commands=%d providers=%d", len(r.Commands()), len(r.Providers()))
	}
}

func TestInitErrorFailsPluginAndRollsBack(t *testing.T) {
	r, notices := newTestRegistry(nil)
	r.Load(context.Background(),
		fakePlugin{"broken", func(_ context.Context, h Host) error {
			_ = h.RegisterTool(namedTool("read"))
			_ = h.RegisterCommand(Command{Name: "c", Run: func(context.Context, CommandCall) (Action, error) { return NoAction{}, nil }})
			_ = h.RegisterProvider(fakeProvider{"p"})
			return errors.New("config missing")
		}},
		fakePlugin{"fine", func(_ context.Context, h Host) error { return h.RegisterTool(namedTool("bash")) }},
	)
	if _, ok := r.Tool("read"); ok {
		t.Fatal("failed plugin's tool must be rolled back")
	}
	if _, ok := r.Command("c"); ok {
		t.Fatal("failed plugin's command must be rolled back")
	}
	if len(r.Providers()) != 0 {
		t.Fatal("failed plugin's provider must be rolled back")
	}
	if _, ok := r.Tool("bash"); !ok {
		t.Fatal("later plugin still loads")
	}
	st := r.Statuses()
	if st[0].State != StateFailed || st[0].Reason != "config missing" {
		t.Fatalf("status = %+v", st[0])
	}
	if len(*notices) != 1 || (*notices)[0] != "plugin broken: failed: config missing" {
		t.Fatalf("notices = %v", *notices)
	}
}

func TestInitPanicFailsPlugin(t *testing.T) {
	r, _ := newTestRegistry(nil)
	r.Load(context.Background(),
		fakePlugin{"boom", func(_ context.Context, h Host) error {
			_ = h.RegisterTool(namedTool("read"))
			panic("nil map write")
		}},
	)
	st := r.Statuses()
	if st[0].State != StateFailed || !strings.Contains(st[0].Reason, "panic: nil map write") {
		t.Fatalf("status = %+v", st[0])
	}
	if len(r.Tools()) != 0 {
		t.Fatal("panicking plugin's tool must be rolled back")
	}
}

func TestHostConfigAndNotice(t *testing.T) {
	r, notices := newTestRegistry(map[string]map[string]any{"cfg": {"threshold": 3}})
	var seen map[string]any
	r.Load(context.Background(),
		fakePlugin{"cfg", func(_ context.Context, h Host) error {
			seen = h.Config()
			h.Notice("hello")
			return nil
		}},
		fakePlugin{"nocfg", func(_ context.Context, h Host) error {
			if h.Config() == nil {
				return errors.New("Config must return an empty map, not nil")
			}
			return nil
		}},
	)
	if seen["threshold"] != 3 {
		t.Fatalf("config = %v", seen)
	}
	if len(*notices) != 1 || (*notices)[0] != "plugin cfg: hello" {
		t.Fatalf("notices = %v", *notices)
	}
	if st := r.Statuses(); st[1].State != StateReady {
		t.Fatalf("nocfg = %+v", st[1])
	}
}

func TestEmptyNamesRefused(t *testing.T) {
	r, _ := newTestRegistry(nil)
	var errs []error
	r.Load(context.Background(),
		fakePlugin{"p", func(_ context.Context, h Host) error {
			errs = append(errs, h.RegisterTool(tool.Tool{}))
			errs = append(errs, h.RegisterCommand(Command{}))
			return nil
		}},
	)
	for _, err := range errs {
		if err == nil {
			t.Fatal("empty name must be refused")
		}
	}
	if len(r.Tools()) != 0 || len(r.Commands()) != 0 {
		t.Fatal("nothing should be registered")
	}
}

func TestLoadNeverExposesHalfLoadedPlugin(t *testing.T) {
	r, _ := newTestRegistry(nil)
	proceed := make(chan struct{})
	loadDone := make(chan struct{})

	go func() {
		defer close(loadDone)
		r.Load(context.Background(),
			fakePlugin{"slow", func(_ context.Context, h Host) error {
				if err := h.RegisterTool(namedTool("read")); err != nil {
					return err
				}
				<-proceed
				return errors.New("boom")
			}},
		)
	}()

	stop := make(chan struct{})
	pollDone := make(chan struct{})
	var sawTool bool
	go func() {
		defer close(pollDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, ok := r.Tool("read"); ok {
				sawTool = true
			}
			r.Tools()
			r.Statuses()
		}
	}()

	close(proceed)
	<-loadDone
	close(stop)
	<-pollDone

	if sawTool {
		t.Fatal("tool became visible while Init was still running")
	}
	if _, ok := r.Tool("read"); ok {
		t.Fatal("failed plugin's tool must never be committed")
	}
	if st := r.Statuses(); len(st) != 1 || st[0].State != StateFailed || st[0].Reason != "boom" {
		t.Fatalf("status = %+v", st)
	}
}

var _ = session.Workspace{} // CommandCall carries one; keep the import honest

// funcPlugin is a plugin whose whole body is its Init.
type funcPlugin struct {
	name string
	init func(h Host) error
}

func (f funcPlugin) Name() string                         { return f.name }
func (f funcPlugin) Init(_ context.Context, h Host) error { return f.init(h) }

func TestStatusWidgetsAndDisable(t *testing.T) {
	var statusCalls int
	var widgets []Widget
	r := NewRegistry(nil, nil)
	r.SetServices(Services{
		StatusChanged: func() { statusCalls++ },
		WidgetChanged: func(w Widget) { widgets = append(widgets, w) },
	})
	r.Disable("skip")
	r.Load(context.Background(),
		funcPlugin{"a", func(h Host) error {
			h.SetStatus("k", []Span{{Text: "one", Role: "muted"}})
			if err := h.SetWidget("w", SlotHeader, []Span{{Text: "hi", Role: "text"}}); err != nil {
				return err
			}
			return h.SetWidget("bad", "nowhere", nil)
		}},
		funcPlugin{"skip", func(h Host) error { t.Error("disabled plugin loaded"); return nil }},
		funcPlugin{"b", func(h Host) error { h.SetStatus("k", []Span{{Text: "two", Role: "muted"}}); return nil }},
	)
	if len(r.Statuses()) != 2 {
		t.Errorf("statuses %+v", r.Statuses())
	}
	if r.Statuses()[0].State != StateFailed || !strings.Contains(r.Statuses()[0].Reason, "unknown widget slot") {
		t.Errorf("a should fail on the bad slot: %+v", r.Statuses()[0])
	}
	// a failed at Init, so its staged status and widget were discarded with the rest.
	items := r.StatusItems()
	if len(items) != 1 || items[0].Owner != "b" || items[0].Key != "k" || items[0].Content[0].Text != "two" {
		t.Errorf("items %+v", items)
	}
	if statusCalls == 0 {
		t.Error("StatusChanged never called")
	}
	if len(r.Widgets()) != 0 {
		t.Errorf("widgets %+v", r.Widgets())
	}
	_ = widgets
}

func TestNoteAndConnectBeforeServices(t *testing.T) {
	r := NewRegistry(nil, nil)
	var gotErr error
	r.Load(context.Background(), funcPlugin{"a", func(h Host) error {
		gotErr = h.Note(ulid.Make(), "x", session.NoteInfo)
		_, cerr := h.Connect(context.Background())
		if !errors.Is(cerr, ErrNoServer) {
			t.Errorf("connect: %v", cerr)
		}
		return nil
	}})
	if !errors.Is(gotErr, ErrNoServer) {
		t.Errorf("note: %v", gotErr)
	}
}

func TestFailWithdrawsEverythingThePluginOwns(t *testing.T) {
	r, _ := newTestRegistry(nil)
	var statusCalls int
	var remaining [][]provider.Provider
	r.SetServices(Services{
		StatusChanged:    func() { statusCalls++ },
		ProvidersChanged: func(ps []provider.Provider) { remaining = append(remaining, ps) },
	})
	r.Load(context.Background(),
		funcPlugin{"a", func(h Host) error {
			if err := h.RegisterTool(namedTool("read")); err != nil {
				return err
			}
			if err := h.RegisterCommand(Command{Name: "init", Run: func(context.Context, CommandCall) (Action, error) { return NoAction{}, nil }}); err != nil {
				return err
			}
			if err := h.RegisterProvider(fakeProvider{"aperture"}); err != nil {
				return err
			}
			if err := h.RegisterHook(HookHandler{Point: HookBeforeTurn, Handle: func(context.Context, HookCall) (any, error) { return nil, nil }}); err != nil {
				return err
			}
			h.SetStatus("k", []Span{{Text: "one", Role: "muted"}})
			return h.SetWidget("w", SlotHeader, []Span{{Text: "hi", Role: "text"}})
		}},
		funcPlugin{"b", func(h Host) error {
			h.SetStatus("k", []Span{{Text: "two", Role: "muted"}})
			if err := h.RegisterProvider(fakeProvider{"mlx"}); err != nil {
				return err
			}
			return h.RegisterTool(namedTool("bash"))
		}},
	)
	before := statusCalls
	r.Fail("a", "process exited")

	if got := r.Tools(); len(got) != 1 || got[0].Name != "bash" {
		t.Errorf("tools %+v", got)
	}
	if _, ok := r.Tool("read"); ok {
		t.Error("read survived Fail")
	}
	if got := r.Commands(); len(got) != 0 {
		t.Errorf("commands %+v", got)
	}
	if _, ok := r.Command("init"); ok {
		t.Error("init survived Fail")
	}
	if got := r.Providers(); len(got) != 1 || got[0].Name() != "mlx" {
		t.Errorf("providers %+v", got)
	}
	// The provider registry keeps its own copy, so Fail has to hand it the survivors.
	if len(remaining) != 1 || len(remaining[0]) != 1 || remaining[0][0].Name() != "mlx" {
		t.Errorf("ProvidersChanged got %+v", remaining)
	}
	if got := r.Hooks(HookBeforeTurn); len(got) != 0 {
		t.Errorf("hooks %+v", got)
	}
	items := r.StatusItems()
	if len(items) != 1 || items[0].Owner != "b" {
		t.Errorf("items %+v", items)
	}
	if got := r.Widgets(); len(got) != 0 {
		t.Errorf("widgets %+v", got)
	}
	st := r.Statuses()
	if len(st) != 2 || st[0].Name != "a" || st[0].State != StateFailed || st[0].Reason != "process exited" {
		t.Errorf("statuses %+v", st)
	}
	if st[1].State != StateReady {
		t.Errorf("b should still be ready: %+v", st[1])
	}
	if statusCalls == before {
		t.Error("Fail did not report the status change")
	}
}

func TestToolViewFiltersAllowAndDeny(t *testing.T) {
	r, _ := newTestRegistry(nil)
	r.Load(context.Background(), fakePlugin{"a", func(_ context.Context, h Host) error {
		for _, n := range []string{"read", "bash", "agent"} {
			if err := h.RegisterTool(namedTool(n)); err != nil {
				return err
			}
		}
		return nil
	}})
	names := func(ts []tool.Tool) []string {
		out := make([]string, 0, len(ts))
		for _, t := range ts {
			out = append(out, t.Name)
		}
		return out
	}
	// A nil allow list is every tool; the deny list still applies.
	v := NewToolView(r, nil, []string{"agent"})
	if got := names(v.Tools()); strings.Join(got, ",") != "read,bash" {
		t.Errorf("nil allow = %v", got)
	}
	if _, ok := v.Tool("agent"); ok {
		t.Error("denied tool resolved")
	}
	if _, ok := v.Tool("bash"); !ok {
		t.Error("allowed tool did not resolve")
	}
	v = NewToolView(r, []string{"read", "agent"}, []string{"agent"})
	if got := names(v.Tools()); strings.Join(got, ",") != "read" {
		t.Errorf("allow minus deny = %v", got)
	}
	// An empty (non-nil) allow list is no tools at all.
	v = NewToolView(r, []string{}, nil)
	if got := v.Tools(); len(got) != 0 {
		t.Errorf("empty allow = %v", names(got))
	}
}

// closerPlugin is a plugin with work outliving Init, the shape Registry.Close exists for.
type closerPlugin struct {
	name   string
	init   error
	closed *[]string
	err    error
}

func (c closerPlugin) Name() string                           { return c.name }
func (c closerPlugin) Init(ctx context.Context, h Host) error { return c.init }
func (c closerPlugin) Close() error {
	*c.closed = append(*c.closed, c.name)
	return c.err
}

// TestCloseClosesEveryLoadedCloser: a plugin doing background work has no other way to hear
// that the process is going away. One that is not a Closer is skipped, one that never loaded
// is never closed, and a failing Close does not stop the ones behind it.
func TestCloseClosesEveryLoadedCloser(t *testing.T) {
	var closed []string
	r := NewRegistry(nil, func(string) {})
	r.Load(context.Background(),
		closerPlugin{name: "first", closed: &closed, err: errors.New("boom")},
		fakePlugin{name: "plain", init: func(context.Context, Host) error { return nil }},
		closerPlugin{name: "failed", init: errors.New("no"), closed: &closed},
		closerPlugin{name: "second", closed: &closed},
	)
	err := r.Close()
	if err == nil || !strings.Contains(err.Error(), "plugin first: boom") {
		t.Errorf("close error %v", err)
	}
	if got := strings.Join(closed, ","); got != "first,second" {
		t.Errorf("closed %q, want first,second: a plugin whose Init failed is not loaded", got)
	}
}
