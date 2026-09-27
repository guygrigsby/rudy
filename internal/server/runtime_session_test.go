// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

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
	runtime.emit(agentruntime.Event{Type: agentruntime.EventTurnCompleted, ThreadID: "thread-1", TurnID: submitted.TurnID, Status: string(agentruntime.TurnCompleted)})
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
	startTurnErr                error
	completeBeforeStartResponse bool
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
	defer r.mu.Unlock()
	if r.thread.ThreadID == "" {
		return agentruntime.Thread{Runtime: ref.Runtime, ThreadID: ref.ThreadID}, nil
	}
	return r.thread, nil
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

var _ plugin.Plugin = codexplugin.New(&sessionRuntime{})
