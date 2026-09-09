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

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
)

// runtimeEnv answers XDG_RUNTIME_DIR with dir and defers every other key to the process
// environment. The default socket is resolved off the builder's own Env, so a test that
// wants a daemon on the path a client will probe steers that one key and leaves the XDG
// roots testBuilderOver planted where they are. It has to be dir rather than t.Setenv
// because a unix socket path is 104 bytes and t.TempDir() spends most of that before the
// runtime directory is even named.
func runtimeEnv(dir string) func(string) string {
	return func(key string) string {
		if key == "XDG_RUNTIME_DIR" {
			return dir
		}
		return os.Getenv(key)
	}
}

// refuseToBuild is a builder that fails the test if it is ever called. Every case where a
// client should reach a server it did not start gets one: attaching is only attaching if no
// plugin was loaded and no registry refreshed in this process.
func refuseToBuild(t *testing.T, why string) buildFunc {
	t.Helper()
	return func(context.Context, BuildOptions) (*Built, error) {
		t.Error(why)
		return nil, errors.New("must not build")
	}
}

// daemonOn starts rudy serve on the default socket for env and returns that path. The
// caller cancels ctx and waits on the returned channel.
func daemonOn(t *testing.T, ctx context.Context, build buildFunc, env func(string) string) (string, <-chan served, *watcher) {
	t.Helper()
	socket := config.XDG(env, t.TempDir()).Socket()
	done, w := startServe(t, ctx, build, socket)
	return socket, done, w
}

