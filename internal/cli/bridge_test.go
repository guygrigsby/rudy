package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/protocol"
)

// The bridge is the one piece of the ssh transport that cannot be tested in process: it is a
// command ssh runs, and what it does is start another command and copy bytes between two
// pipes. So these tests build the binary and run it, which costs a compile and a daemon
// start; -short skips them and gets nothing else about the bridge.

// stopBoxDaemon uses the same authenticated protocol path as an operator. The command is
// idempotent, so cleanup stays safe when a test already stopped its daemon.
func stopBoxDaemon(t *testing.T, env []string) {
	t.Helper()
	home := ""
	for _, item := range env {
		if strings.HasPrefix(item, "HOME=") {
			home = strings.TrimPrefix(item, "HOME=")
			break
		}
	}
	if home == "" {
		t.Error("box environment has no HOME")
		return
	}
	cmd := exec.Command(filepath.Join(home, ".local", "bin", "rudy"), "bridge", "--stop")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("bridge --stop: %v\n%s", err, out)
	}
}

func TestBridgeNoStartExitsWhenNothingServes(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and starts a daemon")
	}
	bin := builtRudy(t)
	// A bare box: --no-start never gets as far as needing a provider.
	_, env := boxHome(t, bin)
	cmd := exec.Command(bin, "bridge", "--no-start")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("bridge --no-start with no daemon: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "no server") {
		t.Fatalf("bridge --no-start said %q, want it to name the missing server", out)
	}
}

func TestBridgeStopWithNoDaemonDoesNotLoadConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := builtRudy(t)
	home, env := boxHome(t, bin)
	configDir := filepath.Join(home, ".config", "rudy")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("[broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "bridge", "--stop")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bridge --stop with no daemon: %v\n%s", err, out)
	}
}

func TestBridgeStopIsIdempotentAndWaitsForSocketCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and starts a daemon")
	}
	bin := builtRudy(t)
	upstream := fakeOpenAI(t, "ok")
	_, env := boxWithProvider(t, bin, upstream.URL)
	start := exec.Command(bin, "bridge")
	start.Env = env
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start through bridge: %v\n%s", err, out)
	}
	stop := exec.Command(bin, "bridge", "--stop")
	stop.Env = env
	if out, err := stop.CombinedOutput(); err != nil {
		t.Fatalf("bridge --stop: %v\n%s", err, out)
	}
	if err := protocol.CheckSocketOwner(socketIn(t, env)); !errors.Is(err, protocol.ErrNoServer) {
		t.Fatalf("socket after bridge --stop = %v, want no server", err)
	}
	stop = exec.Command(bin, "bridge", "--stop")
	stop.Env = env
	if out, err := stop.CombinedOutput(); err != nil {
		t.Fatalf("second bridge --stop: %v\n%s", err, out)
	}
}

func TestBridgeStopRefusesALegacyDaemonWithoutStoppingIt(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := builtRudy(t)
	home, env := boxHome(t, bin)
	socket := socketIn(t, env)
	l, err := protocol.ListenUnix(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	served := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			served <- err
			return
		}
		defer func() { _ = conn.Close() }()
		raw, err := conn.Recv(context.Background())
		if err != nil {
			served <- err
			return
		}
		var hello protocol.Request
		if err := json.Unmarshal(raw, &hello); err != nil {
			served <- err
			return
		}
		response, _ := protocol.NewResponse(hello.ID, protocol.ClientHelloResult{Server: "rudy", Version: "old", Home: home})
		if err := conn.Send(context.Background(), response); err != nil {
			served <- err
			return
		}
		raw, err = conn.Recv(context.Background())
		if err != nil {
			served <- err
			return
		}
		var shutdown protocol.Request
		if err := json.Unmarshal(raw, &shutdown); err != nil {
			served <- err
			return
		}
		if shutdown.Method != protocol.MethodServerShutdown {
			served <- fmt.Errorf("method = %q", shutdown.Method)
			return
		}
		served <- conn.Send(context.Background(), protocol.NewErrorResponse(shutdown.ID, protocol.NewError(protocol.CodeNotFound, "unknown method server.shutdown", nil)))
	}()
	cmd := exec.Command(bin, "bridge", "--stop")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("bridge --stop against a legacy daemon = %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "unknown method server.shutdown") {
		t.Fatalf("bridge --stop said %q", out)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if err := protocol.CheckSocketOwner(socket); err != nil {
		t.Fatalf("legacy daemon socket was removed: %v", err)
	}
}

