// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/guygrigsby/rudy/internal/agentruntime"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/session"
)

type runtimeApproval struct {
	question  agentruntime.ApprovalQuestion
	ctx       context.Context
	answer    chan agentruntime.ApprovalAnswer
	abandon   chan struct{}
	answered  bool
	abandoned bool
}

func (s *Server) RequestApproval(ctx context.Context, question agentruntime.ApprovalQuestion) (agentruntime.ApprovalAnswer, error) {
	waitCtx := ctx
	if timeout := time.Duration(s.d.Config.ToolTimeoutMS) * time.Millisecond; timeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	deny := runtimeDeny("runtime approval refused")
	if err := waitCtx.Err(); err != nil {
		return deny, err
	}
	if err := validateRuntimeQuestion(question); err != nil {
		return deny, err
	}
	s.mu.Lock()
	ls := s.runtimeThreads[runtimeThreadKey{runtime: question.Runtime, threadID: question.ThreadID}]
	s.mu.Unlock()
	if ls == nil || ls.runtime == nil {
		return deny, errors.New("runtime approval has no verified thread binding")
	}

	rs := ls.runtime
	rs.mu.Lock()
	if err := waitCtx.Err(); err != nil {
		rs.mu.Unlock()
		return deny, err
	}
	active := rs.active
	_, itemKnown := rs.items[question.ItemID]
	if active == nil || active.TurnID != question.TurnID || !itemKnown {
		rs.mu.Unlock()
		deny = runtimeDeny("runtime approval binding is stale")
		if err := s.recordRuntimeDecision(ls, question, deny, session.RuntimeByStale); err != nil {
			return deny, err
		}
		return deny, nil
	}

	pending := &runtimeApproval{
		question: question, ctx: waitCtx, answer: make(chan agentruntime.ApprovalAnswer, 1), abandon: make(chan struct{}),
	}
	ls.obsMu.Lock()
	if _, seen := ls.runtimeAnswered[question.RequestID]; seen {
		ls.obsMu.Unlock()
		rs.mu.Unlock()
		return deny, errors.New("runtime approval request id was already used")
	}
	targets := ls.askersObsLocked()
	ls.runtimeAnswered[question.RequestID] = len(targets) == 0
	if len(targets) > 0 {
		ls.runtimeApprovals[question.RequestID] = pending
	}
	ls.obsMu.Unlock()

	if len(targets) == 0 {
		deny = runtimeDeny("no asker is attached")
		err := s.recordRuntimeDecision(ls, question, deny, session.RuntimeByNoAsker)
		rs.mu.Unlock()
		if err != nil {
			return deny, err
		}
		return deny, nil
	}
	rs.mu.Unlock()
	request := runtimePermissionRequest(ls.sess.ID().String(), question)
	for _, target := range targets {
		target.notify(protocol.NotifyRuntimePermissionRequested, request)
	}

	select {
	case answer := <-pending.answer:
		return answer, nil
	case <-pending.abandon:
		deny = runtimeDeny("asker disconnected")
		claimed, err := s.finishRuntimeApproval(ls, pending, deny, session.RuntimeByDisconnect, false)
		if err != nil {
			return deny, err
		}
		if !claimed {
			return <-pending.answer, nil
		}
		return deny, nil
	case <-s.ctx.Done():
		deny = runtimeDeny("server shutting down")
		claimed, err := s.finishRuntimeApproval(ls, pending, deny, session.RuntimeByShutdown, true)
		if err != nil {
			return deny, err
		}
		if !claimed {
			return <-pending.answer, nil
		}
		return deny, s.ctx.Err()
	case <-waitCtx.Done():
		by := session.RuntimeByRuntimeFailure
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			by = session.RuntimeByTimeout
		}
		deny = runtimeDeny(waitCtx.Err().Error())
		claimed, err := s.finishRuntimeApproval(ls, pending, deny, by, true)
		if err != nil {
			return deny, err
		}
		if !claimed {
			return <-pending.answer, nil
		}
		return deny, waitCtx.Err()
	}
}

func validateRuntimeQuestion(question agentruntime.ApprovalQuestion) error {
	if question.Runtime == "" || question.RequestID == "" || question.ThreadID == "" || question.TurnID == "" || question.ItemID == "" {
		return errors.New("runtime approval binding is incomplete")
	}
	switch question.Kind {
	case agentruntime.ApprovalCommand, agentruntime.ApprovalFileChange, agentruntime.ApprovalPermissions:
	default:
		return errors.New("runtime approval kind is invalid")
	}
	if len(question.AllowedScopes) == 0 {
		return errors.New("runtime approval has no allowed scope")
	}
	for _, scope := range question.AllowedScopes {
		if scope != agentruntime.ScopeOnce && scope != agentruntime.ScopeSession {
			return errors.New("runtime approval has an invalid scope")
		}
	}
	return nil
}

