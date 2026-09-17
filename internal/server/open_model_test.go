// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

// staleDefaultProvider serves one model, which is never the one the config names.
type staleDefaultProvider struct{}

func (staleDefaultProvider) Name() string { return "fake" }

func (staleDefaultProvider) ListModels(context.Context) ([]provider.Model, error) {
	return []provider.Model{{
		Ref:           session.ModelRef{Provider: "fake", Model: "m1"},
		DisplayName:   "Fake 1",
		ContextWindow: 100000,
		Capabilities:  provider.Capabilities{Tools: true},
	}}, nil
}

func (staleDefaultProvider) Complete(context.Context, provider.Request, func(provider.Part) error) error {
	return nil
}

type staleDefaultPlugin struct{}

func (staleDefaultPlugin) Name() string { return "fake" }

func (staleDefaultPlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterProvider(staleDefaultProvider{})
}

// openWith builds a Server whose configured default model is spec.
func openWith(t *testing.T, defProvider, defModel string) (*Server, *conn, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	store, err := session.OpenStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	plugins := plugin.NewRegistry(nil, func(string) {})
	plugins.Load(ctx, staleDefaultPlugin{})
	reg := provider.NewRegistry(filepath.Join(dir, "registry.json"), plugins.Providers()...)
	if err := reg.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.MaxTokens = 1000
	cfg.Default.Provider, cfg.Default.Model = defProvider, defModel
	cfg.Permissions.Mode = string(session.ModeOff)
	cfg.Default.Thinking = string(session.ThinkingOff)
	srv := New(Deps{Version: "test", Config: cfg, Store: store, Registry: reg, Plugins: plugins, Gate: gate.New(nil)})
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	_, serverEnd := protocol.Pipe()
	srv.mu.Lock()
	srv.nextID++
	cn := newConn(srv.nextID, serverEnd)
	cn.hello, cn.greeted = true, true
	srv.conns[cn.id] = cn
	srv.mu.Unlock()
	return srv, cn, t.TempDir()
}

// queuedNotices is every notice the server has enqueued for this connection.
func queuedNotices(cn *conn) []string {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	var out []string
	for _, item := range cn.queue {
		req, ok := item.msg.(protocol.Request)
		if !ok || req.Method != protocol.NotifyNotice {
			continue
		}
		out = append(out, string(req.Params))
	}
	return out
}

// TestAStaleConfiguredModelStillOpens is the operator's Monday morning: the model named in
// config.toml is gone from the provider, and rudy has to come up anyway. The session opens on
// the model as written, with a notice saying it is not in the registry, and the picker is
// there to change it. Refusing here means the harness cannot be started at all by the person
// who most needs to fix the config.
func TestAStaleConfiguredModelStillOpens(t *testing.T) {
	srv, cn, cwd := openWith(t, "fake", "gone-away")
	res, e := srv.open(cn, protocol.SessionOpenParams{Cwd: cwd})
	if e != nil {
		t.Fatalf("open with a stale configured model = %v, want a session", e)
	}
	info, ok := res.(protocol.SessionInfo)
	if !ok {
		t.Fatalf("open returned %T", res)
	}
	if info.Model.String() != "fake:gone-away" {
		t.Errorf("session model = %q, want the configured one as written", info.Model)
	}
	var told bool
	for _, n := range queuedNotices(cn) {
		if strings.Contains(n, "gone-away") {
			told = true
		}
	}
	if !told {
		t.Error("nothing told the client its configured model is not in the registry")
	}
}

// TestAnExplicitlyNamedModelStillFails keeps the refusal where a caller asked for something
// specific: that is a request the server cannot honour, not a stale config it can work around.
func TestAnExplicitlyNamedModelStillFails(t *testing.T) {
	srv, cn, cwd := openWith(t, "fake", "m1")
	_, e := srv.open(cn, protocol.SessionOpenParams{Cwd: cwd, Model: "fake:nope"})
	if e == nil || e.Code != protocol.CodeNotFound {
		t.Fatalf("open naming an unknown model = %v, want not_found", e)
	}
}
