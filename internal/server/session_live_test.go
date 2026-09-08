package server

import (
	"path/filepath"
	"testing"

	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
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
