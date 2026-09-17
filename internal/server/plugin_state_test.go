// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/server"
	"github.com/guygrigsby/rudy/internal/session"
)

// slowInitPlugin holds Init open until it is released, which parks the registry on loading
// for as long as a test needs. That is the window a connecting client races: it is in the
// broadcast set from the moment it is accepted, and its connect snapshot is sent later.
type slowInitPlugin struct{ release chan struct{} }

func (p *slowInitPlugin) Name() string { return "slow" }

func (p *slowInitPlugin) Init(ctx context.Context, h plugin.Host) error {
	<-p.release
	return nil
}

// loadingServer wires a server whose one plugin is still loading when it returns. Loading
// finishes when the returned func is called, and Load has returned by the time it does.
func loadingServer(t *testing.T, p *slowInitPlugin) (*server.Server, func()) {
	t.Helper()
	dir := t.TempDir()
	store, err := session.OpenStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	preg := plugin.NewRegistry(nil, func(string) {})
	reg := provider.NewRegistry(filepath.Join(dir, "registry.json"))
	cfg := testConfig()
	srv := server.New(server.Deps{
		Version: "test", Config: cfg, Store: store, Registry: reg, Plugins: preg,
		Gate: gate.New(nil), Hooks: plugin.NewHookRunner(preg, 5*time.Second, func(string) {}),
		Home: t.TempDir(),
	})
	preg.SetServices(srv.PluginServices())
	done := make(chan struct{})
	go func() {
		preg.Load(context.Background(), p)
		close(done)
	}()
	// Loading is in the registry before the test dials, so the snapshot a client gets has
	// something in it to be raced against.
	waitForState(t, preg, "loading")
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv, func() {
		close(p.release)
		<-done
	}
}

func waitForState(t *testing.T, preg *plugin.Registry, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, st := range preg.Statuses() {
			if st.Name == "slow" && string(st.State) == want {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("plugin never reached %q", want)
}

// pluginStates collects every plugin.state for name until the stream has been quiet for a
// beat, so a state that arrives after the one a test was waiting for is still seen.
func pluginStates(t *testing.T, cl *protocol.Client, name string, quiet time.Duration) []string {
	t.Helper()
	var out []string
	for {
		select {
		case n := <-cl.Notifications():
			if n.Method != protocol.NotifyPluginState {
				continue
			}
			var ps protocol.PluginState
			if err := json.Unmarshal(n.Params, &ps); err != nil {
				t.Fatal(err)
			}
			if ps.Name == name {
				out = append(out, ps.State)
			}
		case <-time.After(quiet):
			return out
		}
	}
}

// TestAClientConnectingMidLoadIsToldTheStateTwiceAtMost: a connection joins the broadcast
// set when it is accepted and gets its connect snapshot after, so the same state can reach
// it twice. The contract allows that repeat and forbids the other outcome: what a client is
// told last is what the plugin is (rudy-9jl).
func TestAClientConnectingMidLoadIsToldTheStateTwiceAtMost(t *testing.T) {
	p := &slowInitPlugin{release: make(chan struct{})}
	srv, finish := loadingServer(t, p)
	cl := dialAs(t, srv, false)

	loading := pluginStates(t, cl, "slow", 100*time.Millisecond)
	if len(loading) == 0 {
		t.Fatal("a client that connected while the plugin was loading was told nothing")
	}
	for _, st := range loading {
		if st != "loading" {
			t.Fatalf("states before the plugin finished loading = %v, want every one loading", loading)
		}
	}

	finish()
	ready := pluginStates(t, cl, "slow", 100*time.Millisecond)
	if len(ready) == 0 || ready[len(ready)-1] != "ready" {
		t.Fatalf("states after the plugin loaded = %v, want the last one ready", ready)
	}
	for _, st := range ready {
		if st == "loading" {
			t.Fatalf("states after the plugin loaded = %v, want no loading among them", ready)
		}
	}
}
