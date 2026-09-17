// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/guygrigsby/rudy/internal/protocol"
)

// bridgeStartBudget bounds how long the bridge waits for a daemon it started to answer:
// the daemon binds its socket before it builds, so the wait is a plugin load and a
// registry refresh, not a network. A daemon that gives up before then is not waited out;
// the bridge holds its child and hears the exit.
var bridgeStartBudget = 30 * time.Second

// daemonLogTailLines is how much of the log a failed start is reported with. Enough for the
// notices Build prints on its way to the error, and short enough that an operator reading it
// through ssh's stderr still sees the error at the bottom.
const daemonLogTailLines = 20

// newBridgeCommand is the box side of --host: ssh runs it, it finds or starts the daemon
// and copies messages between ssh's stdio and the socket. A top-level verb like serve,
// for the same reason: it is the process the transport is made of.
func newBridgeCommand(build buildFunc) *cobra.Command {
	var noStart, stopBridge bool
	cmd := &cobra.Command{
		Use:    "bridge",
		Short:  "carry the protocol between stdio and the local daemon, starting one if needed",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			code, err := runBridge(cmd.Context(), BuildOptions{Stderr: cmd.ErrOrStderr()}, noStart, stopBridge, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), err)
				if code == 0 {
					code = 1
				}
			}
			if code != 0 {
				return ExitError{code}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noStart, "no-start", false, "fail when no daemon answers instead of starting one")
	cmd.Flags().BoolVar(&stopBridge, "stop", false, "stop an answering daemon through the protocol")
	return cmd
}

// runBridge dials the default socket, starts rudy serve when nothing answers (unless
// noStart) and then copies messages both ways until either side ends. It never reads or
// interprets a message: the daemon's peer-uid check and the client's hello both happen
// through it, not in it.
func runBridge(ctx context.Context, o BuildOptions, noStart, stop bool, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if noStart && stop {
		return 2, errors.New("--no-start and --stop are mutually exclusive")
	}
	if stop {
		return runBridgeStop(ctx, o)
	}
	paths, cfg, err := localConfig(o)
	if err != nil {
		return 1, err
	}
	socket := paths.Socket()
	conn, exited, err := dialOrStart(ctx, socket, cfg.Log.File, noStart)
	if err != nil {
		return 1, err
	}
	defer func() { _ = conn.Close() }()
	slog.Info("bridge: connect", "socket", socket)
	up := protocol.NewStreamConn(stdin, stdout, nil)
	return copyBoth(ctx, up, conn, exited, cfg.Log.File, stderr)
}

func runBridgeStop(ctx context.Context, o BuildOptions) (int, error) {
	stopCtx, cancel := context.WithTimeout(ctx, bridgeStartBudget)
	defer cancel()
	socket, err := serveSocket("", o)
	if err != nil {
		return 1, err
	}
	if err := protocol.CheckSocketOwner(socket); err != nil {
		if errors.Is(err, protocol.ErrNoServer) {
			return 0, nil
		}
		return 1, err
	}
	conn, err := protocol.DialUnix(stopCtx, socket, attachTimeout)
	if err != nil {
		if errors.Is(err, protocol.ErrNoServer) {
			return 0, nil
		}
		return 1, err
	}
	client := protocol.NewClient(conn)
	defer func() { _ = client.Close() }()
	helloCtx, cancelHello := context.WithTimeout(stopCtx, greetTimeout)
	hello, err := greet(helloCtx, client, "rudy-bridge-stop", Version(), false)
	cancelHello()
	if err != nil {
		return 1, err
	}
	if err := requestDaemonShutdown(stopCtx, client, hello.InstanceID); err != nil {
		return 1, fmt.Errorf("stop daemon: %w", err)
	}
	return 0, nil
}

func requestDaemonShutdown(ctx context.Context, client *protocol.Client, instanceID string) error {
	var result protocol.ServerShutdownResult
	if err := client.Call(ctx, protocol.MethodServerShutdown, struct{}{}, &result); err != nil {
		return err
	}
	if result.InstanceID != instanceID || result.State != protocol.ServerStateShuttingDown {
		return fmt.Errorf("server answered for instance %q in state %q, want instance %q shutting_down", result.InstanceID, result.State, instanceID)
	}
	return waitForDaemonStop(ctx, client, instanceID)
}

