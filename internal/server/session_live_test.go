package server

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/turn"
)

// TestMarkStartingBeforeAnyObserverCallback is the unit-level regression test for the fix
// round 2 residual: installing a runner must make isActive(ls.mirroredState()) true the
// instant mu is released, with no reliance on the runner's own first StateChanged callback
// having fired yet. It installs a runner through the exact sequence startTurn performs -
// mu.Lock, construct the turn.Runner, ls.runner = r, ls.markStarting(""), mu.Unlock - but,
// unlike startTurn, never calls spawnTurn or Run: the Provider here is nil and is never
// invoked, so no Observer callback can possibly have fired by the time this asserts. Without
// markStarting, this fails deterministically (the mirror still reads its zero value,
// turn.State(""), which isActive treats as false), not flakily: nothing here races a real
// goroutine to reproduce the narrow window fix round 1 left open.
func TestMarkStartingBeforeAnyObserverCallback(t *testing.T) {
	dir := t.TempDir()
	store, err := session.OpenStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := session.Open(store, session.SessionOpened{
		SchemaVersion: 1,
		RudyVersion:   "test",
		Workspace:     session.Workspace{Root: dir},
		Model:         session.ModelRef{Provider: "fake", Model: "m1"},
		Thinking:      session.ThinkingOff,
		Mode:          session.ModeStrict,
		Agent:         "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	m := provider.Model{Ref: session.ModelRef{Provider: "fake", Model: "m1"}}
	ls := newLive(sess, m)

	// Confirm the precondition the rest of this test depends on: nothing has marked this
	// session active yet.
	if st, _ := ls.mirroredState(); isActive(st) {
		t.Fatalf("state = %q, want inactive before any install", st)
	}

	tools := plugin.NewRegistry(nil, func(string) {})
	r := turn.NewRunner(turn.Config{
		Session:  sess,
		Provider: nil, // never called: this test never spawns a goroutine that calls Run
		Model:    m,
		Tools:    tools,
		Gate:     gate.New(nil),
		Observer: &fanout{ls: ls, sid: sess.ID().String()},
		System:   "test",
	})

	ls.mu.Lock()
	ls.runner = r
	ls.markStarting("")
	ls.mu.Unlock()

	st, _ := ls.mirroredState()
	if !isActive(st) {
		t.Fatalf("state = %q, want an active state immediately after install, before any Run call", st)
	}
	ls.mu.Lock()
	installed := ls.runner != nil
	ls.mu.Unlock()
	if !installed {
		t.Fatal("runner not installed")
	}
}

// TestFirstAskerNeverAPluginConnection: a plugin connection can send a hello of its own, and
// the one that opened a child session is the connection subscribed to it. Routing that child's
// permission question there would park it behind the tool call it is the answer to, so the
// question walks up to the parent's human instead. handleHello refuses the claim at the door
// as well; both halves are asserted here.
func TestFirstAskerNeverAPluginConnection(t *testing.T) {
	srv := New(Deps{Version: "test"})

	pluginConn := newConn(1, nil)
	pluginConn.plugin = "subagents"
	pluginConn.hello = true
	raw, err := json.Marshal(protocol.ClientHelloParams{Client: "subagents", Version: "test", Asker: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, e := srv.handleHello(pluginConn, raw); e != nil {
		t.Fatalf("hello: %v", e)
	}
	if pluginConn.asker {
		t.Error("a plugin connection claimed asker and the server believed it")
	}

	clientConn := newConn(2, nil)
	if _, e := srv.handleHello(clientConn, raw); e != nil {
		t.Fatalf("hello: %v", e)
	}
	if !clientConn.asker {
		t.Fatal("a client connection that declared asker must be one")
	}

	// The plugin subscribed first, so order alone would pick it.
	parent := &liveSession{}
	parent.conns = []*conn{pluginConn, clientConn}
	child := &liveSession{parent: parent}
	// Even with the flag set by hand, the walk skips a plugin connection.
	pluginConn.asker = true
	child.conns = []*conn{pluginConn}
	if got := child.firstAsker(); got != clientConn {
		t.Errorf("child asker = %+v, want the parent's client connection", got)
	}
	if got := parent.firstAsker(); got != clientConn {
		t.Errorf("parent asker = %+v, want its client connection", got)
	}
	// A session whose only subscriber is a plugin has no asker at all, which is a deny.
	lone := &liveSession{}
	lone.conns = []*conn{pluginConn}
	if got := lone.firstAsker(); got != nil {
		t.Errorf("lone plugin session asker = %+v, want none", got)
	}
}
