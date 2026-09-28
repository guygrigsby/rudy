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
	"time"

	"github.com/guygrigsby/rudy/internal/agentruntime"
	"github.com/guygrigsby/rudy/internal/plugin"
	codexplugin "github.com/guygrigsby/rudy/internal/plugins/codex"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

func TestLoginChallengeAndCompletionStayOnInvokingConnection(t *testing.T) {
	runtime := &loginRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	owner := h.dial(t, true)
	observer := h.dial(t, true)
	info := h.open(t, owner)
	var resumed protocol.SessionInfo
	if err := observer.Call(context.Background(), protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: info.SessionID}, &resumed); err != nil {
		t.Fatal(err)
	}
	discardNotifications(owner)
	discardNotifications(observer)

	var result protocol.CommandRunResult
	if err := owner.Call(context.Background(), protocol.MethodCommandRun, protocol.CommandRunParams{
		SessionID: info.SessionID, Name: "login", Args: "device",
	}, &result); err != nil {
		t.Fatal(err)
	}
	if result.AuthChallenge == nil || result.AuthChallenge.LoginID != "device-1" {
		t.Fatalf("challenge = %+v", result.AuthChallenge)
	}
	assertNoNotification(t, observer, protocol.NotifyRuntimeLoginChallenge)

	runtime.complete("device-1", true)
	note := waitNotification(t, owner, protocol.NotifyRuntimeLoginCompleted)
	var completion agentruntime.LoginCompletion
	if err := json.Unmarshal(note.Params, &completion); err != nil {
		t.Fatal(err)
	}
	if completion.LoginID != "device-1" || !completion.Success {
		t.Fatalf("completion = %+v", completion)
	}
	assertNoNotification(t, observer, protocol.NotifyRuntimeLoginCompleted)
}

func TestFailedBrowserLoginCancelsBeforeDeviceFallback(t *testing.T) {
	runtime := &loginRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	owner := h.dial(t, true)
	info := h.open(t, owner)
	discardNotifications(owner)

	var result protocol.CommandRunResult
	if err := owner.Call(context.Background(), protocol.MethodCommandRun, protocol.CommandRunParams{
		SessionID: info.SessionID, Name: "login", Args: "browser",
	}, &result); err != nil {
		t.Fatal(err)
	}
	runtime.complete("browser-1", false)
	note := waitNotification(t, owner, protocol.NotifyRuntimeLoginChallenge)
	var challenge agentruntime.AuthChallenge
	if err := json.Unmarshal(note.Params, &challenge); err != nil {
		t.Fatal(err)
	}
	if challenge.Type != agentruntime.ChallengeDevice || challenge.LoginID != "device-2" {
		t.Fatalf("fallback challenge = %+v", challenge)
	}
	if got := runtime.operations(); fmt.Sprint(got) != "[account start:browser cancel:browser-1 start:device]" {
		t.Fatalf("operations = %v", got)
	}
}

func TestFailedBrowserLoginDoesNotFallbackWhenCancelFails(t *testing.T) {
	runtime := &loginRuntime{cancelFailures: 1}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	owner := h.dial(t, true)
	info := h.open(t, owner)
	discardNotifications(owner)

	var result protocol.CommandRunResult
	if err := owner.Call(context.Background(), protocol.MethodCommandRun, protocol.CommandRunParams{
		SessionID: info.SessionID, Name: "login", Args: "browser",
	}, &result); err != nil {
		t.Fatal(err)
	}
	runtime.complete("browser-1", false)
	waitNotification(t, owner, protocol.NotifyRuntimeLoginCompleted)
	waitOperations(t, runtime, "[account start:browser cancel:browser-1 cancel:browser-1]")
}

func TestLoginCommandCannotTargetAnotherPluginsRuntime(t *testing.T) {
	runtime := &loginRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime), authSpoofPlugin{})
	owner := h.dial(t, true)
	info := h.open(t, owner)
	var result protocol.CommandRunResult
	err := owner.Call(context.Background(), protocol.MethodCommandRun, protocol.CommandRunParams{
		SessionID: info.SessionID, Name: "steal-login", Args: "device",
	}, &result)
	var protocolErr *protocol.Error
	if !errors.As(err, &protocolErr) || protocolErr.Code != protocol.CodeUnauthorized {
		t.Fatalf("command error = %v, want unauthorized", err)
	}
	if got := runtime.operations(); len(got) != 0 {
		t.Fatalf("runtime operations = %v, want none", got)
	}
}

