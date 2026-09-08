package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

// fakeProvider replays a script of parts, one script per Complete call, and records every
// request it saw. fail, when set, is returned from every Complete call instead.
type fakeProvider struct {
	mu       sync.Mutex
	script   [][]provider.Part
	fail     error
	block    bool // when true, Complete blocks on ctx instead of running fail or script
	calls    int
	requests []provider.Request
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) ListModels(ctx context.Context) ([]provider.Model, error) {
	return []provider.Model{{
		Ref:           session.ModelRef{Provider: "fake", Model: "m"},
		DisplayName:   "Fake M",
		ContextWindow: 100000,
		MaxOutput:     8192,
		Pricing:       provider.Pricing{Input: "0.000001", Output: "0.000002"},
		Capabilities:  provider.Capabilities{Tools: true},
	}}, nil
}

func (f *fakeProvider) Complete(ctx context.Context, req provider.Request, emit func(provider.Part) error) error {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	block := f.block
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	f.mu.Lock()
	if f.fail != nil {
		f.mu.Unlock()
		return f.fail
	}
	i := f.calls
	f.calls++
	if i >= len(f.script) {
		f.mu.Unlock()
		return &provider.Error{Class: session.ErrInternal, Message: "fake script exhausted"}
	}
	parts := f.script[i]
	f.mu.Unlock()
	for _, p := range parts {
		if err := emit(p); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeProvider) request(i int) provider.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

type fakePlugin struct{ p *fakeProvider }

func (fakePlugin) Name() string { return "fake" }

func (f fakePlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterProvider(f.p)
}

// say scripts a text-only reply.
func say(text string) []provider.Part {
	return []provider.Part{
		{Type: provider.PartTextDelta, Text: text},
		{Type: provider.PartUsage, Usage: session.Usage{Input: 10, Output: 2}},
		{Type: provider.PartStop, StopReason: session.StopEndTurn, StopReasonRaw: "stop"},
	}
}

// callTool scripts one tool call and a tool_use stop.
func callTool(id, name, input string) []provider.Part {
	return []provider.Part{
		{Type: provider.PartToolUseStart, ID: id, Name: name},
		{Type: provider.PartToolUseDelta, ID: id, Text: input},
		{Type: provider.PartToolUseEnd, ID: id},
		{Type: provider.PartUsage, Usage: session.Usage{Input: 10, Output: 5}},
		{Type: provider.PartStop, StopReason: session.StopToolUse, StopReasonRaw: "tool_calls"},
	}
}

// testBuilder points every XDG root at a temp dir, replaces the provider plugins with the fake
// and returns a buildFunc that opens mode off on model fake:m.
func testBuilder(t *testing.T, fp *fakeProvider, extra ...plugin.Plugin) buildFunc {
	t.Helper()
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(base, "run"))
	plugins := append(BuiltinTools(), fakePlugin{fp})
	plugins = append(plugins, extra...)
	return func(ctx context.Context, stderr io.Writer) (*Built, error) {
		return Build(ctx, BuildOptions{
			Version: "test",
			Overrides: map[string]any{
				"default.provider": "fake",
				"default.model":    "m",
				"permissions.mode": "off",
			},
			Plugins: plugins,
			Home:    base,
			Stderr:  stderr,
		})
	}
}

func TestBuildLoadsPluginsAndRegistry(t *testing.T) {
	fp := &fakeProvider{}
	b, err := testBuilder(t, fp)(context.Background(), io.Discard)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = b.Server.Shutdown(context.Background()) }()
	if _, ok := b.Plugins.Tool("glob"); !ok {
		t.Fatal("glob tool not registered")
	}
	m, err := b.Registry.Resolve("fake:m")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.DisplayName != "Fake M" {
		t.Fatalf("DisplayName = %q", m.DisplayName)
	}
	if _, err := os.Stat(filepath.Join(b.Paths.Cache, "registry.json")); err != nil {
		t.Fatalf("snapshot not written: %v", err)
	}
}

func TestStoreFromEnv(t *testing.T) {
	base := t.TempDir()
	env := func(key string) string {
		if key == "XDG_DATA_HOME" {
			return filepath.Join(base, "data")
		}
		return ""
	}
	st, err := storeFromEnv(env, base)
	if err != nil {
		t.Fatalf("storeFromEnv: %v", err)
	}
	want := filepath.Join(base, "data", "rudy", "sessions")
	if st.Root() != want {
		t.Fatalf("Root() = %q want %q", st.Root(), want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("store dir not created: %v", err)
	}
}

func TestBuildFailsWithNoModels(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(base, "run"))
	_, err := Build(context.Background(), BuildOptions{
		Version:   "test",
		Overrides: map[string]any{"default.provider": "none", "default.model": "m"},
		Plugins:   BuiltinTools(),
		Home:      base,
		Stderr:    io.Discard,
	})
	if err == nil {
		t.Fatal("expected an error with no providers and no snapshot")
	}
}
