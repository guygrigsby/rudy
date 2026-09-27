// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const fakeCodexAnswer = "completed by fake Codex"

func TestRealCodexLoginApprovalRestartAndResume(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the real PTY fixture uses a POSIX opener")
	}
	root := t.TempDir()
	bin := builtRudy(t)
	fake := builtFakeCodex(t)
	scratchConfig(t, root, []byte(codexTestConfig))
	env, methods := fakeCodexProcessEnv(t, root, fake, true)
	env = envList{"XDG_RUNTIME_DIR=" + sockDir(t)}.With(env)
	socket := socketIn(t, env)

	daemon := startDaemon(t, bin, root, env, socket)
	pty, client := startTUI(t, bin, root, env)
	log := drain(pty)
	waitFor(t, log, "the Codex model", firstFrameWait, func(s string) bool {
		return strings.Contains(s, "codex:gpt-test")
	})

	typeIn(t, pty, log, "/login")
	press(t, pty, keyEnter)
	waitForCodex(t, log, methods, "the device login fallback", drawWait, func(s string) bool {
		return strings.Contains(s, "https://auth.openai.com/device") && strings.Contains(s, "OPENAI-CODE")
	})

	typeIn(t, pty, log, "fix it")
	press(t, pty, keyEnter)
	waitFor(t, log, "the runtime command approval", drawWait, func(s string) bool {
		return strings.Contains(s, "Run fake command") && strings.Contains(s, "allow once [y]")
	})
	press(t, pty, "y")
	waitFor(t, log, "the completed runtime answer", drawWait, func(s string) bool {
		return strings.Contains(s, fakeCodexAnswer)
	})

	entries := newestEntries(t, root)
	body, err := os.ReadFile(entries)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"kind":"runtime_permission_decision"`) ||
		!strings.Contains(string(body), `"decision":"allow"`) {
		t.Fatalf("runtime allow was not durable:\n%s", body)
	}
	link := filepath.Join(filepath.Dir(entries), "runtime.toml")
	linkBody, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if string(linkBody) != "runtime = 'codex'\nthread_id = 'thread-1'\n" &&
		string(linkBody) != "runtime = \"codex\"\nthread_id = \"thread-1\"\n" {
		t.Fatalf("runtime link = %q", linkBody)
	}
	if info, err := os.Stat(link); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("runtime link mode: info=%v err=%v", info, err)
	}

	typeIn(t, pty, log, slashCommand)
	press(t, pty, keyEnter)
	waitExit(t, client, log)
	stopDaemon(t, daemon)

	daemon = startDaemon(t, bin, root, env, socket)
	rpty, resumed := startTUI(t, bin, root, env, "--resume", sessionID(entries))
	rlog := drain(rpty)
	waitFor(t, rlog, "the cold runtime projection", firstFrameWait, func(s string) bool {
		return strings.Contains(s, fakeCodexAnswer)
	})
	if got := strings.Count(rlog.text(), fakeCodexAnswer); got != 1 {
		t.Fatalf("cold projection rendered the final answer %d times, want once:\n%s", got, tail(rlog.text()))
	}
	typeIn(t, rpty, rlog, slashCommand)
	press(t, rpty, keyEnter)
	waitExit(t, resumed, rlog)
	stopDaemon(t, daemon)

	got := readCodexMethods(t, methods)
	if n := methodCount(got, "thread/start"); n != 1 {
		t.Fatalf("thread/start count = %d, want 1; methods = %v", n, got)
	}
	if n := methodCount(got, "turn/start"); n != 1 {
		t.Fatalf("turn/start count = %d, want 1; methods = %v", n, got)
	}
	if methodCount(got, "thread/resume") == 0 || methodCount(got, "thread/read") == 0 {
		t.Fatalf("restart did not reconcile the thread: %v", got)
	}
}

func TestRealCodexHeadlessLoginUsesDeviceCode(t *testing.T) {
	root := t.TempDir()
	bin := builtRudy(t)
	fake := builtFakeCodex(t)
	scratchConfig(t, root, []byte(codexTestConfig))
	env, _ := fakeCodexProcessEnv(t, root, fake, false)
	out := runClient(t, bin, root, env, "-p", "/login")
	if !strings.Contains(out, "https://auth.openai.com/device") || !strings.Contains(out, "OPENAI-CODE") {
		t.Fatalf("headless login output = %q", out)
	}
}

const codexTestConfig = `[default]
provider = "codex"
model = "gpt-test"

[permissions]
mode = "strict"
`

type envList []string

func (extra envList) With(base []string) []string {
	values := append([]string(nil), base...)
	for _, replacement := range extra {
		name, _, _ := strings.Cut(replacement, "=")
		prefix := name + "="
		replaced := false
		for i, value := range values {
			if strings.HasPrefix(value, prefix) {
				values[i] = replacement
				replaced = true
				break
			}
		}
		if !replaced {
			values = append(values, replacement)
		}
	}
	return values
}

func fakeCodexProcessEnv(t *testing.T, root, fake string, failBrowser bool) ([]string, string) {
	t.Helper()
	binDir := filepath.Join(root, "fake-bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fake, filepath.Join(binDir, "codex")); err != nil {
		t.Fatal(err)
	}
	opener := filepath.Join(binDir, browserCommand())
	exit := "0"
	if failBrowser {
		exit = "1"
	}
	if err := os.WriteFile(opener, []byte("#!/bin/sh\nexit "+exit+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	methods := filepath.Join(root, "fake-codex.methods")
	extra := envList{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_CODEX_LOG=" + methods,
		"FAKE_CODEX_STATE=" + filepath.Join(root, "fake-codex-state.json"),
	}
	return extra.With(scratchEnv(root)), methods
}

func readCodexMethods(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(body))
}

func methodCount(methods []string, want string) int {
	var count int
	for _, method := range methods {
		if method == want {
			count++
		}
	}
	return count
}

func waitForCodex(t *testing.T, log *ptyLog, methods, want string, d time.Duration, match func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if match(log.text()) {
			return
		}
		if time.Now().After(deadline) {
			methodBody, _ := os.ReadFile(methods)
			t.Fatalf("waited %s for %s, never saw it; Codex methods:\n%s\nterminal:\n%s", d, want, methodBody, tail(log.text()))
		}
		time.Sleep(pollEvery)
	}
}

func TestEnvListReplacesValues(t *testing.T) {
	got := envList{"PATH=/fake", "NEW=value"}.With([]string{"HOME=/home", "PATH=/real"})
	joined := strings.Join(got, "\n")
	if strings.Count(joined, "PATH=") != 1 || !strings.Contains(joined, "PATH=/fake") || !strings.Contains(joined, "NEW=value") {
		t.Fatalf("env = %v", got)
	}
}
