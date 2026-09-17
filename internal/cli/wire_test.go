// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/protocol"
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
	return testBuilderOver(t, fp, nil, extra...)
}

// testBuilderOver is testBuilder with over laid on top of its three defaults, for the tests
// that need a config value the default set does not carry.
func testBuilderOver(t *testing.T, fp *fakeProvider, over map[string]any, extra ...plugin.Plugin) buildFunc {
	t.Helper()
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
	// The runtime dir is not under base: it holds the default socket, a unix path is 104
	// bytes, and t.TempDir() spends most of that on the test's own name before the socket is
	// named. Every client probes that path now, so a fixture that cannot be dialed would be
	// a probe failure rather than the empty runtime dir it is meant to be.
	t.Setenv("XDG_RUNTIME_DIR", sockDir(t))
	plugins := append(BuiltinTools(), fakePlugin{fp})
	plugins = append(plugins, extra...)
	overrides := map[string]any{
		"default.provider": "fake",
		"default.model":    "m",
		"permissions.mode": "off",
	}
	maps.Copy(overrides, over)
	// The caller's Stderr and Socket are its own; everything else is the test's.
	return func(ctx context.Context, o BuildOptions) (*Built, error) {
		o.Version = "test"
		o.Overrides = overrides
		o.Plugins = plugins
		o.Home = base
		return Build(ctx, o)
	}
}

