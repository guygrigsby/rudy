// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/cli/hosts"
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

// TestTheBoxsLastFrameSurvivesSshExiting: the daemon on the box dies mid-turn, the bridge
// writes its last frames and ssh exits. Those frames are sitting in a pipe this process
// holds, and StdinPipe/StdoutPipe would have cmd.Wait close the read end the moment ssh
// went, concurrently with the reader: the frames are lost and the operator is told "file
// already closed" instead of what happened. The reads here happen after ssh has exited and
// been reaped, which is that ordering made deterministic in both directions.
func TestTheBoxsLastFrameSurvivesSshExiting(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "ssh")
	// One frame and then gone: no read of stdin and no wait, so the process is over before
	// anything here has read a byte.
	const frame = `{"jsonrpc":"2.0","method":"turn.state","params":{"turn_id":"t"}}`
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nprintf '%s\\n' '"+frame+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	conn, proc, err := sshTransport(exec.Command(shim))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	<-proc.exited

	ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	got, err := conn.Recv(ctx)
	if err != nil {
		t.Fatalf("the frame ssh wrote on its way out: %v", err)
	}
	if string(got) != frame {
		t.Fatalf("frame = %s", got)
	}
	// And then the stream ends plainly. A read end closed under the reader ends it with
	// "file already closed", which says nothing about the box.
	if _, err := conn.Recv(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("end of the stream = %v, want io.EOF", err)
	}
}

// TestExit111NamesTheInstall: a box with no rudy and nothing this client can install from.
// The version here is "dev", which names no commit, so the install cannot run and the
// operator is told what is missing rather than watched a build that could never start.
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
	if !strings.Contains(err.Error(), "make") {
		t.Fatalf("dial from a dev build said %q; it should say why it could not install", err)
	}
}

// TestExit111InstallsAndRetries is the whole install path on the real binary: a box that
// answers ssh, holds rudy's source and has no rudy on its PATH. The remote line exits 111,
// the client builds rudy there at its own revision and dials again, and the session it opens
// is on the box.
func TestExit111InstallsAndRetries(t *testing.T) {
	home, _ := aBoxThatCanInstall(t)
	if err := os.Remove(filepath.Join(home, ".local", "bin", "rudy")); err != nil { // a box with no rudy
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	d, code, err := dial(context.Background(), testBuilder(t, &fakeProvider{}), BuildOptions{Stderr: &stderr}, dialOptions{Host: "box"}, "t", false)
	if err != nil || code != 0 {
		t.Fatalf("dial = %d, %v\n%s", code, err, stderr.String())
	}
	d.Close()
	if !strings.Contains(stderr.String(), "installed rudy") {
		t.Fatalf("no install notice: %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "rudy")); err != nil {
		t.Fatalf("the install did not put rudy on the box: %v", err)
	}
}

// TestAVersionMismatchIsANoticeNotARestart: the box's daemon is serving somebody's session
// and the operator's Mac has moved on a commit. Restarting it under them would end whatever
// it is running, so the dial goes through and says what the difference is and what to type.
func TestAVersionMismatchIsANoticeNotARestart(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and starts a daemon")
	}
	bin := builtRudy(t)
	upstream := fakeOpenAI(t, "unused")
	_, env := boxWithProvider(t, bin, upstream.URL)
	t.Cleanup(func() { stopBoxDaemon(t, env) })
	t.Setenv("RUDY_SSH", sshShim(t, env))
	hermeticLocalConfig(t)
	pinVersion(t, "v0.9.9-2-gother99") // a Mac ahead of the box

	var stderr bytes.Buffer
	d, code, err := dial(context.Background(), testBuilder(t, &fakeProvider{}), BuildOptions{Stderr: &stderr}, dialOptions{Host: "box"}, "t", false)
	if err != nil || code != 0 {
		t.Fatalf("dial = %d, %v\n%s", code, err, stderr.String())
	}
	d.Close()
	for _, want := range []string{"host runs rudy " + builtVersion, "this is v0.9.9-2-gother99", "rudy hosts install box --force"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("the mismatch notice %q lacks %q", stderr.String(), want)
		}
	}
}

