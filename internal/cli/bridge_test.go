package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/protocol"
)

// The bridge is the one piece of the ssh transport that cannot be tested in process: it is a
// command ssh runs, and what it does is start another command and copy bytes between two
// pipes. So these tests build the binary and run it, which costs a compile and a daemon
// start; -short skips them and gets nothing else about the bridge.

// stopBoxDaemon kills the daemon a bridge started, by the pid file the bridge told it to
// write. Without it a `go test` run leaves a rudy serve behind holding a socket and a store.
// The path comes off socketIn, which resolves the box's own environment through the code
// that resolves it: a path assembled here would be this test's guess at the default.
func stopBoxDaemon(t *testing.T, env []string) {
	t.Helper()
	pidFile := filepath.Join(filepath.Dir(socketIn(t, env)), "serve.pid")
	// A missing file is a failure, not a nothing-to-do: the bridge passed --pidfile, so a
	// daemon that served without writing one is a daemon nobody can stop, here or in rudy
	// hosts install. The test that leaks it would otherwise pass.
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Errorf("read %s: %v", pidFile, err)
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Errorf("pid file %s holds %q: %v", pidFile, data, err)
		return
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Errorf("kill %d: %v", pid, err)
	}
	// The daemon unwinds its sessions before it goes; waiting for the pid to clear keeps the
	// next test from finding the socket still bound.
	for range 100 {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("daemon %d still running after SIGTERM", pid)
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
	_ = c2.Close()
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
	// The floor here is the daemon's own defaultRefreshTimeout, not anything the bridge
	// waits for: the daemon spends 20s finding out it has no provider and only then exits.
	// What this bounds is the bridge adding its start budget on top of that.
	select {
	case err = <-done:
	case <-time.After(90 * time.Second):
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
	if !strings.Contains(out.String(), "no provider registered") {
		t.Fatalf("bridge said %q, want the provider error from the daemon's log", out.String())
	}
}
