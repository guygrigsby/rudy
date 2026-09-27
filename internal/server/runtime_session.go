// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/agentruntime"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/turn"
)

type runtimeThreadKey struct {
	runtime  string
	threadID string
}

type runtimeSessionState struct {
	mu        sync.Mutex
	runtime   agentruntime.Runtime
	name      string
	thread    *agentruntime.ThreadRef
	active    *agentruntime.TurnRef
	terminal  string
	steering  bool
	mutating  bool
	ambiguous bool
	sequence  uint64
	revision  uint64
	items     map[string]agentruntime.ItemType
	usage     session.Usage
}

type namedRuntimeSink struct {
	server  *Server
	runtime string
}

func (s namedRuntimeSink) RuntimeEvent(event agentruntime.Event) {
	s.server.runtimeEvent(s.runtime, event)
}
func (s namedRuntimeSink) AccountUpdated(state agentruntime.AccountState) {
	state.Runtime = s.runtime
	s.server.AccountUpdated(state)
}
func (s namedRuntimeSink) LoginCompleted(completion agentruntime.LoginCompletion) {
	completion.Runtime = s.runtime
	s.server.LoginCompleted(completion)
}
func (s namedRuntimeSink) RequestApproval(ctx context.Context, question agentruntime.ApprovalQuestion) (agentruntime.ApprovalAnswer, error) {
	question.Runtime = s.runtime
	return s.server.RequestApproval(ctx, question)
}

// RuntimeSink binds callbacks to the registered runtime that produced them. Upstream thread
// ids never select a runtime or session on their own.
func (s *Server) RuntimeSink(runtime string) agentruntime.Sink {
	return namedRuntimeSink{server: s, runtime: runtime}
}

func newRuntimeSession(runtime agentruntime.Runtime) *runtimeSessionState {
	return &runtimeSessionState{runtime: runtime, name: runtime.Name(), items: map[string]agentruntime.ItemType{}}
}

func (s *Server) refreshRuntimeSession(ctx context.Context, ls *liveSession) *protocol.Error {
	rs := ls.runtime
	if rs == nil {
		return nil
	}
	rs.mu.Lock()
	if rs.mutating {
		rs.mu.Unlock()
		return perr(protocol.CodeConflict, "a runtime mutation is in progress")
	}
	if rs.thread == nil {
		rs.mu.Unlock()
		return nil
	}
	ref := *rs.thread
	revision := rs.revision
	rs.mutating = true
	rs.mu.Unlock()

	if err := rs.runtime.ResumeThread(ctx, ref); err != nil {
		rs.finishRuntimeMutation(err)
		return runtimeOperationError(rs.name, err)
	}
	thread, err := rs.runtime.ReadThread(ctx, ref)
	if err != nil {
		rs.finishRuntimeMutation(err)
		return runtimeOperationError(rs.name, err)
	}
	if thread.Runtime != rs.name || thread.ThreadID != ref.ThreadID {
		err = errors.New("runtime returned a mismatched thread binding")
		rs.finishRuntimeMutation(err)
		return runtimeOperationError(rs.name, err)
	}
	entries, active, err := projectRuntimeThread(thread, ref)
	if err != nil {
		rs.finishRuntimeMutation(err)
		return runtimeOperationError(rs.name, err)
	}
	rs.mu.Lock()
	changed := rs.revision != revision
	rs.mutating = false
	rs.ambiguous = false
	if !changed {
		rs.active = active
		rs.terminal = ""
		clear(rs.items)
	}
	rs.mu.Unlock()
	ls.obsMu.Lock()
	if changed {
		ls.runtimeEntries = mergeRuntimeEntries(entries, ls.runtimeEntries)
	} else {
		ls.runtimeEntries = entries
		if active == nil {
			ls.state = turn.Completed
			ls.turnID = ""
		} else {
			ls.state = turn.Streaming
			ls.turnID = active.TurnID
		}
	}
	ls.obsMu.Unlock()
	return nil
}