func runtimePermissionRequest(sessionID string, question agentruntime.ApprovalQuestion) protocol.RuntimePermissionRequested {
	return protocol.RuntimePermissionRequested{
		SessionID: sessionID, TurnID: question.TurnID, RequestID: question.RequestID, ItemID: question.ItemID,
		Kind: question.Kind, Summary: question.Summary, Command: question.Command, CWD: question.CWD,
		Reason: question.Reason, Changes: question.Changes, Network: question.Network,
		Permissions: question.Permissions, AllowedScopes: question.AllowedScopes,
	}
}

func runtimeDeny(reason string) agentruntime.ApprovalAnswer {
	if reason == "" {
		reason = "runtime approval denied"
	}
	return agentruntime.ApprovalAnswer{Decision: agentruntime.DecisionDeny, Scope: agentruntime.ScopeOnce, Reason: reason}
}

func (s *Server) handleRuntimeApprovalAnswer(cn *conn, raw []byte) (any, *protocol.Error) {
	var params protocol.RuntimeApprovalAnswerParams
	if e := decode(raw, &params); e != nil {
		return nil, e
	}
	if !cn.isAsker() {
		return nil, perr(protocol.CodeUnauthorized, "connection did not declare asker")
	}
	ls, e := s.lookup(params.SessionID)
	if e != nil {
		return nil, e
	}
	if !cn.subscribed(ls.sess.ID()) {
		return nil, perr(protocol.CodeUnauthorized, "connection is not attached to the runtime session")
	}

	rs := ls.runtime
	if rs == nil {
		return nil, perr(protocol.CodeNotFound, "session has no runtime approval")
	}
	rs.mu.Lock()
	if rs.active == nil || rs.active.TurnID != params.TurnID {
		rs.mu.Unlock()
		return nil, perr(protocol.CodeConflict, "runtime approval belongs to another turn")
	}
	ls.obsMu.Lock()
	pending, found := ls.runtimeApprovals[params.RequestID]
	already := ls.runtimeAnswered[params.RequestID]
	if !found {
		ls.obsMu.Unlock()
		rs.mu.Unlock()
		if already {
			return nil, perr(protocol.CodeConflict, "runtime approval was already answered")
		}
		return nil, perr(protocol.CodeNotFound, "no pending runtime approval")
	}
	if pending.answered || pending.abandoned || already {
		ls.obsMu.Unlock()
		rs.mu.Unlock()
		return nil, perr(protocol.CodeConflict, "runtime approval was already answered")
	}
	if err := pending.ctx.Err(); err != nil {
		ls.obsMu.Unlock()
		rs.mu.Unlock()
		return nil, perr(protocol.CodeConflict, "runtime approval is no longer active")
	}
	if pending.question.TurnID != params.TurnID {
		ls.obsMu.Unlock()
		rs.mu.Unlock()
		return nil, perr(protocol.CodeConflict, "runtime approval binding changed")
	}
	if _, itemKnown := rs.items[pending.question.ItemID]; !itemKnown {
		ls.obsMu.Unlock()
		rs.mu.Unlock()
		return nil, perr(protocol.CodeConflict, "runtime approval item is no longer active")
	}
	answer := agentruntime.ApprovalAnswer{Decision: params.Decision, Scope: params.Scope, Reason: params.Reason, Granted: slices.Clone(params.Granted)}
	if err := validateRuntimeAnswer(pending.question, answer); err != nil {
		ls.obsMu.Unlock()
		rs.mu.Unlock()
		return nil, perr(protocol.CodeInvalidArgument, err.Error())
	}
	pending.answered = true
	ls.runtimeAnswered[params.RequestID] = true
	ls.obsMu.Unlock()

	if err := s.recordRuntimeDecision(ls, pending.question, answer, session.RuntimeByAsker); err != nil {
		pending.answer <- runtimeDeny("could not persist runtime approval")
		rs.mu.Unlock()
		return nil, protocol.ErrorFrom(err)
	}
	pending.answer <- answer
	rs.mu.Unlock()
	return struct{}{}, nil
}

