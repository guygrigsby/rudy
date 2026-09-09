package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

// serveTestBudget bounds every wait in these tests. It is longer than
// serveShutdownBudget so a shutdown that runs past its own budget shows up as the exit
// code it returns rather than as a test that hangs.
const serveTestBudget = 20 * time.Second

// sockDir is a short directory to put a test socket in. A unix socket path is 104 bytes on
// darwin and t.TempDir() under /var/folders spends most of that before the test has named a
// file, so the sockets these tests serve on cannot live beside the rest of the fixture.
func sockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rudy-cli-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// watcher is the stderr runServe writes to, with a channel that closes the moment a wanted
// string has been written. Polling a buffer would race the goroutine writing it; this way
// the test blocks on the line itself, which is exactly what "serving on" is printed before
// the accept loop for.
type watcher struct {
	want string
	seen chan struct{}

	mu   sync.Mutex
	buf  bytes.Buffer
	once sync.Once
}

func newWatcher(want string) *watcher {
	return &watcher{want: want, seen: make(chan struct{})}
}

func (w *watcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.want) {
		w.once.Do(func() { close(w.seen) })
	}
	return n, err
}

func (w *watcher) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// served is what a runServe goroutine reports when it returns.
type served struct {
	code int
	err  error
}

// startServe runs runServe on socket and blocks until it says it is serving there. The
// returned channel carries its exit code once the caller has cancelled its context.
func startServe(t *testing.T, ctx context.Context, build buildFunc, socket string) (<-chan served, *watcher) {
	t.Helper()
	w := newWatcher("serving on " + socket)
	done := make(chan served, 1)
	go func() {
		code, err := runServe(ctx, build, socket, io.Discard, w)
		done <- served{code, err}
	}()
	select {
	case <-w.seen:
	case s := <-done:
		t.Fatalf("runServe returned %d before it served: %v; stderr %q", s.code, s.err, w.String())
	case <-time.After(serveTestBudget):
		t.Fatalf("no serving line after %s; stderr %q", serveTestBudget, w.String())
	}
	return done, w
}

// waitServed takes the exit code, or fails the test if runServe never returns.
func waitServed(t *testing.T, done <-chan served, w *watcher) served {
	t.Helper()
	select {
	case s := <-done:
		return s
	case <-time.After(serveTestBudget):
		t.Fatalf("runServe did not return after %s; stderr %q", serveTestBudget, w.String())
		return served{}
	}
}

// awaitLine blocks until the command says it is serving, or fails the test with whatever it
// returned instead.
func awaitLine(t *testing.T, w *watcher, done <-chan error) {
	t.Helper()
	select {
	case <-w.seen:
	case err := <-done:
		t.Fatalf("rudy serve returned before it served: %v; stderr %q", err, w.String())
	case <-time.After(serveTestBudget):
		t.Fatalf("no serving line after %s; stderr %q", serveTestBudget, w.String())
	}
}

// callCtx bounds one request. Nothing a test asks a served socket for should take longer
// than the budget, and a bounded call is what turns a server that never answers into a
// failure with a message instead of a package that runs to the timeout.
func callCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), serveTestBudget)
	t.Cleanup(cancel)
	return ctx
}

// dialServe connects to the socket, greets the server and opens a session on a scratch
// workspace, the way any client of rudy serve starts.
func dialServe(t *testing.T, socket string) (*protocol.Client, protocol.SessionInfo) {
	t.Helper()
	conn, err := protocol.DialUnix(context.Background(), socket, serveTestBudget)
	if err != nil {
		t.Fatalf("dial %s: %v", socket, err)
	}
	client := protocol.NewClient(conn)
	t.Cleanup(func() { _ = client.Close() })
	var hello protocol.ClientHelloResult
	if err := client.Call(callCtx(t), protocol.MethodClientHello,
		protocol.ClientHelloParams{Client: "serve-test", Version: "test"}, &hello); err != nil {
		t.Fatalf("hello: %v", err)
	}
	var info protocol.SessionInfo
	if err := client.Call(callCtx(t), protocol.MethodSessionOpen,
		protocol.SessionOpenParams{Cwd: t.TempDir()}, &info); err != nil {
		t.Fatalf("open: %v", err)
	}
	return client, info
}

// submitOne sends one prompt and returns the turn it started.
func submitOne(t *testing.T, client *protocol.Client, sessionID, prompt string) string {
	t.Helper()
	var res protocol.SessionSubmitResult
	if err := client.Call(callCtx(t), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: sessionID, Content: []session.Block{session.TextBlock(prompt)}, Source: session.SourceTyped,
	}, &res); err != nil {
		t.Fatalf("submit: %v", err)
	}
	return res.TurnID
}

// awaitState blocks until the turn reaches want.
func awaitState(t *testing.T, client *protocol.Client, turnID, want string) {
	t.Helper()
	for {
		select {
		case n, ok := <-client.Notifications():
			if !ok {
				t.Fatalf("the server closed the connection before turn.state %s", want)
			}
			if n.Method != protocol.NotifyTurnState {
				continue
			}
			var ts protocol.TurnStateChanged
			if err := json.Unmarshal(n.Params, &ts); err != nil {
				t.Fatalf("turn.state: %v", err)
			}
			if ts.TurnID == turnID && ts.State == want {
				return
			}
		case <-time.After(serveTestBudget):
			t.Fatalf("no turn.state %s after %s", want, serveTestBudget)
		}
	}
}

