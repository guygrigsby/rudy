package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// blockingProvider never answers ListModels; it blocks until its context is done, standing
// in for a stalled proxy on the real path this fix guards against.
type blockingProvider struct{ name string }

func (b blockingProvider) Name() string { return b.name }

func (b blockingProvider) ListModels(ctx context.Context) ([]provider.Model, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (b blockingProvider) Complete(ctx context.Context, req provider.Request, emit func(provider.Part) error) error {
	<-ctx.Done()
	return ctx.Err()
}

type blockingPlugin struct{ p blockingProvider }

func (blockingPlugin) Name() string { return "blocking" }

func (bp blockingPlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterProvider(bp.p)
}

func TestBuildFailsFastWhenRefreshTimesOutWithNoSnapshot(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(base, "run"))
	var stderr bytes.Buffer
	start := time.Now()
	_, err := Build(context.Background(), BuildOptions{
		Version: "test",
		Overrides: map[string]any{
			"default.provider": "blocking",
			"default.model":    "m",
			"permissions.mode": "off",
		},
		Plugins:        append(BuiltinTools(), blockingPlugin{blockingProvider{"blocking"}}),
		Home:           base,
		Stderr:         &stderr,
		RefreshTimeout: 200 * time.Millisecond,
	})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Build took %v with no snapshot, want under 1s", elapsed)
	}
	if err == nil {
		t.Fatal("expected a fatal error when the registry cannot be reached")
	}
	if !strings.Contains(err.Error(), "blocking") {
		t.Errorf("error %q does not name the default provider", err.Error())
	}
	if !strings.Contains(err.Error(), "200ms") {
		t.Errorf("error %q does not mention the timeout", err.Error())
	}
	if !strings.Contains(stderr.String(), "refreshing model registry from blocking") {
		t.Errorf("stderr %q missing the pre-refresh notice", stderr.String())
	}
}

func TestBuildWarnsAndSucceedsWhenRefreshTimesOutWithASnapshot(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(base, "run"))
	cacheDir := filepath.Join(base, "cache", "rudy")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	snapshot := `{"fetched_at":"2026-09-07T00:00:00Z","models":[` +
		`{"ref":{"provider":"blocking","model":"m"},"display_name":"Blocking M",` +
		`"context_window":1000,"max_output":100,"pricing":{"input":"0","output":"0"},` +
		`"capabilities":{"tools":true}}]}`
	if err := os.WriteFile(filepath.Join(cacheDir, "registry.json"), []byte(snapshot), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	start := time.Now()
	b, err := Build(context.Background(), BuildOptions{
		Version: "test",
		Overrides: map[string]any{
			"default.provider": "blocking",
			"default.model":    "m",
			"permissions.mode": "off",
		},
		Plugins:        append(BuiltinTools(), blockingPlugin{blockingProvider{"blocking"}}),
		Home:           base,
		Stderr:         &stderr,
		RefreshTimeout: 200 * time.Millisecond,
	})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Build took %v with a snapshot present, want under 1s", elapsed)
	}
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = b.Server.Shutdown(context.Background()) }()
	if !strings.Contains(stderr.String(), "registry refresh:") {
		t.Errorf("stderr %q missing the refresh warning", stderr.String())
	}
	if strings.Contains(stderr.String(), "refreshing model registry from") {
		t.Errorf("stderr %q printed the pre-refresh notice despite a snapshot on disk", stderr.String())
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