func TestBuildLoadsPluginsAndRegistry(t *testing.T) {
	fp := &fakeProvider{}
	b, err := testBuilder(t, fp)(context.Background(), BuildOptions{Stderr: io.Discard})
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

// TestBuildWiresTheServerSocketFromPaths: wire.go sets server.Deps.Socket from
// paths.Socket(), the only place a Server ever learns which socket it was built with. Two
// Build() calls over the same XDG roots are two rudy processes finding the same session
// store on disk; the second one resuming a session the first still holds live gets the
// unavailable error named after the second server's own socket, proving the value flowed
// from Build's paths through to the server rather than staying its zero value.
func TestBuildWiresTheServerSocketFromPaths(t *testing.T) {
	fp := &fakeProvider{}
	build := testBuilder(t, fp)
	ctx := context.Background()

	b1, err := build(ctx, BuildOptions{Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Build (1): %v", err)
	}
	defer func() { _ = b1.Server.Shutdown(context.Background()) }()
	cl1, close1, err := serveInMemory(b1, "test", false)
	if err != nil {
		t.Fatalf("serveInMemory (1): %v", err)
	}
	defer close1()
	var info protocol.SessionInfo
	if err := cl1.Call(ctx, protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: t.TempDir()}, &info); err != nil {
		t.Fatalf("open: %v", err)
	}

	b2, err := build(ctx, BuildOptions{Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Build (2): %v", err)
	}
	defer func() { _ = b2.Server.Shutdown(context.Background()) }()
	cl2, close2, err := serveInMemory(b2, "test", false)
	if err != nil {
		t.Fatalf("serveInMemory (2): %v", err)
	}
	defer close2()

	var resumed protocol.SessionInfo
	err = cl2.Call(ctx, protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: info.SessionID}, &resumed)
	var pe *protocol.Error
	if !errors.As(err, &pe) {
		t.Fatalf("resume of a locked session = %v, want *protocol.Error", err)
	}
	if pe.Code != protocol.CodeUnavailable {
		t.Fatalf("code = %d, want CodeUnavailable", pe.Code)
	}
	var data struct {
		Socket string `json:"socket"`
	}
	if !protocol.ErrorData(pe, &data) {
		t.Fatalf("no data on %+v", pe)
	}
	if data.Socket != b2.Paths.Socket() {
		t.Fatalf("socket = %q, want %q (the resuming Build's own paths.Socket())", data.Socket, b2.Paths.Socket())
	}
}

// TestBuildTakesTheSocketFromOptions: rudy serve --socket serves a path the XDG defaults
// know nothing about, and a locked session has to name the socket a client can actually
// reach this server on, not the one it would have used. Same shape as the test above: the
// second Build is the second process, and the error it answers a locked resume with is
// where the option shows up.
func TestBuildTakesTheSocketFromOptions(t *testing.T) {
	fp := &fakeProvider{}
	build := testBuilder(t, fp)
	ctx := context.Background()
	const socket = "/tmp/rudy-not-the-default.sock"

	b1, err := build(ctx, BuildOptions{Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Build (1): %v", err)
	}
	defer func() { _ = b1.Server.Shutdown(context.Background()) }()
	cl1, close1, err := serveInMemory(b1, "test", false)
	if err != nil {
		t.Fatalf("serveInMemory (1): %v", err)
	}
	defer close1()
	var info protocol.SessionInfo
	if err := cl1.Call(ctx, protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: t.TempDir()}, &info); err != nil {
		t.Fatalf("open: %v", err)
	}

	b2, err := build(ctx, BuildOptions{Stderr: io.Discard, Socket: socket})
	if err != nil {
		t.Fatalf("Build (2): %v", err)
	}
	defer func() { _ = b2.Server.Shutdown(context.Background()) }()
	cl2, close2, err := serveInMemory(b2, "test", false)
	if err != nil {
		t.Fatalf("serveInMemory (2): %v", err)
	}
	defer close2()

	var resumed protocol.SessionInfo
	err = cl2.Call(ctx, protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: info.SessionID}, &resumed)
	var pe *protocol.Error
	if !errors.As(err, &pe) {
		t.Fatalf("resume of a locked session = %v, want *protocol.Error", err)
	}
	var data struct {
		Socket string `json:"socket"`
	}
	if !protocol.ErrorData(pe, &data) {
		t.Fatalf("no data on %+v", pe)
	}
	if data.Socket != socket {
		t.Fatalf("socket = %q, want %q (BuildOptions.Socket)", data.Socket, socket)
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
	// The runtime dir is not under base: it holds the default socket, a unix path is 104
	// bytes, and t.TempDir() spends most of that on the test's own name before the socket is
	// named. Every client probes that path now, so a fixture that cannot be dialed would be
	// a probe failure rather than the empty runtime dir it is meant to be.
	t.Setenv("XDG_RUNTIME_DIR", sockDir(t))
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
	// The runtime dir is not under base: it holds the default socket, a unix path is 104
	// bytes, and t.TempDir() spends most of that on the test's own name before the socket is
	// named. Every client probes that path now, so a fixture that cannot be dialed would be
	// a probe failure rather than the empty runtime dir it is meant to be.
	t.Setenv("XDG_RUNTIME_DIR", sockDir(t))
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
	// The runtime dir is not under base: it holds the default socket, a unix path is 104
	// bytes, and t.TempDir() spends most of that on the test's own name before the socket is
	// named. Every client probes that path now, so a fixture that cannot be dialed would be
	// a probe failure rather than the empty runtime dir it is meant to be.
	t.Setenv("XDG_RUNTIME_DIR", sockDir(t))
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

// summarizeFixture is a registry holding the fake provider's one model, ready for the memory
// plugin's summarize closure.
func summarizeFixture(t *testing.T, script ...[]provider.Part) (*fakeProvider, *provider.Registry, *config.Config) {
	t.Helper()
	fp := &fakeProvider{script: script}
	reg := provider.NewRegistry(filepath.Join(t.TempDir(), "registry.json"), fp)
	if err := reg.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	cfg := &config.Config{MaxTokens: 4096}
	cfg.Default.Provider, cfg.Default.Model = "fake", "m"
	return fp, reg, cfg
}

// TestSummarizeRunsOnePromptWithThinkingOff pins the request the fold makes: the default model,
// the prompt as a single user message, no thinking, and only the text deltas collected.
func TestSummarizeRunsOnePromptWithThinkingOff(t *testing.T) {
	fp, reg, cfg := summarizeFixture(t, []provider.Part{
		{Type: provider.PartThinkingDelta, Text: "ignored"},
		{Type: provider.PartTextDelta, Text: "[high] a | id"},
		{Type: provider.PartTextDelta, Text: "\n"},
		{Type: provider.PartStop, StopReason: session.StopEndTurn},
	})
	sid := ulid.Make()
	out, err := summarizeWith(cfg, reg, func(string) {})(context.Background(), sid.String(), "observe this")
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if out != "[high] a | id\n" {
		t.Errorf("out %q", out)
	}
	req := fp.request(0)
	if req.Model != (session.ModelRef{Provider: "fake", Model: "m"}) {
		t.Errorf("model %+v", req.Model)
	}
	if req.Thinking != session.ThinkingOff {
		t.Errorf("thinking %q", req.Thinking)
	}
	if req.System != "" || len(req.Tools) != 0 {
		t.Errorf("request carries a system prompt or tools: %+v", req)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != provider.RoleUser ||
		len(req.Messages[0].Content) != 1 || req.Messages[0].Content[0].Text != "observe this" {
		t.Errorf("messages %+v", req.Messages)
	}
	if req.MaxTokens != 4096 {
		t.Errorf("max tokens %d", req.MaxTokens)
	}
	// The fold is made on a session's behalf, and the request says so: the provider sends it
	// as X-Rudy-Session like every other request that session makes.
	if req.SessionID != sid {
		t.Errorf("session id %s, want %s", req.SessionID, sid)
	}
}

// TestSummarizeFallsBackToTheDefaultModelOnce: a summary_model that is not in the registry
// still folds, on the default model, and says so once rather than once per turn.
func TestSummarizeFallsBackToTheDefaultModelOnce(t *testing.T) {
	fp, reg, cfg := summarizeFixture(t, say("one"), say("two"))
	cfg.Memory.SummaryModel = "gone:away"
	var notices []string
	summarize := summarizeWith(cfg, reg, func(s string) { notices = append(notices, s) })
	for range 2 {
		if _, err := summarize(context.Background(), ulid.Make().String(), "observe this"); err != nil {
			t.Fatalf("summarize: %v", err)
		}
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "gone:away") {
		t.Fatalf("notices %q", notices)
	}
	if got := fp.request(1).Model.Model; got != "m" {
		t.Errorf("second call model %q, want the default", got)
	}
}

// TestSummarizeResolvesTheConfiguredModel: when summary_model does resolve it is what the fold
// runs on, and nothing is said.
func TestSummarizeResolvesTheConfiguredModel(t *testing.T) {
	fp, reg, cfg := summarizeFixture(t, say("one"))
	cfg.Default.Model = "unused"
	cfg.Memory.SummaryModel = "fake:m"
	var notices []string
	if _, err := summarizeWith(cfg, reg, func(s string) { notices = append(notices, s) })(context.Background(), "not-a-ulid", "p"); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if len(notices) != 0 {
		t.Errorf("notices %q", notices)
	}
	if got := fp.request(0).Model.Model; got != "m" {
		t.Errorf("model %q", got)
	}
}

// closingPlugin records that Built.Close reached the plugin registry.
type closingPlugin struct{ closed chan struct{} }

func (closingPlugin) Name() string                            { return "closing" }
func (closingPlugin) Init(context.Context, plugin.Host) error { return nil }
func (c closingPlugin) Close() error                          { close(c.closed); return nil }

// TestBuiltCloseShutsTheServerAndThePlugins: every CLI exit path goes through Built.Close, and
// a plugin with work outliving a hook (a memory fold, a bundle commit) is only waited on
// because that path reaches the registry as well as the server.
func TestBuiltCloseShutsTheServerAndThePlugins(t *testing.T) {
	fp := &fakeProvider{}
	c := closingPlugin{closed: make(chan struct{})}
	b, err := testBuilder(t, fp, c)(context.Background(), BuildOptions{Stderr: io.Discard})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := b.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-c.closed:
	default:
		t.Fatal("Built.Close did not close the plugins")
	}
}

// writeManifest puts one plugin.toml under root/plugins/<dir>.
func writeManifest(t *testing.T, root, dir, name string) {
	t.Helper()
	pdir := filepath.Join(root, "plugins", dir)
	if err := os.MkdirAll(pdir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "name = \"" + name + "\"\nversion = \"0.1.0\"\nprotocol_version = 1\ncommand = \"" + name + "\"\n"
	if err := os.WriteFile(filepath.Join(pdir, "plugin.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverPluginsSkipsDisabledAndReportsMismatches(t *testing.T) {
	data := t.TempDir()
	cfgRoot := t.TempDir()
	writeManifest(t, data, "hello", "hello")
	writeManifest(t, cfgRoot, "other", "other")
	writeManifest(t, cfgRoot, "wrongdir", "elsewhere")
	lock := filepath.Join(data, "plugins.lock.toml")
	body := "[plugins.hello]\nenabled = true\n\n[plugins.other]\nenabled = false\n"
	if err := os.WriteFile(lock, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	ms, errs := discoverPlugins([]string{data, cfgRoot}, lock)
	if len(ms) != 1 || ms[0].Name != "hello" {
		t.Fatalf("manifests = %+v", ms)
	}
	if ms[0].Command != "hello" || ms[0].Dir != filepath.Join(data, "plugins", "hello") {
		t.Fatalf("manifest = %+v", ms[0])
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "elsewhere") {
		t.Fatalf("errors = %v", errs)
	}
}

func TestDiscoverPluginsWithoutALockKeepsEverything(t *testing.T) {
	data := t.TempDir()
	writeManifest(t, data, "hello", "hello")
	ms, errs := discoverPlugins([]string{data}, filepath.Join(data, "plugins.lock.toml"))
	if len(ms) != 1 || len(errs) != 0 {
		t.Fatalf("manifests = %+v, errors = %v", ms, errs)
	}
}

func TestDiscoverPluginsSpawnsNothingWhenTheLockIsCorrupt(t *testing.T) {
	data := t.TempDir()
	writeManifest(t, data, "hello", "hello")
	lock := filepath.Join(data, "plugins.lock.toml")
	if err := os.WriteFile(lock, []byte("[plugins.hello\nenabled = \"yes"), 0o600); err != nil {
		t.Fatal(err)
	}
	ms, errs := discoverPlugins([]string{data}, lock)
	if len(ms) != 0 {
		t.Fatalf("manifests = %+v, want none while the lock is unreadable", ms)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "spawning no plugins until it is fixed") {
		t.Fatalf("errors = %v", errs)
	}
}

// TestBuildWithNoProvidersNamesTheConfigFile pins what a fresh install is told. Nothing was
// dialed, so a timeout is the wrong story: the error names the file that holds a provider
// and the command that writes it, and no refresh line is printed for an empty provider set.
func TestBuildWithNoProvidersNamesTheConfigFile(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
	t.Setenv("XDG_RUNTIME_DIR", sockDir(t))
	var stderr bytes.Buffer
	_, err := Build(context.Background(), BuildOptions{
		Version: "test",
		Plugins: BuiltinTools(),
		Home:    base,
		Stderr:  &stderr,
	})
	if err == nil {
		t.Fatal("expected an error with no provider configured")
	}
	want := filepath.Join(base, "config", "rudy", "config.toml")
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name %s", err.Error(), want)
	}
	if !strings.Contains(err.Error(), "rudy config sync") {
		t.Errorf("error %q does not name the command that writes it", err.Error())
	}
	if strings.Contains(err.Error(), "could not be reached") {
		t.Errorf("error %q blames a timeout for a provider that was never dialed", err.Error())
	}
	if strings.Contains(stderr.String(), "refreshing model registry") {
		t.Errorf("stderr %q announced a refresh with no provider to refresh from", stderr.String())
	}
}

// TestSummarizeAsksForWhatTheSummaryModelServes: the fold is a provider request like any
// other, so it resolves max_tokens against its own model. It passed cfg.MaxTokens raw, which
// with the default 0 meant a request that named no limit at all: omitted on the OpenAI wire
// and refused outright on Anthropic's, where max_tokens is required.
func TestSummarizeAsksForWhatTheSummaryModelServes(t *testing.T) {
	fp, reg, cfg := summarizeFixture(t, []provider.Part{
		{Type: provider.PartTextDelta, Text: "summary"},
		{Type: provider.PartStop, StopReason: session.StopEndTurn},
	})
	cfg.MaxTokens = 0
	sid := ulid.Make()
	if _, err := summarizeWith(cfg, reg, func(string) {})(context.Background(), sid.String(), "observe this"); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if got := fp.request(0).MaxTokens; got != 8192 {
		t.Errorf("fold asked for %d output tokens, want the model's 8192", got)
	}
}
