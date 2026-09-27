// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/agentruntime"
	"github.com/guygrigsby/rudy/internal/plugin"
	codexplugin "github.com/guygrigsby/rudy/internal/plugins/codex"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/turn"
)

func TestRuntimeFirstSubmitFsyncsLinkBeforeTurnStart(t *testing.T) {
	runtime := &sessionRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	runtime.links = h.store.RuntimeLinks()
	client := h.dial(t, true)
	info := openRuntimeSession(t, client, h.ws, nil)
	if info.Execution != (session.Execution{Kind: session.ExecutionRuntime, Runtime: "codex"}) || info.ThreadLinked {
		t.Fatalf("open info = %+v", info)
	}
	var submitted protocol.SessionSubmitResult
	if err := client.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hello")},
	}, &submitted); err != nil {
		t.Fatal(err)
	}
	if submitted.TurnID != "turn-1" {
		t.Fatalf("turn id = %q", submitted.TurnID)
	}
	if got := fmt.Sprint(runtime.operations()); got != "[thread/start link/fsync turn/start]" {
		t.Fatalf("operations = %s", got)
	}
}

func TestRuntimeSessionRefusesExplicitTools(t *testing.T) {
	runtime := &sessionRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	client := h.dial(t, true)
	empty := []string{}
	var info protocol.SessionInfo
	err := client.Call(context.Background(), protocol.MethodSessionOpen, protocol.SessionOpenParams{
		Cwd: h.ws, Model: "codex:gpt", Tools: empty,
	}, &info)
	var protocolErr *protocol.Error
	if !errors.As(err, &protocolErr) || protocolErr.Code != protocol.CodeRefusedByInvariant {
		t.Fatalf("open error = %v, want refused_by_invariant", err)
	}
}

func TestRuntimeSessionSkipsNativeSessionOpenedHook(t *testing.T) {
	runtime := &sessionRuntime{}
	hooks := &hookRecorder{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime), hooks)
	runtime.links = h.store.RuntimeLinks()
	client := h.dial(t, true)
	info := openRuntimeSession(t, client, h.ws, nil)
	var submitted protocol.SessionSubmitResult
	if err := client.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hello")},
	}, &submitted); err != nil {
		t.Fatal(err)
	}
	opened, _ := hooks.counts()
	if len(opened) != 0 {
		t.Fatalf("runtime session fired native session_opened hooks: %+v", opened)
	}
}

func TestRuntimeLiveCompletionPublishesProjectedEntry(t *testing.T) {
	runtime := &sessionRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	runtime.links = h.store.RuntimeLinks()
	client := h.dial(t, true)
	info := openRuntimeSession(t, client, h.ws, nil)
	discardNotifications(client)
	var submitted protocol.SessionSubmitResult
	if err := client.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hello")},
	}, &submitted); err != nil {
		t.Fatal(err)
	}
	runtime.emit(agentruntime.Event{
		Type: agentruntime.EventItemCompleted, ThreadID: "thread-1", TurnID: submitted.TurnID,
		Item: agentruntime.Item{ItemID: "item-1", Type: agentruntime.ItemAgentMessage, Content: []session.Block{session.TextBlock("answer")}},
	})
	note := waitNotification(t, client, protocol.NotifyRuntimeEntry)
	var got protocol.RuntimeEntryParams
	if err := json.Unmarshal(note.Params, &got); err != nil {
		t.Fatal(err)
	}
	if got.SessionID != info.SessionID || got.Entry.ThreadID != "thread-1" || got.Entry.ItemID != "item-1" {
		t.Fatalf("runtime entry = %+v", got)
	}
}

func TestRuntimeUsageUpdateReachesAttachedClients(t *testing.T) {
	runtime := &sessionRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	runtime.links = h.store.RuntimeLinks()
	client := h.dial(t, true)
	info := openRuntimeSession(t, client, h.ws, nil)
	discardNotifications(client)
	var submitted protocol.SessionSubmitResult
	if err := client.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hello")},
	}, &submitted); err != nil {
		t.Fatal(err)
	}
	want := session.Usage{Input: 10, Output: 2, CacheRead: 3}
	runtime.emit(agentruntime.Event{Type: agentruntime.EventUsageUpdated, ThreadID: "thread-1", TurnID: submitted.TurnID, Usage: want})
	note := waitNotification(t, client, protocol.NotifyRuntimeUsageUpdated)
	var got protocol.RuntimeUsageUpdated
	if err := json.Unmarshal(note.Params, &got); err != nil {
		t.Fatal(err)
	}
	if got.SessionID != info.SessionID || got.TurnID != submitted.TurnID || got.Usage != want {
		t.Fatalf("runtime usage = %+v", got)
	}
}

