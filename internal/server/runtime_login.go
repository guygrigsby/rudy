// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/guygrigsby/rudy/internal/agentruntime"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/protocol"
)

const (
	maxPendingLoginCompletions = 64
	loginCancelTimeout         = 2 * time.Second
	loginCancelRetryInitial    = 25 * time.Millisecond
	loginCancelRetryMaximum    = 2 * time.Second
)

type deferredResponse struct {
	result any
	after  func()
}

type runtimeLoginKey struct {
	runtime string
	loginID string
}

type runtimeLoginAttempt struct {
	owner *conn
	mode  agentruntime.LoginMode
	ctx   context.Context
}

var _ agentruntime.Sink = (*Server)(nil)

func (s *Server) startRuntimeLogin(ctx context.Context, owner *conn, action plugin.AuthChallenge) (any, *protocol.Error) {
	if action.Mode != agentruntime.LoginBrowser && action.Mode != agentruntime.LoginDevice {
		return nil, perr(protocol.CodeInvalidArgument, "login mode must be browser or device")
	}
	runtime, ok := s.d.Plugins.Runtime(action.Runtime)
	if !ok {
		return nil, perr(protocol.CodeUnavailable, "runtime unavailable: "+action.Runtime)
	}
	if err := s.cancelOwnedLogins(ctx, owner, action.Runtime); err != nil {
		return nil, runtimeError(action.Runtime, err)
	}
	account, err := runtime.Account(ctx)
	if err != nil {
		return nil, runtimeError(action.Runtime, err)
	}
	s.AccountUpdated(account)
	if account.Authenticated && account.AuthMode == "chatgpt" {
		if err := s.d.Registry.RefreshSource(ctx, action.Runtime); err != nil {
			owner.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "warn", Text: action.Runtime + " model refresh: " + err.Error()})
		}
		notice := "already signed in to " + action.Runtime + " with ChatGPT"
		owner.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "info", Text: notice})
		return protocol.CommandRunResult{Notice: notice}, nil
	}

	challenge, err := runtime.StartLogin(ctx, action.Mode)
	if err != nil {
		return nil, runtimeError(action.Runtime, err)
	}
	pending, bindErr := s.bindLogin(owner, action.Runtime, challenge, action.Mode)
	if bindErr != nil {
		s.cancelRuntimeLoginEventually(action.Runtime, challenge.LoginID)
		return nil, runtimeError(action.Runtime, bindErr)
	}
	if pending != nil {
		return deferredResponse{
			result: protocol.CommandRunResult{AuthChallenge: &challenge},
			after:  func() { s.finishLogin(*pending) },
		}, nil
	}
	return protocol.CommandRunResult{AuthChallenge: &challenge}, nil
}

func (s *Server) bindLogin(owner *conn, runtimeName string, challenge agentruntime.AuthChallenge, mode agentruntime.LoginMode) (*agentruntime.LoginCompletion, error) {
	if challenge.Runtime != runtimeName || challenge.LoginID == "" {
		return nil, errors.New("runtime returned a challenge with mismatched identity")
	}
	if mode == agentruntime.LoginBrowser && challenge.Type != agentruntime.ChallengeBrowser ||
		mode == agentruntime.LoginDevice && challenge.Type != agentruntime.ChallengeDevice {
		return nil, errors.New("runtime returned a challenge with mismatched mode")
	}
	key := runtimeLoginKey{runtime: challenge.Runtime, loginID: challenge.LoginID}
	s.mu.Lock()
	if s.conns[owner.id] != owner {
		s.mu.Unlock()
		return nil, errors.New("login owner disconnected")
	}
	if _, exists := s.loginAttempts[key]; exists {
		s.mu.Unlock()
		return nil, errors.New("runtime reused an active login id")
	}
	if _, finished := s.finishedLogins[key]; finished {
		s.mu.Unlock()
		return nil, errors.New("runtime reused a completed login id")
	}
	s.loginAttempts[key] = runtimeLoginAttempt{owner: owner, mode: mode, ctx: owner.lifetime}
	pending, ok := s.pendingLogins[key]
	if ok {
		delete(s.pendingLogins, key)
	}
	s.mu.Unlock()
	if !ok {
		return nil, nil
	}
	return &pending, nil
}

func (s *Server) RuntimeEvent(agentruntime.Event) {}

func (s *Server) AccountUpdated(state agentruntime.AccountState) {
	s.mu.Lock()
	s.accountStates[state.Runtime] = state
	s.mu.Unlock()
	for _, client := range s.clientConns() {
		client.notify(protocol.NotifyRuntimeAccountUpdated, state)
	}
}

