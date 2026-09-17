package turn

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// waitFor polls until cond holds or the budget runs out, which is how a test watches a pool
// fill without guessing at a sleep.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestTheSchedulerFillsEveryWorkerAndNoMore is the bound ADR 0034 puts on tool execution:
// work runs concurrently, and 64 workers is all there is however many Turns are asking.
func TestTheSchedulerFillsEveryWorkerAndNoMore(t *testing.T) {
	s := NewScheduler()
	t.Cleanup(s.Close)

	var running atomic.Int64
	release := make(chan struct{})
	var wg sync.WaitGroup
	for range schedulerWorkers + 8 {
		wg.Go(func() {
			_ = s.Submit(context.Background(), LaneRoot, func(context.Context) {
				running.Add(1)
				<-release
				running.Add(-1)
			})
		})
	}
	waitFor(t, "the general lane to fill", func() bool { return running.Load() == int64(generalWorkers) })
	time.Sleep(20 * time.Millisecond)
	if got := running.Load(); got != int64(generalWorkers) {
		t.Errorf("%d jobs running, want the %d general workers and no more", got, generalWorkers)
	}
	close(release)
	wg.Wait()
	waitFor(t, "the pool to drain", func() bool { return running.Load() == 0 })
}

// TestRootWorkCannotTakeTheChildLane is the deadlock ADR 0034 exists to prevent: 48 root
// agent calls parked waiting on children must leave workers able to run those children.
func TestRootWorkCannotTakeTheChildLane(t *testing.T) {
	s := NewScheduler()
	t.Cleanup(s.Close)

	var rootRunning, childRunning atomic.Int64
	parked := make(chan struct{})
	var wg sync.WaitGroup
	// Every general worker and every root queue slot, taken by a call that will not finish
	// until a child has run: the shape of a full response of agent calls.
	for range generalWorkers + rootQueue {
		wg.Go(func() {
			_ = s.Submit(context.Background(), LaneRoot, func(context.Context) {
				rootRunning.Add(1)
				<-parked
				rootRunning.Add(-1)
			})
		})
	}
	waitFor(t, "the general workers to park", func() bool { return rootRunning.Load() == int64(generalWorkers) })

	childDone := make(chan struct{}, childWorkers)
	for range childWorkers {
		wg.Go(func() {
			_ = s.Submit(context.Background(), LaneChild, func(context.Context) {
				childRunning.Add(1)
				childDone <- struct{}{}
			})
		})
	}
	for range childWorkers {
		select {
		case <-childDone:
		case <-time.After(3 * time.Second):
			t.Fatal("child work never ran while root work held every general worker")
		}
	}
	// A 49th root job cannot take the reserved capacity those children just used.
	admitted := make(chan error, 1)
	go func() {
		admitted <- s.Submit(context.Background(), LaneRoot, func(context.Context) { rootRunning.Add(1) })
	}()
	select {
	case err := <-admitted:
		t.Fatalf("a root job entered the reserved lane: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(parked)
	wg.Wait()
	<-admitted
}

// TestSubmitAnswersItsCallersCancellation keeps backpressure from becoming a park: a caller
// waiting for queue space leaves when its Turn is cut.
func TestSubmitAnswersItsCallersCancellation(t *testing.T) {
	s := NewScheduler()
	t.Cleanup(s.Close)

	release := make(chan struct{})
	var wg sync.WaitGroup
	for range generalWorkers + rootQueue {
		wg.Go(func() {
			_ = s.Submit(context.Background(), LaneRoot, func(context.Context) { <-release })
		})
	}
	waitFor(t, "the root lane to fill", func() bool { return s.queued(LaneRoot) == rootQueue })

	ctx, cancel := context.WithCancel(context.Background())
	refused := make(chan error, 1)
	var parked atomic.Bool
	go func() {
		parked.Store(true)
		refused <- s.Submit(ctx, LaneRoot, func(context.Context) { t.Error("a cancelled submission ran") })
	}()
	// Cancelled only once the submitter is demonstrably waiting for queue space, so this
	// proves the wait answers cancellation and not just the guard at the top of Submit.
	waitFor(t, "the submitter to park", func() bool { return parked.Load() && s.queued(LaneRoot) == rootQueue })
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-refused:
		if err == nil {
			t.Fatal("a cancelled submission was admitted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Submit ignored its caller's cancellation")
	}
	close(release)
	wg.Wait()
}

// TestAJobCancelledBeforeItRunsIsHandedADeadContext covers the other half of cancellation: a
// Turn cut while its calls sit in the queue still gets each of them back, because each one
// owes the log a terminal result, and each one is handed a context that is already done so it
// cannot run the tool.
func TestAJobCancelledBeforeItRunsIsHandedADeadContext(t *testing.T) {
	s := NewScheduler()
	t.Cleanup(s.Close)

	release := make(chan struct{})
	var wg sync.WaitGroup
	for range generalWorkers {
		wg.Go(func() { _ = s.Submit(context.Background(), LaneRoot, func(context.Context) { <-release }) })
	}
	waitFor(t, "the workers to fill", func() bool { return s.queued(LaneRoot) == 0 && s.running() == generalWorkers })

	ctx, cancel := context.WithCancel(context.Background())
	var called, live atomic.Bool
	if err := s.Submit(ctx, LaneRoot, func(jobCtx context.Context) {
		called.Store(true)
		live.Store(jobCtx.Err() == nil)
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	cancel()
	close(release)
	wg.Wait()
	waitFor(t, "the pool to drain", func() bool { return s.running() == 0 && s.queued(LaneRoot) == 0 })
	if !called.Load() {
		t.Error("a queued job was dropped, so the Turn waiting on it never got it back")
	}
	if live.Load() {
		t.Error("a job whose Turn was cut ran with a live context")
	}
}

// TestCloseCancelsAndJoins is what Server shutdown rests on: admission closes, running work
// is cancelled and every worker is joined before Close returns.
func TestCloseCancelsAndJoins(t *testing.T) {
	s := NewScheduler()
	started := make(chan struct{}, generalWorkers)
	var cancelled atomic.Int64
	for range generalWorkers {
		if err := s.Submit(context.Background(), LaneRoot, func(ctx context.Context) {
			started <- struct{}{}
			<-ctx.Done()
			cancelled.Add(1)
		}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}
	for range generalWorkers {
		<-started
	}
	s.Close()
	if got := cancelled.Load(); got != int64(generalWorkers) {
		t.Errorf("%d jobs saw the cancellation, want %d, and Close returned before they finished", got, generalWorkers)
	}
	if err := s.Submit(context.Background(), LaneRoot, func(context.Context) { t.Error("a job ran after Close") }); err == nil {
		t.Error("admission stayed open after Close")
	}
	s.Close() // idempotent: Shutdown may reach it twice
}

// TestNoResultResumesTheProviderWhileACallIsStillPending is the rule that makes the per-call
// states safe to act on: a finished call does not resume the Turn. The provider is asked
// again only once every call of the response is done, whatever order they finished in.
func TestNoResultResumesTheProviderWhileACallIsStillPending(t *testing.T) {
	s := openTestSession(t, session.ModeStrict)
	rec := &recorder{}
	p := &scripted{scripts: [][]provider.Part{
		append(append(toolCall("tu_fast", "danger", `{"x":1}`), toolCall("tu_slow", "danger", `{"y":2}`)...),
			stop(session.StopToolUse, "tool_use")),
		{text("done"), stop(session.StopEndTurn, "end_turn")},
	}}
	held := make(chan struct{})
	asked := make(chan string, 2)
	asker := askerFunc(func(ctx context.Context, q Question) (Answer, error) {
		asked <- q.ToolUseID
		if q.ToolUseID == "tu_slow" {
			<-held
		}
		return Answer{Decision: session.Allow, Scope: session.ScopeOnce, Reason: "ok"}, nil
	})
	r := newRunner(t, s, p, toolSet{"danger": echoTool(tool.Unsafe, "danger")}, asker, rec)
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), userMsg(session.SourceTyped, "go")) }()

	for range 2 {
		select {
		case <-asked:
		case <-time.After(3 * time.Second):
			t.Fatal("both calls should have reached the asker at once")
		}
	}
	// tu_fast is answered and has run by now; the provider must not have been asked again.
	waitFor(t, "the quick call to finish", func() bool { return rec.count(session.KindToolResult) == 1 })
	p.mu.Lock()
	calls := p.calls
	p.mu.Unlock()
	if calls != 1 {
		t.Errorf("the provider was asked %d times while a call was still awaiting permission", calls)
	}
	close(held)
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := rec.count(session.KindToolResult); got != 2 {
		t.Errorf("tool results = %d, want 2", got)
	}
}

// TestCloseRunsBackEveryQueuedJob is the wedge a dropped queue would cause: the Turn that
// submitted a call waits for it to come back, and a job the pool swallowed on Close would
// leave that Turn parked forever with an allow already in its log and no result to follow it.
func TestCloseRunsBackEveryQueuedJob(t *testing.T) {
	s := NewScheduler()
	release := make(chan struct{})
	var handed atomic.Int64
	for range generalWorkers {
		if err := s.Submit(context.Background(), LaneRoot, func(context.Context) {
			handed.Add(1)
			<-release
		}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}
	waitFor(t, "the workers to fill", func() bool { return handed.Load() == int64(generalWorkers) })

	queued := rootQueue
	for range queued {
		if err := s.Submit(context.Background(), LaneRoot, func(context.Context) { handed.Add(1) }); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}
	for range childQueue {
		if err := s.Submit(context.Background(), LaneChild, func(context.Context) { handed.Add(1) }); err != nil {
			t.Fatalf("Submit child: %v", err)
		}
	}
	close(release)
	s.Close()
	if got, want := handed.Load(), int64(generalWorkers+queued+childQueue); got != want {
		t.Errorf("%d of %d jobs came back, so %d Turns are waiting on a call the pool swallowed", got, want, want-got)
	}
	if s.running() != 0 {
		t.Errorf("%d jobs still counted in flight after Close", s.running())
	}
}

// TestACallRunsThroughThePool exercises the seam the rest of the turn tests skip: with a live
// Scheduler wired, a call reaches a worker, runs there and comes back with its result.
func TestACallRunsThroughThePool(t *testing.T) {
	s := openTestSession(t, session.ModeOff)
	rec := &recorder{}
	sched := NewScheduler()
	t.Cleanup(sched.Close)
	p := &scripted{scripts: callThenDone("tu1", "echo", `{"x":1}`)}
	r := newRunner(t, s, p, toolSet{"echo": echoTool(tool.Safe, "echo")}, nil, rec)
	r.cfg.Scheduler = sched

	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "go")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var result session.ToolResult
	for _, e := range s.Entries() {
		if tr, ok := e.Payload.(session.ToolResult); ok {
			result = tr
		}
	}
	if result.Outcome != session.OutcomeOK {
		t.Fatalf("the call did not run on the pool: %+v", result)
	}
	var seq []ToolState
	rec.mu.Lock()
	for _, ts := range rec.toolStates {
		seq = append(seq, ts.state)
	}
	rec.mu.Unlock()
	want := []ToolState{ToolGating, ToolQueued, ToolRunning, ToolDone}
	if len(seq) != len(want) {
		t.Fatalf("tool states %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("tool states %v, want %v", seq, want)
		}
	}
}
