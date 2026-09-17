// SPDX-License-Identifier: AGPL-3.0-or-later

package turn

import (
	"context"
	"sync"
)

// inflight is the set of tool calls currently running in a turn, keyed by tool_use id. It
// replaces the Runner's single cancel slot: calls of one assistant message run at once
// (ADR 0028), so cancelling the turn has to reach every one of them rather than whichever
// registered last.
type inflight struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	// cancelled is set once cancelAll has run, so a call registering afterwards is cancelled
	// immediately rather than running on past an interrupt that already happened.
	cancelled bool
}

func newInflight() *inflight { return &inflight{cancels: map[string]context.CancelFunc{}} }

// add registers a running call. It cancels immediately, and reports false, when the turn has
// already been cancelled.
func (f *inflight) add(id string, cancel context.CancelFunc) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelled {
		cancel()
		return false
	}
	f.cancels[id] = cancel
	return true
}

func (f *inflight) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.cancels, id)
}

// cancelAll cancels every running call and every call that registers later.
func (f *inflight) cancelAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = true
	for _, c := range f.cancels {
		c()
	}
}

// reset returns the set to its empty, uncancelled state for a new turn. Without it a runner
// whose turn was steered would refuse every call of the turn the steer resumes: cancelAll
// latches, and a Runner outlives the turn that cancelled it (Run is called again to resume a
// steer, and the tests run several turns through one Runner). Called from Run, where the last
// turn's calls have all been joined, so there is nobody left to strand.
func (f *inflight) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.cancels)
	f.cancelled = false
}

func (f *inflight) len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cancels)
}
