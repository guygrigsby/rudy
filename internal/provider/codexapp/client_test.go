// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/agentruntime"
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

func TestStderrRedactionRemovesSecretsAndQueryValues(t *testing.T) {
	in := "Authorization: Bearer secret https://auth.openai.com/x?code=abc&state=visible Cookie: sid=cookie-secret"
	got := codexapp.Redact(in)
	for _, secret := range []string{"secret", "abc", "visible", "cookie-secret"} {
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

func count(values []string, want string) int {
	var n int
	for _, value := range values {
		if value == want {
			n++
		}
	}
	return n
}