func TestRuntimeEventRejectsUnboundThreadAndTurn(t *testing.T) {
	runtime := &sessionRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	runtime.links = h.store.RuntimeLinks()
	client := h.dial(t, true)
	info := openRuntimeSession(t, client, h.ws, nil)
	var submitted protocol.SessionSubmitResult
	if err := client.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hello")},
	}, &submitted); err != nil {
		t.Fatal(err)
	}
	discardNotifications(client)
	item := agentruntime.Item{ItemID: "item-1", Type: agentruntime.ItemAgentMessage, Content: []session.Block{session.TextBlock("wrong")}}
	runtime.emit(agentruntime.Event{Type: agentruntime.EventItemCompleted, ThreadID: "other-thread", TurnID: submitted.TurnID, Item: item})
	runtime.emit(agentruntime.Event{Type: agentruntime.EventItemCompleted, ThreadID: "thread-1", TurnID: "other-turn", Item: item})
	assertNoNotification(t, client, protocol.NotifyRuntimeEntry)
}

func TestRuntimeResumeRefreshesCanonicalProjection(t *testing.T) {
	runtime := &sessionRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	runtime.links = h.store.RuntimeLinks()
	owner := h.dial(t, true)
	info := openRuntimeSession(t, owner, h.ws, nil)
	var submitted protocol.SessionSubmitResult
	if err := owner.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hello")},
	}, &submitted); err != nil {
		t.Fatal(err)
	}
	runtime.emit(agentruntime.Event{Type: agentruntime.EventTurnCompleted, ThreadID: "thread-1", TurnID: submitted.TurnID, Status: string(agentruntime.TurnCompleted)})
	runtime.setThread(agentruntime.Thread{
		Runtime: "codex", ThreadID: "thread-1", Turns: []agentruntime.Turn{{
			TurnID: submitted.TurnID, Status: agentruntime.TurnCompleted,
			Items: []agentruntime.Item{{ItemID: "item-1", Type: agentruntime.ItemAgentMessage, Content: []session.Block{session.TextBlock("answer")}}},
		}},
	})
	viewer := h.dial(t, true)
	var resumed protocol.SessionInfo
	if err := viewer.Call(context.Background(), protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: info.SessionID}, &resumed); err != nil {
		t.Fatal(err)
	}
	if !resumed.ThreadLinked {
		t.Fatal("resume did not report linked runtime thread")
	}
	note := waitNotification(t, viewer, protocol.NotifyRuntimeEntry)
	var got protocol.RuntimeEntryParams
	if err := json.Unmarshal(note.Params, &got); err != nil {
		t.Fatal(err)
	}
	if got.Entry.ItemID != "item-1" {
		t.Fatalf("runtime entry = %+v", got.Entry)
	}
	if operations := fmt.Sprint(runtime.operations()); !strings.Contains(operations, "thread/resume thread/read") {
		t.Fatalf("operations = %s", operations)
	}
}

func TestRuntimeResumeCannotResurrectCompletedTurnFromStaleRead(t *testing.T) {
	runtime := &sessionRuntime{readStarted: make(chan struct{}, 1), readRelease: make(chan struct{})}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	runtime.links = h.store.RuntimeLinks()
	owner := h.dial(t, true)
	info := openRuntimeSession(t, owner, h.ws, nil)
	var submitted protocol.SessionSubmitResult
	if err := owner.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hello")},
	}, &submitted); err != nil {
		t.Fatal(err)
	}
	runtime.setThread(agentruntime.Thread{Runtime: "codex", ThreadID: "thread-1", Turns: []agentruntime.Turn{{
		TurnID: submitted.TurnID, Status: agentruntime.TurnRunning,
	}}})
	viewer := h.dial(t, true)
	resumeDone := make(chan error, 1)
	go func() {
		var resumed protocol.SessionInfo
		resumeDone <- viewer.Call(context.Background(), protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: info.SessionID}, &resumed)
	}()
	<-runtime.readStarted
	runtime.emit(agentruntime.Event{Type: agentruntime.EventTurnCompleted, ThreadID: "thread-1", TurnID: submitted.TurnID, Status: string(agentruntime.TurnCompleted)})
	close(runtime.readRelease)
	if err := <-resumeDone; err != nil {
		t.Fatal(err)
	}
	var next protocol.SessionSubmitResult
	if err := owner.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Source: session.SourceTyped, Content: []session.Block{session.TextBlock("again")},
	}, &next); err != nil {
		t.Fatalf("submit after stale refresh: %v", err)
	}
}