// TestServeListensAndServesAClient is the whole command in one pass: a client that dialed
// the socket gets a turn out of the server behind it, and the signal that ends the process
// leaves neither an exit code nor a socket file behind.
func TestServeListensAndServesAClient(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{script: [][]provider.Part{say("ok")}}
	build := testBuilder(t, fp)
	socket := filepath.Join(sockDir(t), "s")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done, w := startServe(t, ctx, build, socket)
	client, info := dialServe(t, socket)
	awaitState(t, client, submitOne(t, client, info.SessionID, "hi"), "completed")

	cancel()
	s := waitServed(t, done, w)
	if s.code != 0 || s.err != nil {
		t.Fatalf("runServe = %d, %v; stderr %q", s.code, s.err, w.String())
	}
	if _, err := os.Stat(socket); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the socket is still there after shutdown: %v", err)
	}
}

// TestServeCommandServesTheSocketFlag: the command is registered on the root and its
// --socket is the path it serves on, which is the whole difference between a daemon an
// operator can place where they like and one that only ever binds the default.
func TestServeCommandServesTheSocketFlag(t *testing.T) {
	t.Chdir(t.TempDir())
	build := testBuilder(t, &fakeProvider{})
	socket := filepath.Join(sockDir(t), "s")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := newWatcher("serving on " + socket)
	root := newRoot("test", build)
	root.SetOut(io.Discard)
	root.SetErr(w)
	root.SetArgs([]string{"serve", "--socket", socket})
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	awaitLine(t, w, done)
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("nothing bound at --socket: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("rudy serve = %v; stderr %q", err, w.String())
		}
	case <-time.After(serveTestBudget):
		t.Fatalf("rudy serve did not return after %s; stderr %q", serveTestBudget, w.String())
	}
}

// TestServeStopsOnASignal: a daemon is stopped by a signal, from a service manager as often
// as from a Ctrl-C, so the signal has to be the same clean shutdown a cancelled context is.
// SIGTERM is the one this command added; SIGINT rides the same NotifyContext call. Nothing
// mocks the delivery: the signal is sent to this process, which is exactly what would kill
// the test binary if the handler were not installed.
func TestServeStopsOnASignal(t *testing.T) {
	t.Chdir(t.TempDir())
	build := testBuilder(t, &fakeProvider{})
	socket := filepath.Join(sockDir(t), "s")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done, w := startServe(t, ctx, build, socket)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	if s := waitServed(t, done, w); s.code != 0 || s.err != nil {
		t.Fatalf("runServe = %d, %v; stderr %q", s.code, s.err, w.String())
	}
	if _, err := os.Stat(socket); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the socket is still there after the signal: %v", err)
	}
}

// shrinkShutdownBudget makes the shutdown budget too small to finish in, and puts it back
// afterwards. What a daemon does when the budget runs out is behavior; ten seconds of
// waiting for it is not a test.
func shrinkShutdownBudget(t *testing.T) time.Duration {
	t.Helper()
	old := serveShutdownBudget
	t.Cleanup(func() { serveShutdownBudget = old })
	serveShutdownBudget = time.Nanosecond
	return serveShutdownBudget
}

// TestServeOverBudgetShutdownExitsOne: Server.Shutdown bounds its wait on the context it is
// given and reports only what closing the sessions produced, so a shutdown that abandoned a
// turn still writing comes back nil. An operator whose daemon dropped work mid-flight has to
// hear about it, in the exit code a service manager reads and in a line naming the budget.
func TestServeOverBudgetShutdownExitsOne(t *testing.T) {
	t.Chdir(t.TempDir())
	budget := shrinkShutdownBudget(t)
	build := testBuilder(t, &fakeProvider{block: true})
	socket := filepath.Join(sockDir(t), "s")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := newWatcher("serving on " + socket)
	root := newRoot("test", build)
	root.SetOut(io.Discard)
	root.SetErr(w)
	root.SetArgs([]string{"serve", "--socket", socket})
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	awaitLine(t, w, done)

	client, info := dialServe(t, socket)
	awaitState(t, client, submitOne(t, client, info.SessionID, "hi"), "streaming")

	cancel()
	select {
	case err := <-done:
		if got := exitCode(t, err); got != 1 {
			t.Fatalf("rudy serve = %d, want 1; stderr %q", got, w.String())
		}
	case <-time.After(serveTestBudget):
		t.Fatalf("rudy serve did not return after %s; stderr %q", serveTestBudget, w.String())
	}
	if want := "shutdown ran past " + budget.String(); !strings.Contains(w.String(), want) {
		t.Fatalf("stderr %q, want %q", w.String(), want)
	}
}

