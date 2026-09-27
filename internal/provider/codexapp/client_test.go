// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/agentruntime"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/provider/codexapp"
	"github.com/guygrigsby/rudy/internal/session"
)

var (
	fakeOnce sync.Once
	fakePath string
	fakeErr  error
)

func fakeCommand(t *testing.T, env ...string) codexapp.Command {
	t.Helper()
	fakeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "rudy-fake-codex-")
		if err != nil {
			fakeErr = err
			return
		}
		fakePath = filepath.Join(dir, "codex")
		cmd := exec.Command("go", "build", "-o", fakePath, "./testdata/fakecodex")
		cmd.Dir = "."
		if out, err := cmd.CombinedOutput(); err != nil {
			fakeErr = errors.New(err.Error() + ": " + string(out))
		}
	})
	if fakeErr != nil {
		t.Fatal(fakeErr)
	}
	return codexapp.Command{Path: fakePath, Env: env}
}

func TestClientInitializesBeforeAnyOtherRequest(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "methods")
	client := codexapp.NewClient(fakeCommand(t, "FAKE_CODEX_LOG="+logPath))
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	methods := readMethods(t, logPath)
	if got, want := strings.Join(methods, ","), "initialize,initialized"; got != want {
		t.Fatalf("methods = %q, want %q", got, want)
	}
}

func TestConcurrentStartProducesOneProcess(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "methods")
	client := codexapp.NewClient(fakeCommand(t, "FAKE_CODEX_LOG="+logPath))
	t.Cleanup(func() { _ = client.Close() })

	const callers = 16
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- client.Start(context.Background())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	methods := readMethods(t, logPath)
	if got := count(methods, "initialize"); got != 1 {
		t.Fatalf("initialize count = %d, want 1; methods = %v", got, methods)
	}
	if got := count(methods, "initialized"); got != 1 {
		t.Fatalf("initialized count = %d, want 1; methods = %v", got, methods)
	}
}

func TestStartRejectsCodexBelowMinimumVersion(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "methods")
	client := codexapp.NewClient(fakeCommand(t,
		"FAKE_CODEX_LOG="+logPath,
		"FAKE_CODEX_VERSION=0.154.9",
	))
	t.Cleanup(func() { _ = client.Close() })

	err := client.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "requires codex-cli >= 0.155.1") {
		t.Fatalf("Start error = %v, want minimum-version error", err)
	}
	if _, err := os.Stat(logPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("app server started despite old version: %v", err)
	}
}

func TestProcessEnvironmentExcludesAmbientVariables(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), "environment.json")
	command := fakeCommand(t,
		"FAKE_CODEX_ENV_LOG="+envPath,
		"FAKE_CODEX_FORBID_ENV=RUDY_AMBIENT_SECRET",
		"RUDY_EXPLICIT_SETTING=enabled",
		"SHELL=/explicit/shell",
	)
	t.Setenv("HOME", "/ambient/home")
	t.Setenv("SHELL", "/ambient/shell")
	t.Setenv("RUDY_AMBIENT_SECRET", "must-not-cross-process-boundary")

	client := codexapp.NewClient(command)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	payload, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	var entries []string
	if err := json.Unmarshal(payload, &entries); err != nil {
		t.Fatal(err)
	}
	environment := make(map[string]string, len(entries))
	for _, entry := range entries {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			environment[name] = value
		}
	}
	if _, ok := environment["RUDY_AMBIENT_SECRET"]; ok {
		t.Fatal("ambient secret reached Codex process")
	}
	if got, want := environment["HOME"], "/ambient/home"; got != want {
		t.Fatalf("HOME = %q, want %q", got, want)
	}
	if got, want := environment["SHELL"], "/explicit/shell"; got != want {
		t.Fatalf("SHELL = %q, want explicit override %q", got, want)
	}
	if got, want := environment["RUDY_EXPLICIT_SETTING"], "enabled"; got != want {
		t.Fatalf("RUDY_EXPLICIT_SETTING = %q, want %q", got, want)
	}
}

func TestLoginCompletionBeforeStartResponseIsMatchedByLoginID(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "methods")
	client := codexapp.NewClient(fakeCommand(t,
		"FAKE_CODEX_LOG="+logPath,
		"FAKE_CODEX_LOGIN_EARLY=login-1",
	))
	t.Cleanup(func() { _ = client.Close() })
	sink := newRuntimeSink()
	client.SetSink(sink)

	challenge, err := client.StartLogin(context.Background(), agentruntime.LoginBrowser)
	if err != nil {
		t.Fatal(err)
	}
	if challenge.LoginID == "" || challenge.LoginID == "login-1" || challenge.Type != agentruntime.ChallengeBrowser {
		t.Fatalf("challenge = %+v", challenge)
	}
	select {
	case completion := <-sink.logins:
		if completion.LoginID != challenge.LoginID || !completion.Success {
			t.Fatalf("completion = %+v", completion)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for login completion")
	}
}