func TestRuntimeInterruptUsesRuntimeAndWaitsForTerminalEvent(t *testing.T) {
	runtime := &sessionRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	runtime.links = h.store.RuntimeLinks()
	client := h.dial(t, true)
	info := openRuntimeSession(t, client, h.ws, nil)
	var submitted protocol.SessionSubmitResult
	if err := client.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hello")},
	}, &submitted); err != nil {
		t.Fatal(err)
	}
	var interrupted struct {
		TurnID string `json:"turn_id"`
		State  string `json:"state"`
	}
	if err := client.Call(context.Background(), protocol.MethodSessionInterrupt, protocol.SessionInterruptParams{
		SessionID: info.SessionID, How: session.InterruptCancel,
	}, &interrupted); err != nil {
		t.Fatal(err)
	}
	if interrupted.TurnID != submitted.TurnID || interrupted.State != string(turn.Streaming) {
		t.Fatalf("interrupt = %+v", interrupted)
	}
	if operations := fmt.Sprint(runtime.operations()); !strings.Contains(operations, "turn/interrupt") {
		t.Fatalf("operations = %s", operations)
	}
}

func TestInterruptedRuntimeTurnReturnsToIdle(t *testing.T) {
	runtime := &sessionRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	runtime.links = h.store.RuntimeLinks()
	client := h.dial(t, true)
	info := openRuntimeSession(t, client, h.ws, nil)
	var submitted protocol.SessionSubmitResult
	if err := client.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hello")},
	}, &submitted); err != nil {
		t.Fatal(err)
	}
	waitNotification(t, client, protocol.NotifyTurnState)
	runtime.emit(agentruntime.Event{Type: agentruntime.EventTurnCompleted, ThreadID: "thread-1", TurnID: submitted.TurnID, Status: string(agentruntime.TurnInterrupted)})
	note := waitNotification(t, client, protocol.NotifyTurnState)
	var got protocol.TurnStateChanged
	if err := json.Unmarshal(note.Params, &got); err != nil {
		t.Fatal(err)
	}
	if got.State != string(turn.Idle) {
		t.Fatalf("interrupted runtime state = %q, want idle", got.State)
	}
}

func TestRuntimeSessionRefusesNativeOnlyOperations(t *testing.T) {
	runtime := &sessionRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	client := h.dial(t, true)
	info := openRuntimeSession(t, client, h.ws, nil)
	tests := []struct {
		method string
		params any
	}{
		{protocol.MethodSessionCompact, protocol.SessionCompactParams{SessionID: info.SessionID}},
		{protocol.MethodSessionShell, protocol.SessionShellParams{SessionID: info.SessionID, Command: "pwd"}},
		{protocol.MethodSessionSetModel, protocol.SessionSetModelParams{SessionID: info.SessionID, Model: "fake:m1"}},
	}
	for _, test := range tests {
		var result any
		err := client.Call(context.Background(), test.method, test.params, &result)
		var protocolErr *protocol.Error
		if !errors.As(err, &protocolErr) || protocolErr.Code != protocol.CodeRefusedByInvariant {
			t.Fatalf("%s error = %v, want refused_by_invariant", test.method, err)
		}
	}
}

func TestRuntimeForkBindsDistinctThreadAtLatestProjection(t *testing.T) {
	runtime := &sessionRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	runtime.links = h.store.RuntimeLinks()
	client := h.dial(t, true)
	info := openRuntimeSession(t, client, h.ws, nil)
	var submitted protocol.SessionSubmitResult
	if err := client.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hello")},
	}, &submitted); err != nil {
		t.Fatal(err)
	}
	runtime.emit(agentruntime.Event{
		Type: agentruntime.EventItemCompleted, ThreadID: "thread-1", TurnID: submitted.TurnID,
		Item: agentruntime.Item{ItemID: "item-1", Type: agentruntime.ItemAgentMessage, Content: []session.Block{session.TextBlock("answer")}},
	})
	waitNotification(t, client, protocol.NotifyRuntimeEntry)
	runtime.emit(agentruntime.Event{Type: agentruntime.EventTurnCompleted, ThreadID: "thread-1", TurnID: submitted.TurnID, Status: string(agentruntime.TurnCompleted)})
	waitNotification(t, client, protocol.NotifyTurnState)
	runtime.setThread(agentruntime.Thread{Runtime: "codex", ThreadID: "thread-fork", Turns: []agentruntime.Turn{{
		TurnID: submitted.TurnID, Status: agentruntime.TurnCompleted,
		Items: []agentruntime.Item{{ItemID: "item-1", Type: agentruntime.ItemAgentMessage, Content: []session.Block{session.TextBlock("answer")}}},
	}}})
	var forked protocol.SessionInfo
	if err := client.Call(context.Background(), protocol.MethodSessionFork, protocol.SessionForkParams{SessionID: info.SessionID}, &forked); err != nil {
		t.Fatal(err)
	}
	childID, err := ulid.Parse(forked.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	link, err := h.store.RuntimeLinks().Read(childID)
	if err != nil {
		t.Fatal(err)
	}
	if link != (session.RuntimeLink{Runtime: "codex", ThreadID: "thread-fork"}) || !forked.ThreadLinked {
		t.Fatalf("fork binding = %+v, info = %+v", link, forked)
	}
	note := waitNotification(t, client, protocol.NotifyRuntimeEntry)
	var projected protocol.RuntimeEntryParams
	if err := json.Unmarshal(note.Params, &projected); err != nil {
		t.Fatal(err)
	}
	if projected.SessionID != forked.SessionID || projected.Entry.ItemID != "item-1" {
		t.Fatalf("fork projection = %+v", projected)
	}
}

