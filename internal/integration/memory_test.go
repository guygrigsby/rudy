// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration_test

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestFinalizeFoldFailureReachesTheTerminal is rudy-6zu on the path a person actually takes.
// A headless run folds once, at close, and that fold finishes after the server has closed the
// session log, so the note it would have written comes back not_found (ADR 0041). The failure
// used to go down with the note: the run said nothing, exited 0 and left a bundle that had
// recorded nothing.
func TestFinalizeFoldFailureReachesTheTerminal(t *testing.T) {
	h := newHome(t)
	h.provider = newFakeProvider(t)
	bundle := initBundle(t, h)
	h.writeConfig(t, fmt.Sprintf(`[default]
provider = "fake"
model = "m1"
thinking = "off"

[permissions]
mode = "off"

[memory]
enabled = true
dir = %q

[providers.fake]
wire = "openai_chat"
base_url = "%s/v1"
`, bundle, h.provider.URL()))

	// The turn's own request is answered. Everything after it is the fold's summarizer, which
	// is the call this test refuses.
	var answered atomic.Bool
	h.provider.onCompletion(func(w http.ResponseWriter, r *http.Request) {
		if answered.CompareAndSwap(false, true) {
			writeSSE(w, `{"choices":[{"index":0,"delta":{"content":"ok"}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, "[DONE]")
			return
		}
		http.Error(w, "no summaries today", http.StatusInternalServerError)
	})

	r := h.run(t, 120*time.Second, "-p", "say something worth folding")
	assertNoPanic(t, r.out())
	if r.code != 0 {
		t.Fatalf("exit %d; a failed fold is not a failed run\n%s", r.code, r.out())
	}
	if !strings.Contains(r.stdout, "ok") {
		t.Fatalf("stdout %q, want the model's answer", r.stdout)
	}
	if !strings.Contains(r.stderr, "memory: fold failed") {
		t.Fatalf("the fold failed and said nothing about it\nstderr: %s", r.stderr)
	}
	// And it said it to the operator rather than to the log, which is the half of ADR 0041
	// that cannot change quietly: by the time a finalize fold has anything to report, the
	// session it belongs to is closed and its log is a finished file.
	if log := onlySessionLog(t, h); strings.Contains(log, `"kind":"note"`) {
		t.Errorf("a note reached a closed session's log:\n%s", log)
	}
}

// onlySessionLog is the transcript of the one session a run left behind.
func onlySessionLog(t *testing.T, h *home) string {
	t.Helper()
	dir := filepath.Join(h.root, "data", "rudy", "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("sessions dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("sessions %d, want 1", len(entries))
	}
	raw, err := os.ReadFile(filepath.Join(dir, entries[0].Name(), "entries.jsonl"))
	if err != nil {
		t.Fatalf("session log: %v", err)
	}
	return string(raw)
}

// initBundle stands up a memory bundle the way `memory init` does: the root index and log the
// SDK looks for, and a repository with an identity, since the fold commits what it writes.
// The SDK is not importable here (it stays inside its plugin, see make vendor-types), so this
// writes the three files by hand.
func initBundle(t *testing.T, h *home) string {
	t.Helper()
	root := filepath.Join(h.root, "memory")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"index.md":   "# Memory Index\n",
		"log.md":     "# Directory Update Log\n",
		".gitignore": ".state/\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "integration@rudy.test"},
		{"config", "user.name", "rudy integration"},
		{"add", "-A"},
		{"commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = h.env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return root
}
