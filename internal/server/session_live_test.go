package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"slices"
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

// TestAskersNeverIncludeAPluginConnection: a plugin connection can send a hello of its own,
// and the one that opened a child session is the connection subscribed to it. Routing that
// child's permission question there would park it behind the tool call it is the answer to,
// so the question walks up to the parent's humans instead. handleHello refuses the claim at
// the door as well; both halves are asserted here, along with the walk returning every asker
// of the session it lands on rather than only the first.
func TestAskersNeverIncludeAPluginConnection(t *testing.T) {
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

	secondClient := newConn(3, nil)
	if _, e := srv.handleHello(secondClient, raw); e != nil {
		t.Fatalf("hello: %v", e)
	}

	// The plugin subscribed first, so order alone would pick it.
	parent := &liveSession{}
	parent.conns = []*conn{pluginConn, clientConn, secondClient}
	child := &liveSession{parent: parent}
	// Even with the flag set by hand, the walk skips a plugin connection.
	pluginConn.asker = true
	child.conns = []*conn{pluginConn}
	want := []*conn{clientConn, secondClient}
	if got := child.askers(); !slices.Equal(got, want) {
		t.Errorf("child askers = %+v, want the parent's client connections", got)
	}
	if got := parent.askers(); !slices.Equal(got, want) {
		t.Errorf("parent askers = %+v, want its client connections", got)
	}
	// A session whose only subscriber is a plugin has no asker at all, which is a deny.
	lone := &liveSession{}
	lone.conns = []*conn{pluginConn}
	if got := lone.askers(); len(got) != 0 {
		t.Errorf("lone plugin session askers = %+v, want none", got)
	}
}

// newTestLive opens a real session on disk and wraps it, the setup every liveSession unit test
// below needs before it can call anything that reads sess.ID().
func newTestLive(t *testing.T) *liveSession {
	t.Helper()
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
	return newLive(sess, provider.Model{Ref: session.ModelRef{Provider: "fake", Model: "m1"}})
}

// askerConn is a client connection that declared asker, with no transport under it: notify
// only enqueues, so nothing here needs a pump.
func askerConn(id int) *conn {
	cn := newConn(id, nil)
	cn.asker = true
	return cn
}

// TestAnAnswerRacingTheQuestionLeavesNothingStanding: session.answer can land between Ask
// publishing the pending channel under mu and publishing the question under obsMu. The
// tool_use id is in the assistant_message every subscriber already holds, so a client can
// answer before it has been asked. The handler then deletes a standing entry that is not there
// yet, and Ask publishes it a moment later, leaving a decided question standing for the rest
// of the turn: every asker attaching afterwards is shown a question whose decision has already
// replayed, and answering it is a conflict. Ask has to drop it when it takes the answer, the
// same as on the two branches beside it.
//
// The state that interleaving leaves behind is set up directly rather than raced for: the
// answer is delivered through the pending channel without standing being deleted, which is
// exactly what the handler's early half leaves.
func TestAnAnswerRacingTheQuestionLeavesNothingStanding(t *testing.T) {
	ls := newTestLive(t)
	ls.conns = []*conn{askerConn(1)}
	r := turn.NewRunner(turn.Config{
		Session:  ls.sess,
		Provider: nil, // never called: only TurnID is asked of this runner
		Model:    ls.model,
		Tools:    plugin.NewRegistry(nil, func(string) {}),
		Gate:     gate.New(nil),
		Observer: &fanout{ls: ls, sid: ls.sess.ID().String()},
		System:   "test",
	})
	a := &liveAsker{ls: ls, sid: ls.sess.ID().String(), runner: r}

	type outcome struct {
		ans turn.Answer
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		ans, err := a.Ask(context.Background(), turn.Question{ToolUseID: "tu1", Tool: "bash"})
		done <- outcome{ans, err}
	}()

	// Once the question is standing, stand has run and the pending channel is published.
	for {
		ls.obsMu.Lock()
		published := len(ls.standing) == 1
		ls.obsMu.Unlock()
		if published {
			break
		}
		runtime.Gosched()
	}
	ls.mu.Lock()
	ch, ok := ls.pending["tu1"]
	delete(ls.pending, "tu1")
	ls.mu.Unlock()
	if !ok {
		t.Fatal("the question was published with no pending channel behind it")
	}
	// The handler's other half ran while standing was still empty, so nothing is deleted
	// here. Only the answer arrives.
	ch <- turn.Answer{Decision: session.Allow, Scope: session.ScopeOnce, Reason: "yes"}
	got := <-done
	if got.err != nil || got.ans.Decision != session.Allow {
		t.Fatalf("ask = %+v, %v, want the allow it was sent", got.ans, got.err)
	}

	// An asker attaching now is owed nothing: the question has been decided.
	if at := ls.subscribeLocked(askerConn(2)); len(at.standing) != 0 {
		t.Fatalf("a decided question is still standing: %+v", at.standing)
	}
}

// TestAttachBeforeTheFirstStateChangeHearsNoStaleTurn: startTurn marks a session active before
// the runner has appended anything, so between that and the runner's first StateChanged there
// is a live turn whose id nobody knows yet (it is the id of the user_message the runner has
// still to append). A client attaching in that window must not be handed the previous turn's
// id on a turn.state: that names a turn which has already finished. It hears nothing, and the
// runner's first state change reaches it as a live notification a moment later. A steer resume,
// which does know its id, still sends it.
func TestAttachBeforeTheFirstStateChangeHearsNoStaleTurn(t *testing.T) {
	ls := newTestLive(t)
	// A turn ran and rested, leaving its id in the mirror.
	ls.obsMu.Lock()
	ls.state, ls.turnID = turn.Completed, "the turn before"
	ls.obsMu.Unlock()

	ls.markStarting("") // startTurn's fresh turn: active, id not known yet
	if at := ls.subscribeLocked(askerConn(1)); at.state != nil {
		t.Fatalf("attach heard turn.state %+v before the turn had an id", *at.state)
	}

	ls.markStarting("turn-1") // startTurn's steer resume: the id is already known
	at := ls.subscribeLocked(askerConn(2))
	if at.state == nil || at.state.TurnID != "turn-1" || at.state.State != string(turn.Streaming) {
		t.Fatalf("attach state = %+v, want streaming on turn-1", at.state)
	}
}