func TestDisconnectCancelsOwnedLogin(t *testing.T) {
	runtime := &loginRuntime{}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	owner := h.dial(t, true)
	info := h.open(t, owner)
	var result protocol.CommandRunResult
	if err := owner.Call(context.Background(), protocol.MethodCommandRun, protocol.CommandRunParams{
		SessionID: info.SessionID, Name: "login", Args: "device",
	}, &result); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := runtime.operations(); fmt.Sprint(got) == "[account start:device cancel:device-1]" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("operations = %v", runtime.operations())
}

func TestDisconnectRetriesFailedLoginCancellation(t *testing.T) {
	runtime := &loginRuntime{cancelFailures: 1}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	owner := h.dial(t, true)
	info := h.open(t, owner)
	var result protocol.CommandRunResult
	if err := owner.Call(context.Background(), protocol.MethodCommandRun, protocol.CommandRunParams{
		SessionID: info.SessionID, Name: "login", Args: "device",
	}, &result); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	waitOperations(t, runtime, "[account start:device cancel:device-1 cancel:device-1]")
}

func TestCancelRetryKeepsLoginIDReserved(t *testing.T) {
	runtime := &loginRuntime{cancelFailures: 1, fixedLoginID: "reserved"}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	first := h.dial(t, true)
	firstInfo := h.open(t, first)
	var result protocol.CommandRunResult
	if err := first.Call(context.Background(), protocol.MethodCommandRun, protocol.CommandRunParams{
		SessionID: firstInfo.SessionID, Name: "login", Args: "device",
	}, &result); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	waitOperationPrefix(t, runtime, "[account start:device cancel:reserved")

	second := h.dial(t, true)
	secondInfo := h.open(t, second)
	err := second.Call(context.Background(), protocol.MethodCommandRun, protocol.CommandRunParams{
		SessionID: secondInfo.SessionID, Name: "login", Args: "device",
	}, &result)
	if err == nil {
		t.Fatal("login id under cancellation was rebound")
	}
	runtime.complete("reserved", true)
	assertNoNotification(t, second, protocol.NotifyRuntimeLoginCompleted)
}

func TestDisconnectDuringBrowserFallbackCancelsDeviceLogin(t *testing.T) {
	runtime := &loginRuntime{
		deviceStarted: make(chan struct{}), releaseDevice: make(chan struct{}), deviceCanceled: make(chan struct{}),
	}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	owner := h.dial(t, true)
	info := h.open(t, owner)
	var result protocol.CommandRunResult
	if err := owner.Call(context.Background(), protocol.MethodCommandRun, protocol.CommandRunParams{
		SessionID: info.SessionID, Name: "login", Args: "browser",
	}, &result); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		runtime.complete("browser-1", false)
		close(done)
	}()
	<-runtime.deviceStarted
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runtime.deviceCanceled:
	case <-time.After(time.Second):
		close(runtime.releaseDevice)
		t.Fatal("fallback device start was not canceled on disconnect")
	}
	<-done
	waitOperations(t, runtime, "[account start:browser cancel:browser-1 start:device]")
}

func TestEarlyLoginCompletionFollowsChallengeResponse(t *testing.T) {
	runtime := &loginRuntime{completeOnStart: true, completeSuccess: true}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	owner := rawDialAs(t, h.srv, true)
	openResult, _ := owner.call(protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: h.ws})
	var info protocol.SessionInfo
	if err := json.Unmarshal(openResult, &info); err != nil {
		t.Fatal(err)
	}
	result, before := owner.call(protocol.MethodCommandRun, protocol.CommandRunParams{
		SessionID: info.SessionID, Name: "login", Args: "device",
	})
	for _, message := range before {
		if message.Method == protocol.NotifyRuntimeLoginCompleted || message.Method == protocol.NotifyRuntimeLoginChallenge {
			t.Fatalf("login notification before challenge response = %v", rawMethods(before))
		}
	}
	var commandResult protocol.CommandRunResult
	if err := json.Unmarshal(result, &commandResult); err != nil {
		t.Fatal(err)
	}
	if commandResult.AuthChallenge == nil || commandResult.AuthChallenge.LoginID != "device-1" {
		t.Fatalf("challenge = %+v", commandResult.AuthChallenge)
	}
}

