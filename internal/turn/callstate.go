package turn

import (
	"sync"

	"github.com/guygrigsby/rudy/internal/session"
)

// callStates is where every admitted call of the current response has got to, and the one
// place the Turn's coarse state is derived from. A Turn has one state and its calls have
// their own (see ToolState); with calls running at once, the Turn's has to be a reading of
// the whole set rather than whatever the last call to change happened to be. Otherwise a
// call that came back from the operator would report the Turn as running tools while another
// call still waits on a question nobody has answered.
type callStates struct {
	mu sync.Mutex
	at map[string]ToolState
}

// start replaces the set with the calls of one response, each gating and none runnable.
func (c *callStates) start(calls []session.Block) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = make(map[string]ToolState, len(calls))
	for _, tu := range calls {
		c.at[tu.ID] = ToolGating
	}
}

// set records one call's state and returns the Turn state the whole set now reads as, and
// whether the Turn has one at all: a set with every call done leaves the Turn's state to the
// loop, which is about to ask the provider again or rest.
func (c *callStates) set(id string, s ToolState) (State, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at == nil {
		c.at = map[string]ToolState{}
	}
	c.at[id] = s
	return c.deriveLocked()
}

// deriveLocked is the reading: a question outstanding anywhere makes the Turn awaiting
// permission, because that is what the operator is being asked for; otherwise any call still
// gating, queued or running makes it running tools.
func (c *callStates) deriveLocked() (State, bool) {
	coarse, any := State(""), false
	for _, s := range c.at {
		switch s {
		case ToolAwaitingPermission:
			return AwaitingPermission, true
		case ToolGating, ToolQueued, ToolRunning:
			coarse, any = RunningTool, true
		}
	}
	return coarse, any
}

// toolState publishes one call's state and moves the Turn to whatever the set now reads as.
func (r *Runner) toolState(turnID string, tu session.Block, s ToolState) {
	coarse, ok := r.states.set(tu.ID, s)
	r.cfg.Observer.ToolStateChanged(turnID, tu.ID, tu.Name, s)
	if ok {
		r.setState(coarse)
	}
}
