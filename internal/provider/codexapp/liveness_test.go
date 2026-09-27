// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/guygrigsby/rudy/internal/agentruntime"
)

type capturingLoginSink struct {
	completions []agentruntime.LoginCompletion
}

func (*capturingLoginSink) RuntimeEvent(agentruntime.Event)          {}
func (*capturingLoginSink) AccountUpdated(agentruntime.AccountState) {}
func (s *capturingLoginSink) LoginCompleted(completion agentruntime.LoginCompletion) {
	s.completions = append(s.completions, completion)
}
func (*capturingLoginSink) RequestApproval(context.Context, agentruntime.ApprovalQuestion) (agentruntime.ApprovalAnswer, error) {
	return agentruntime.ApprovalAnswer{}, nil
}

func TestMalformedCompletionFailsOnlyMatchingActiveTurn(t *testing.T) {
	client := NewClient(Command{})
	t.Cleanup(func() { _ = client.Close() })
	sink := &collectingRuntimeSink{}
	client.SetSink(sink)
	client.handleNotification(wireNotification{method: methodTurnStarted, params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"inProgress"}}`)})
	client.handleNotification(wireNotification{method: methodTurnCompleted, params: json.RawMessage(`{"threadId":"thread-2","turn":{"id":"turn-2","status":"broken"}}`)})
	client.handleNotification(wireNotification{method: methodTurnCompleted, params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"broken"}}`)})
	if len(sink.events) != 2 {
		t.Fatalf("events = %+v, want started and runtime failure", sink.events)
	}
	failed := sink.events[1]
	if failed.Type != agentruntime.EventRuntimeFailed || failed.ThreadID != "thread-1" || failed.TurnID != "turn-1" || failed.Status != string(agentruntime.TurnFailed) {
		t.Fatalf("malformed completion produced %+v, want matching failed turn", failed)
	}
	// A late valid completion must not turn the failed turn into a success.
	client.handleNotification(wireNotification{method: methodTurnCompleted, params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`)})
	if len(sink.events) != 2 {
		t.Fatalf("late completion resurrected failed turn: %+v", sink.events)
	}
}

func TestCompletionBeforeStartResponseIsNotTrackedAsActive(t *testing.T) {
	client := NewClient(Command{})
	t.Cleanup(func() { _ = client.Close() })
	sink := &collectingRuntimeSink{}
	client.SetSink(sink)
	client.handleNotification(wireNotification{method: methodTurnStarted, params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"inProgress"}}`)})
	client.handleNotification(wireNotification{method: methodTurnCompleted, params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`)})
	client.trackTurn(agentruntime.TurnRef{
		ThreadRef: agentruntime.ThreadRef{Runtime: "codex", ThreadID: "thread-1"}, TurnID: "turn-1",
	}, nil)
	client.failProcessTurns(nil)
	if len(sink.events) != 2 {
		t.Fatalf("completed-before-response turn got later failure: %+v", sink.events)
	}
}

func TestOldProcessCompletionCannotFinishReplacementLogin(t *testing.T) {
	client := NewClient(Command{})
	t.Cleanup(func() { client.process = nil; _ = client.Close() })
	sink := &capturingLoginSink{}
	client.SetSink(sink)
	oldProcess, replacement := &appProcess{}, &appProcess{}
	client.process = replacement
	client.activeLogins["login-device"] = "public-new"
	client.loginProcesses["login-device"] = replacement
	client.handleNotification(wireNotification{
		process: oldProcess, method: methodAccountLoginCompleted,
		params: json.RawMessage(`{"loginId":"login-device","success":true}`),
	})
	if len(sink.completions) != 0 {
		t.Fatalf("old process completed replacement login: %+v", sink.completions)
	}
	client.handleNotification(wireNotification{
		process: replacement, method: methodAccountLoginCompleted,
		params: json.RawMessage(`{"loginId":"login-device","success":true}`),
	})
	if len(sink.completions) != 1 || sink.completions[0].LoginID != "public-new" {
		t.Fatalf("replacement completion = %+v", sink.completions)
	}
}