func TestRuntimeForkFailureRemovesCreatedChild(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, *sessionRuntime, *session.Store)
	}{
		{
			name: "read thread",
			prepare: func(_ *testing.T, runtime *sessionRuntime, _ *session.Store) {
				runtime.readErr = errors.New("read failed")
			},
		},
		{
			name: "write runtime link",
			prepare: func(t *testing.T, _ *sessionRuntime, store *session.Store) {
				t.Helper()
				if err := store.RuntimeLinks().Write(session.NewID(), session.RuntimeLink{
					Runtime: "codex", ThreadID: "thread-fork",
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := &sessionRuntime{}
			h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
			runtime.links = h.store.RuntimeLinks()
			client := h.dial(t, true)
			info := openRuntimeSession(t, client, h.ws, nil)
			var submitted protocol.SessionSubmitResult
			if err := client.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
				SessionID: info.SessionID, Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hello")},
			}, &submitted); err != nil {
				t.Fatal(err)
			}
			runtime.emit(agentruntime.Event{
				Type: agentruntime.EventItemCompleted, ThreadID: "thread-1", TurnID: submitted.TurnID,
				Item: agentruntime.Item{ItemID: "item-1", Type: agentruntime.ItemAgentMessage, Content: []session.Block{session.TextBlock("answer")}},
			})
			waitNotification(t, client, protocol.NotifyRuntimeEntry)
			runtime.emit(agentruntime.Event{
				Type: agentruntime.EventTurnCompleted, ThreadID: "thread-1", TurnID: submitted.TurnID,
				Status: string(agentruntime.TurnCompleted),
			})
			waitNotification(t, client, protocol.NotifyTurnState)
			runtime.setThread(agentruntime.Thread{Runtime: "codex", ThreadID: "thread-fork"})
			test.prepare(t, runtime, h.store)

			var forked protocol.SessionInfo
			err := client.Call(context.Background(), protocol.MethodSessionFork, protocol.SessionForkParams{
				SessionID: info.SessionID,
			}, &forked)
			if err == nil {
				t.Fatal("fork succeeded, want error")
			}
			childID := runtime.lastReadSession()
			if childID.Compare(ulid.ULID{}) == 0 {
				t.Fatal("runtime did not receive the created child id")
			}
			listed, listErr := h.store.List()
			if listErr != nil {
				t.Fatal(listErr)
			}
			if len(listed) != 1 || listed[0].ID.String() != info.SessionID {
				t.Fatalf("sessions after failed fork = %+v, want only parent %s", listed, info.SessionID)
			}
			if _, statErr := os.Stat(h.store.Dir(childID)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("created child directory remains after failed fork: %v", statErr)
			}
			if child, loadErr := session.Load(h.store, childID); loadErr == nil {
				_ = child.Close()
				t.Fatal("created child remains resumable after failed fork")
			}
		})
	}
}

func TestRuntimeAmbiguousTurnRequiresReadReconciliation(t *testing.T) {
	runtime := &sessionRuntime{startTurnErr: agentruntime.ErrAmbiguous}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	runtime.links = h.store.RuntimeLinks()
	client := h.dial(t, true)
	info := openRuntimeSession(t, client, h.ws, nil)
	submit := func() error {
		var result protocol.SessionSubmitResult
		return client.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
			SessionID: info.SessionID, Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hello")},
		}, &result)
	}
	for range 2 {
		err := submit()
		var protocolErr *protocol.Error
		if !errors.As(err, &protocolErr) || protocolErr.Code != protocol.CodeAmbiguous {
			t.Fatalf("submit error = %v, want ambiguous", err)
		}
	}
	runtime.setStartTurnError(nil)
	var resumed protocol.SessionInfo
	if err := client.Call(context.Background(), protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: info.SessionID}, &resumed); err != nil {
		t.Fatal(err)
	}
	if err := submit(); err != nil {
		t.Fatalf("submit after reconciliation: %v", err)
	}
}

