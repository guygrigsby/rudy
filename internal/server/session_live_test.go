package server

import (
	"context"
	"encoding/json"
	"errors"
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

// TestToolStateVocabularyMatchesTheProtocol is the drift guard on the two copies of the tool
// state vocabulary. fanout.ToolStateChanged sends turn.ToolState straight onto the wire as a
// string, so a value renamed on one side and not the other would be a notification no client
// understands, with nothing else to catch it.
func TestToolStateVocabularyMatchesTheProtocol(t *testing.T) {
	for _, c := range []struct {
		domain turn.ToolState
		wire   string
	}{
		{turn.ToolRunning, protocol.ToolStateRunning},
		{turn.ToolAwaitingPermission, protocol.ToolStateAwaitingPermission},
		{turn.ToolDone, protocol.ToolStateDone},
	} {
		if string(c.domain) != c.wire {
			t.Errorf("turn.ToolState %q is sent as %q", c.domain, c.wire)
		}
	}
}

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

// TestWatchersNeverIncludeAPluginConnection is watchers' own version of the test above: the
// mirror walk, downward instead of up, excludes the same plugin connection for the same reason
// (ADR 0028 decision 6) - it is the child's own subscriber, already reading these
// notifications, and must not also receive them relayed through its parent. child needs a real
// sess (newTestLive, not a bare &liveSession{}) because watchers() reads ls.sess.ID() to check
// each candidate against its own subscriber set, which the plugin-exclusion path alone never
// exercised.
func TestWatchersNeverIncludeAPluginConnection(t *testing.T) {
	pluginConn := newConn(1, nil)
	pluginConn.plugin = "subagents"
	clientConn := newConn(2, nil)
	secondClient := newConn(3, nil)

	parent := &liveSession{}
	parent.conns = []*conn{pluginConn, clientConn, secondClient}
	child := newTestLive(t)
	child.parent = parent

	want := []*conn{clientConn, secondClient}
	if got := child.watchers(); !slices.Equal(got, want) {
		t.Errorf("watchers = %+v, want the parent's non-plugin connections", got)
	}

	// A root session has no parent to walk to, so nobody is watching it this way.
	root := &liveSession{}
	if got := root.watchers(); got != nil {
		t.Errorf("a root session's watchers = %+v, want none", got)
	}
}

// TestWatchersExcludesAConnectionAlreadySubscribedToTheChild: a client that watches the parent
// and has also resumed the child directly (the obvious way to open a subagent's own transcript)
// must not be handed the same notification twice - entry.appended could be de-duplicated by id,
// but stream.delta is never replayed and could not be.
func TestWatchersExcludesAConnectionAlreadySubscribedToTheChild(t *testing.T) {
	child := newTestLive(t)
	watchesBoth := newConn(1, nil)
	watchesBoth.subs[child.sess.ID()] = child
	watchesParentOnly := newConn(2, nil)

	parent := &liveSession{}
	parent.conns = []*conn{watchesBoth, watchesParentOnly}
	child.parent = parent

	want := []*conn{watchesParentOnly}
	if got := child.watchers(); !slices.Equal(got, want) {
		t.Errorf("watchers = %+v, want only the connection not already subscribed to the child", got)
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
	pend, ok := ls.pending["tu1"]
	delete(ls.pending, "tu1")
	ls.mu.Unlock()
	if !ok {
		t.Fatal("the question was published with no pending channel behind it")
	}
	// The handler's other half ran while standing was still empty, so nothing is deleted
	// here. Only the answer arrives.
	pend.ch <- turn.Answer{Decision: session.Allow, Scope: session.ScopeOnce, Reason: "yes"}
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

// answer calls the exact same resolveAnswer Server.handleAnswer calls, so a test exercises the
// real claim-then-settle logic rather than a hand-rolled copy of it that could drift from what
// the RPC path actually does. It is called from subscribeAsker's watcher goroutine, never the
// test's own, so a missing question is reported with Errorf rather than Fatalf: FailNow is only
// safe from the goroutine running the test.
func (ls *liveSession) answer(t *testing.T, toolUseID string, d session.Decision, scope session.Scope) {
	t.Helper()
	ans := turn.Answer{Decision: d, Scope: scope, Reason: "test"}
	ch, settle, ok, _ := ls.resolveAnswer(toolUseID, ans)
	if !ok {
		t.Errorf("answer: no pending question for %s", toolUseID)
		return
	}
	ch <- ans
	for _, sch := range settle {
		sch <- ans
	}
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

// pendingCount is how many calls are currently registered as the active raiser of some
// question: at most one per distinct askKey, since a joiner never registers into pending (only
// the call that raises a question does). Self-locking (takes ls.mu).
func (ls *liveSession) pendingCount() int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return len(ls.pending)
}

// askingCount is how many distinct questions are currently standing at the coalescing layer.
// Self-locking (takes ls.mu).
func (ls *liveSession) askingCount() int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return len(ls.asking)
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
		wg.Go(func() {
			one := q
			one.ToolUseID = fmt.Sprintf("tu%d", i)
			answers[i], errs[i] = asker.Ask(context.Background(), one)
		})
	}
	// Let all three reach the asker before any answer lands. Only the raiser ever registers
	// into pending, so pendingCount staying at 1 alongside waitingAsks reaching 3 is the
	// raiser-only invariant holding: two calls sharing a question, not two more questions.
	waitFor(t, func() bool { return ls.waitingAsks() == 3 && ls.pendingCount() == 1 })
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
// questions, each asked and answered on its own. Both calls must actually be parked before
// either is answered: the watcher answers as soon as a prompt arrives, so without release
// holding the first open until the second has also registered, the two calls could run one
// after the other and this would pass even against a key that ignores Prefix entirely, which is
// the bug it exists to catch.
func TestDifferentMatchersAskSeparately(t *testing.T) {
	ls, asker := newAskTestSession(t)
	var prompts atomic.Int32
	release := make(chan struct{})
	ls.subscribeAsker(t, func(req protocol.PermissionRequested) {
		prompts.Add(1)
		<-release
		ls.answer(t, req.ToolUseID, session.Allow, session.ScopeOnce)
	})

	var wg sync.WaitGroup
	for i, prefix := range []string{"git status", "go build"} {
		wg.Go(func() {
			_, _ = asker.Ask(context.Background(), turn.Question{
				ToolUseID: fmt.Sprintf("tu%d", i), Tool: "bash",
				Matcher: session.Matcher{Tool: "bash", Prefix: prefix},
			})
		})
	}
	waitFor(t, func() bool { return ls.waitingAsks() == 2 })
	close(release)
	wg.Wait()
	if got := prompts.Load(); got != 2 {
		t.Fatalf("prompts = %d, want 2: different matchers are different questions", got)
	}
}

// TestSessionScopeAnswerSettlesOtherParkedQuestions is ADR 0028 decision 4's second mechanism:
// two calls with different inputs under the same matcher raise two separate questions (fix (a)
// stops them coalescing on the matcher alone), but both read the session's allowances before
// either has recorded one and so both park. Since they are genuinely different questions, both
// do reach the operator as their own permission.requested (there is nothing to coalesce them on
// yet) - the fix is that only one of them needs deciding: answering it for the session scope
// grants an allowance that covers the other too, which must take it directly rather than sit
// asking for something now already granted.
func TestSessionScopeAnswerSettlesOtherParkedQuestions(t *testing.T) {
	ls, asker := newAskTestSession(t)
	var prompts atomic.Int32
	ls.subscribeAsker(t, func(protocol.PermissionRequested) { prompts.Add(1) })

	m := session.Matcher{Tool: "write"}
	inputs := []string{`{"path":"a"}`, `{"path":"b"}`}
	toolUseIDs := []string{"tu0", "tu1"}
	var wg sync.WaitGroup
	answers := make([]turn.Answer, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() {
			answers[i], errs[i] = asker.Ask(context.Background(), turn.Question{
				ToolUseID: toolUseIDs[i], Tool: "write",
				Input: json.RawMessage(inputs[i]), Matcher: m,
			})
		})
	}
	// Both calls must be genuinely parked, not just registered as waiting on their own
	// question: pendingCount reaching 2 is each one's own raiser having actually asked, which
	// is what makes this the case decision 4 is about rather than one call beating the other
	// to the answer. Only the first is ever explicitly answered.
	waitFor(t, func() bool { return ls.pendingCount() == 2 })
	ls.answer(t, toolUseIDs[0], session.Allow, session.ScopeSession)
	wg.Wait()

	// subscribeAsker's watcher counts notifications on its own goroutine, independent of
	// wg.Wait() (which only waits on the Ask calls, not on that goroutine catching up), so the
	// count is polled rather than read the instant the calls return.
	waitFor(t, func() bool { return prompts.Load() == 2 })
	if got := prompts.Load(); got != 2 {
		t.Fatalf("prompts = %d, want 2: both calls genuinely parked before either was decided, so both raised their own question", got)
	}
	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if answers[i].Decision != session.Allow || answers[i].Scope != session.ScopeSession {
			t.Fatalf("call %d = %+v, want a session-scope allow", i, answers[i])
		}
	}
}