// aBoxThatCanInstall is a box serving turns whose ~/projects/rudy is a checkout carrying a
// ref named for this client's revision, and whose make install puts the built binary where
// the remote line's PATH looks. That is what a real box's make install does through go
// install; building rudy for real inside a test would cost a minute per case.
func aBoxThatCanInstall(t *testing.T) (home string, env []string) {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the binary and starts a daemon")
	}
	requireBoxGit(t)
	requireBoxMake(t)
	bin := builtRudy(t)
	upstream := fakeOpenAI(t, "unused")
	home, env = boxWithProvider(t, bin, upstream.URL)
	source := filepath.Join(home, "projects", "rudy")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "Makefile"), []byte("install:\n\tmkdir -p $$HOME/.local/bin && ln -sf "+bin+" $$HOME/.local/bin/rudy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRepoAt(t, source)
	// Revision(builtVersion) is test001, and a tag by that name is a checkout target.
	gitAt(t, source, "update-ref", "refs/tags/test001", "HEAD")
	t.Cleanup(func() { stopBoxDaemon(t, env) })
	t.Setenv("RUDY_SSH", sshShim(t, env))
	hermeticLocalConfig(t)
	pinVersion(t, builtVersion)
	return home, env
}

// requireBoxMake skips unless make is where the box fixture's PATH can find it.
func requireBoxMake(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/usr/bin/make"); err != nil {
		t.Skip("the box fixture's PATH is /usr/bin:/bin and make is not there")
	}
}

// hermeticLocalConfig gives this process a home and a config directory of its own. The keys
// the client reads on its way to a box (remote.host, remote.source) then come from the
// shipped defaults rather than from whatever config the developer running the tests has,
// and the default remote.source is the ~/projects/rudy the box expands against its own home.
func hermeticLocalConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

