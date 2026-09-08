package server

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

// idleProvider is registered so a turn can be wired at all; the turn in this test dies on
// its first append, long before any completion, so Complete must never run.
type idleProvider struct{ t *testing.T }

func (idleProvider) Name() string { return "idle" }

func (idleProvider) ListModels(context.Context) ([]provider.Model, error) {
	return []provider.Model{{
		Ref:           session.ModelRef{Provider: "idle", Model: "m1"},
		DisplayName:   "Idle 1",
		ContextWindow: 1000,
		Capabilities:  provider.Capabilities{Tools: true},
	}}, nil
}

func (p idleProvider) Complete(context.Context, provider.Request, func(provider.Part) error) error {
	p.t.Error("the provider must not be called for a turn that never started")
	return errors.New("must not run")
}

type idlePlugin struct{ p idleProvider }

func (idlePlugin) Name() string { return "idle" }

func (ip idlePlugin) Init(ctx context.Context, h plugin.Host) error { return h.RegisterProvider(ip.p) }

// TestStartTurnReturnsWhenTheFirstAppendFails is the other half of the invalid-submit fix,
// for every path that reaches startTurn: when the runner fails before appending anything,
// startTurn must return an error rather than park forever on a first append that will never
// come. command.run's SubmitPrompt reaches startTurn without going through submit's
// validation, so the wait itself has to be answerable.
func TestStartTurnReturnsWhenTheFirstAppendFails(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := session.OpenStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	plugins := plugin.NewRegistry(nil, func(string) {})
	plugins.Load(ctx, idlePlugin{idleProvider{t}})
	reg := provider.NewRegistry(filepath.Join(dir, "registry.json"), plugins.Providers()...)
	if err := reg.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.MaxTokens = 1000
	srv := New(Deps{Version: "test", Config: cfg, Store: store, Registry: reg, Plugins: plugins, Gate: gate.New(nil)})
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	m, err := reg.Resolve("idle:m1")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := session.Open(store, session.SessionOpened{
		SchemaVersion: 1, RudyVersion: "test",
		Workspace: session.Workspace{Root: t.TempDir(), ProjectID: "local/test"},
		Model:     m.Ref, Thinking: session.ThinkingOff, Mode: session.ModeOff, Agent: "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	ls := newLive(sess, m)
	srv.mu.Lock()
	srv.live[sess.ID()] = ls
	srv.mu.Unlock()

	type result struct {
		id  string
		err error
	}
	done := make(chan result, 1)
	go func() {
		id, e := srv.startTurn(ls, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{{Type: session.BlockText}}})
		done <- result{id, e}
	}()
	select {
	case got := <-done:
		if got.err == nil {
			t.Fatalf("startTurn returned turn %q and no error for a turn that never started", got.id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("startTurn parked waiting for a first append that will never happen")
	}
}