func mergeRuntimeEntries(snapshot, live []agentruntime.ProjectedEntry) []agentruntime.ProjectedEntry {
	liveByID := make(map[ulid.ULID]agentruntime.ProjectedEntry, len(live))
	for _, entry := range live {
		liveByID[entry.ID] = entry
	}
	merged := make([]agentruntime.ProjectedEntry, 0, len(snapshot)+len(live))
	seen := make(map[ulid.ULID]bool, len(snapshot)+len(live))
	for _, entry := range snapshot {
		if newer, ok := liveByID[entry.ID]; ok {
			entry = newer
		}
		merged = append(merged, entry)
		seen[entry.ID] = true
	}
	for _, entry := range live {
		if !seen[entry.ID] {
			merged = append(merged, entry)
		}
	}
	return merged
}

func projectRuntimeThread(thread agentruntime.Thread, ref agentruntime.ThreadRef) ([]agentruntime.ProjectedEntry, *agentruntime.TurnRef, error) {
	seenTurns := map[string]bool{}
	seenItems := map[string]bool{}
	var entries []agentruntime.ProjectedEntry
	var active *agentruntime.TurnRef
	for _, runtimeTurn := range thread.Turns {
		if runtimeTurn.TurnID == "" || seenTurns[runtimeTurn.TurnID] {
			return nil, nil, errors.New("runtime returned an empty or duplicate turn id")
		}
		seenTurns[runtimeTurn.TurnID] = true
		if runtimeTurn.Status == agentruntime.TurnRunning {
			if active != nil {
				return nil, nil, errors.New("runtime returned more than one active turn")
			}
			turnRef := agentruntime.TurnRef{ThreadRef: ref, TurnID: runtimeTurn.TurnID}
			active = &turnRef
		}
		for _, item := range runtimeTurn.Items {
			key := runtimeTurn.TurnID + "\x00" + item.ItemID
			if item.ItemID == "" || item.Type == "" || seenItems[key] {
				return nil, nil, errors.New("runtime returned an empty or duplicate item binding")
			}
			seenItems[key] = true
			entries = append(entries, agentruntime.Project(thread, runtimeTurn, item))
		}
	}
	return entries, active, nil
}

func (s *Server) interruptRuntimeTurn(ctx context.Context, ls *liveSession, how session.Interrupt) (InterruptResult, *protocol.Error) {
	rs := ls.runtime
	rs.mu.Lock()
	if rs.mutating {
		rs.mu.Unlock()
		return InterruptResult{}, perr(protocol.CodeConflict, "a runtime mutation is in progress")
	}
	if rs.active == nil {
		rs.mu.Unlock()
		return InterruptResult{}, perr(protocol.CodeRefusedByInvariant, "no active turn")
	}
	ref := *rs.active
	if how == session.InterruptSteer {
		rs.steering = true
		rs.mu.Unlock()
		ls.obsMu.Lock()
		ls.state = turn.Steering
		ls.turnID = ref.TurnID
		ls.broadcastObsLocked(protocol.NotifyTurnState, protocol.TurnStateChanged{
			SessionID: ls.sess.ID().String(), TurnID: ref.TurnID, State: string(turn.Steering),
		})
		ls.obsMu.Unlock()
		return InterruptResult{TurnID: ref.TurnID, State: string(turn.Steering)}, nil
	}
	rs.mutating = true
	rs.mu.Unlock()
	err := rs.runtime.InterruptTurn(ctx, ref)
	rs.finishRuntimeMutation(err)
	if err != nil {
		return InterruptResult{}, runtimeOperationError(rs.name, err)
	}
	state, _ := ls.mirroredState()
	return InterruptResult{TurnID: ref.TurnID, State: string(state)}, nil
}

