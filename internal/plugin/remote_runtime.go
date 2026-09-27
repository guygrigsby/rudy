// SPDX-License-Identifier: AGPL-3.0-or-later

package plugin

import (
	"context"
	"fmt"
	"sync"

	"github.com/guygrigsby/rudy/internal/agentruntime"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
)

// remoteRuntime is an AgentRuntime registered by a spawned plugin. Every operation goes
// back over that plugin's authenticated connection.
type remoteRuntime struct {
	name string
	sp   *Spawned

	mu   sync.RWMutex
	sink agentruntime.Sink
}

var _ agentruntime.Runtime = (*remoteRuntime)(nil)

func (r *remoteRuntime) Name() string { return r.name }

func (r *remoteRuntime) Account(ctx context.Context) (agentruntime.AccountState, error) {
	var out agentruntime.AccountState
	if err := r.sp.peer.Client().Call(ctx, protocol.MethodRuntimeAccountRead, protocol.RuntimeAccountReadParams{Runtime: r.name}, &out); err != nil {
		return agentruntime.AccountState{}, err
	}
	out.Runtime = r.name
	return out, nil
}

func (r *remoteRuntime) StartLogin(ctx context.Context, mode agentruntime.LoginMode) (agentruntime.AuthChallenge, error) {
	var out agentruntime.AuthChallenge
	if err := r.sp.peer.Client().Call(ctx, protocol.MethodRuntimeLoginStart, protocol.RuntimeLoginStartParams{Runtime: r.name, Mode: mode}, &out); err != nil {
		return agentruntime.AuthChallenge{}, err
	}
	if out.LoginID == "" {
		return agentruntime.AuthChallenge{}, fmt.Errorf("runtime %s: login response has no login id", r.name)
	}
	out.Runtime = r.name
	return out, nil
}

func (r *remoteRuntime) CancelLogin(ctx context.Context, loginID string) error {
	return r.sp.peer.Client().Call(ctx, protocol.MethodRuntimeLoginCancel, protocol.RuntimeLoginCancelParams{Runtime: r.name, LoginID: loginID}, nil)
}

func (r *remoteRuntime) ListModels(ctx context.Context) ([]provider.Model, error) {
	var models []provider.Model
	cursor := ""
	seen := map[string]bool{}
	for {
		var out protocol.RuntimeModelListResult
		if err := r.sp.peer.Client().Call(ctx, protocol.MethodRuntimeModelList, protocol.RuntimeModelListParams{Runtime: r.name, Cursor: cursor}, &out); err != nil {
			return nil, err
		}
		for _, model := range out.Models {
			model.Ref.Provider = r.name
			model.OwnerKind = provider.OwnerRuntime
			models = append(models, model)
		}
		if out.NextCursor == "" {
			return models, nil
		}
		if seen[out.NextCursor] {
			return nil, fmt.Errorf("runtime %s: repeated model cursor %q", r.name, out.NextCursor)
		}
		seen[out.NextCursor] = true
		cursor = out.NextCursor
	}
}

func (r *remoteRuntime) StartThread(ctx context.Context, req agentruntime.StartThreadRequest) (agentruntime.ThreadRef, error) {
	var out protocol.RuntimeThreadResult
	err := r.sp.peer.Client().Call(ctx, protocol.MethodRuntimeThreadStart, protocol.RuntimeThreadStartParams{
		Runtime: r.name, SessionID: req.SessionID.String(), CWD: req.Workspace.Root,
		Model: req.Model, Thinking: req.Thinking, Mode: req.Mode,
	}, &out)
	if err != nil {
		return agentruntime.ThreadRef{}, err
	}
	if out.ThreadID == "" {
		return agentruntime.ThreadRef{}, fmt.Errorf("runtime %s: thread start returned an empty id", r.name)
	}
	return agentruntime.ThreadRef{Runtime: r.name, SessionID: req.SessionID, ThreadID: out.ThreadID}, nil
}

func (r *remoteRuntime) ResumeThread(ctx context.Context, ref agentruntime.ThreadRef) error {
	return r.sp.peer.Client().Call(ctx, protocol.MethodRuntimeThreadResume, threadParams(r.name, ref), nil)
}