func validateRuntimeAnswer(question agentruntime.ApprovalQuestion, answer agentruntime.ApprovalAnswer) error {
	if answer.Reason == "" {
		return errors.New("runtime approval answer requires a reason")
	}
	if answer.Decision != agentruntime.DecisionAllow && answer.Decision != agentruntime.DecisionDeny {
		return errors.New("runtime approval answer has invalid decision")
	}
	if answer.Decision == agentruntime.DecisionDeny {
		if answer.Scope != agentruntime.ScopeOnce || len(answer.Granted) > 0 {
			return errors.New("runtime approval deny must use once scope and grant nothing")
		}
		return nil
	}
	if !slices.Contains(question.AllowedScopes, answer.Scope) {
		return errors.New("runtime approval scope was not offered")
	}
	if question.Kind != agentruntime.ApprovalPermissions && len(answer.Granted) > 0 {
		return errors.New("only a permissions approval may grant permission members")
	}
	for _, granted := range answer.Granted {
		if !slices.Contains(question.Permissions, granted) {
			return fmt.Errorf("runtime approval granted an unrequested permission %q", granted)
		}
	}
	return nil
}

func (s *Server) finishRuntimeApproval(ls *liveSession, pending *runtimeApproval, answer agentruntime.ApprovalAnswer, by session.RuntimeDecidedBy, remove bool) (bool, error) {
	ls.obsMu.Lock()
	if pending.answered {
		ls.obsMu.Unlock()
		return false, nil
	}
	pending.answered = true
	ls.runtimeAnswered[pending.question.RequestID] = true
	if remove {
		delete(ls.runtimeApprovals, pending.question.RequestID)
	}
	ls.obsMu.Unlock()
	if err := s.recordRuntimeDecision(ls, pending.question, answer, by); err != nil {
		return true, err
	}
	if remove {
		s.publishRuntimeApprovalResolved(ls, pending.question.RequestID)
	}
	return true, nil
}

func (s *Server) recordRuntimeDecision(ls *liveSession, question agentruntime.ApprovalQuestion, answer agentruntime.ApprovalAnswer, by session.RuntimeDecidedBy) error {
	decision := session.Deny
	if answer.Decision == agentruntime.DecisionAllow {
		decision = session.Allow
	}
	scope := session.Scope(answer.Scope)
	record := session.RuntimePermissionDecision{
		Runtime: question.Runtime, ThreadID: question.ThreadID, TurnID: question.TurnID,
		ItemID: question.ItemID, RequestID: question.RequestID, ApprovalKind: session.RuntimeApprovalKind(question.Kind),
		Decision: decision, DecidedBy: by, Scope: scope, Reason: answer.Reason,
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.closed {
		return errors.New("runtime approval session closed")
	}
	_, err := ls.appendAndBroadcastLocked(record)
	return err
}

func (s *Server) resolveRuntimeApprovals(ls *liveSession, requestID, turnID string) {
	ls.obsMu.Lock()
	type resolution struct {
		id      string
		pending *runtimeApproval
		deny    bool
	}
	var resolved []resolution
	for id, pending := range ls.runtimeApprovals {
		if requestID != "" && id != requestID || turnID != "" && pending.question.TurnID != turnID {
			continue
		}
		delete(ls.runtimeApprovals, id)
		deny := !pending.answered
		if deny {
			pending.answered = true
			ls.runtimeAnswered[id] = true
		}
		resolved = append(resolved, resolution{id: id, pending: pending, deny: deny})
	}
	ls.obsMu.Unlock()
	for _, resolution := range resolved {
		if resolution.deny {
			answer := runtimeDeny("runtime approval became stale")
			_ = s.recordRuntimeDecision(ls, resolution.pending.question, answer, session.RuntimeByStale)
			resolution.pending.answer <- answer
		}
		s.publishRuntimeApprovalResolved(ls, resolution.id)
	}
}

func (s *Server) publishRuntimeApprovalResolved(ls *liveSession, requestID string) {
	params := protocol.RuntimePermissionResolved{SessionID: ls.sess.ID().String(), RequestID: requestID}
	ls.obsMu.Lock()
	ls.broadcastObsLocked(protocol.NotifyRuntimePermissionResolved, params)
	ls.obsMu.Unlock()
	ls.notifyWatchers(protocol.NotifyRuntimePermissionResolved, params)
}

func (s *Server) failRuntimeApprovalsForItem(ls *liveSession, turnID, itemID string) {
	ls.obsMu.Lock()
	var failed []*runtimeApproval
	for id, pending := range ls.runtimeApprovals {
		if pending.answered || pending.question.TurnID != turnID || pending.question.ItemID != itemID {
			continue
		}
		pending.answered = true
		ls.runtimeAnswered[id] = true
		delete(ls.runtimeApprovals, id)
		failed = append(failed, pending)
	}
	ls.obsMu.Unlock()
	for _, pending := range failed {
		answer := runtimeDeny("runtime approval item completed")
		_ = s.recordRuntimeDecision(ls, pending.question, answer, session.RuntimeByStale)
		pending.answer <- answer
		s.publishRuntimeApprovalResolved(ls, pending.question.RequestID)
	}
}