// TestRaiserCancelledPromotesAJoiner: the call raising a question can be cancelled (its own tool
// call was killed, or its own context otherwise ended) while another call is still waiting on
// the same question. That cancellation is not an answer, and it must not deny the healthy call
// waiting behind it: the question passes to it instead, and it raises the same question itself.
func TestRaiserCancelledPromotesAJoiner(t *testing.T) {
	ls, asker := newAskTestSession(t)
	var prompts atomic.Int32
	release := make(chan struct{})
	ls.subscribeAsker(t, func(req protocol.PermissionRequested) {
		prompts.Add(1)
		if req.ToolUseID == "raiser" {
			return // never answered: this call is about to be cancelled instead
		}
		<-release
		ls.answer(t, req.ToolUseID, session.Allow, session.ScopeOnce)
	})

	q := turn.Question{Tool: "bash", Matcher: session.Matcher{Tool: "bash", Prefix: "git status"}}
	raiserCtx, cancel := context.WithCancel(context.Background())
	raiserDone := make(chan struct{})
	go func() {
		one := q
		one.ToolUseID = "raiser"
		_, _ = asker.Ask(raiserCtx, one)
		close(raiserDone)
	}()
	waitFor(t, func() bool { return ls.pendingCount() == 1 })

	var joinAns turn.Answer
	var joinErr error
	joinDone := make(chan struct{})
	go func() {
		one := q
		one.ToolUseID = "joiner"
		joinAns, joinErr = asker.Ask(context.Background(), one)
		close(joinDone)
	}()
	waitFor(t, func() bool { return ls.waitingAsks() == 2 })

	cancel()
	<-raiserDone
	// The joiner is promoted and raises the question itself in the raiser's place.
	waitFor(t, func() bool { return prompts.Load() == 2 })
	close(release)
	<-joinDone

	if joinErr != nil {
		t.Fatalf("joiner: %v", joinErr)
	}
	if joinAns.Decision != session.Allow {
		t.Fatalf("joiner decision = %q, want allow: a cancelled raiser must not deny a healthy joiner", joinAns.Decision)
	}
}