func (s *Server) forkRuntimeAt(ctx context.Context, cn *conn, parent *liveSession, at string) (protocol.SessionInfo, *protocol.Error) {
	parent.obsMu.Lock()
	latest := ""
	if n := len(parent.runtimeEntries); n > 0 {
		latest = parent.runtimeEntries[n-1].ID.String()
	}
	parent.obsMu.Unlock()
	if at == "" {
		at = latest
	}
	if _, err := ulid.Parse(at); err != nil {
		return protocol.SessionInfo{}, perr(protocol.CodeInvalidArgument, "bad entry id")
	}
	if latest == "" || at != latest {
		return protocol.SessionInfo{}, perr(protocol.CodeRefusedByInvariant, "runtime sessions fork only at the newest projected entry")
	}

	rs := parent.runtime
	rs.mu.Lock()
	if rs.mutating {
		rs.mu.Unlock()
		return protocol.SessionInfo{}, perr(protocol.CodeConflict, "a runtime mutation is in progress")
	}
	if rs.ambiguous {
		rs.mu.Unlock()
		return protocol.SessionInfo{}, perr(protocol.CodeAmbiguous, "runtime state requires reconciliation")
	}
	if rs.thread == nil {
		rs.mu.Unlock()
		return protocol.SessionInfo{}, perr(protocol.CodeRefusedByInvariant, "runtime thread is not linked")
	}
	if rs.active != nil {
		rs.mu.Unlock()
		return protocol.SessionInfo{}, perr(protocol.CodeConflict, "a turn is active")
	}
	parentRef := *rs.thread
	rs.mutating = true
	rs.mu.Unlock()

	childRef, err := rs.runtime.ForkThread(ctx, parentRef)
	rs.finishRuntimeMutation(err)
	if err != nil {
		return protocol.SessionInfo{}, runtimeOperationError(rs.name, err)
	}
	if childRef.Runtime != rs.name || childRef.ThreadID == "" || childRef.ThreadID == parentRef.ThreadID {
		return protocol.SessionInfo{}, runtimeOperationError(rs.name, errors.New("runtime returned a reused or mismatched fork binding"))
	}

	parent.mu.Lock()
	if parent.closed {
		parent.mu.Unlock()
		return protocol.SessionInfo{}, perr(protocol.CodeUnavailable, "session unavailable, retry")
	}
	localAt := parent.latestEntryID()
	localID, parseErr := ulid.Parse(localAt)
	if parseErr != nil {
		parent.mu.Unlock()
		return protocol.SessionInfo{}, perr(protocol.CodeInternal, "runtime session has no local fork point")
	}
	child, forkErr := parent.sess.Fork(s.d.Store, localID)
	parent.mu.Unlock()
	if forkErr != nil {
		return protocol.SessionInfo{}, protocol.ErrorFrom(forkErr)
	}
	childRef.SessionID = child.ID()
	thread, err := rs.runtime.ReadThread(ctx, childRef)
	if err != nil {
		_ = child.Close()
		return protocol.SessionInfo{}, runtimeOperationError(rs.name, err)
	}
	if thread.Runtime != rs.name || thread.ThreadID != childRef.ThreadID {
		_ = child.Close()
		return protocol.SessionInfo{}, runtimeOperationError(rs.name, errors.New("runtime returned a mismatched fork thread"))
	}
	entries, active, err := projectRuntimeThread(thread, childRef)
	if err != nil {
		_ = child.Close()
		return protocol.SessionInfo{}, runtimeOperationError(rs.name, err)
	}
	if err := s.d.Store.RuntimeLinks().Write(child.ID(), session.RuntimeLink{Runtime: rs.name, ThreadID: childRef.ThreadID}); err != nil {
		_ = child.Close()
		return protocol.SessionInfo{}, protocol.ErrorFrom(err)
	}
	childLive := newLive(child, parent.model)
	childLive.runtime = newRuntimeSession(rs.runtime)
	childLive.runtime.thread = &childRef
	childLive.runtime.active = active
	childLive.runtimeEntries = entries
	if active != nil {
		childLive.state = turn.Streaming
		childLive.turnID = active.TurnID
	}
	childLive.runtimeLinked = true
	return s.installAndAttach(cn, childLive)
}

