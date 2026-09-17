package turn

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/guygrigsby/rudy/internal/session"
)

// Lane is which admission lane a tool call belongs to: a call of a root Session or a call of
// a child Session opened by an agent tool. The distinction is the whole reason the pool has
// two lanes (ADR 0034).
type Lane int

const (
	LaneRoot Lane = iota
	LaneChild
)

// The pool's shape, fixed by ADR 0034. Sixty-four workers and sixty-four queued jobs across
// two lanes: forty-eight general workers that may serve either lane, sixteen reserved for
// child work, and a queue per lane sized to match. A child Session cannot invoke the agent
// tool, so forty-eight root agent calls may wait on child Turns while sixteen workers remain
// able to run them.
const (
	generalWorkers   = 48
	childWorkers     = 16
	schedulerWorkers = generalWorkers + childWorkers
	rootQueue        = 48
	childQueue       = 16
)

// ErrSchedulerClosed is a submission to a scheduler whose admission has closed, which is what
// every caller sees once Server shutdown begins.
var ErrSchedulerClosed = errors.New("turn: scheduler closed")

// job is one admitted tool call: the work and the context of the Turn that submitted it. The
// context is carried rather than captured so a worker can drop a job whose Turn was cut
// before the job reached a worker.
type job struct {
	ctx context.Context
	run func(context.Context)
}

// Scheduler runs tool calls on one Server-owned pool. Turns submit with cancellation-aware
// backpressure instead of starting a goroutine per call, so one Session's response, or a
// hundred Sessions' responses together, can occupy at most this many workers (ADR 0034).
//
// Fairness within a lane is not promised. What is promised is that root work never occupies
// the reserved workers, that a free general worker will take child work, and that no
// submission and no queued job survives Close.
type Scheduler struct {
	root  chan job
	child chan job

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	closeOnce sync.Once
	closed    atomic.Bool
	inFlight  atomic.Int64
}

// NewScheduler starts the pool. The Server owns exactly one and closes it during shutdown.
func NewScheduler() *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Scheduler{
		root:   make(chan job, rootQueue),
		child:  make(chan job, childQueue),
		ctx:    ctx,
		cancel: cancel,
	}
	for range generalWorkers {
		s.wg.Go(func() { s.serve(true) })
	}
	for range childWorkers {
		s.wg.Go(func() { s.serve(false) })
	}
	return s
}

// serve is one worker. A general worker selects over both lanes, so it helps child work
// whenever it is free and child work is waiting; a reserved worker reads only the child lane,
// which is what keeps root work from ever filling the pool.
func (s *Scheduler) serve(general bool) {
	for {
		if general {
			select {
			case <-s.ctx.Done():
				return
			case j := <-s.root:
				s.run(j)
			case j := <-s.child:
				s.run(j)
			}
			continue
		}
		select {
		case <-s.ctx.Done():
			return
		case j := <-s.child:
			s.run(j)
		}
	}
}

// run executes one job, always, and under both cancellations: its own Turn's and the pool's,
// so Close cuts work already running and not only work still queued.
//
// Always, including a job whose Turn was cut while it waited in the queue, because the Turn
// that submitted it is waiting for it to come back and the call it names still owes the log a
// terminal result. What a cancelled job must not do is run the tool, which is the job body's
// own check: it is handed a context that is already done.
func (s *Scheduler) run(j job) {
	defer s.inFlight.Add(-1)
	ctx, cancel := context.WithCancel(j.ctx)
	defer cancel()
	defer context.AfterFunc(s.ctx, cancel)()
	if s.ctx.Err() != nil {
		cancel()
	}
	j.run(ctx)
}

// Submit queues one call, waiting for space when the lane is full. It returns when the job is
// admitted, when the caller's context ends or when admission has closed; it never returns
// because the pool is busy. The lane is the Session's own: a root call may never enter the
// child lane, whatever the wait.
func (s *Scheduler) Submit(ctx context.Context, lane Lane, run func(context.Context)) error {
	if s.closed.Load() {
		return ErrSchedulerClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	queue := s.root
	if lane == LaneChild {
		queue = s.child
	}
	s.inFlight.Add(1)
	select {
	case queue <- job{ctx: ctx, run: run}:
		// A Close that ran between the check above and this send has already drained, so
		// this job would sit in a queue nobody reads. Drain again rather than leave the
		// caller waiting on a call that will never come back.
		if s.closed.Load() {
			s.drain()
		}
		return nil
	case <-ctx.Done():
		s.inFlight.Add(-1)
		return ctx.Err()
	case <-s.ctx.Done():
		s.inFlight.Add(-1)
		return ErrSchedulerClosed
	}
}

// Close shuts the pool down: admission stops, every queued and running job is cancelled, every
// worker is joined, and every job still in a queue is handed back to the Turn that submitted
// it with a context already done. Handed back rather than dropped: that Turn is waiting for
// its call to return and holds an allow for it in the log, so a swallowed job parks the Turn
// for the life of the process. Idempotent, because Server shutdown may reach it twice.
func (s *Scheduler) Close() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.cancel()
		s.wg.Wait()
		s.drain()
	})
}

// drain hands back every job still queued, each with a dead context. Safe to run while
// workers are alive (a channel receive is exclusive) and after they are gone, which is why
// both Close and a submission that raced it call it.
func (s *Scheduler) drain() {
	for {
		select {
		case j := <-s.root:
			s.run(j)
		case j := <-s.child:
			s.run(j)
		default:
			return
		}
	}
}

// queued is how many jobs are waiting in one lane, for the tests that prove the bound.
func (s *Scheduler) queued(lane Lane) int {
	if lane == LaneChild {
		return len(s.child)
	}
	return len(s.root)
}

// running is how many jobs are admitted and not finished, queued ones included.
func (s *Scheduler) running() int { return int(s.inFlight.Load()) }

// submit puts one call on the Server's pool, or runs it on its own goroutine when no pool is
// wired. The fallback is the pre-scheduler shape and belongs to a turn with no Server behind
// it; admission still bounds such a turn to 64 calls (see admit).
func (r *Runner) submit(ctx context.Context, run func(context.Context)) error {
	if r.cfg.Scheduler == nil {
		go run(ctx)
		return nil
	}
	return r.cfg.Scheduler.Submit(ctx, r.cfg.Lane, run)
}

// submissionOutcome is what a call the pool would not take asks the turn to do: a cancelled
// Turn ends as an interrupt, and a closed pool is the Server going away under it.
func submissionOutcome(err error) toolOutcome {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return toolOutcome{ctxErr: err}
	}
	return toolOutcome{class: session.ErrInternal, err: err}
}