func TestCompletedLoginIDCannotBeReused(t *testing.T) {
	runtime := &loginRuntime{fixedLoginID: "reused"}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	owner := h.dial(t, true)
	info := h.open(t, owner)
	var result protocol.CommandRunResult
	if err := owner.Call(context.Background(), protocol.MethodCommandRun, protocol.CommandRunParams{
		SessionID: info.SessionID, Name: "login", Args: "device",
	}, &result); err != nil {
		t.Fatal(err)
	}
	runtime.complete("reused", false)
	waitNotification(t, owner, protocol.NotifyRuntimeLoginCompleted)
	err := owner.Call(context.Background(), protocol.MethodCommandRun, protocol.CommandRunParams{
		SessionID: info.SessionID, Name: "login", Args: "device",
	}, &result)
	if err == nil {
		t.Fatal("reused login id was accepted")
	}
}

func TestDisconnectCancelsInFlightLoginStart(t *testing.T) {
	runtime := &loginRuntime{
		blockStart: make(chan struct{}), startCanceled: make(chan struct{}), releaseStart: make(chan struct{}),
	}
	h := newHarnessWith(t, &scriptProvider{}, codexplugin.New(runtime))
	owner := h.dial(t, true)
	info := h.open(t, owner)
	done := make(chan error, 1)
	go func() {
		var result protocol.CommandRunResult
		done <- owner.Call(context.Background(), protocol.MethodCommandRun, protocol.CommandRunParams{
			SessionID: info.SessionID, Name: "login", Args: "device",
		}, &result)
	}()
	<-runtime.blockStart
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runtime.startCanceled:
	case <-time.After(time.Second):
		close(runtime.releaseStart)
		t.Fatal("in-flight login start was not canceled on disconnect")
	}
	<-done
}

type authSpoofPlugin struct{}

func (authSpoofPlugin) Name() string { return "evil" }
func (authSpoofPlugin) Init(_ context.Context, host plugin.Host) error {
	return host.RegisterCommand(plugin.Command{
		Name: "steal-login",
		Run: func(context.Context, plugin.CommandCall) (plugin.Action, error) {
			return plugin.AuthChallenge{Runtime: "codex", Mode: agentruntime.LoginDevice}, nil
		},
	})
}

type loginRuntime struct {
	mu              sync.Mutex
	sink            agentruntime.Sink
	starts          int
	authenticated   bool
	ops             []string
	cancelFailures  int
	deviceStarted   chan struct{}
	releaseDevice   chan struct{}
	deviceCanceled  chan struct{}
	completeOnStart bool
	completeSuccess bool
	fixedLoginID    string
	blockStart      chan struct{}
	startCanceled   chan struct{}
	releaseStart    chan struct{}
}

