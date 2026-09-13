package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostWithSocketOrEmbedIsAUsageError(t *testing.T) {
	for _, d := range []dialOptions{{Host: "box", Socket: "/x"}, {Host: "box", Embed: true}} {
		_, code, err := dial(context.Background(), testBuilder(t, &fakeProvider{}), BuildOptions{Stderr: io.Discard}, d, "t", false)
		if code != 2 || err == nil {
			t.Fatalf("dial(%+v) = %d, %v; want 2", d, code, err)
		}
	}
}

// TestCwdAndNoSyncNeedAHost: both only describe where a workspace goes on a box, so without
// one they are a typo the operator should hear about rather than a pair of flags silently
// doing nothing to a local run.
func TestCwdAndNoSyncNeedAHost(t *testing.T) {
	for _, d := range []dialOptions{{Cwd: "/srv/work"}, {NoSync: true}} {
		_, code, err := dial(context.Background(), testBuilder(t, &fakeProvider{}), BuildOptions{Stderr: io.Discard}, d, "t", false)
		if code != 2 || err == nil {
			t.Fatalf("dial(%+v) = %d, %v; want 2", d, code, err)
		}
	}
}

// deadSSH is a shim that exits 255 the way ssh does when it cannot connect.
func deadSSH(t *testing.T) {
	t.Helper()
	shim := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\necho 'ssh: connect to host box port 22: Connection refused' >&2\nexit 255\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUDY_SSH", shim)
}

func TestHostNeverFallsBackToALocalServer(t *testing.T) {
	deadSSH(t)
	_, code, err := dial(context.Background(), testBuilder(t, &fakeProvider{}), BuildOptions{Stderr: io.Discard}, dialOptions{Host: "box"}, "t", false)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("dial over a dead ssh = %d, %v; want exit 1 carrying ssh's stderr and no embedded server", code, err)
	}
}

// TestRemoteHostIsTheDefaultAndEmbedStillWins: remote.host says where the kernel runs for an
// invocation that named no transport. --embed and --socket each answer that same question, so
// an operator who typed one is not contradicting a config key they set weeks ago and must not
// be told to drop a --host they never passed; only --host itself is the contradiction, which
// TestHostWithSocketOrEmbedIsAUsageError covers.
func TestRemoteHostIsTheDefaultAndEmbedStillWins(t *testing.T) {
	deadSSH(t)
	build := testBuilder(t, &fakeProvider{})
	o := BuildOptions{Stderr: io.Discard, Overrides: map[string]any{"remote.host": "box"}}
	_, code, err := dial(context.Background(), build, o, dialOptions{}, "t", false)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("dial with remote.host set = %d, %v; want the ssh failure, not a local server", code, err)
	}
	d, code, err := dial(context.Background(), build, o, dialOptions{Embed: true}, "t", false)
	if err != nil || code != 0 {
		t.Fatalf("dial --embed with remote.host set = %d, %v; want a kernel in this process", code, err)
	}
	defer d.Close()
	if !d.Host.IsZero() {
		t.Fatalf("dial --embed went to host %s", d.Host)
	}
}

func TestExit111NamesTheInstall(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := builtRudy(t)
	home, env := boxHome(t, bin)
	if err := os.Remove(filepath.Join(home, ".local", "bin", "rudy")); err != nil { // a box with no rudy
		t.Fatal(err)
	}
	t.Setenv("RUDY_SSH", sshShim(t, env))
	_, code, err := dial(context.Background(), testBuilder(t, &fakeProvider{}), BuildOptions{Stderr: io.Discard}, dialOptions{Host: "box"}, "t", false)
	if code != 1 || !errors.Is(err, errNoRudyOnHost) {
		t.Fatalf("dial to a box without rudy = %d, %v; want errNoRudyOnHost", code, err)
	}
}

func TestPrintOverHostRunsTheTurnOnTheBox(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and starts a daemon")
	}
	bin := builtRudy(t)
	upstream := fakeOpenAI(t, "hello from the box")
	home, env := boxWithProvider(t, bin, upstream.URL)
	t.Cleanup(func() { stopBoxDaemon(t, env) })
	t.Setenv("RUDY_SSH", sshShim(t, env))
	// The local cwd is under the local home, so the placement is the same relative path
	// under the box home. Neither tree needs to hold anything for a --no-sync open: the
	// daemon detects the workspace at the placement, which must exist there.
	localHome := t.TempDir()
	t.Setenv("HOME", localHome)
	local := filepath.Join(localHome, "projects", "demo")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	placement := filepath.Join(home, "projects", "demo")
	if err := os.MkdirAll(placement, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(local)

	// The builder is handed over and must never be called: --host runs the kernel on the box,
	// and a local Build would be a second kernel over this machine's store.
	inner := testBuilder(t, &fakeProvider{})
	builds := 0
	build := func(ctx context.Context, o BuildOptions) (*Built, error) {
		builds++
		return inner(ctx, o)
	}

	var stdout, stderr bytes.Buffer
	code, err := runPrint(context.Background(), printOptions{Output: "json"}, dialOptions{Host: "box", NoSync: true}, "hi", build, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v\nstderr: %s", code, err, stderr.String())
	}
	var res printResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("%v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if res.Result != "hello from the box" {
		t.Fatalf("result = %q", res.Result)
	}
	if builds != 0 {
		t.Fatalf("the local builder ran %d times under --host; the kernel belongs on the box", builds)
	}
	// The session lives on the box, at the placement, in the box's store.
	entries, err := filepath.Glob(filepath.Join(home, ".local", "share", "rudy", "sessions", "*", "entries.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("box sessions = %v, want exactly one", entries)
	}
	first, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), placement) {
		t.Fatalf("the box session's workspace is not the placement %s:\n%s", placement, first)
	}
}