// TestResumeByIDNeedsNoPlacement: a --resume names its session outright, and the session
// carries its own workspace. So the cwd it was typed from is not read at all, and a cwd with
// no place on the host must not fail it: `rudy --host box --resume <id>` from /tmp is a
// perfectly good command, and telling the operator to pass --cwd for a directory nothing is
// going to use is a refusal with no cause behind it.
func TestResumeByIDNeedsNoPlacement(t *testing.T) {
	build := testBuilder(t, &fakeProvider{})
	o := BuildOptions{Stderr: io.Discard}
	// A directory outside the local home, which is what Place has no answer for.
	outside := t.TempDir()

	first, code, err := dial(context.Background(), build, o, dialOptions{Embed: true}, "t", false)
	if err != nil || code != 0 {
		t.Fatalf("dial = %d, %v", code, err)
	}
	opened, code, err := openOrResume(context.Background(), first, printOptions{}, outside, "--resume", io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("open = %d, %v", code, err)
	}
	first.Close() // the store's flock, before the second server takes it

	d, code, err := dial(context.Background(), build, o, dialOptions{Embed: true}, "t", false)
	if err != nil || code != 0 {
		t.Fatalf("second dial = %d, %v", code, err)
	}
	defer d.Close()
	// Now the same connection, told it is remote. The fixture is only worth anything if this
	// cwd really has no placement.
	host, err := hosts.ParseHost("box")
	if err != nil {
		t.Fatal(err)
	}
	d.Host, d.Home = host, "/home/box"
	if _, err := d.place(outside); err == nil {
		t.Fatalf("fixture: %s has a placement under %s, so this test proves nothing", outside, d.Paths.Home)
	}
	got, code, err := openOrResume(context.Background(), d, printOptions{Resume: opened.SessionID}, outside, "--resume", io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("resume by id over --host = %d, %v; a resume never reads the cwd", code, err)
	}
	if got.SessionID != opened.SessionID {
		t.Fatalf("resumed %s, want %s", got.SessionID, opened.SessionID)
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

// TestPrintOverHostSyncsTheTreeFirst is the real path this wave's sync exists for: no
// --no-sync, a checkout under the local home and nothing at all at the placement, so the
// session opens on a checkout the client made on the box out of this one, committed history
// and uncommitted edit alike. Everything goes through the ssh shim, git's own connection
// included, since RUDY_SSH is what git is told to push with.
func TestPrintOverHostSyncsTheTreeFirst(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and starts a daemon")
	}
	requireBoxGit(t)
	bin := builtRudy(t)
	upstream := fakeOpenAI(t, "hello from the box")
	home, env := boxWithProvider(t, bin, upstream.URL)
	t.Cleanup(func() { stopBoxDaemon(t, env) })
	t.Setenv("RUDY_SSH", sshShim(t, env))

	localHome := t.TempDir()
	t.Setenv("HOME", localHome)
	local := filepath.Join(localHome, "projects", "demo")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRepoAt(t, local)
	if err := os.WriteFile(filepath.Join(local, "draft.txt"), []byte("not committed"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(local)
	placement := filepath.Join(home, "projects", "demo")

	var stdout, stderr bytes.Buffer
	code, err := runPrint(context.Background(), printOptions{Output: "json"}, dialOptions{Host: "box"}, "hi", testBuilder(t, &fakeProvider{}), &stdout, &stderr)
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
	if got := gitAt(t, placement, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Fatalf("the placement is not a checkout on main: %q\nstderr: %s", got, stderr.String())
	}
	if got, err := os.ReadFile(filepath.Join(placement, "one.txt")); err != nil || string(got) != "one" {
		t.Fatalf("the committed file did not arrive: %q %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(placement, "draft.txt")); err != nil || string(got) != "not committed" {
		t.Fatalf("the uncommitted file did not arrive: %q %v", got, err)
	}
}

// TestPrintOverHostPullsACopiedTreeBack is the other half of the sync, on the tree that has no
// other way home: a directory no git tracks goes over as a copy, the session runs on it there,
// and the client that opened that session tars the placement back when it closes.
//
// The box's work is a file written into the placement between the two runs by hand rather than
// by a tool call through the fake upstream: what is under test is the close-time pull, and a
// tool_calls fixture would be a second, larger fake upstream to keep for the same assertion.
// The first run is the copy leg (nothing at the placement), the second is the box holding a
// copy already, which is the other way a session ends up on a tree that comes home by tar.
func TestPrintOverHostPullsACopiedTreeBack(t *testing.T) {
	local, placement, build := aBoxAndAPlainTree(t, "hello from the box")
	var stdout, stderr bytes.Buffer
	code, err := runPrint(context.Background(), printOptions{Output: "json"}, dialOptions{Host: "box"}, "hi", build, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("first run = %d, %v\nstderr: %s", code, err, stderr.String())
	}
	if got, err := os.ReadFile(filepath.Join(placement, "a.txt")); err != nil || string(got) != "a" {
		t.Fatalf("the tree was not copied to the placement: %q %v\nstderr: %s", got, err, stderr.String())
	}

	if err := os.WriteFile(filepath.Join(placement, "box.txt"), []byte("box work"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	code, err = runPrint(context.Background(), printOptions{Output: "json"}, dialOptions{Host: "box"}, "hi", build, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("second run = %d, %v\nstderr: %s", code, err, stderr.String())
	}
	if got, err := os.ReadFile(filepath.Join(local, "box.txt")); err != nil || string(got) != "box work" {
		t.Fatalf("the box's file did not come back when the session closed: %q %v\nstderr: %s", got, err, stderr.String())
	}
}

// TestTUIOverHostPullsACopiedTreeBack is the same close, on the other client. The launcher
// draws nothing and returns, which is the client exiting, and the tree still has to come home:
// the pull hangs off the connection being remote and the tree being a copy, not off which
// command opened the session.
func TestTUIOverHostPullsACopiedTreeBack(t *testing.T) {
	local, placement, build := aBoxAndAPlainTree(t, "hello from the box")
	// The box holds the copy already, with work of its own in it.
	if err := os.MkdirAll(placement, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(placement, "box.txt"), []byte("box work"), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeTerminal(t, true)
	var stderr bytes.Buffer
	drawn := false
	// The client drew and quit with the session at rest, which is the close that pulls.
	launch := func(ctx context.Context, r clientRun) (bool, error) { drawn = true; return true, nil }
	code := runTUI(context.Background(), build, dialOptions{Host: "box"}, resumeWith(printOptions{}, "--resume", &stderr), launch, "", &stderr)
	if code != 0 || !drawn {
		t.Fatalf("runTUI = %d, drawn %v\nstderr: %s", code, drawn, stderr.String())
	}
	if got, err := os.ReadFile(filepath.Join(local, "box.txt")); err != nil || string(got) != "box work" {
		t.Fatalf("the box's file did not come back when the client exited: %q %v\nstderr: %s", got, err, stderr.String())
	}
}

// TestPrintOverHostLeavesATreeATurnIsStillWriting: closing the connection hangs up the bridge
// and leaves the daemon on the box running whatever it was running, so an interrupted run is a
// placement still being written. Nothing is pulled over the operator's tree, and they are told
// what to type once the box is done rather than left to wonder where the work went.
//
// The turn hangs because the upstream never answers, which is the box mid-tool-call as far as
// this client can tell: the interrupt lands while the daemon is still working either way.
func TestPrintOverHostLeavesATreeATurnIsStillWriting(t *testing.T) {
	upstream, asked, release := fakeOpenAIMidTurn(t)
	local, placement, build := aBoxAndAPlainTreeAt(t, upstream.URL)
	// Registered after the box's own cleanups, so it runs before them: the handler lets go
	// before anything tries to stop the daemon that is waiting on it.
	t.Cleanup(release)
	if err := os.MkdirAll(placement, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(placement, "box.txt"), []byte("half written"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-asked
		cancel()
	}()
	var stdout, stderr bytes.Buffer
	code, err := runPrint(ctx, printOptions{Output: "json"}, dialOptions{Host: "box"}, "hi", build, &stdout, &stderr)
	if code != 130 || err != nil {
		t.Fatalf("interrupted run = %d, %v; want 130\nstderr: %s", code, err, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(local, "box.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the tree came home while a turn was still running on it: %v", err)
	}
	if !strings.Contains(stderr.String(), "a turn is still running") || !strings.Contains(stderr.String(), "rudy hosts pull box") {
		t.Fatalf("the operator was not told what to do: %q", stderr.String())
	}
}

// fakeOpenAIMidTurn is fakeOpenAI's models endpoint with a completions endpoint that never
// answers: it says when the box has asked for the turn and holds until released, which is a
// daemon still working when the operator's Ctrl-C lands.
func fakeOpenAIMidTurn(t *testing.T) (srv *httptest.Server, asked <-chan struct{}, release func()) {
	t.Helper()
	started, done := make(chan struct{}), make(chan struct{})
	var once, closed sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"m","object":"model"}]}`)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		select {
		case <-done:
		case <-r.Context().Done():
		}
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, started, func() { closed.Do(func() { close(done) }) }
}

// aBoxAndAPlainTree is a box serving turns, this machine's cwd a directory no git tracks under
// a temp home, and nothing at the placement yet. The close-time pull tests differ in which
// client they run and in nothing else, so the fixture is one.
func aBoxAndAPlainTree(t *testing.T, reply string) (local, placement string, build buildFunc) {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the binary and starts a daemon")
	}
	return aBoxAndAPlainTreeAt(t, fakeOpenAI(t, reply).URL)
}

// aBoxAndAPlainTreeAt is the same fixture against an upstream the caller stood up itself.
func aBoxAndAPlainTreeAt(t *testing.T, upstream string) (local, placement string, build buildFunc) {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the binary and starts a daemon")
	}
	requireBoxGit(t)
	bin := builtRudy(t)
	home, env := boxWithProvider(t, bin, upstream)
	t.Cleanup(func() { stopBoxDaemon(t, env) })
	t.Setenv("RUDY_SSH", sshShim(t, env))

	localHome := t.TempDir()
	t.Setenv("HOME", localHome)
	local = filepath.Join(localHome, "projects", "plain")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(local)
	return local, filepath.Join(home, "projects", "plain"), testBuilder(t, &fakeProvider{})
}

// requireBoxGit skips unless both ends of the sync have git and tar. This machine's half
// finds them anywhere on PATH; the box's half runs under the fixture's own environment,
// whose PATH is /usr/bin:/bin. It also puts a hermetic git environment on the test process,
// so no developer's global config decides whether these tests pass.
func requireBoxGit(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"git", "tar"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
		if _, err := os.Stat("/usr/bin/" + bin); err != nil {
			t.Skipf("the box fixture's PATH is /usr/bin:/bin and %s is not there", bin)
		}
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL":   "/dev/null",
		"GIT_CONFIG_SYSTEM":   "/dev/null",
		"GIT_AUTHOR_NAME":     "rudy test",
		"GIT_AUTHOR_EMAIL":    "rudy@test",
		"GIT_COMMITTER_NAME":  "rudy test",
		"GIT_COMMITTER_EMAIL": "rudy@test",
		"GIT_TERMINAL_PROMPT": "0",
	} {
		t.Setenv(k, v)
	}
}

// gitRepoAt makes dir a checkout on main with one commit, the tree a client has when it
// reaches for a box.
func gitRepoAt(t *testing.T, dir string) {
	t.Helper()
	gitAt(t, dir, "init", "-q", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(dir, "one.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitAt(t, dir, "add", "-A")
	gitAt(t, dir, "commit", "-qm", "one")
}

// gitAt runs one git command in dir and returns its first line of output.
func gitAt(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line
}