// TestJoinerCancelledLeavesTheRaiserUnaffected: a call that leaves a shared question before it
// is answered (its own context ended while it was only ever a joiner, never the raiser) takes
// its own cancellation and nothing else. The raiser, and the question it put to the operator,
// are unaffected.
func TestJoinerCancelledLeavesTheRaiserUnaffected(t *testing.T) {
	ls, asker := newAskTestSession(t)
	var prompts atomic.Int32
	release := make(chan struct{})
	ls.subscribeAsker(t, func(req protocol.PermissionRequested) {
		prompts.Add(1)
		<-release
		ls.answer(t, req.ToolUseID, session.Allow, session.ScopeOnce)
	})

	q := turn.Question{Tool: "bash", Matcher: session.Matcher{Tool: "bash", Prefix: "git status"}}
	var raiserAns turn.Answer
	var raiserErr error
	raiserDone := make(chan struct{})
	go func() {
		one := q
		one.ToolUseID = "raiser"
		raiserAns, raiserErr = asker.Ask(context.Background(), one)
		close(raiserDone)
	}()
	waitFor(t, func() bool { return ls.pendingCount() == 1 })

	joinCtx, cancelJoin := context.WithCancel(context.Background())
	var joinErr error
	joinDone := make(chan struct{})
	go func() {
		one := q
		one.ToolUseID = "joiner"
		_, joinErr = asker.Ask(joinCtx, one)
		close(joinDone)
	}()
	waitFor(t, func() bool { return ls.waitingAsks() == 2 })

	cancelJoin()
	<-joinDone
	if !errors.Is(joinErr, context.Canceled) {
		t.Fatalf("joiner err = %v, want context.Canceled", joinErr)
	}

	// The raiser's own question is unaffected: it still gets a real answer.
	waitFor(t, func() bool { return ls.waitingAsks() == 1 })
	close(release)
	<-raiserDone
	if raiserErr != nil {
		t.Fatalf("raiser: %v", raiserErr)
	}
	if raiserAns.Decision != session.Allow {
		t.Fatalf("raiser decision = %q, want allow", raiserAns.Decision)
	}
	if got := prompts.Load(); got != 1 {
		t.Fatalf("prompts = %d, want 1: the joiner leaving must not raise a question of its own", got)
	}
}

