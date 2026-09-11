package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/server"
	"github.com/guygrigsby/rudy/internal/session"
)

// helloBin is the example spawned plugin, built once for the whole package; "" means go is
// not on PATH and every test that needs a child process skips.
var helloBin string

func TestMain(m *testing.M) {
	os.Exit(func() int {
		if _, err := exec.LookPath("go"); err != nil {
			return m.Run()
		}
		dir, err := os.MkdirTemp("", "rudy-hello-plugin")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer func() { _ = os.RemoveAll(dir) }()
		bin := filepath.Join(dir, "hello")
		build := exec.Command("go", "build", "-o", bin, "./examples/plugins/hello")
		build.Dir = filepath.Join("..", "..")
		if out, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build hello plugin: %v\n%s", err, out)
			return 1
		}
		helloBin = bin
		return m.Run()
	}())
}

// providerPlugin registers one provider and nothing else, so a spawned plugin's own tool and
// command are the only ones in the registry.
type providerPlugin struct{ p provider.Provider }

func (pp *providerPlugin) Name() string { return "fake" }

func (pp *providerPlugin) Init(_ context.Context, h plugin.Host) error {
	return h.RegisterProvider(pp.p)
}

// spawnHarness is a server whose client is already attached when the plugins load, which is
// what makes a load's own state notifications observable.
type spawnHarness struct {
	srv     *server.Server
	cl      *protocol.Client
	ws      string
	prov    *scriptProvider
	notices func() []string
}

func newSpawnHarness(t *testing.T, prov *scriptProvider, env map[string]string) *spawnHarness {
	t.Helper()
	if helloBin == "" {
		t.Skip("go is not on PATH, cannot build the example plugin")
	}
	ctx := context.Background()
	dir := t.TempDir()
	store, err := session.OpenStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var notices []string
	preg := plugin.NewRegistry(nil, func(s string) {
		mu.Lock()
		notices = append(notices, s)
		mu.Unlock()
	})
	reg := provider.NewRegistry(filepath.Join(dir, "registry.json"))
	srv := server.New(server.Deps{
		Version:  "test",
		Config:   testConfig(),
		Store:    store,
		Registry: reg,
		Plugins:  preg,
		Gate:     gate.New(nil),
		Hooks:    plugin.NewHookRunner(preg, 5*time.Second, func(string) {}),
	})
	services := srv.PluginServices()
	services.ProvidersChanged = func(ps []provider.Provider) { reg.SetProviders(ps...) }
	preg.SetServices(services)
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		_ = preg.Close(context.Background())
	})
	h := &spawnHarness{srv: srv, ws: t.TempDir(), prov: prov}
	h.notices = func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), notices...)
	}
	h.cl = dialAs(t, srv, true)
	sp := plugin.NewSpawned(
		plugin.Manifest{Name: "hello", Version: "0.1.0", ProtocolVersion: 1, Command: helloBin, Env: env},
		plugin.SpawnServices{
			ServePlugin: srv.ServePlugin,
			Version:     "test",
			Workspaces:  []string{h.ws},
			Fail:        preg.Fail,
		},
	)
	preg.Load(ctx, &providerPlugin{p: prov}, sp)
	reg.SetProviders(preg.Providers()...)
	if err := reg.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	return h
}

// helloStates collects the plugin.state notifications for the hello plugin until it reaches
// last.
func helloStates(t *testing.T, cl *protocol.Client, last string) []string {
	t.Helper()
	var states []string
	drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyPluginState {
			return false
		}
		var ps protocol.PluginState
		if err := json.Unmarshal(n.Params, &ps); err != nil {
			t.Fatal(err)
		}
		if ps.Name != "hello" {
			return false
		}
		if ps.Origin != plugin.OriginSpawned {
			t.Errorf("origin = %q, want %q", ps.Origin, plugin.OriginSpawned)
		}
		states = append(states, ps.State)
		if ps.State == string(plugin.StateFailed) && !strings.Contains(ps.Reason, "exit status 3") {
			t.Errorf("failure reason = %q", ps.Reason)
		}
		return ps.State == last
	})
	return states
}

func (h *spawnHarness) openSession(t *testing.T) protocol.SessionInfo {
	t.Helper()
	var info protocol.SessionInfo
	err := h.cl.Call(context.Background(), protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: h.ws}, &info)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return info
}