func TestBridgeStopRefusesBareEOFAfterShutdownAcceptance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := builtRudy(t)
	home, env := boxHome(t, bin)
	socket := socketIn(t, env)
	l, err := protocol.ListenUnix(socket)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			served <- err
			return
		}
		defer func() { _ = conn.Close() }()
		raw, err := conn.Recv(context.Background())
		if err != nil {
			served <- err
			return
		}
		var hello protocol.Request
		if err := json.Unmarshal(raw, &hello); err != nil {
			served <- err
			return
		}
		const instance = "accepted-instance"
		response, _ := protocol.NewResponse(hello.ID, protocol.ClientHelloResult{Server: "rudy", Version: "old", InstanceID: instance, Home: home})
		if err := conn.Send(context.Background(), response); err != nil {
			served <- err
			return
		}
		raw, err = conn.Recv(context.Background())
		if err != nil {
			served <- err
			return
		}
		var shutdown protocol.Request
		if err := json.Unmarshal(raw, &shutdown); err != nil {
			served <- err
			return
		}
		response, _ = protocol.NewResponse(shutdown.ID, protocol.ServerShutdownResult{InstanceID: instance, State: protocol.ServerStateShuttingDown})
		served <- conn.Send(context.Background(), response)
	}()
	cmd := exec.Command(bin, "bridge", "--stop")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("bridge --stop after bare EOF = %v, want exit 1\n%s", err, out)
	}
	if !strings.Contains(string(out), "server.stopped") {
		t.Fatalf("bridge --stop said %q, want missing server.stopped proof", out)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
}

func TestBridgeStartsADaemonAndCarriesTheHello(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and starts a daemon")
	}
	bin := builtRudy(t)
	upstream := fakeOpenAI(t, "ok")
	home, env := boxWithProvider(t, bin, upstream.URL)
	t.Cleanup(func() { stopBoxDaemon(t, env) })
	cmd := exec.Command(bin, "bridge")
	cmd.Env = env
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	conn := protocol.NewStreamConn(stdout, stdin, stdin)
	c := protocol.NewClient(conn)
	ctx, done := context.WithTimeout(context.Background(), 30*time.Second)
	defer done()
	var hello protocol.ClientHelloResult
	if err := c.Call(ctx, protocol.MethodClientHello, protocol.ClientHelloParams{Client: "test", Version: "0"}, &hello); err != nil {
		t.Fatalf("hello through the bridge: %v\nstderr: %s", err, stderr.String())
	}
	if hello.Home != home {
		t.Fatalf("hello.home = %q, want the box home %q", hello.Home, home)
	}
	if hello.Version != "v0.0.0-1-gtest001" {
		t.Fatalf("hello.version = %q", hello.Version)
	}
	if _, err := ulid.Parse(hello.InstanceID); err != nil {
		t.Fatalf("hello.instance_id = %q, want a ULID: %v", hello.InstanceID, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(socketIn(t, env)), "serve.pid")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("serve.pid exists after protocol-owned startup: %v", err)
	}
	instanceID := hello.InstanceID
	_ = c.Close()
	// The daemon the bridge started outlives the bridge: a second bridge joins it rather
	// than starting another, which the socket's lock would refuse anyway.
	second := exec.Command(bin, "bridge", "--no-start")
	second.Env = env
	sin, _ := second.StdinPipe()
	sout, _ := second.StdoutPipe()
	var secondErr bytes.Buffer
	second.Stderr = &secondErr
	if err := second.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Process.Kill(); _, _ = second.Process.Wait() })
	c2 := protocol.NewClient(protocol.NewStreamConn(sout, sin, sin))
	if err := c2.Call(ctx, protocol.MethodClientHello, protocol.ClientHelloParams{Client: "test", Version: "0"}, &hello); err != nil {
		t.Fatalf("second bridge did not join the running daemon: %v\nstderr: %s", err, secondErr.String())
	}
	if hello.InstanceID != instanceID {
		t.Fatalf("second bridge reached instance %s, want the running instance %s", hello.InstanceID, instanceID)
	}
	_ = c2.Close()
}