// TestArrivingAfterResolutionAsksAgain: a call sharing a matcher and input with a question that
// has already been answered once does not take that old answer. resolveAnswer retires the
// standing entry in the same critical section that claims the answer, so by the time a call's
// own Ask returns, the key is free for the next call to raise fresh.
func TestArrivingAfterResolutionAsksAgain(t *testing.T) {
	ls, asker := newAskTestSession(t)
	var prompts atomic.Int32
	ls.subscribeAsker(t, func(req protocol.PermissionRequested) {
		prompts.Add(1)
		ls.answer(t, req.ToolUseID, session.Allow, session.ScopeOnce)
	})

	q := turn.Question{Tool: "bash", Matcher: session.Matcher{Tool: "bash", Prefix: "git status"}}
	first := q
	first.ToolUseID = "tu-first"
	if ans, err := asker.Ask(context.Background(), first); err != nil || ans.Decision != session.Allow {
		t.Fatalf("first ask = %+v, %v", ans, err)
	}
	if n := ls.askingCount(); n != 0 {
		t.Fatalf("asking has %d entries once the first call resolved, want 0", n)
	}

	second := q
	second.ToolUseID = "tu-second"
	if ans, err := asker.Ask(context.Background(), second); err != nil || ans.Decision != session.Allow {
		t.Fatalf("second ask = %+v, %v", ans, err)
	}
	if got := prompts.Load(); got != 2 {
		t.Fatalf("prompts = %d, want 2: a call arriving after the question resolved must ask again, not take the old answer", got)
	}
}