func (s *Server) startRuntimeTurn(ctx context.Context, ls *liveSession, msg session.UserMessage) (string, *protocol.Error) {
	rs := ls.runtime
	if rs == nil {
		return "", perr(protocol.CodeInternal, "runtime session state unavailable")
	}
	rs.mu.Lock()
	if rs.mutating {
		rs.mu.Unlock()
		return "", perr(protocol.CodeConflict, "a runtime mutation is in progress")
	}
	if rs.ambiguous {
		rs.mu.Unlock()
		return "", perr(protocol.CodeAmbiguous, "runtime state requires reconciliation")
	}
	if msg.Source == session.SourceSteer {
		if !rs.steering || rs.active == nil {
			rs.mu.Unlock()
			return "", perr(protocol.CodeRefusedByInvariant, "session is not steering")
		}
		turnRef := *rs.active
		rs.mutating = true
		rs.mu.Unlock()
		ref, err := rs.runtime.SteerTurn(ctx, agentruntime.SteerTurnRequest{Turn: turnRef, Content: msg.Content})
		rs.mu.Lock()
		rs.mutating = false
		if err == nil && ref.TurnID != turnRef.TurnID {
			err = fmt.Errorf("runtime steer changed turn id from %q to %q", turnRef.TurnID, ref.TurnID)
		}
		if errors.Is(err, agentruntime.ErrAmbiguous) {
			rs.ambiguous = true
		}
		rs.steering = false
		rs.mu.Unlock()
		if err != nil {
			return "", runtimeOperationError(rs.name, err)
		}
		return ref.TurnID, nil
	}
	if rs.active != nil {
		rs.mu.Unlock()
		return "", perr(protocol.CodeConflict, "a turn is active")
	}
	rs.mutating = true
	rs.terminal = ""
	thread := rs.thread
	rs.mu.Unlock()

	if thread == nil {
		view := deriveInfo(ls.sess.ID(), ls.snapshotEntries())
		created, err := rs.runtime.StartThread(ctx, agentruntime.StartThreadRequest{
			SessionID: ls.sess.ID(), Workspace: view.Workspace, Model: view.Model, Thinking: view.Thinking, Mode: view.Mode,
		})
		if err != nil {
			rs.finishRuntimeMutation(err)
			return "", runtimeOperationError(rs.name, err)
		}
		if created.Runtime != rs.name || created.SessionID != ls.sess.ID() || created.ThreadID == "" {
			rs.finishRuntimeMutation(nil)
			return "", runtimeOperationError(rs.name, errors.New("runtime returned a mismatched thread binding"))
		}
		if err := s.d.Store.RuntimeLinks().Write(ls.sess.ID(), session.RuntimeLink{Runtime: rs.name, ThreadID: created.ThreadID}); err != nil {
			rs.finishRuntimeMutation(err)
			if errors.Is(err, session.ErrRuntimeLinkConflict) {
				return "", perr(protocol.CodeConflict, err.Error())
			}
			return "", protocol.ErrorFrom(err)
		}
		s.mu.Lock()
		bindErr := s.bindRuntimeThreadRefLocked(ls, rs.name, created.ThreadID)
		s.mu.Unlock()
		if bindErr != nil {
			rs.finishRuntimeMutation(nil)
			return "", bindErr
		}
		thread = &created
		rs.mu.Lock()
		rs.thread = thread
		rs.mu.Unlock()
		ls.obsMu.Lock()
		ls.runtimeLinked = true
		ls.obsMu.Unlock()
	}
	view := deriveInfo(ls.sess.ID(), ls.snapshotEntries())
	ref, err := rs.runtime.StartTurn(ctx, agentruntime.StartTurnRequest{
		Thread: *thread, Content: msg.Content, Model: view.Model, Thinking: view.Thinking, Mode: view.Mode,
	})
	rs.mu.Lock()
	rs.mutating = false
	if errors.Is(err, agentruntime.ErrAmbiguous) {
		rs.ambiguous = true
	}
	completedBeforeResponse := false
	if err == nil {
		if ref.Runtime != rs.name || ref.ThreadID != thread.ThreadID || ref.TurnID == "" {
			err = errors.New("runtime returned a mismatched turn binding")
		} else if rs.terminal == ref.TurnID {
			completedBeforeResponse = true
			rs.active = nil
			rs.steering = false
		} else {
			rs.active = &ref
			rs.steering = false
		}
	}
	rs.mu.Unlock()
	if err != nil {
		return "", runtimeOperationError(rs.name, err)
	}
	if completedBeforeResponse {
		return ref.TurnID, nil
	}
	ls.obsMu.Lock()
	ls.state = turn.Streaming
	ls.turnID = ref.TurnID
	ls.broadcastObsLocked(protocol.NotifyTurnState, protocol.TurnStateChanged{
		SessionID: ls.sess.ID().String(), TurnID: ref.TurnID, State: string(turn.Streaming),
	})
	ls.obsMu.Unlock()
	return ref.TurnID, nil
}

func (rs *runtimeSessionState) finishRuntimeMutation(err error) {
	rs.mu.Lock()
	rs.mutating = false
	if errors.Is(err, agentruntime.ErrAmbiguous) {
		rs.ambiguous = true
	}
	rs.mu.Unlock()
}

func runtimeOperationError(runtime string, err error) *protocol.Error {
	if errors.Is(err, agentruntime.ErrAmbiguous) {
		return protocol.ErrorFrom(err)
	}
	return protocol.ErrorFrom(&agentruntime.Error{Runtime: runtime, Message: err.Error()})
}