// runTurn submits one message and drains until the turn completes.
func (h *spawnHarness) runTurn(t *testing.T, info protocol.SessionInfo) []session.Entry {
	t.Helper()
	ctx := context.Background()
	var sub protocol.SessionSubmitResult
	err := h.cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID,
		Content:   []session.Block{session.TextBlock("go")},
		Source:    session.SourceTyped,
	}, &sub)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	ns := drain(t, h.cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyTurnState {
			return false
		}
		var ts protocol.TurnStateChanged
		if err := json.Unmarshal(n.Params, &ts); err != nil {
			t.Fatal(err)
		}
		return ts.State == "completed"
	})
	return entries(t, ns)
}

func TestSpawnedPluginServesToolHookAndCommand(t *testing.T) {
	h := newSpawnHarness(t, &scriptProvider{toolName: "hello_upper", toolInput: `{"text":"hello"}`}, nil)
	if got := helloStates(t, h.cl, string(plugin.StateReady)); !equalStrings(got, []string{"loading", "ready"}) {
		t.Fatalf("plugin states = %v (notices %v)", got, h.notices())
	}
	info := h.openSession(t)
	es := h.runTurn(t, info)

	// A safe tool is still recorded as decided by class before it runs.
	want := []session.Kind{
		session.KindSessionOpened, session.KindUserMessage, session.KindAssistantMessage,
		session.KindPermissionDecision, session.KindToolResult, session.KindAssistantMessage,
	}
	if !equalKinds(kinds(es), want) {
		t.Fatalf("kinds = %v, want %v", kinds(es), want)
	}
	tr, ok := es[4].Payload.(session.ToolResult)
	if !ok {
		t.Fatalf("entry 4 = %#v", es[4].Payload)
	}
	if tr.Outcome != session.OutcomeOK || len(tr.Content) != 1 || tr.Content[0].Text != "HELLO" {
		t.Fatalf("tool result = %+v", tr)
	}
	// The tool the child registered was offered, and its before_turn hook reached the prompt.
	req := h.prov.request(0)
	if !hasTool(req.Tools, "hello_upper") {
		t.Fatalf("tools = %v", defNames(req.Tools))
	}
	if !strings.Contains(req.System, "hello plugin was here") {
		t.Fatalf("system prompt = %q", req.System)
	}

	var cr protocol.CommandRunResult
	err := h.cl.Call(context.Background(), protocol.MethodCommandRun, protocol.CommandRunParams{
		SessionID: info.SessionID, Name: "hello",
	}, &cr)
	if err != nil {
		t.Fatalf("command run: %v", err)
	}
	if cr.Notice != "hello from the plugin" {
		t.Fatalf("notice = %q", cr.Notice)
	}
}

func TestSpawnedPluginThatDiesIsWithdrawn(t *testing.T) {
	h := newSpawnHarness(t, &scriptProvider{textOnly: true}, map[string]string{"HELLO_CRASH": "1"})
	got := helloStates(t, h.cl, string(plugin.StateFailed))
	if !equalStrings(got, []string{"loading", "ready", "failed"}) {
		t.Fatalf("plugin states = %v (notices %v)", got, h.notices())
	}
	joined := strings.Join(h.notices(), "\n")
	if !strings.Contains(joined, "hello") || !strings.Contains(joined, "exit status 3") {
		t.Fatalf("notices = %v", h.notices())
	}
	info := h.openSession(t)
	h.runTurn(t, info)
	if req := h.prov.request(0); hasTool(req.Tools, "hello_upper") {
		t.Fatalf("a dead plugin's tool is still offered: %v", defNames(req.Tools))
	}
	// Its command went with it.
	err := h.cl.Call(context.Background(), protocol.MethodCommandRun, protocol.CommandRunParams{
		SessionID: info.SessionID, Name: "hello",
	}, &protocol.CommandRunResult{})
	if c := code(t, err); c != protocol.CodeNotFound {
		t.Fatalf("command run after the plugin died: code %d (%v)", c, err)
	}
}

func hasTool(defs []provider.ToolDef, name string) bool {
	for _, d := range defs {
		if d.Name == name {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