func TestCanceledLoginStartStopsAmbiguousProcess(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "methods")
	client := codexapp.NewClient(fakeCommand(t,
		"FAKE_CODEX_LOG="+logPath,
		"FAKE_CODEX_LOGIN_BLOCK=1",
	))
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.StartLogin(ctx, agentruntime.LoginBrowser)
		done <- err
	}()
	waitForMethod(t, logPath, "account/login/start")
	cancel()
	if err := <-done; !errors.Is(err, agentruntime.ErrAmbiguous) {
		t.Fatalf("StartLogin error = %v, want ambiguous", err)
	}
	accountCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if _, err := client.Account(accountCtx); err != nil {
		t.Fatalf("restart after canceled login: %v", err)
	}
	if got := count(readMethods(t, logPath), "initialize"); got != 2 {
		t.Fatalf("initialize count = %d, want 2", got)
	}
}

func TestCanceledQueuedLoginDoesNotAbortActiveProcess(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "methods")
	client := codexapp.NewClient(fakeCommand(t,
		"FAKE_CODEX_LOG="+logPath,
		"FAKE_CODEX_LOGIN_BLOCK=1",
	))
	t.Cleanup(func() { _ = client.Close() })
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := client.StartLogin(firstCtx, agentruntime.LoginBrowser)
		firstDone <- err
	}()
	waitForMethod(t, logPath, "account/login/start")

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		_, err := client.StartLogin(secondCtx, agentruntime.LoginDevice)
		secondDone <- err
	}()
	cancelSecond()
	select {
	case err := <-secondDone:
		if !errors.Is(err, context.Canceled) || errors.Is(err, agentruntime.ErrAmbiguous) {
			t.Fatalf("queued StartLogin error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled queued StartLogin did not return")
	}
	select {
	case err := <-firstDone:
		t.Fatalf("active StartLogin was aborted: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancelFirst()
	<-firstDone
}

func TestCanceledUnsentLoginDoesNotAbortSharedProcess(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "methods")
	client := codexapp.NewClient(fakeCommand(t, "FAKE_CODEX_LOG="+logPath))
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.StartLogin(ctx, agentruntime.LoginBrowser); !errors.Is(err, context.Canceled) || errors.Is(err, agentruntime.ErrAmbiguous) {
		t.Fatalf("StartLogin error = %v, want unsent cancellation", err)
	}
	if _, err := client.Account(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := count(readMethods(t, logPath), "initialize"); got != 1 {
		t.Fatalf("initialize count = %d, want 1", got)
	}
}

func TestSameProcessLoginIDReuseFailsClosed(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "methods")
	client := codexapp.NewClient(fakeCommand(t,
		"FAKE_CODEX_LOG="+logPath,
		"FAKE_CODEX_LOGIN_EARLY=login-1",
	))
	t.Cleanup(func() { _ = client.Close() })
	sink := newRuntimeSink()
	client.SetSink(sink)
	if _, err := client.StartLogin(context.Background(), agentruntime.LoginBrowser); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sink.logins:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first completion")
	}
	if _, err := client.StartLogin(context.Background(), agentruntime.LoginBrowser); err == nil || !strings.Contains(err.Error(), "reused a login id") {
		t.Fatalf("second StartLogin error = %v", err)
	}
}

func TestListModelsPagesAndMapsCapabilities(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "methods")
	client := codexapp.NewClient(fakeCommand(t,
		"FAKE_CODEX_LOG="+logPath,
		"FAKE_CODEX_MODELS=pages",
	))
	t.Cleanup(func() { _ = client.Close() })

	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v", models)
	}
	first := models[0]
	if first.Ref != (session.ModelRef{Provider: "codex", Model: "gpt-a"}) || first.OwnerKind != provider.OwnerRuntime {
		t.Fatalf("first model identity = %+v", first)
	}
	if !first.Capabilities.Tools || first.Capabilities.Vision || !first.Capabilities.Reasoning {
		t.Fatalf("first model capabilities = %+v", first.Capabilities)
	}
	if got := strings.Join(first.ReasoningEfforts, ","); got != "low,medium,high" {
		t.Fatalf("reasoning efforts = %q", got)
	}
	if first.ContextWindow != 0 || first.MaxOutput != 0 || first.Pricing != (provider.Pricing{}) {
		t.Fatalf("unknown limits or pricing were fabricated: %+v", first)
	}
	if got := count(readMethods(t, logPath), "model/list"); got != 2 {
		t.Fatalf("model/list count = %d, want 2", got)
	}
}

func TestLostTurnStartIsAmbiguousAndNotRetried(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "methods")
	client := codexapp.NewClient(fakeCommand(t,
		"FAKE_CODEX_LOG="+logPath,
		"FAKE_CODEX_DROP=turn/start",
	))
	t.Cleanup(func() { _ = client.Close() })

	_, err := client.StartTurn(context.Background(), agentruntime.StartTurnRequest{
		Thread: agentruntime.ThreadRef{
			Runtime:   "codex",
			SessionID: ulid.Make(),
			ThreadID:  "thread-1",
		},
		Content: []session.Block{session.TextBlock("hello")},
	})
	if !errors.Is(err, agentruntime.ErrAmbiguous) {
		t.Fatalf("StartTurn error = %v, want ErrAmbiguous", err)
	}

	methods := readMethods(t, logPath)
	if got := count(methods, "turn/start"); got != 1 {
		t.Fatalf("turn/start count = %d, want 1; methods = %v", got, methods)
	}
}