func (s *Server) runtimeEvent(runtimeName string, event agentruntime.Event) {
	if event.ThreadID == "" {
		return
	}
	s.mu.Lock()
	ls := s.runtimeThreads[runtimeThreadKey{runtime: runtimeName, threadID: event.ThreadID}]
	s.mu.Unlock()
	if ls == nil || ls.runtime == nil || ls.runtime.name != runtimeName {
		return
	}
	rs := ls.runtime
	rs.mu.Lock()
	if event.Sequence != 0 && event.Sequence <= rs.sequence {
		rs.mu.Unlock()
		return
	}
	if event.Sequence != 0 {
		rs.sequence = event.Sequence
	}
	rs.revision++
	activeID := ""
	if rs.active != nil {
		activeID = rs.active.TurnID
	}
	if event.Type != agentruntime.EventTurnStarted && event.TurnID != "" && activeID != "" && event.TurnID != activeID {
		rs.mu.Unlock()
		return
	}
	var (
		entry           *agentruntime.ProjectedEntry
		delta           *protocol.RuntimeDeltaParams
		state           *protocol.TurnStateChanged
		resolvedRequest string
		resolvedTurn    string
		completedItem   string
		usage           *protocol.RuntimeUsageUpdated
	)
	switch event.Type {
	case agentruntime.EventTurnStarted:
		if event.TurnID == "" || activeID != "" && activeID != event.TurnID {
			rs.mu.Unlock()
			return
		}
		ref := agentruntime.TurnRef{ThreadRef: agentruntime.ThreadRef{
			Runtime: runtimeName, SessionID: ls.sess.ID(), ThreadID: event.ThreadID,
		}, TurnID: event.TurnID}
		rs.active = &ref
		rs.steering = false
		state = &protocol.TurnStateChanged{SessionID: ls.sess.ID().String(), TurnID: event.TurnID, State: string(turn.Streaming)}
	case agentruntime.EventItemStarted:
		if event.TurnID == "" || event.ItemID == "" || event.Item.Type == "" || activeID != event.TurnID {
			rs.mu.Unlock()
			return
		}
		rs.items[event.ItemID] = event.Item.Type
	case agentruntime.EventItemDelta:
		kind := event.Item.Type
		if kind == "" {
			kind = rs.items[event.ItemID]
		}
		if event.TurnID == "" || event.ItemID == "" || kind == "" || activeID != event.TurnID {
			rs.mu.Unlock()
			return
		}
		delta = &protocol.RuntimeDeltaParams{
			SessionID: ls.sess.ID().String(), TurnID: event.TurnID, ItemID: event.ItemID, Kind: kind, Text: event.Text,
			Replace: event.Status == "replace",
		}
	case agentruntime.EventItemCompleted:
		if event.TurnID == "" || event.Item.ItemID == "" || event.Item.Type == "" || activeID != event.TurnID {
			rs.mu.Unlock()
			return
		}
		projected := agentruntime.Project(
			agentruntime.Thread{Runtime: runtimeName, ThreadID: event.ThreadID},
			agentruntime.Turn{TurnID: event.TurnID, Status: agentruntime.TurnRunning, Usage: rs.usage}, event.Item,
		)
		entry = &projected
		completedItem = event.Item.ItemID
		delete(rs.items, event.Item.ItemID)
	case agentruntime.EventDiffUpdated, agentruntime.EventPlanUpdated:
		kind := agentruntime.ItemDiff
		if event.Type == agentruntime.EventPlanUpdated {
			kind = agentruntime.ItemPlan
		}
		itemID := event.ItemID
		if itemID == "" {
			itemID = "aggregate:" + string(kind)
		}
		delta = &protocol.RuntimeDeltaParams{
			SessionID: ls.sess.ID().String(), TurnID: event.TurnID, ItemID: itemID, Kind: kind, Text: event.Text, Replace: true,
		}
	case agentruntime.EventUsageUpdated:
		rs.usage = event.Usage
		usage = &protocol.RuntimeUsageUpdated{SessionID: ls.sess.ID().String(), TurnID: event.TurnID, Usage: rs.usage}
	case agentruntime.EventRuntimeFailed:
		if event.TurnID == "" || activeID != event.TurnID {
			rs.mu.Unlock()
			return
		}
		rs.ambiguous = true
		rs.active = nil
		rs.terminal = event.TurnID
		rs.steering = false
		clear(rs.items)
		state = &protocol.TurnStateChanged{SessionID: ls.sess.ID().String(), TurnID: event.TurnID, State: string(turn.Failed)}
		resolvedTurn = event.TurnID
	case agentruntime.EventTurnCompleted:
		if event.TurnID == "" || activeID != event.TurnID {
			rs.mu.Unlock()
			return
		}
		if event.Usage != (session.Usage{}) {
			rs.usage = event.Usage
			usage = &protocol.RuntimeUsageUpdated{SessionID: ls.sess.ID().String(), TurnID: event.TurnID, Usage: rs.usage}
		}
		terminal := turn.Completed
		switch agentruntime.TurnStatus(event.Status) {
		case agentruntime.TurnFailed:
			terminal = turn.Failed
		case agentruntime.TurnInterrupted:
			terminal = turn.Idle
		}
		rs.active = nil
		rs.terminal = event.TurnID
		rs.steering = false
		clear(rs.items)
		state = &protocol.TurnStateChanged{SessionID: ls.sess.ID().String(), TurnID: event.TurnID, State: string(terminal)}
		resolvedTurn = event.TurnID
	case agentruntime.EventRequestResolved:
		if event.RequestID == "" {
			rs.mu.Unlock()
			return
		}
		resolvedRequest = event.RequestID
	case agentruntime.EventWarning, agentruntime.EventError:
		if event.ItemID == "" || event.TurnID == "" || activeID != event.TurnID {
			rs.mu.Unlock()
			return
		}
		kind := agentruntime.ItemWarning
		if event.Type == agentruntime.EventError {
			kind = agentruntime.ItemError
		}
		projected := agentruntime.Project(
			agentruntime.Thread{Runtime: runtimeName, ThreadID: event.ThreadID},
			agentruntime.Turn{TurnID: event.TurnID, Status: agentruntime.TurnRunning, Usage: rs.usage},
			agentruntime.Item{ItemID: event.ItemID, Type: kind, Status: event.Status, Content: []session.Block{session.TextBlock(event.Text)}},
		)
		entry = &projected
	}
	rs.mu.Unlock()
	if resolvedRequest != "" {
		s.resolveRuntimeApprovals(ls, resolvedRequest, "")
	}
	if resolvedTurn != "" {
		s.resolveRuntimeApprovals(ls, "", resolvedTurn)
	}
	if completedItem != "" {
		s.failRuntimeApprovalsForItem(ls, event.TurnID, completedItem)
	}

	if entry != nil {
		s.publishRuntimeEntry(ls, *entry)
	}
	if delta != nil {
		ls.obsMu.Lock()
		ls.broadcastObsLocked(protocol.NotifyRuntimeDelta, *delta)
		ls.obsMu.Unlock()
		ls.notifyWatchers(protocol.NotifyRuntimeDelta, *delta)
	}
	if usage != nil {
		ls.obsMu.Lock()
		ls.broadcastObsLocked(protocol.NotifyRuntimeUsageUpdated, *usage)
		ls.obsMu.Unlock()
	}
	if state != nil {
		ls.obsMu.Lock()
		ls.state = turn.State(state.State)
		ls.turnID = state.TurnID
		ls.broadcastObsLocked(protocol.NotifyTurnState, *state)
		ls.obsMu.Unlock()
		ls.notifyWatchers(protocol.NotifyTurnState, *state)
		if !isActive(turn.State(state.State)) {
			s.mu.Lock()
			s.closeIfUnusedLocked(ls)
		}
	}
}

func (s *Server) publishRuntimeEntry(ls *liveSession, entry agentruntime.ProjectedEntry) {
	ls.obsMu.Lock()
	replaced := false
	for i := range ls.runtimeEntries {
		if ls.runtimeEntries[i].ID == entry.ID {
			ls.runtimeEntries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		ls.runtimeEntries = append(ls.runtimeEntries, entry)
	}
	params := protocol.RuntimeEntryParams{SessionID: ls.sess.ID().String(), Entry: entry}
	ls.broadcastObsLocked(protocol.NotifyRuntimeEntry, params)
	ls.obsMu.Unlock()
	ls.notifyWatchers(protocol.NotifyRuntimeEntry, params)
}
