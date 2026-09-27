// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"encoding/json"
	"log/slog"

	"github.com/guygrigsby/rudy/internal/agentruntime"
)

type activeTurn struct {
	ref     agentruntime.TurnRef
	process *appProcess
}

type failedLogin struct{ rawID, publicID string }

func (c *Client) watchProcess(process *appProcess) {
	<-process.peer.done
	c.enqueueProcessDeath(process)
}

func (c *Client) trackRunningThread(ref agentruntime.ThreadRef, thread agentruntime.Thread, process *appProcess) {
	for _, turn := range thread.Turns {
		if turn.Status == agentruntime.TurnRunning {
			c.trackTurn(agentruntime.TurnRef{ThreadRef: ref, TurnID: turn.TurnID}, process)
		}
	}
}

func (c *Client) trackTurn(ref agentruntime.TurnRef, process *appProcess) {
	c.turnMu.Lock()
	if c.finishedTurns[ref.ThreadID] == ref.TurnID {
		c.turnMu.Unlock()
		return
	}
	c.activeTurns[ref.ThreadID] = activeTurn{ref: ref, process: process}
	c.turnMu.Unlock()
	if process != nil && !process.peer.alive() {
		c.enqueueProcessDeath(process)
	}
}

func (c *Client) enqueueProcessDeath(process *appProcess) {
	select {
	case c.notifications <- wireNotification{dead: process}:
	case <-c.dispatchStop:
	}
}

func (c *Client) failProcessTurns(process *appProcess) {
	c.turnMu.Lock()
	var failed []agentruntime.TurnRef
	for threadID, active := range c.activeTurns {
		if active.process == process {
			failed = append(failed, active.ref)
			c.finishedTurns[threadID] = active.ref.TurnID
			delete(c.activeTurns, threadID)
		}
	}
	c.turnMu.Unlock()
	for _, ref := range failed {
		c.emitRuntimeFailure(ref)
	}
}

func (c *Client) failProcessLogins(process *appProcess) {
	c.emitFailedLogins(c.collectFailedProcessLogins(process))
}

func (c *Client) collectFailedProcessLogins(process *appProcess) []failedLogin {
	c.loginMu.Lock()
	var failed []failedLogin
	for rawID, owner := range c.loginProcesses {
		if owner != process {
			continue
		}
		publicID, active := c.activeLogins[rawID]
		delete(c.activeLogins, rawID)
		delete(c.loginProcesses, rawID)
		if !active {
			continue
		}
		c.rememberFinishedLoginLocked(rawID)
		failed = append(failed, failedLogin{rawID: rawID, publicID: publicID})
	}
	c.loginMu.Unlock()
	return failed
}

func (c *Client) emitFailedLogins(failed []failedLogin) {
	for _, login := range failed {
		reason := "Codex App Server stopped before login completed"
		c.emitLoginCompletion(wireLoginCompletion{LoginID: &login.rawID, Error: &reason}, login.publicID)
	}
}

func (c *Client) failMalformedNotification(notification wireNotification, err error) {
	slog.Warn("codex app server notification rejected", "method", notification.method, "error", Redact(err.Error()))
	var binding struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Turn     struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(notification.params, &binding) != nil || binding.ThreadID == "" {
		c.failProcessTurns(notification.process)
		return
	}
	turnID := binding.TurnID
	if turnID == "" {
		turnID = binding.Turn.ID
	}
	c.turnMu.Lock()
	active, found := c.activeTurns[binding.ThreadID]
	if found && (turnID == "" || active.ref.TurnID == turnID) && (notification.process == nil || active.process == notification.process) {
		c.finishedTurns[binding.ThreadID] = active.ref.TurnID
		delete(c.activeTurns, binding.ThreadID)
	} else {
		found = false
	}
	c.turnMu.Unlock()
	if found {
		c.emitRuntimeFailure(active.ref)
	}
}

func (c *Client) emitRuntimeFailure(ref agentruntime.TurnRef) {
	event := agentruntime.Event{
		Type: agentruntime.EventRuntimeFailed, ThreadID: ref.ThreadID,
		TurnID: ref.TurnID, Status: string(agentruntime.TurnFailed),
		Sequence: c.eventSequence.Add(1),
	}
	if sink := c.currentSink(); sink != nil {
		sink.RuntimeEvent(event)
	}
}

func (c *Client) observeTurnEvent(event agentruntime.Event, process *appProcess) bool {
	switch event.Type {
	case agentruntime.EventTurnStarted:
		c.turnMu.Lock()
		active, found := c.activeTurns[event.ThreadID]
		if c.finishedTurns[event.ThreadID] == event.TurnID {
			c.turnMu.Unlock()
			return false
		}
		if !found || active.ref.TurnID == event.TurnID {
			delete(c.finishedTurns, event.ThreadID)
			c.activeTurns[event.ThreadID] = activeTurn{
				ref:     agentruntime.TurnRef{ThreadRef: agentruntime.ThreadRef{Runtime: c.Name(), ThreadID: event.ThreadID}, TurnID: event.TurnID},
				process: process,
			}
		}
		c.turnMu.Unlock()
	case agentruntime.EventTurnCompleted:
		c.turnMu.Lock()
		active, found := c.activeTurns[event.ThreadID]
		if found && active.ref.TurnID == event.TurnID {
			c.finishedTurns[event.ThreadID] = event.TurnID
			delete(c.activeTurns, event.ThreadID)
		}
		c.turnMu.Unlock()
		return found && active.ref.TurnID == event.TurnID
	}
	return true
}