// TestTwoBridgesRacingBothGreetTheWinnersDaemon: on a cold box both bridges find nothing
// serving and both start a daemon. One binds the socket and the other exits exitSocketBusy
// in milliseconds, which is the winner arriving and not a failure: the loser's client is
// attached to a healthy daemon and must not be told its connection died with the exit status
// of a process that never served it.
func TestTwoBridgesRacingBothGreetTheWinnersDaemon(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and starts a daemon")
	}
	bin := builtRudy(t)
	upstream := fakeOpenAI(t, "ok")
	home, env := boxWithProvider(t, bin, upstream.URL)
	t.Cleanup(func() { stopBoxDaemon(t, env) })
	ctx, done := context.WithTimeout(context.Background(), 60*time.Second)
	defer done()

	type runner struct {
		cmd    *exec.Cmd
		stdin  io.WriteCloser
		client *protocol.Client
		errb   *bytes.Buffer
		wait   chan error
	}
	// Started back to back with nothing between them, so both dial the cold socket before
	// either child has bound it. That is the race; a start ordered by a wait would not be one.
	const bridges = 2
	rs := make([]*runner, 0, bridges)
	for range bridges {
		cmd := exec.Command(bin, "bridge")
		cmd.Env = env
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var errb bytes.Buffer
		cmd.Stderr = &errb
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
		r := &runner{cmd: cmd, stdin: stdin, errb: &errb, wait: make(chan error, 1)}
		r.client = protocol.NewClient(protocol.NewStreamConn(stdout, stdin, stdin))
		go func() { r.wait <- cmd.Wait() }()
		rs = append(rs, r)
	}

	for i, r := range rs {
		var hello protocol.ClientHelloResult
		if err := r.client.Call(ctx, protocol.MethodClientHello, protocol.ClientHelloParams{Client: "test", Version: "0"}, &hello); err != nil {
			t.Fatalf("bridge %d hello: %v\nstderr: %s", i, err, r.errb.String())
		}
		// Both greeted the same daemon, whichever one won: one box, one home.
		if hello.Home != home {
			t.Fatalf("bridge %d hello.home = %q, want %q", i, hello.Home, home)
		}
	}

	// Closing stdin is the client hanging up, which is the one thing that ends a bridge whose
	// daemon is healthy. Both must go quietly: the loser's dead child is not its client's news.
	for i, r := range rs {
		_ = r.client.Close()
		_ = r.stdin.Close()
		select {
		case err := <-r.wait:
			if err != nil {
				t.Errorf("bridge %d exited %v, want a clean exit; stderr: %s", i, err, r.errb.String())
			}
		case <-time.After(30 * time.Second):
			t.Errorf("bridge %d did not exit; stderr: %s", i, r.errb.String())
		}
	}
}

