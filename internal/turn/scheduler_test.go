package turn

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	go func() {
		refused <- s.Submit(ctx, LaneRoot, func(context.Context) { t.Error("a cancelled submission ran") })
	}()
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