func TestRuntimeProcessLossRequiresReadReconciliation(t *testing.T) {
	runtime := &sessionRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	runtime.links = h.store.RuntimeLinks()
	client := h.dial(t, true)
	info := openRuntimeSession(t, client, h.ws, nil)
	submit := func() error {
		var result protocol.SessionSubmitResult
		return client.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
			SessionID: info.SessionID, Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hello")},
		}, &result)
	}
	if err := submit(); err != nil {
		t.Fatal(err)
	}
	runtime.emit(agentruntime.Event{
		Type: agentruntime.EventRuntimeFailed, ThreadID: "thread-1", TurnID: "turn-1",
	})
	err := submit()
	var protocolErr *protocol.Error
	if !errors.As(err, &protocolErr) || protocolErr.Code != protocol.CodeAmbiguous {
		t.Fatalf("submit after process loss = %v, want ambiguous", err)
	}
	var resumed protocol.SessionInfo
	if err := client.Call(context.Background(), protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: info.SessionID}, &resumed); err != nil {
		t.Fatal(err)
	}
	if err := submit(); err != nil {
		t.Fatalf("submit after canonical refresh: %v", err)
	}
}

func TestRuntimeCompletionBeforeTurnStartResponseDoesNotResurrectTurn(t *testing.T) {
	runtime := &sessionRuntime{completeBeforeStartResponse: true}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	runtime.links = h.store.RuntimeLinks()
	client := h.dial(t, true)
	info := openRuntimeSession(t, client, h.ws, nil)
	submit := func() error {
		var result protocol.SessionSubmitResult
		return client.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
			SessionID: info.SessionID, Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hello")},
		}, &result)
	}
	if err := submit(); err != nil {
		t.Fatal(err)
	}
	if err := submit(); err != nil {
		t.Fatalf("second submit after early completion: %v", err)
	}
}

func TestRuntimeApprovalAllowIsRecordedBeforeRuntimeReturns(t *testing.T) {
	runtime := &sessionRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	runtime.links = h.store.RuntimeLinks()
	client := h.dial(t, true)
	info := openRuntimeSession(t, client, h.ws, nil)
	var submitted protocol.SessionSubmitResult
	if err := client.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hello")},
	}, &submitted); err != nil {
		t.Fatal(err)
	}
	runtime.emit(agentruntime.Event{
		Type: agentruntime.EventItemStarted, ThreadID: "thread-1", TurnID: submitted.TurnID, ItemID: "item-1",
		Item: agentruntime.Item{ItemID: "item-1", Type: agentruntime.ItemCommand},
	})
	discardNotifications(client)
	answer := make(chan agentruntime.ApprovalAnswer, 1)
	go func() {
		answer <- runtime.ask(context.Background(), agentruntime.ApprovalQuestion{
			RequestID: "request-1", ThreadID: "thread-1", TurnID: submitted.TurnID, ItemID: "item-1",
			Kind: agentruntime.ApprovalCommand, Command: "pwd", AllowedScopes: []agentruntime.ApprovalScope{agentruntime.ScopeOnce},
		})
	}()
	note := waitNotification(t, client, protocol.NotifyRuntimePermissionRequested)
	var requested protocol.RuntimePermissionRequested
	if err := json.Unmarshal(note.Params, &requested); err != nil {
		t.Fatal(err)
	}
	if requested.SessionID != info.SessionID || requested.RequestID != "request-1" {
		t.Fatalf("runtime approval request = %+v", requested)
	}
	var result struct{}
	if err := client.Call(context.Background(), protocol.MethodRuntimeApprovalAnswer, protocol.RuntimeApprovalAnswerParams{
		SessionID: info.SessionID, TurnID: submitted.TurnID, RequestID: "request-1",
		Decision: agentruntime.DecisionAllow, Scope: agentruntime.ScopeOnce, Reason: "approved",
	}, &result); err != nil {
		t.Fatal(err)
	}
	got := <-answer
	if got.Decision != agentruntime.DecisionAllow || got.Scope != agentruntime.ScopeOnce {
		t.Fatalf("runtime answer = %+v", got)
	}
	entryNote := waitNotification(t, client, protocol.NotifyEntryAppended)
	var appended protocol.EntryAppended
	if err := json.Unmarshal(entryNote.Params, &appended); err != nil {
		t.Fatal(err)
	}
	decision, ok := appended.Entry.Payload.(session.RuntimePermissionDecision)
	if !ok || decision.RequestID != "request-1" || decision.Decision != session.Allow {
		t.Fatalf("runtime decision = %+v", appended.Entry.Payload)
	}
	runtime.emit(agentruntime.Event{Type: agentruntime.EventRequestResolved, ThreadID: "thread-1", RequestID: "request-1"})
	resolved := waitNotification(t, client, protocol.NotifyRuntimePermissionResolved)
	var resolution protocol.RuntimePermissionResolved
	if err := json.Unmarshal(resolved.Params, &resolution); err != nil {
		t.Fatal(err)
	}
	if resolution.SessionID != info.SessionID || resolution.RequestID != "request-1" {
		t.Fatalf("runtime approval resolution = %+v", resolution)
	}
}