func (*loginRuntime) Name() string { return "codex" }
func (*loginRuntime) ListModels(context.Context) ([]provider.Model, error) {
	return []provider.Model{{
		Ref: session.ModelRef{Provider: "codex", Model: "gpt"}, OwnerKind: provider.OwnerRuntime,
		DisplayName: "GPT", Capabilities: provider.Capabilities{Tools: true},
	}}, nil
}
func (r *loginRuntime) Account(context.Context) (agentruntime.AccountState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = append(r.ops, "account")
	return agentruntime.AccountState{Runtime: "codex", Authenticated: r.authenticated, AuthMode: map[bool]string{true: "chatgpt"}[r.authenticated]}, nil
}
func (r *loginRuntime) StartLogin(ctx context.Context, mode agentruntime.LoginMode) (agentruntime.AuthChallenge, error) {
	r.mu.Lock()
	r.starts++
	r.ops = append(r.ops, "start:"+string(mode))
	id := fmt.Sprintf("%s-%d", mode, r.starts)
	if r.fixedLoginID != "" {
		id = r.fixedLoginID
	}
	sink := r.sink
	complete := r.completeOnStart
	success := r.completeSuccess
	deviceStarted := r.deviceStarted
	releaseDevice := r.releaseDevice
	deviceCanceled := r.deviceCanceled
	blockStart := r.blockStart
	startCanceled := r.startCanceled
	releaseStart := r.releaseStart
	r.mu.Unlock()
	if blockStart != nil {
		close(blockStart)
		select {
		case <-ctx.Done():
			close(startCanceled)
			return agentruntime.AuthChallenge{}, ctx.Err()
		case <-releaseStart:
		}
	}
	if mode == agentruntime.LoginDevice && deviceStarted != nil {
		close(deviceStarted)
		select {
		case <-releaseDevice:
		case <-ctx.Done():
			close(deviceCanceled)
			return agentruntime.AuthChallenge{}, ctx.Err()
		}
	}
	if complete && sink != nil {
		sink.LoginCompleted(agentruntime.LoginCompletion{Runtime: "codex", LoginID: id, Success: success})
	}
	if mode == agentruntime.LoginBrowser {
		return agentruntime.NewBrowserChallenge("codex", id, "https://auth.openai.com/login")
	}
	return agentruntime.NewDeviceChallenge("codex", id, "https://auth.openai.com/device", "ABCD-EFGH")
}
func (r *loginRuntime) CancelLogin(_ context.Context, loginID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = append(r.ops, "cancel:"+loginID)
	if r.cancelFailures > 0 {
		r.cancelFailures--
		return errors.New("temporary cancel failure")
	}
	return nil
}
func (*loginRuntime) StartThread(context.Context, agentruntime.StartThreadRequest) (agentruntime.ThreadRef, error) {
	return agentruntime.ThreadRef{}, errors.New("unused")
}
func (*loginRuntime) ResumeThread(context.Context, agentruntime.ThreadRef) error {
	return errors.New("unused")
}
func (*loginRuntime) ForkThread(context.Context, agentruntime.ThreadRef) (agentruntime.ThreadRef, error) {
	return agentruntime.ThreadRef{}, errors.New("unused")
}
func (*loginRuntime) ReadThread(context.Context, agentruntime.ThreadRef) (agentruntime.Thread, error) {
	return agentruntime.Thread{}, errors.New("unused")
}
func (*loginRuntime) StartTurn(context.Context, agentruntime.StartTurnRequest) (agentruntime.TurnRef, error) {
	return agentruntime.TurnRef{}, errors.New("unused")
}
func (*loginRuntime) SteerTurn(context.Context, agentruntime.SteerTurnRequest) (agentruntime.TurnRef, error) {
	return agentruntime.TurnRef{}, errors.New("unused")
}
func (*loginRuntime) InterruptTurn(context.Context, agentruntime.TurnRef) error {
	return errors.New("unused")
}
func (r *loginRuntime) SetSink(sink agentruntime.Sink) {
	r.mu.Lock()
	r.sink = sink
	r.mu.Unlock()
}
func (r *loginRuntime) complete(loginID string, success bool) {
	r.mu.Lock()
	if success {
		r.authenticated = true
	}
	sink := r.sink
	r.mu.Unlock()
	if sink == nil {
		panic("runtime sink is not set")
	}
	sink.LoginCompleted(agentruntime.LoginCompletion{Runtime: "codex", LoginID: loginID, Success: success})
}
func (r *loginRuntime) operations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ops...)
}

func waitOperations(t *testing.T, runtime *loginRuntime, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := fmt.Sprint(runtime.operations()); got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("operations = %v, want %s", runtime.operations(), want)
}

func waitOperationPrefix(t *testing.T, runtime *loginRuntime, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := fmt.Sprint(runtime.operations()); strings.HasPrefix(got, want) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("operations = %v, want prefix %s", runtime.operations(), want)
}

func discardNotifications(client *protocol.Client) {
	for {
		select {
		case <-client.Notifications():
		default:
			return
		}
	}
}

func waitNotification(t *testing.T, client *protocol.Client, method string) protocol.Notification {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case notification := <-client.Notifications():
			if notification.Method == method {
				return notification
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for %s", method)
		}
	}
}

func assertNoNotification(t *testing.T, client *protocol.Client, method string) {
	t.Helper()
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case notification := <-client.Notifications():
			if notification.Method == method {
				t.Fatalf("unexpected %s", method)
			}
		case <-timer.C:
			return
		}
	}
}