// TestDangerousQuestionNeverSettlesFromASessionAllow is ADR 0011's ordering carried through
// settlement (ADR 0028 decision 4, amended commit 70153dd): a session-scope allow must never
// resolve a question the Gate marked dangerous, only that question's own answer. Two concurrent
// dangerous bash calls on different paths park under two questions (same matcher, different
// input, per fix (a)); without settleLocked's skip on Dangerous, the operator answering the first
// "allow for the session" would silence the second, which is exactly the ordering ADR 0011 exists
// to refuse - a session allowance never outranks the dangerous set.
func TestDangerousQuestionNeverSettlesFromASessionAllow(t *testing.T) {
	ls, asker := newAskTestSession(t)
	var prompts atomic.Int32
	ls.subscribeAsker(t, func(protocol.PermissionRequested) { prompts.Add(1) })

	m := session.Matcher{Tool: "bash", Prefix: "rm -rf"}
	inputs := []string{`{"command":"rm -rf /tmp/a"}`, `{"command":"rm -rf /tmp/b"}`}
	toolUseIDs := []string{"tu0", "tu1"}
	var wg sync.WaitGroup
	answers := make([]turn.Answer, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() {
			answers[i], errs[i] = asker.Ask(context.Background(), turn.Question{
				ToolUseID: toolUseIDs[i], Tool: "bash",
				Input: json.RawMessage(inputs[i]), Matcher: m, Dangerous: true,
			})
		})
	}
	waitFor(t, func() bool { return ls.pendingCount() == 2 })

	// Answering the first for the session must not touch the second: it stays parked, waiting
	// for its own answer, because both are dangerous.
	ls.answer(t, toolUseIDs[0], session.Allow, session.ScopeSession)
	if got := ls.pendingCount(); got != 1 {
		t.Fatalf("pendingCount = %d after the first answer, want 1: a dangerous question must not be settled by another call's session allow", got)
	}
	if got := ls.waitingAsks(); got != 1 {
		t.Fatalf("waitingAsks = %d, want 1: the second dangerous call must still be waiting on its own question", got)
	}

	ls.answer(t, toolUseIDs[1], session.Allow, session.ScopeOnce)
	wg.Wait()

	// subscribeAsker's watcher counts notifications on its own goroutine, independent of
	// wg.Wait() (which only waits on the Ask calls, not on that goroutine catching up), so the
	// count is polled rather than read the instant the calls return.
	waitFor(t, func() bool { return prompts.Load() == 2 })
	if got := prompts.Load(); got != 2 {
		t.Fatalf("prompts = %d, want 2: two dangerous calls with different inputs are two separate questions", got)
	}
	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if answers[i].Decision != session.Allow {
			t.Fatalf("call %d decision = %q, want allow", i, answers[i].Decision)
		}
	}
}

// TestRetireIfCurrentLockedSkipsAReplacedEntry is the deterministic half of the stale-holder
// finding: a departing raiser (or leave's hand-off) must not delete asking[key] once a new call
// has already created a fresh standingAsk there. resolveAnswer can retire a key while its old
// raiser has not yet resumed from a's ask select (the channel send does not wait for the
// receiver); a new call arriving in that gap sees no entry, raises its own, and gathers joiners.
// If the old raiser then deleted by key alone, that live, joined-on entry would vanish and a
// later call would raise a duplicate question for one already standing.
//
// This proves the guard in isolation rather than forcing the full interleaving through real
// goroutines: doing that deterministically would mean pausing a goroutine after it has a value
// ready on a buffered channel but before it resumes from the receive, which nothing in this
// package (or the race detector) gives a hook to force reliably. A raced version of this test
// would be timing-dependent in exactly the way the project's testing rules ask not to write, so
// this checks the mechanism the fix actually relies on instead.
func TestRetireIfCurrentLockedSkipsAReplacedEntry(t *testing.T) {
	key := askKey{matcher: session.Matcher{Tool: "bash", Prefix: "git status"}, input: ""}
	stale := &standingAsk{done: make(chan struct{})}
	fresh := &standingAsk{done: make(chan struct{})}
	asking := map[askKey]*standingAsk{key: fresh}

	// The stale holder resumes after resolveAnswer already retired its own entry and a new
	// call created a fresh one at the same key with joiners of its own.
	retireIfCurrentLocked(asking, key, stale)
	if got := asking[key]; got != fresh {
		t.Fatalf("asking[key] = %p, want the fresh entry %p left untouched", got, fresh)
	}

	// The entry that actually is current still retires normally.
	retireIfCurrentLocked(asking, key, fresh)
	if _, ok := asking[key]; ok {
		t.Fatal("asking[key] still present after retiring the entry that was actually current")
	}
}
