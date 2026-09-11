package server

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// newAskTestSession builds a liveSession with one asker connection attached and the liveAsker
// in front of it, the setup every coalescing test below shares. The runner's Provider is nil,
// as elsewhere in this file: liveAsker.Ask only ever calls TurnID on it.
func newAskTestSession(t *testing.T) (*liveSession, *liveAsker) {
	t.Helper()
	ls := newTestLive(t)
	ls.conns = []*conn{askerConn(1)}
	r := turn.NewRunner(turn.Config{
		Session:  ls.sess,
		Provider: nil,
		Model:    ls.model,
		Tools:    plugin.NewRegistry(nil, func(string) {}),
		Gate:     gate.New(nil),
		Observer: &fanout{ls: ls, sid: ls.sess.ID().String()},
		System:   "test",
	})
	return ls, &liveAsker{ls: ls, sid: ls.sess.ID().String(), runner: r}
}

// subscribeAsker watches ls's one asker connection for permission.requested notifications and
// calls fn with each, in the order sent, until the test ends. conn.notify only ever enqueues
// (see conn.send), so reading the queue directly is the whole of what a pump would otherwise do
// with no transport underneath to send to.
func (ls *liveSession) subscribeAsker(t *testing.T, fn func(protocol.PermissionRequested)) {
	t.Helper()
	if len(ls.conns) != 1 {
		t.Fatalf("subscribeAsker wants exactly one connection, got %d", len(ls.conns))
	}
	cn := ls.conns[0]
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-cn.wake:
			}
			for {
				cn.mu.Lock()
				if len(cn.queue) == 0 {
					cn.mu.Unlock()
					break
				}
				msg := cn.queue[0]
				cn.queue = cn.queue[1:]
				cn.mu.Unlock()
				req, ok := msg.(protocol.Request)
				if !ok || req.Method != protocol.NotifyPermissionRequested {
					continue
				}
				var pr protocol.PermissionRequested
				if err := json.Unmarshal(req.Params, &pr); err != nil {
					t.Errorf("permission.requested params: %v", err)
					continue
				}
				fn(pr)
			}
		}
	}()
}

// answer plays session.answer's own claim-then-send against ls directly (see
// Server.handleAnswer), without a live connection or dispatcher to send it through. It is
// called from subscribeAsker's watcher goroutine, never the test's own, so a missing question
// is reported with Errorf rather than Fatalf: FailNow is only safe from the goroutine running
// the test.
func (ls *liveSession) answer(t *testing.T, toolUseID string, d session.Decision, scope session.Scope) {
	t.Helper()
	ls.mu.Lock()
	ch, ok := ls.pending[toolUseID]
	delete(ls.pending, toolUseID)
	ls.obsMu.Lock()
	if ok {
		ls.answered[toolUseID] = true
		delete(ls.standing, toolUseID)
	}
	ls.obsMu.Unlock()
	ls.mu.Unlock()
	if !ok {
		t.Errorf("answer: no pending question for %s", toolUseID)
		return
	}
	ch <- turn.Answer{Decision: d, Scope: scope, Reason: "test"}
}

// waitingAsks is how many calls are currently parked at the coalescing layer across every
// standing question: the raiser doing the real ask plus whoever has joined it. Self-locking
// (takes ls.mu).
func (ls *liveSession) waitingAsks() int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	n := 0
	for _, st := range ls.asking {
		n += st.waiting
	}
	return n
}

// waitFor polls pred until it holds or five seconds pass, failing the test if it never does.
func waitFor(t *testing.T, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	if !pred() {
		t.Fatal("waitFor: condition never became true")
	}
}

// TestConcurrentAsksOnOneMatcherAskOnce: several goroutines racing the same tool call's
// permission question raise it once and all take the one answer, rather than each putting its
// own question to the operator.
func TestConcurrentAsksOnOneMatcherAskOnce(t *testing.T) {
	ls, asker := newAskTestSession(t)

	var prompts atomic.Int32
	release := make(chan struct{})
	ls.subscribeAsker(t, func(req protocol.PermissionRequested) {
		prompts.Add(1)
		<-release
		ls.answer(t, req.ToolUseID, session.Allow, session.ScopeOnce)
	})

	q := turn.Question{Tool: "bash", Matcher: session.Matcher{Tool: "bash", Prefix: "git status"}}
	var wg sync.WaitGroup
	answers := make([]turn.Answer, 3)
	errs := make([]error, 3)
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			one := q
			one.ToolUseID = fmt.Sprintf("tu%d", i)
			answers[i], errs[i] = asker.Ask(context.Background(), one)
		}()
	}
	// Let all three reach the asker before any answer lands.
	waitFor(t, func() bool { return ls.waitingAsks() == 3 })
	close(release)
	wg.Wait()

	if got := prompts.Load(); got != 1 {
		t.Fatalf("the operator was asked %d times, want 1", got)
	}
	for i := range 3 {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if answers[i].Decision != session.Allow {
			t.Fatalf("call %d decision = %q, want allow", i, answers[i].Decision)
		}
	}
}

// TestDifferentMatchersAskSeparately: two calls whose matcher differs are two different
// questions, each asked and answered on its own.
func TestDifferentMatchersAskSeparately(t *testing.T) {
	ls, asker := newAskTestSession(t)
	var prompts atomic.Int32
	ls.subscribeAsker(t, func(req protocol.PermissionRequested) {
		prompts.Add(1)
		ls.answer(t, req.ToolUseID, session.Allow, session.ScopeOnce)
	})

	var wg sync.WaitGroup
	for i, prefix := range []string{"git status", "go build"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = asker.Ask(context.Background(), turn.Question{
				ToolUseID: fmt.Sprintf("tu%d", i), Tool: "bash",
				Matcher: session.Matcher{Tool: "bash", Prefix: prefix},
			})
		}()
	}
	wg.Wait()
	if got := prompts.Load(); got != 2 {
		t.Fatalf("prompts = %d, want 2: different matchers are different questions", got)
	}
}