func (r *remoteRuntime) ForkThread(ctx context.Context, ref agentruntime.ThreadRef) (agentruntime.ThreadRef, error) {
	var out protocol.RuntimeThreadResult
	if err := r.sp.peer.Client().Call(ctx, protocol.MethodRuntimeThreadFork, threadParams(r.name, ref), &out); err != nil {
		return agentruntime.ThreadRef{}, err
	}
	if out.ThreadID == "" || out.ThreadID == ref.ThreadID {
		return agentruntime.ThreadRef{}, fmt.Errorf("runtime %s: thread fork did not return a distinct id", r.name)
	}
	return agentruntime.ThreadRef{Runtime: r.name, SessionID: ref.SessionID, ThreadID: out.ThreadID}, nil
}

func (r *remoteRuntime) ReadThread(ctx context.Context, ref agentruntime.ThreadRef) (agentruntime.Thread, error) {
	var out agentruntime.Thread
	if err := r.sp.peer.Client().Call(ctx, protocol.MethodRuntimeThreadRead, threadParams(r.name, ref), &out); err != nil {
		return agentruntime.Thread{}, err
	}
	if out.ThreadID != ref.ThreadID {
		return agentruntime.Thread{}, fmt.Errorf("runtime %s: read returned thread %q, want %q", r.name, out.ThreadID, ref.ThreadID)
	}
	out.Runtime = r.name
	return out, nil
}

func threadParams(runtime string, ref agentruntime.ThreadRef) protocol.RuntimeThreadRefParams {
	return protocol.RuntimeThreadRefParams{Runtime: runtime, SessionID: ref.SessionID.String(), ThreadID: ref.ThreadID}
}

func (r *remoteRuntime) StartTurn(ctx context.Context, req agentruntime.StartTurnRequest) (agentruntime.TurnRef, error) {
	var out protocol.RuntimeTurnResult
	err := r.sp.peer.Client().Call(ctx, protocol.MethodRuntimeTurnStart, protocol.RuntimeTurnStartParams{
		Runtime: r.name, SessionID: req.Thread.SessionID.String(), ThreadID: req.Thread.ThreadID,
		Content: req.Content, Model: req.Model, Thinking: req.Thinking, Mode: req.Mode,
	}, &out)
	if err != nil {
		return agentruntime.TurnRef{}, err
	}
	if out.TurnID == "" {
		return agentruntime.TurnRef{}, fmt.Errorf("runtime %s: turn start returned an empty id", r.name)
	}
	return agentruntime.TurnRef{ThreadRef: req.Thread, TurnID: out.TurnID}, nil
}

func (r *remoteRuntime) SteerTurn(ctx context.Context, req agentruntime.SteerTurnRequest) (agentruntime.TurnRef, error) {
	var out protocol.RuntimeTurnResult
	err := r.sp.peer.Client().Call(ctx, protocol.MethodRuntimeTurnSteer, protocol.RuntimeTurnSteerParams{
		Runtime: r.name, SessionID: req.Turn.SessionID.String(), ThreadID: req.Turn.ThreadID,
		TurnID: req.Turn.TurnID, Content: req.Content,
	}, &out)
	if err != nil {
		return agentruntime.TurnRef{}, err
	}
	if out.TurnID != req.Turn.TurnID {
		return agentruntime.TurnRef{}, fmt.Errorf("runtime %s: steer changed turn id", r.name)
	}
	return req.Turn, nil
}

func (r *remoteRuntime) InterruptTurn(ctx context.Context, ref agentruntime.TurnRef) error {
	return r.sp.peer.Client().Call(ctx, protocol.MethodRuntimeTurnInterrupt, protocol.RuntimeTurnRefParams{
		Runtime: r.name, SessionID: ref.SessionID.String(), ThreadID: ref.ThreadID, TurnID: ref.TurnID,
	}, nil)
}

func (r *remoteRuntime) SetSink(sink agentruntime.Sink) {
	r.mu.Lock()
	r.sink = sink
	r.mu.Unlock()
}

func (r *remoteRuntime) deliverEvent(event agentruntime.Event) {
	r.mu.RLock()
	sink := r.sink
	r.mu.RUnlock()
	if sink != nil {
		sink.RuntimeEvent(event)
	}
}

func (r *remoteRuntime) deliverAccount(account agentruntime.AccountState) {
	r.mu.RLock()
	sink := r.sink
	r.mu.RUnlock()
	if sink != nil {
		account.Runtime = r.name
		sink.AccountUpdated(account)
	}
}

func (r *remoteRuntime) deliverLogin(completion agentruntime.LoginCompletion) {
	r.mu.RLock()
	sink := r.sink
	r.mu.RUnlock()
	if sink != nil {
		completion.Runtime = r.name
		sink.LoginCompleted(completion)
	}
}