func TestStaleRuntimeApprovalDeniesAndRecordsReason(t *testing.T) {
	runtime := &sessionRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	runtime.links = h.store.RuntimeLinks()
	client := h.dial(t, true)
	info := openRuntimeSession(t, client, h.ws, nil)
	var submitted protocol.SessionSubmitResult
	if err := client.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hello")},
	}, &submitted); err != nil {
		t.Fatal(err)
	}
	discardNotifications(client)
	got := runtime.ask(context.Background(), agentruntime.ApprovalQuestion{
		RequestID: "stale-request", ThreadID: "thread-1", TurnID: "stale-turn", ItemID: "item-1", Kind: agentruntime.ApprovalCommand,
		AllowedScopes: []agentruntime.ApprovalScope{agentruntime.ScopeOnce},
	})
	if got.Decision != agentruntime.DecisionDeny {
		t.Fatalf("stale answer = %+v", got)
	}
	note := waitNotification(t, client, protocol.NotifyEntryAppended)
	var appended protocol.EntryAppended
	if err := json.Unmarshal(note.Params, &appended); err != nil {
		t.Fatal(err)
	}
	decision, ok := appended.Entry.Payload.(session.RuntimePermissionDecision)
	if !ok || decision.DecidedBy != session.RuntimeByStale || decision.Decision != session.Deny {
		t.Fatalf("stale decision = %+v", appended.Entry.Payload)
	}
	assertNoNotification(t, client, protocol.NotifyRuntimePermissionRequested)
}

func TestRuntimeApprovalWithoutAskerFailsClosedAndRecordsReason(t *testing.T) {
	runtime := &sessionRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	runtime.links = h.store.RuntimeLinks()
	client := h.dial(t, false)
	info := openRuntimeSession(t, client, h.ws, nil)
	var submitted protocol.SessionSubmitResult
	if err := client.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hello")},
	}, &submitted); err != nil {
		t.Fatal(err)
	}
	runtime.emit(agentruntime.Event{
		Type: agentruntime.EventItemStarted, ThreadID: "thread-1", TurnID: submitted.TurnID, ItemID: "item-1",
		Item: agentruntime.Item{ItemID: "item-1", Type: agentruntime.ItemCommand},
	})
	discardNotifications(client)
	got := runtime.ask(context.Background(), agentruntime.ApprovalQuestion{
		RequestID: "no-asker", ThreadID: "thread-1", TurnID: submitted.TurnID, ItemID: "item-1", Kind: agentruntime.ApprovalCommand,
		AllowedScopes: []agentruntime.ApprovalScope{agentruntime.ScopeOnce},
	})
	if got.Decision != agentruntime.DecisionDeny {
		t.Fatalf("no-asker answer = %+v", got)
	}
	note := waitNotification(t, client, protocol.NotifyEntryAppended)
	var appended protocol.EntryAppended
	if err := json.Unmarshal(note.Params, &appended); err != nil {
		t.Fatal(err)
	}
	decision, ok := appended.Entry.Payload.(session.RuntimePermissionDecision)
	if !ok || decision.DecidedBy != session.RuntimeByNoAsker || decision.Decision != session.Deny {
		t.Fatalf("no-asker decision = %+v", appended.Entry.Payload)
	}
	assertNoNotification(t, client, protocol.NotifyRuntimePermissionRequested)
}

func TestRuntimeApprovalDeniesWhenLastAskerDisconnects(t *testing.T) {
	runtime := &sessionRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	runtime.links = h.store.RuntimeLinks()
	client := h.dial(t, true)
	info := openRuntimeSession(t, client, h.ws, nil)
	var submitted protocol.SessionSubmitResult
	if err := client.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hello")},
	}, &submitted); err != nil {
		t.Fatal(err)
	}
	runtime.emit(agentruntime.Event{
		Type: agentruntime.EventItemStarted, ThreadID: "thread-1", TurnID: submitted.TurnID, ItemID: "item-1",
		Item: agentruntime.Item{ItemID: "item-1", Type: agentruntime.ItemCommand},
	})
	discardNotifications(client)
	answer := make(chan agentruntime.ApprovalAnswer, 1)
	go func() {
		answer <- runtime.ask(context.Background(), agentruntime.ApprovalQuestion{
			RequestID: "disconnect", ThreadID: "thread-1", TurnID: submitted.TurnID, ItemID: "item-1", Kind: agentruntime.ApprovalCommand,
			AllowedScopes: []agentruntime.ApprovalScope{agentruntime.ScopeOnce},
		})
	}()
	waitNotification(t, client, protocol.NotifyRuntimePermissionRequested)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-answer:
		if got.Decision != agentruntime.DecisionDeny {
			t.Fatalf("disconnect answer = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime approval remained blocked after last asker disconnected")
	}
}

