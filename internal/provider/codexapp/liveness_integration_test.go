// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/agentruntime"
	"github.com/guygrigsby/rudy/internal/provider/codexapp"
	"github.com/guygrigsby/rudy/internal/session"
)

type livenessSink struct{ events chan agentruntime.Event }

func (s *livenessSink) RuntimeEvent(event agentruntime.Event)     { s.events <- event }
func (*livenessSink) AccountUpdated(agentruntime.AccountState)    {}
func (*livenessSink) LoginCompleted(agentruntime.LoginCompletion) {}
func (*livenessSink) RequestApproval(ctx context.Context, _ agentruntime.ApprovalQuestion) (agentruntime.ApprovalAnswer, error) {
	<-ctx.Done()
	return agentruntime.ApprovalAnswer{}, ctx.Err()
}

func TestProcessExitAfterTurnStartFailsActiveTurn(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "methods")
	client := codexapp.NewClient(fakeCommand(t, "FAKE_CODEX_LOG="+logPath, "FAKE_CODEX_DROP=turn/interrupt"))
	t.Cleanup(func() { _ = client.Close() })
	sink := &livenessSink{events: make(chan agentruntime.Event, 16)}
	client.SetSink(sink)
	ref, err := client.StartTurn(context.Background(), agentruntime.StartTurnRequest{
		Thread:  agentruntime.ThreadRef{Runtime: "codex", SessionID: ulid.Make(), ThreadID: "thread-1"},
		Content: []session.Block{session.TextBlock("hello")},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = client.InterruptTurn(context.Background(), ref)
	deadline := time.After(2 * time.Second)
	failed := false
	for !failed {
		select {
		case event := <-sink.events:
			if event.Type == agentruntime.EventRuntimeFailed && event.TurnID == ref.TurnID {
				if event.Status != string(agentruntime.TurnFailed) {
					t.Fatalf("terminal status = %q, want failed", event.Status)
				}
				failed = true
			}
		case <-deadline:
			t.Fatal("process exit left active turn without terminal failure")
		}
	}
	if _, err := client.ReadThread(context.Background(), ref.ThreadRef); err != nil {
		t.Fatalf("read after process exit: %v", err)
	}
	if got := count(readMethods(t, logPath), "initialize"); got != 2 {
		t.Fatalf("initialize count = %d, want lazy restart on next call", got)
	}
}

func TestProcessExitAfterLoginChallengeFailsMatchingLogin(t *testing.T) {
	client := codexapp.NewClient(fakeCommand(t,
		"FAKE_CODEX_LOG="+filepath.Join(t.TempDir(), "methods"),
		"FAKE_CODEX_DROP=account/read",
		"FAKE_CODEX_HOLD_LOGIN_COMPLETION=1",
	))
	t.Cleanup(func() { _ = client.Close() })
	sink := newRuntimeSink()
	client.SetSink(sink)
	challenge, err := client.StartLogin(context.Background(), agentruntime.LoginDevice)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = client.Account(context.Background())
	select {
	case completion := <-sink.logins:
		if completion.LoginID != challenge.LoginID || completion.Success || completion.Error == "" {
			t.Fatalf("process-loss completion = %+v, want failed matching login", completion)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("process exit left login challenge pending")
	}
}

func TestCancelLoginAfterProcessExitAllowsBrowserFallback(t *testing.T) {
	client := codexapp.NewClient(fakeCommand(t,
		"FAKE_CODEX_LOG="+filepath.Join(t.TempDir(), "methods"),
		"FAKE_CODEX_DROP=account/read",
	))
	t.Cleanup(func() { _ = client.Close() })
	sink := newRuntimeSink()
	client.SetSink(sink)
	challenge, err := client.StartLogin(context.Background(), agentruntime.LoginBrowser)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = client.Account(context.Background())
	select {
	case <-sink.logins:
	case <-time.After(2 * time.Second):
		t.Fatal("process exit did not finish browser attempt")
	}
	if err := client.CancelLogin(context.Background(), challenge.LoginID); err != nil {
		t.Fatalf("cancel dead browser attempt: %v", err)
	}
	if _, err := client.StartLogin(context.Background(), agentruntime.LoginDevice); err != nil {
		t.Fatalf("device fallback after dead browser: %v", err)
	}
}