func (s *Server) LoginCompleted(completion agentruntime.LoginCompletion) {
	key := runtimeLoginKey{runtime: completion.Runtime, loginID: completion.LoginID}
	s.mu.Lock()
	if _, finished := s.finishedLogins[key]; finished {
		s.mu.Unlock()
		return
	}
	_, found := s.loginAttempts[key]
	if !found {
		if len(s.pendingLogins) < maxPendingLoginCompletions {
			s.pendingLogins[key] = completion
		}
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.finishLogin(completion)
}

func (s *Server) finishLogin(completion agentruntime.LoginCompletion) {
	key := runtimeLoginKey{runtime: completion.Runtime, loginID: completion.LoginID}
	s.mu.Lock()
	attempt, ok := s.loginAttempts[key]
	if ok {
		delete(s.loginAttempts, key)
		s.rememberFinishedLoginLocked(key)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	runtime, exists := s.d.Plugins.Runtime(completion.Runtime)
	if !exists {
		completion.Success = false
		completion.Error = "runtime unavailable"
	}
	if completion.Success && exists {
		account, err := runtime.Account(attempt.ctx)
		switch {
		case err != nil:
			completion.Success = false
			completion.Error = err.Error()
		case !account.Authenticated || account.AuthMode != "chatgpt":
			completion.Success = false
			completion.Error = "Codex did not report a ChatGPT account"
		default:
			s.AccountUpdated(account)
			if err := s.d.Registry.RefreshSource(attempt.ctx, completion.Runtime); err != nil {
				attempt.owner.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "warn", Text: completion.Runtime + " model refresh: " + err.Error()})
			}
		}
	}
	if !completion.Success && attempt.mode == agentruntime.LoginBrowser && exists {
		if err := runtime.CancelLogin(attempt.ctx, completion.LoginID); err != nil {
			s.retryRuntimeLoginCancellation(runtime, completion.LoginID)
			completion.Error = err.Error()
			attempt.owner.notify(protocol.NotifyRuntimeLoginCompleted, completion)
			return
		}
		challenge, err := runtime.StartLogin(attempt.ctx, agentruntime.LoginDevice)
		if err == nil {
			pending, bindErr := s.bindLogin(attempt.owner, completion.Runtime, challenge, agentruntime.LoginDevice)
			if bindErr != nil {
				s.cancelRuntimeLoginEventually(completion.Runtime, challenge.LoginID)
				return
			}
			attempt.owner.notify(protocol.NotifyRuntimeLoginChallenge, challenge)
			if pending != nil {
				s.finishLogin(*pending)
			}
			return
		}
		completion.Error = err.Error()
	}
	attempt.owner.notify(protocol.NotifyRuntimeLoginCompleted, completion)
}

func (s *Server) cancelOwnedLogins(ctx context.Context, owner *conn, runtimeName string) error {
	type cancellation struct {
		runtime string
		loginID string
	}
	var cancellations []cancellation
	s.mu.Lock()
	for key, attempt := range s.loginAttempts {
		if attempt.owner != owner || runtimeName != "" && key.runtime != runtimeName {
			continue
		}
		delete(s.loginAttempts, key)
		delete(s.pendingLogins, key)
		s.rememberFinishedLoginLocked(key)
		cancellations = append(cancellations, cancellation{runtime: key.runtime, loginID: key.loginID})
	}
	s.mu.Unlock()
	var errs []error
	for _, cancellation := range cancellations {
		runtime, ok := s.d.Plugins.Runtime(cancellation.runtime)
		if !ok {
			continue
		}
		if err := runtime.CancelLogin(ctx, cancellation.loginID); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", cancellation.runtime, err))
			s.retryRuntimeLoginCancellation(runtime, cancellation.loginID)
		}
	}
	return errors.Join(errs...)
}

func (s *Server) rememberFinishedLoginLocked(key runtimeLoginKey) {
	s.finishedLogins[key] = struct{}{}
}

func (s *Server) cancelRuntimeLoginEventually(runtimeName, loginID string) {
	runtime, ok := s.d.Plugins.Runtime(runtimeName)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, loginCancelTimeout)
	err := runtime.CancelLogin(ctx, loginID)
	cancel()
	if err != nil {
		s.retryRuntimeLoginCancellation(runtime, loginID)
	}
}

func (s *Server) retryRuntimeLoginCancellation(runtime agentruntime.Runtime, loginID string) {
	go func() {
		delay := loginCancelRetryInitial
		for {
			timer := time.NewTimer(delay)
			select {
			case <-s.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			ctx, cancel := context.WithTimeout(s.ctx, loginCancelTimeout)
			err := runtime.CancelLogin(ctx, loginID)
			cancel()
			if err == nil {
				return
			}
			delay *= 2
			if delay > loginCancelRetryMaximum {
				delay = loginCancelRetryMaximum
			}
		}
	}()
}

func (s *Server) cancelConnectionLogins(owner *conn) {
	ctx, cancel := context.WithTimeout(context.Background(), loginCancelTimeout)
	defer cancel()
	_ = s.cancelOwnedLogins(ctx, owner, "")
}

func runtimeError(runtime string, err error) *protocol.Error {
	return protocol.ErrorFrom(&agentruntime.Error{Runtime: runtime, Message: err.Error()})
}