func TestCanceledRuntimeApprovalIsNotPresented(t *testing.T) {
	runtime := &sessionRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	runtime.links = h.store.RuntimeLinks()
	client := h.dial(t, true)
	info := openRuntimeSession(t, client, h.ws, nil)
	var submitted protocol.SessionSubmitResult
	if err := client.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hello")},
	}, &submitted); err != nil {
		t.Fatal(err)
	}
	runtime.emit(agentruntime.Event{
		Type: agentruntime.EventItemStarted, ThreadID: "thread-1", TurnID: submitted.TurnID, ItemID: "item-1",
		Item: agentruntime.Item{ItemID: "item-1", Type: agentruntime.ItemCommand},
	})
	discardNotifications(client)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := runtime.ask(ctx, agentruntime.ApprovalQuestion{
		RequestID: "canceled", ThreadID: "thread-1", TurnID: submitted.TurnID, ItemID: "item-1", Kind: agentruntime.ApprovalCommand,
		AllowedScopes: []agentruntime.ApprovalScope{agentruntime.ScopeOnce},
	})
	if got.Decision != agentruntime.DecisionDeny {
		t.Fatalf("canceled answer = %+v", got)
	}
	assertNoNotification(t, client, protocol.NotifyRuntimePermissionRequested)
}

func TestRuntimeThreadCannotBindTwoLiveSessions(t *testing.T) {
	runtime := &sessionRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	runtime.links = h.store.RuntimeLinks()
	client := h.dial(t, true)
	first := openRuntimeSession(t, client, h.ws, nil)
	second := openRuntimeSession(t, client, h.ws, nil)
	for _, sessionID := range []string{first.SessionID, second.SessionID} {
		var submitted protocol.SessionSubmitResult
		err := client.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
			SessionID: sessionID, Source: session.SourceTyped, Content: []session.Block{session.TextBlock("hello")},
		}, &submitted)
		if sessionID == first.SessionID && err != nil {
			t.Fatalf("first submit: %v", err)
		}
		if sessionID == second.SessionID {
			var got *protocol.Error
			if !errors.As(err, &got) || got.Code != protocol.CodeConflict {
				t.Fatalf("second submit error = %v, want conflict", err)
			}
		}
	}
}