// waitForDaemonStop requires an explicit terminal proof before EOF. EOF alone can be a
// process crash, transport loss or failed cleanup and must never authorize a replacement.
func waitForDaemonStop(ctx context.Context, client *protocol.Client, instanceID string) error {
	for {
		select {
		case note, ok := <-client.Notifications():
			if !ok {
				return errors.New("daemon connection reached EOF without server.stopped proof")
			}
			if note.Method != protocol.NotifyServerStopped {
				continue
			}
			var stopped protocol.ServerStoppedParams
			if err := json.Unmarshal(note.Params, &stopped); err != nil {
				return fmt.Errorf("decode server.stopped: %w", err)
			}
			if stopped.InstanceID != instanceID || stopped.State != protocol.ServerStateStopped {
				return fmt.Errorf("server.stopped answered for instance %q in state %q, want instance %q stopped", stopped.InstanceID, stopped.State, instanceID)
			}
			if err := client.Wait(ctx); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// dialOrStart is the attach-or-spawn step. ErrNoServer is the one error that means start;
// anything else (a refused owner, a busy path with no answer) is reported as it is. The
// second return is the exit of a daemon this call started, nil when it attached to one that
// was already there: nothing this process did can end a daemon it does not own.
func dialOrStart(ctx context.Context, socket, logFile string, noStart bool) (protocol.Conn, <-chan error, error) {
	if err := protocol.CheckSocketOwner(socket); err != nil && !errors.Is(err, protocol.ErrNoServer) {
		return nil, nil, err
	}
	conn, err := protocol.DialUnix(ctx, socket, attachTimeout)
	if err == nil {
		return conn, nil, nil
	}
	if !errors.Is(err, protocol.ErrNoServer) {
		return nil, nil, err
	}
	if noStart {
		return nil, nil, fmt.Errorf("no server on %s and --no-start given; run rudy serve, or drop --no-start", socket)
	}
	exited, err := spawnDaemon(socket, logFile)
	if err != nil {
		return nil, nil, err
	}
	slog.Info("bridge: daemon started", "socket", socket)
	deadline := time.Now().Add(bridgeStartBudget)
	wait := 50 * time.Millisecond
	for time.Now().Before(deadline) {
		conn, err = protocol.DialUnix(ctx, socket, attachTimeout)
		if err == nil {
			return conn, exited, nil
		}
		if !errors.Is(err, protocol.ErrNoServer) {
			return nil, nil, err
		}
		// Checked after the dial and before the sleep, so a daemon that gave up on a missing
		// provider or a config it could not parse is reported the moment it goes rather than
		// at the end of a budget that was sized for a slow start. The dial comes first because
		// a daemon that served and then exited is still a daemon that served.
		select {
		case werr := <-exited:
			// exitSocketBusy is the other bridge winning the race, which is a daemon arriving
			// and not a daemon failing: keep dialing until the winner finishes building. The
			// child is gone either way, so there is no exit left to consult and exited goes
			// nil, which is how an attached bridge already reads.
			if exitCodeOf(werr) != exitSocketBusy {
				return nil, nil, daemonExited(werr, logFile)
			}
			slog.Info("bridge: another daemon holds the socket; waiting for it", "socket", socket)
			exited = nil
		default:
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(wait):
		}
		if wait < time.Second {
			wait *= 2
		}
	}
	return nil, nil, fmt.Errorf("started rudy serve but nothing answered on %s within %s; see %s", socket, bridgeStartBudget, logFile)
}

// daemonExitGrace is how long a daemon-side disconnect waits for the child's exit status. The
// child has already gone by then, so the wait is a wait4 landing and nothing else; the bound
// is there for the case where the daemon closed one connection and stayed up.
const daemonExitGrace = time.Second

// exitCodeOf is the process exit code an error from cmd.Wait carries, or -1 for an error that
// is not one: a signal, or a failure to run the thing at all.
func exitCodeOf(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// daemonStatus takes the exit of a daemon this process started, or reports that none arrived
// within grace. A nil channel is a daemon this process attached to rather than started, and
// it never has an exit to report, so it does not wait at all.
func daemonStatus(exited <-chan error, grace time.Duration) (error, bool) {
	if exited == nil {
		return nil, false
	}
	select {
	case werr := <-exited:
		return werr, true
	case <-time.After(grace):
		return nil, false
	}
}

// daemonExited is what an operator gets when the daemon the bridge started did not live long
// enough to answer. The exit status alone says nothing about why, and the reason is in the
// log the daemon's stderr was pointed at, on the far side of an ssh connection nobody is
// going to go and read; so the tail comes back with the error.
func daemonExited(werr error, logFile string) error {
	if werr == nil {
		werr = errors.New("exit status 0")
	}
	return fmt.Errorf("rudy serve exited: %v; last lines of %s:\n%s", werr, logFile, logTail(logFile, daemonLogTailLines))
}

// logTail is the last n lines of a file, or a parenthesised reason there are none. It reads
// from the end: a log that has been appended to for a month is not read into memory to
// report twenty lines of it.
func logTail(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return "(" + err.Error() + ")"
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return "(" + err.Error() + ")"
	}
	const maxTailBytes = 64 << 10
	off, size := int64(0), fi.Size()
	if size > maxTailBytes {
		off = size - maxTailBytes
	}
	buf := make([]byte, size-off)
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return "(" + err.Error() + ")"
	}
	text := strings.TrimRight(string(buf), "\n")
	if text == "" {
		return "(" + path + " is empty)"
	}
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// spawnDaemon runs this binary's serve detached: its own session so ssh ending does not
// take it, stdio on the log file so a plugin's complaint has somewhere to go. Two bridges
// racing both get here; the loser's serve exits exitSocketBusy and the loser's dial loop
// finds the winner.
//
// The child is kept rather than released, and its exit comes back on the returned channel.
// Setsid is what makes the daemon outlive this process, so holding the child costs nothing
// and buys the one thing a start budget cannot give: knowing that waiting longer is pointless.
func spawnDaemon(socket, logFile string) (<-chan error, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(logFile), 0o700); err != nil {
		return nil, err
	}
	out, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = out.Close() }()
	cmd := exec.Command(self, "serve", "--socket", socket)
	cmd.Stdout = out
	cmd.Stderr = out
	// Not this process's stdin: that is the protocol stream from the client, and a daemon
	// holding the other end of it would be reading the bridge's own traffic.
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start rudy serve: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return exited, nil
}

// copyBoth moves messages in both directions and returns when either side ends. The
// daemon closing is exit 0 with a line on stderr; the client closing is exit 0 silently.
//
// It watches the daemon's exit as well as the two streams because rudy serve binds its
// socket before it builds: the connect succeeds against a daemon that is still loading
// plugins, so a daemon that then gives up (no provider, a config it cannot parse) would
// otherwise reach the client as a stream that closed for no stated reason. A daemon that
// exits non-zero is that failure and is reported with its log; one that exits 0 shut down,
// which the copy loops report as the EOF it is. exited is nil when this process attached to
// a daemon it did not start, and a nil channel is a case that never fires.
func copyBoth(ctx context.Context, up, down protocol.Conn, exited <-chan error, logFile string, stderr io.Writer) (int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// failure carries which end produced it, because that is what decides whether the daemon's
	// exit status is worth waiting a moment for. A client that hung up says nothing about the
	// daemon, and a bridge that paused on every one of those would pause on the common case.
	type failure struct {
		daemon bool
		err    error
	}
	errc := make(chan failure, 2)
	pipe := func(from, to protocol.Conn, fromDaemon bool) {
		for {
			msg, err := from.Recv(ctx)
			if err != nil {
				errc <- failure{fromDaemon, err}
				return
			}
			if err := to.Send(ctx, msg); err != nil {
				errc <- failure{!fromDaemon, err}
				return
			}
		}
	}
	go pipe(up, down, false)
	go pipe(down, up, true)
	// Only a stream ending ends this, never the child exiting: a bridge whose own serve lost
	// the start race has a dead child and a perfectly good connection to the winner, and a
	// select that watched the exit would fail that connection on the spot.
	f := <-errc
	cancel()
	if f.daemon {
		// The socket closing and the daemon exiting are one event seen twice, and they race: a
		// daemon that gave up during its build closes the connection it had already been dialed
		// on in the same instant it goes. The exit is the half that carries the reason, so it
		// gets a moment to land before a closed stream is called a clean end. exitSocketBusy is
		// not a reason for anything: that child never served this connection.
		if werr, ok := daemonStatus(exited, daemonExitGrace); ok && werr != nil && exitCodeOf(werr) != exitSocketBusy {
			return 1, daemonExited(werr, logFile)
		}
		// Said rather than left silent: this stderr is the operator's terminal on the far end
		// of ssh, and a session that ended because the daemon went is not the same news as one
		// that ended because they closed it.
		_, _ = fmt.Fprintln(stderr, "the daemon closed the connection")
	}
	err := f.err
	if errors.Is(err, io.EOF) || errors.Is(err, protocol.ErrConnClosed) || errors.Is(err, context.Canceled) {
		return 0, nil
	}
	return 1, err
}