// TestBridgeWaitsOutADaemonThatLostTheSocket is the losing half of the race on purpose. Two
// bridges started together almost always have the winner bound before the loser's redial
// comes round, so the path where the loser's child exits first is a coin toss and not
// something TestTwoBridgesRacing can promise to cover. Holding the socket's lock without
// binding the socket produces exactly that state to order: nothing answers a dial, and a
// daemon started into it cannot take the lock and exits exitSocketBusy. The bridge must read
// that as the winner being on its way and keep dialing, because the alternative is a client
// told there is no daemon at the moment another bridge is starting one.
func TestBridgeWaitsOutADaemonThatLostTheSocket(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and starts a daemon")
	}
	bin := builtRudy(t)
	upstream := fakeOpenAI(t, "ok")
	home, env := boxWithProvider(t, bin, upstream.URL)
	socket := socketIn(t, env)
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatal(err)
	}
	// protocol.lockPath: the lock a server holds is the socket path plus .lock, in the same
	// 0700 directory. Taking it here is the whole point of the test, and if that convention
	// ever moves, the wait below stops seeing a losing daemon and says so.
	lock, err := os.OpenFile(socket+".lock", os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("take the socket lock: %v", err)
	}

	cmd := exec.Command(bin, "bridge")
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	// The daemon's stderr goes to the box's log, so the line it prints on a busy socket is
	// how the test knows the child has lost and exited rather than merely been started.
	logFile := filepath.Join(home, ".cache", "rudy", "rudy.log")
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(readAll(logFile), "a server is already serving") {
		if time.Now().After(deadline) {
			t.Fatalf("no daemon reported a busy socket within the wait; log:\n%s\nbridge stderr: %s", readAll(logFile), errb.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Still up: a bridge that took its child's exit for a verdict would be gone by now.
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("bridge exited when its daemon lost the socket: %v; stderr: %s", err, errb.String())
	}

	// The winner arrives: release the lock and serve the socket the bridge is still dialing.
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	winner := exec.Command(bin, "serve")
	winner.Env = env
	winner.Stdout, winner.Stderr = io.Discard, io.Discard
	if err := winner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = winner.Process.Kill(); _, _ = winner.Process.Wait() })

	ctx, done := context.WithTimeout(context.Background(), bridgeStartBudget)
	defer done()
	c := protocol.NewClient(protocol.NewStreamConn(stdout, stdin, stdin))
	var hello protocol.ClientHelloResult
	if err := c.Call(ctx, protocol.MethodClientHello, protocol.ClientHelloParams{Client: "test", Version: "0"}, &hello); err != nil {
		t.Fatalf("the bridge did not join the daemon that won the socket: %v\nstderr: %s", err, errb.String())
	}
	if hello.Home != home {
		t.Fatalf("hello.home = %q, want %q", hello.Home, home)
	}
	_ = c.Close()
}

// readAll is a file's contents, or "" for a file that is not there yet. The log this reads is
// written by another process and polled for a line, so absent and empty are the same thing.
func readAll(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// TestBridgeReportsADaemonThatCannotServe: a box with no provider cannot build a daemon, and
// the bridge finds out when the child exits rather than by waiting out its start budget. The
// operator gets the provider error the daemon wrote to the log, not "nothing answered".
func TestBridgeReportsADaemonThatCannotServe(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and starts a daemon")
	}
	bin := builtRudy(t)
	_, env := boxHome(t, bin) // bare: no provider, so the daemon's build fails
	cmd := exec.Command(bin, "bridge")
	cmd.Env = env
	// Stdin is held open for the life of the bridge, which is what ssh does: closing it is a
	// client that hung up, and the bridge is right to exit 0 on that before the daemon has
	// decided anything.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	start := time.Now()
	// The daemon finds out it has no provider in milliseconds: nothing is dialed when no
	// provider is configured. So this bounds the one thing that could go wrong, the bridge
	// waiting out bridgeStartBudget on a child it has already buried.
	select {
	case err = <-done:
	case <-time.After(bridgeStartBudget - 5*time.Second):
		t.Fatalf("bridge never returned; it said %q", out.String())
	}
	took := time.Since(start)
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("bridge on a box with no provider: %v (after %s)\n%s", err, took, out.String())
	}
	if !strings.Contains(out.String(), "rudy serve exited") {
		t.Fatalf("bridge said %q, want it to say the daemon exited", out.String())
	}
	if !strings.Contains(out.String(), "no model provider is configured") {
		t.Fatalf("bridge said %q, want the provider error from the daemon's log", out.String())
	}
}