func TestInvalidMutationResultIsAmbiguous(t *testing.T) {
	tests := []struct {
		method string
		call   func(*codexapp.Client) error
	}{
		{
			method: "thread/start",
			call: func(client *codexapp.Client) error {
				_, err := client.StartThread(context.Background(), agentruntime.StartThreadRequest{SessionID: ulid.Make()})
				return err
			},
		},
		{
			method: "thread/fork",
			call: func(client *codexapp.Client) error {
				_, err := client.ForkThread(context.Background(), agentruntime.ThreadRef{Runtime: "codex", SessionID: ulid.Make(), ThreadID: "thread-1"})
				return err
			},
		},
		{
			method: "turn/start",
			call: func(client *codexapp.Client) error {
				_, err := client.StartTurn(context.Background(), agentruntime.StartTurnRequest{
					Thread:  agentruntime.ThreadRef{Runtime: "codex", SessionID: ulid.Make(), ThreadID: "thread-1"},
					Content: []session.Block{session.TextBlock("hello")},
				})
				return err
			},
		},
		{
			method: "turn/steer",
			call: func(client *codexapp.Client) error {
				_, err := client.SteerTurn(context.Background(), agentruntime.SteerTurnRequest{
					Turn: agentruntime.TurnRef{
						ThreadRef: agentruntime.ThreadRef{Runtime: "codex", SessionID: ulid.Make(), ThreadID: "thread-1"},
						TurnID:    "turn-1",
					},
					Content: []session.Block{session.TextBlock("steer")},
				})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.method, func(t *testing.T) {
			client := codexapp.NewClient(fakeCommand(t,
				"FAKE_CODEX_LOG="+filepath.Join(t.TempDir(), "methods"),
				"FAKE_CODEX_INVALID_MUTATION="+test.method,
			))
			t.Cleanup(func() { _ = client.Close() })
			if err := test.call(client); !errors.Is(err, agentruntime.ErrAmbiguous) {
				t.Fatalf("%s error = %v, want ErrAmbiguous", test.method, err)
			}
		})
	}
}

func TestReadThreadRejectsDifferentReturnedThread(t *testing.T) {
	client := codexapp.NewClient(fakeCommand(t, "FAKE_CODEX_LOG="+filepath.Join(t.TempDir(), "methods"), "FAKE_CODEX_READ_WRONG=1"))
	t.Cleanup(func() { _ = client.Close() })
	_, err := client.ReadThread(context.Background(), agentruntime.ThreadRef{Runtime: "codex", SessionID: ulid.Make(), ThreadID: "thread-1"})
	if err == nil || !strings.Contains(err.Error(), "different thread") {
		t.Fatalf("ReadThread error = %v, want identity mismatch", err)
	}
}

func TestStderrRedactionRemovesSecretsAndQueryValues(t *testing.T) {
	in := "Authorization: Bearer secret https://auth.openai.com/x?code=abc&state=visible Cookie: sid=cookie-secret"
	got := codexapp.Redact(in)
	for _, secret := range []string{"secret", "abc", "visible", "cookie-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("Redact(%q) leaked %q in %q", in, secret, got)
		}
	}
}

func TestRedactRemovesJSONSecretValues(t *testing.T) {
	in := `{"authorization":"Bearer json-authorization","cookie":"sid=json-cookie","code":"json-code","token":"json-token","access_token":"json-access","refresh_token":"json-refresh","client_secret":"json-client"}`
	got := codexapp.Redact(in)
	for _, secret := range []string{"json-authorization", "json-cookie", "json-code", "json-token", "json-access", "json-refresh", "json-client"} {
		if strings.Contains(got, secret) {
			t.Fatalf("Redact(%q) leaked %q in %q", in, secret, got)
		}
	}
}

func readMethods(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

func waitForMethod(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil && count(strings.Fields(string(b)), want) > 0 {
			return
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", want)
}

func count(values []string, want string) int {
	var n int
	for _, value := range values {
		if value == want {
			n++
		}
	}
	return n
}

type runtimeSink struct {
	logins chan agentruntime.LoginCompletion
}

func newRuntimeSink() *runtimeSink {
	return &runtimeSink{logins: make(chan agentruntime.LoginCompletion, 4)}
}

func (*runtimeSink) RuntimeEvent(agentruntime.Event)          {}
func (*runtimeSink) AccountUpdated(agentruntime.AccountState) {}
func (s *runtimeSink) LoginCompleted(completion agentruntime.LoginCompletion) {
	s.logins <- completion
}
func (*runtimeSink) RequestApproval(context.Context, agentruntime.ApprovalQuestion) (agentruntime.ApprovalAnswer, error) {
	return agentruntime.ApprovalAnswer{}, errors.New("unexpected approval")
}