// TestServeReleasesTheSocketWhenTheBuildFails: the listen now happens first, so a build that
// fails after it leaves a bound socket and a held lock behind unless the failure path gives
// them back. Nothing would serve that socket, and the next rudy would find the path busy for
// as long as this process took to exit.
func TestServeReleasesTheSocketWhenTheBuildFails(t *testing.T) {
	socket := filepath.Join(sockDir(t), "s")
	fail := func(context.Context, BuildOptions) (*Built, error) {
		return nil, errors.New("no provider reachable")
	}
	var errb bytes.Buffer
	code, err := runServe(context.Background(), fail, socket, io.Discard, &errb)
	if code != 1 || err == nil {
		t.Fatalf("runServe = %d, %v; want 1 and the build error", code, err)
	}
	if _, serr := os.Stat(socket); !errors.Is(serr, fs.ErrNotExist) {
		t.Fatalf("the socket is still bound after a failed build: %v", serr)
	}
	l, lerr := protocol.ListenUnix(socket)
	if lerr != nil {
		t.Fatalf("the path is still locked after a failed build: %v", lerr)
	}
	_ = l.Close()
}

// TestServeSocketFollowsTheBuildOptions: the default socket is resolved off the same
// environment and home the build runs under. Reading the process environment here instead
// would have a command whose options name their own environment listen on one path and tell
// its clients to attach through another, and under a test builder that is the developer's
// own socket.
func TestServeSocketFollowsTheBuildOptions(t *testing.T) {
	base := t.TempDir()
	run := filepath.Join(base, "run")
	o := BuildOptions{
		Env: func(key string) string {
			if key == "XDG_RUNTIME_DIR" {
				return run
			}
			return ""
		},
		Home: base,
	}
	got, err := serveSocket("", o)
	if err != nil {
		t.Fatalf("serveSocket: %v", err)
	}
	if want := filepath.Join(run, "rudy", "rudy.sock"); got != want {
		t.Fatalf("default socket = %q, want %q", got, want)
	}
	// An operator who named a socket gets that one whatever the environment says.
	named := filepath.Join(base, "named.sock")
	if got, err := serveSocket(named, o); err != nil || got != named {
		t.Fatalf("serveSocket(%q) = %q, %v", named, got, err)
	}
}

// TestServeRefusesABusySocket: the socket is what makes one rudy the one every client
// reaches, so a second server on the same path is refused rather than left to split the
// client population between two stores.
func TestServeRefusesABusySocket(t *testing.T) {
	t.Chdir(t.TempDir())
	build := testBuilder(t, &fakeProvider{})
	socket := filepath.Join(sockDir(t), "s")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done, w := startServe(t, ctx, build, socket)

	// The second server is given a builder it must never call: a socket somebody else holds
	// is answered before a plugin is loaded or a registry refreshed, not after.
	refuse := func(context.Context, BuildOptions) (*Built, error) {
		t.Error("rudy serve wired a server before it had the socket")
		return nil, errors.New("must not build")
	}
	var errb bytes.Buffer
	code, err := runServe(context.Background(), refuse, socket, io.Discard, &errb)
	if code != 1 || err != nil {
		t.Fatalf("second runServe = %d, %v; stderr %q", code, err, errb.String())
	}
	if want := "a server is already serving " + socket; !strings.Contains(errb.String(), want) {
		t.Fatalf("stderr %q, want %q", errb.String(), want)
	}

	cancel()
	if s := waitServed(t, done, w); s.code != 0 || s.err != nil {
		t.Fatalf("first runServe = %d, %v", s.code, s.err)
	}
}

// TestServeShutdownInterruptsARunningTurn: a signal arriving mid-turn is not a kill. The
// turn is cancelled and says so in its own log, so the next client to resume the session
// finds a record that ends where the work stopped, and the process still exits 0.
func TestServeShutdownInterruptsARunningTurn(t *testing.T) {
	t.Chdir(t.TempDir())
	build := testBuilder(t, &fakeProvider{block: true})
	socket := filepath.Join(sockDir(t), "s")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done, w := startServe(t, ctx, build, socket)
	client, info := dialServe(t, socket)
	awaitState(t, client, submitOne(t, client, info.SessionID, "hi"), "streaming")

	cancel()
	if s := waitServed(t, done, w); s.code != 0 || s.err != nil {
		t.Fatalf("runServe = %d, %v; stderr %q", s.code, s.err, w.String())
	}

	id, err := ulid.Parse(info.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	// Load rather than the notifications: the entry has to be in the log the next process
	// reads, and the load also proves the shutdown let go of the session's lock.
	s, err := session.Load(testStore(t), id)
	if err != nil {
		t.Fatalf("load %s: %v", info.SessionID, err)
	}
	defer func() { _ = s.Close() }()
	entries := s.Entries()
	last := entries[len(entries)-1]
	ti, ok := last.Payload.(session.TurnInterrupted)
	if !ok {
		t.Fatalf("the log ends with %s, want turn_interrupted", last.Payload.Kind())
	}
	if ti.How != session.InterruptCancel {
		t.Fatalf("turn_interrupted how = %s, want %s", ti.How, session.InterruptCancel)
	}
}
