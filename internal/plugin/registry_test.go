package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"

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
