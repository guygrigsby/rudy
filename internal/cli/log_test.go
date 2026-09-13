package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/provider"
)

// logLines reads rudy.log under the built cache dir and decodes every line. An absent file
// is an empty slice: the assertions say what they expected to find.
func logLines(t *testing.T, b *Built) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(b.Paths.Cache, "rudy.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read rudy.log: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("rudy.log line is not JSON: %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func TestBuildWritesAStartRecordToTheLog(t *testing.T) {
	fp := &fakeProvider{}
	b, err := testBuilder(t, fp)(context.Background(), BuildOptions{Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = b.Server.Shutdown(context.Background()) }()

	lines := logLines(t, b)
	if len(lines) == 0 {
		t.Fatal("rudy.log has no records after Build")
	}
	first := lines[0]
	if first["msg"] != "rudy: start" {
		t.Fatalf("first record msg = %v, want the start record", first["msg"])
	}
	if first["level"] != "INFO" {
		t.Fatalf("start record level = %v", first["level"])
	}
	if first["version"] != "test" {
		t.Fatalf("start record version = %v", first["version"])
	}
	if pid, ok := first["pid"].(float64); !ok || int(pid) != os.Getpid() {
		t.Fatalf("start record pid = %v, want %d: two processes append to one file and the pid is what tells them apart", first["pid"], os.Getpid())
	}
}

func TestLogLevelFiltersRecords(t *testing.T) {
	fp := &fakeProvider{}
	b, err := testBuilderOver(t, fp, map[string]any{"log.level": "warn"})(context.Background(), BuildOptions{Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = b.Server.Shutdown(context.Background()) }()

	for _, rec := range logLines(t, b) {
		if rec["level"] == "INFO" || rec["level"] == "DEBUG" {
			t.Fatalf("log.level=warn let through %v", rec)
		}
	}
}

func TestANoticeIsMirroredIntoTheLog(t *testing.T) {
	fp := &fakeProvider{}
	// A named prompt file that does not exist is a notice, not a failed boot (ADR 0024).
	missing := filepath.Join(t.TempDir(), "nope.md")
	var stderr bytes.Buffer
	b, err := testBuilderOver(t, fp, map[string]any{"prompt.file": missing})(context.Background(), BuildOptions{Stderr: &stderr})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = b.Server.Shutdown(context.Background()) }()

	if !strings.Contains(stderr.String(), "nope.md") {
		t.Fatalf("the notice did not reach stderr: %q", stderr.String())
	}
	var mirrored bool
	for _, rec := range logLines(t, b) {
		if msg, _ := rec["msg"].(string); strings.Contains(msg, "nope.md") {
			mirrored = true
			if rec["level"] != "WARN" {
				t.Fatalf("a notice is mirrored at WARN, got %v", rec["level"])
			}
		}
	}
	if !mirrored {
		t.Fatal("the notice the operator saw is not in rudy.log; that is the first thing anyone reads after a failure")
	}
}

// msgsIn returns every msg in rudy.log under the cache dir the test's XDG env points at,
// for tests that reach the kernel through a command rather than through Build directly.
func msgsIn(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(os.Getenv("XDG_CACHE_HOME"), "rudy", "rudy.log"))
	if err != nil {
		t.Fatalf("read rudy.log: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var rec struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("rudy.log line is not JSON: %q", line)
		}
		out = append(out, rec.Msg)
	}
	return out
}

func TestATurnLeavesATrail(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{script: [][]provider.Part{say("ok")}}
	code, err := runPrint(context.Background(), printOptions{Output: "text"}, dialOptions{}, "hi", testBuilder(t, fp), io.Discard, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	msgs := msgsIn(t)
	for _, want := range []string{"rudy: start", "plugin: ready", "session: open", "turn: start", "turn: completed", "session: detach"} {
		if !slices.Contains(msgs, want) {
			t.Fatalf("rudy.log has no %q record after a full turn; records were %v", want, msgs)
		}
	}
	if slices.Contains(msgs, "rudy: exit") {
		t.Fatalf("a clean run logged an exit record: %v", msgs)
	}
}

func TestAnUnwritableLogFileDoesNotFailBuild(t *testing.T) {
	fp := &fakeProvider{}
	// A regular file where the log's parent directory should be makes the open fail.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	b, err := testBuilderOver(t, fp, map[string]any{"log.file": filepath.Join(blocker, "rudy.log")})(context.Background(), BuildOptions{Stderr: &stderr})
	if err != nil {
		t.Fatalf("a log file that cannot be opened must not fail boot: %v", err)
	}
	defer func() { _ = b.Server.Shutdown(context.Background()) }()
	if !strings.Contains(stderr.String(), "rudy.log") {
		t.Fatalf("no notice said the log file could not be opened: %q", stderr.String())
	}
}