// TestDialAttachesWhenTheSocketAnswers is the whole point of the probe: a daemon is already
// serving the default socket, so the client talks to it rather than wiring a second server
// over the same session store. The builder must never be called, and the client comes back
// with no Built at all, since the store, the registry and the plugins live in the daemon.
func TestDialAttachesWhenTheSocketAnswers(t *testing.T) {
	t.Chdir(t.TempDir())
	build := testBuilder(t, &fakeProvider{})
	env := runtimeEnv(sockDir(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, done, w := daemonOn(t, ctx, build, env)

	refuse := refuseToBuild(t, "dial wired a server of its own while a daemon was serving the socket")
	d, code, err := dial(context.Background(), refuse,
		BuildOptions{Stderr: io.Discard, Env: env, Home: t.TempDir()}, dialOptions{}, "dial-test", false)
	if err != nil || code != 0 {
		t.Fatalf("dial = %d, %v; daemon stderr %q", code, err, w.String())
	}
	defer d.Close()
	if d.Built != nil {
		t.Fatal("an attached client has no Built: the store, registry and plugins are the daemon's")
	}
	if d.Version != "test" {
		t.Fatalf("hello version %q, want the daemon's %q", d.Version, "test")
	}
	if d.Config == nil || d.Paths.Config == "" {
		t.Fatalf("an attached client still resolves its own theme and keys: %+v %+v", d.Config, d.Paths)
	}
	var info protocol.SessionInfo
	if err := d.Client.Call(callCtx(t), protocol.MethodSessionOpen,
		protocol.SessionOpenParams{Cwd: t.TempDir()}, &info); err != nil {
		t.Fatalf("open over the attached client: %v", err)
	}

	cancel()
	if s := waitServed(t, done, w); s.code != 0 || s.err != nil {
		t.Fatalf("runServe = %d, %v", s.code, s.err)
	}
}

// TestDialEmbedsWhenNothingAnswers is the other half: an empty runtime directory means
// nobody is serving, and a client that refused to run without a daemon would be a harness
// that has to be started twice.
func TestDialEmbedsWhenNothingAnswers(t *testing.T) {
	t.Chdir(t.TempDir())
	build := testBuilder(t, &fakeProvider{})
	env := runtimeEnv(sockDir(t))

	d, code, err := dial(context.Background(), build,
		BuildOptions{Stderr: io.Discard, Env: env, Home: t.TempDir()}, dialOptions{}, "dial-test", false)
	if err != nil || code != 0 {
		t.Fatalf("dial = %d, %v", code, err)
	}
	defer d.Close()
	if d.Built == nil {
		t.Fatal("nothing answered the probe, so this client has to be its own server")
	}
	var info protocol.SessionInfo
	if err := d.Client.Call(callCtx(t), protocol.MethodSessionOpen,
		protocol.SessionOpenParams{Cwd: t.TempDir()}, &info); err != nil {
		t.Fatalf("open over the embedded server: %v", err)
	}
}

// TestDialExplicitSocketFails: --socket is an operator naming the server they mean. Falling
// back to an embedded one would answer a different question than the one they asked, on a
// store they may not have expected to touch, so a path nothing answers is an error that
// names it.
func TestDialExplicitSocketFails(t *testing.T) {
	socket := filepath.Join(sockDir(t), "nobody.sock")
	refuse := refuseToBuild(t, "--socket named a server, so a client that cannot reach it must not build one")

	d, code, err := dial(context.Background(), refuse,
		BuildOptions{Stderr: io.Discard}, dialOptions{Socket: socket}, "dial-test", false)
	if err == nil {
		d.Close()
		t.Fatal("--socket naming nothing has to fail rather than embed")
	}
	if d != nil {
		t.Fatalf("a failed dial returns no client: %+v", d)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(err.Error(), socket) {
		t.Fatalf("the error must name the socket: %v", err)
	}
}

// TestDialEmbedSkipsTheProbe: --embed is a client saying it wants its own server, which is
// only true if it holds however many daemons are answering.
func TestDialEmbedSkipsTheProbe(t *testing.T) {
	t.Chdir(t.TempDir())
	build := testBuilder(t, &fakeProvider{})
	env := runtimeEnv(sockDir(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, done, w := daemonOn(t, ctx, build, env)

	d, code, err := dial(context.Background(), build,
		BuildOptions{Stderr: io.Discard, Env: env, Home: t.TempDir()}, dialOptions{Embed: true}, "dial-test", false)
	if err != nil || code != 0 {
		t.Fatalf("dial = %d, %v; daemon stderr %q", code, err, w.String())
	}
	defer d.Close()
	if d.Built == nil {
		t.Fatal("--embed serves in this process whatever is answering the default socket")
	}

	cancel()
	if s := waitServed(t, done, w); s.code != 0 || s.err != nil {
		t.Fatalf("runServe = %d, %v", s.code, s.err)
	}
}

// TestDialRefusesSocketAndEmbedTogether: one flag names a server to attach to and the other
// says to be one. Picking a winner would make the losing flag silently mean nothing.
func TestDialRefusesSocketAndEmbedTogether(t *testing.T) {
	refuse := refuseToBuild(t, "a usage error is answered before anything is wired")
	d, code, err := dial(context.Background(), refuse, BuildOptions{Stderr: io.Discard},
		dialOptions{Socket: filepath.Join(sockDir(t), "s"), Embed: true}, "dial-test", false)
	if err == nil {
		d.Close()
		t.Fatal("--socket with --embed has to be refused")
	}
	if code != 2 {
		t.Fatalf("code = %d, want 2 for a usage error", code)
	}
	if !strings.Contains(err.Error(), "--socket") || !strings.Contains(err.Error(), "--embed") {
		t.Fatalf("the error must name both flags: %v", err)
	}
}

// TestPrintAttachedRunsATurnInTheDaemon drives the real --print path over a socket: the turn
// runs in the daemon's server, against the daemon's provider, and the session it opened is
// in the store that daemon holds. The builder fails the test if it is called, so nothing
// here can be an embedded run that happened to work.
func TestPrintAttachedRunsATurnInTheDaemon(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	fp := &fakeProvider{script: [][]provider.Part{say("ok")}}
	build := testBuilder(t, fp)
	socket := filepath.Join(sockDir(t), "s")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done, w := startServe(t, ctx, build, socket)

	refuse := refuseToBuild(t, "--socket attached, so --print must not wire a server of its own")
	var out bytes.Buffer
	code, err := runPrint(context.Background(), printOptions{Output: "json"}, dialOptions{Socket: socket},
		"hi", refuse, &out, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("runPrint = %d, %v; daemon stderr %q", code, err, w.String())
	}
	var res printResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if res.Result != "ok" {
		t.Fatalf("result = %q, want the daemon's provider answer", res.Result)
	}
	// The cost cell resolves the model, which an attached client can only do over
	// registry.list: there is no provider registry in this process.
	if res.Cost == "" {
		t.Fatalf("no cost in %+v: an attached client resolves the model over registry.list", res)
	}
	list, err := testStore(t).List()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range list {
		if s.ID.String() == res.SessionID {
			found = true
			if s.Workspace.Root != dir {
				t.Fatalf("the session opened on %q, want the client's cwd %q", s.Workspace.Root, dir)
			}
		}
	}
	if !found {
		t.Fatalf("session %s is not in the daemon's store: %+v", res.SessionID, list)
	}

	cancel()
	if s := waitServed(t, done, w); s.code != 0 || s.err != nil {
		t.Fatalf("runServe = %d, %v", s.code, s.err)
	}
}

// TestContinueAttachedFindsTheNewest: --continue has no store to read when the client is
// attached, so the newest session for this directory comes back over session.list and the
// same rule picks it. A child session is the trap: it is newer and sits on the same
// workspace, and resuming it would put the operator in a subagent's log.
func TestContinueAttachedFindsTheNewest(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	fp := &fakeProvider{script: [][]provider.Part{say("resumed")}}
	build := testBuilder(t, fp)
	st := testStore(t)
	first := openTestSession(t, st, dir)
	_ = first.Close()
	second := openTestSession(t, st, dir)
	_ = second.Close()
	child := openChildSession(t, st, dir, second.ID().String())
	_ = child.Close()

	socket := filepath.Join(sockDir(t), "s")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done, w := startServe(t, ctx, build, socket)

	refuse := refuseToBuild(t, "--socket attached, so --continue must not wire a server of its own")
	var out bytes.Buffer
	code, err := runPrint(context.Background(), printOptions{Output: "json", Continue: true},
		dialOptions{Socket: socket}, "hi", refuse, &out, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("runPrint = %d, %v; out %q; daemon stderr %q", code, err, out.String(), w.String())
	}
	var res printResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if res.SessionID != second.ID().String() {
		t.Fatalf("--continue resumed %s, want the newest non-child %s (first %s, child %s)",
			res.SessionID, second.ID(), first.ID(), child.ID())
	}

	cancel()
	if s := waitServed(t, done, w); s.code != 0 || s.err != nil {
		t.Fatalf("runServe = %d, %v", s.code, s.err)
	}
}

// TestLockedSessionPrintsTheSocketHint: a session another rudy holds is not a broken session,
// it is a session reachable through that process's socket. The message says how to reach it
// instead of reporting a lock the operator cannot do anything with.
func TestLockedSessionPrintsTheSocketHint(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	build := testBuilder(t, &fakeProvider{})
	holder, err := build(context.Background(), BuildOptions{Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = holder.Close(context.Background()) }()
	client, closeConn, err := serveInMemory(holder, "holder", false)
	if err != nil {
		t.Fatalf("serveInMemory: %v", err)
	}
	defer closeConn()
	var info protocol.SessionInfo
	if err := client.Call(callCtx(t), protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: dir}, &info); err != nil {
		t.Fatalf("open: %v", err)
	}

	var errb bytes.Buffer
	code, err := runPrint(context.Background(), printOptions{Output: "text", Resume: info.SessionID},
		dialOptions{Embed: true}, "hi", build, io.Discard, &errb)
	if err != nil {
		t.Fatalf("runPrint = %d, %v", code, err)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1; stderr %q", code, errb.String())
	}
	want := "session " + info.SessionID + " is held by another process; attach with --socket " + holder.Paths.Socket()
	if !strings.Contains(errb.String(), want) {
		t.Fatalf("stderr %q, want %q", errb.String(), want)
	}
}