func TestColdRuntimeThreadCollisionDoesNotStealLiveBinding(t *testing.T) {
	runtime := &sessionRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	client := h.dial(t, true)
	first := openRuntimeSession(t, client, h.ws, nil)
	second := openRuntimeSession(t, client, h.ws, nil)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	firstID, err := ulid.Parse(first.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := ulid.Parse(second.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	links := h.store.RuntimeLinks()
	body := []byte("runtime = \"codex\"\nthread_id = \"shared-thread\"\n")
	for _, id := range []ulid.ULID{firstID, secondID} {
		if err := os.WriteFile(links.Path(id), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root := filepath.Dir(filepath.Dir(links.Path(firstID)))
	restarted := &sessionRuntime{}
	srv, store := newServerAt(t, testConfig(), root, "", &fakePlugin{prov: &scriptProvider{}}, codexplugin.New(restarted))
	restarted.links = store.RuntimeLinks()
	h2 := &harness{srv: srv, ws: h.ws, store: store}
	resumer := h2.dial(t, true)
	var resumed protocol.SessionInfo
	if err := resumer.Call(context.Background(), protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: first.SessionID}, &resumed); err != nil {
		t.Fatalf("resume first: %v", err)
	}
	err = resumer.Call(context.Background(), protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: second.SessionID}, &resumed)
	var got *protocol.Error
	if !errors.As(err, &got) || got.Code != protocol.CodeConflict {
		t.Fatalf("resume second error = %v, want conflict", err)
	}
}

func openRuntimeSession(t *testing.T, client *protocol.Client, cwd string, tools []string) protocol.SessionInfo {
	t.Helper()
	var info protocol.SessionInfo
	if err := client.Call(context.Background(), protocol.MethodSessionOpen, protocol.SessionOpenParams{
		Cwd: cwd, Model: "codex:gpt", Tools: tools,
	}, &info); err != nil {
		t.Fatal(err)
	}
	return info
}

type sessionRuntime struct {
	mu                          sync.Mutex
	sink                        agentruntime.Sink
	ops                         []string
	links                       *session.RuntimeLinkStore
	thread                      agentruntime.Thread
	readErr                     error
	lastReadSessionID           ulid.ULID
	startTurnErr                error
	completeBeforeStartResponse bool
	readStarted                 chan struct{}
	readRelease                 chan struct{}
}

func (*sessionRuntime) Name() string { return "codex" }
func (*sessionRuntime) ListModels(context.Context) ([]provider.Model, error) {
	return []provider.Model{{
		Ref: session.ModelRef{Provider: "codex", Model: "gpt"}, OwnerKind: provider.OwnerRuntime,
		DisplayName: "GPT", Capabilities: provider.Capabilities{Tools: true},
	}}, nil
}
func (*sessionRuntime) Account(context.Context) (agentruntime.AccountState, error) {
	return agentruntime.AccountState{Runtime: "codex", Authenticated: true, AuthMode: "chatgpt"}, nil
}
func (*sessionRuntime) StartLogin(context.Context, agentruntime.LoginMode) (agentruntime.AuthChallenge, error) {
	return agentruntime.AuthChallenge{}, errors.New("unused")
}
func (*sessionRuntime) CancelLogin(context.Context, string) error { return errors.New("unused") }
func (r *sessionRuntime) StartThread(_ context.Context, request agentruntime.StartThreadRequest) (agentruntime.ThreadRef, error) {
	r.record("thread/start")
	return agentruntime.ThreadRef{Runtime: "codex", SessionID: request.SessionID, ThreadID: "thread-1"}, nil
}
func (r *sessionRuntime) ResumeThread(context.Context, agentruntime.ThreadRef) error {
	r.record("thread/resume")
	return nil
}
func (*sessionRuntime) ForkThread(_ context.Context, ref agentruntime.ThreadRef) (agentruntime.ThreadRef, error) {
	// Recorded by callers that need to assert runtime ownership of the fork.
	ref.ThreadID = "thread-fork"
	return ref, nil
}
func (r *sessionRuntime) ReadThread(_ context.Context, ref agentruntime.ThreadRef) (agentruntime.Thread, error) {
	r.record("thread/read")
	r.mu.Lock()
	thread := r.thread
	err := r.readErr
	r.lastReadSessionID = ref.SessionID
	started, release := r.readStarted, r.readRelease
	r.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		<-release
	}
	if err != nil {
		return agentruntime.Thread{}, err
	}
	if thread.ThreadID == "" {
		return agentruntime.Thread{Runtime: ref.Runtime, ThreadID: ref.ThreadID}, nil
	}
	return thread, nil
}

func (r *sessionRuntime) lastReadSession() ulid.ULID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastReadSessionID
}
func (r *sessionRuntime) StartTurn(_ context.Context, request agentruntime.StartTurnRequest) (agentruntime.TurnRef, error) {
	if r.links == nil {
		return agentruntime.TurnRef{}, errors.New("link store unavailable")
	}
	link, err := r.links.Read(request.Thread.SessionID)
	if err != nil || link.Runtime != "codex" || link.ThreadID != request.Thread.ThreadID {
		return agentruntime.TurnRef{}, fmt.Errorf("link not durable before turn/start: %w", err)
	}
	r.record("link/fsync")
	r.record("turn/start")
	r.mu.Lock()
	err = r.startTurnErr
	completeBeforeResponse := r.completeBeforeStartResponse
	r.completeBeforeStartResponse = false
	r.mu.Unlock()
	if err != nil {
		return agentruntime.TurnRef{}, err
	}
	if completeBeforeResponse {
		r.emit(agentruntime.Event{Type: agentruntime.EventTurnStarted, ThreadID: request.Thread.ThreadID, TurnID: "turn-1"})
		r.emit(agentruntime.Event{Type: agentruntime.EventTurnCompleted, ThreadID: request.Thread.ThreadID, TurnID: "turn-1", Status: string(agentruntime.TurnCompleted)})
	}
	return agentruntime.TurnRef{ThreadRef: request.Thread, TurnID: "turn-1"}, nil
}
func (*sessionRuntime) SteerTurn(_ context.Context, request agentruntime.SteerTurnRequest) (agentruntime.TurnRef, error) {
	return request.Turn, nil
}
func (r *sessionRuntime) InterruptTurn(context.Context, agentruntime.TurnRef) error {
	r.record("turn/interrupt")
	return nil
}
func (r *sessionRuntime) SetSink(sink agentruntime.Sink) {
	r.mu.Lock()
	r.sink = sink
	r.mu.Unlock()
}
func (r *sessionRuntime) record(operation string) {
	r.mu.Lock()
	r.ops = append(r.ops, operation)
	r.mu.Unlock()
}
func (r *sessionRuntime) operations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ops...)
}

func (r *sessionRuntime) emit(event agentruntime.Event) {
	r.mu.Lock()
	sink := r.sink
	r.mu.Unlock()
	sink.RuntimeEvent(event)
}

func (r *sessionRuntime) setThread(thread agentruntime.Thread) {
	r.mu.Lock()
	r.thread = thread
	r.mu.Unlock()
}

func (r *sessionRuntime) setStartTurnError(err error) {
	r.mu.Lock()
	r.startTurnErr = err
	r.mu.Unlock()
}

func (r *sessionRuntime) ask(ctx context.Context, question agentruntime.ApprovalQuestion) agentruntime.ApprovalAnswer {
	r.mu.Lock()
	sink := r.sink
	r.mu.Unlock()
	answer, _ := sink.RequestApproval(ctx, question)
	return answer
}

var _ plugin.Plugin = codexplugin.New(&sessionRuntime{})
