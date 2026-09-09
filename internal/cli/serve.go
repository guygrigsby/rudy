package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/server"
)

// serveShutdownBudget bounds the unwind of rudy serve. It is longer than the budget a
// client run gets because a daemon can be holding several attached sessions with a turn
// running in each, and every one of them has to record its turn_interrupted entry and close
// its log before the process leaves.
const serveShutdownBudget = 10 * time.Second

// newServeCommand is the daemon: the same server the embedded client runs, on a unix socket
// instead of an in-process pipe. It takes the same buildFunc every other command does, since
// there is one wiring and a daemon that built its own would be a second one.
func newServeCommand(build buildFunc) *cobra.Command {
	var socket string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "serve the protocol on a unix socket until interrupted",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			stderr := cmd.ErrOrStderr()
			code, err := runServe(cmd.Context(), build, socket, cmd.OutOrStdout(), stderr)
			if err != nil {
				_, _ = fmt.Fprintln(stderr, err)
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
	cmd.Flags().StringVar(&socket, "socket", "", "unix socket to serve on (default: rudy.sock under the XDG runtime dir)")
	return cmd
}

// runServe builds a server, listens on socket and serves every connection the transport
// accepts until the context ends or a signal arrives. It returns the process exit code: 0
// for a clean shutdown, 1 for a socket another server holds or a shutdown that ran past its
// budget.
//
// Everything it prints is a diagnostic, so it all goes to stderr with the prefix Build's own
// notices use; stdout stays free for a future flag that wants to report the socket to a
// program rather than to an operator.
func runServe(ctx context.Context, build buildFunc, socket string, _ io.Writer, stderr io.Writer) (int, error) {
	// SIGTERM as well as SIGINT: a daemon is stopped by a service manager at least as often
	// as by a Ctrl-C, and both mean the same thing here.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	socket, err := serveSocket(socket)
	if err != nil {
		return 1, err
	}
	notice := func(text string) { _, _ = fmt.Fprintln(stderr, "rudy:", text) }

	// The socket goes into the build: it is what a locked session names when it tells the
	// second process to attach here instead of opening the same log twice, so it has to be
	// the socket this process is actually about to serve on, not the default.
	b, err := build(ctx, BuildOptions{Stderr: stderr, Socket: socket})
	if err != nil {
		return 1, err
	}
	// unwind is the whole shutdown, in the order that keeps a second Ctrl-C useful: stop() puts
	// SIGINT back to its default disposition before anything that waits, so an operator who
	// gives up on the shutdown budget kills the process instead of cancelling a context
	// nothing is reading any more. See runPrint, which does the same.
	unwind := func() error {
		stop()
		shutdownCtx, done := context.WithTimeout(context.Background(), serveShutdownBudget)
		defer done()
		return b.Close(shutdownCtx)
	}

	l, err := protocol.ListenUnix(socket)
	if err != nil {
		_ = unwind()
		if errors.Is(err, protocol.ErrSocketBusy) {
			_, _ = fmt.Fprintf(stderr, "a server is already serving %s\n", socket)
			return 1, nil
		}
		return 1, err
	}
	// Printed before the accept loop starts, and before anything dials: the listener is bound
	// by now, so a client that reads this line can connect on the next one.
	notice("serving on " + socket)

	// Every connection is served under a context of this one, so the signal that ends the
	// process ends the serve loops too. Shutdown waits for those loops before it closes a
	// session, and a loop only ends with its own context or its client's disconnect, neither
	// of which Shutdown controls.
	connCtx, endConns := context.WithCancel(ctx)
	defer endConns()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		accept(connCtx, l, b.Server, &wg, notice)
	}()

	<-ctx.Done()

	// Closing the listener first stops new connections and takes the socket off disk, so
	// nothing dials a server on its way out and the next one finds the path clear. A failure
	// here is worth saying but not worth failing on: the sessions are what the exit code is
	// about, and ListenUnix clears a socket left behind anyway.
	if err := l.Close(); err != nil {
		notice(err.Error())
	}
	closeErr := unwind()
	// The serve loops are already unwinding under connCtx, which ended with ctx above; this
	// is the wait that makes it true that nothing is still writing to stderr, or to a
	// session, once runServe has returned. Shutdown waits for the loops it knows about, not
	// for the accept goroutine or for a connection accepted in the moment it began.
	endConns()
	wg.Wait()
	if closeErr != nil {
		return 1, closeErr
	}
	return 0, nil
}

// serveSocket resolves an unset --socket to the default, the same path Build would have
// handed the server: an operator who names no socket means the one every client probes.
func serveSocket(socket string) (string, error) {
	if socket != "" {
		return socket, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home directory: %w", err)
	}
	return config.XDG(os.Getenv, home).Socket(), nil
}

// accept hands every connection the listener admits to the server, each on its own
// goroutine, and returns once the listener is closed. A peer the transport refused (another
// user's process reaching into a 0700 directory) is logged and the loop carries on: one
// connection is never a reason to stop serving the others. So is any other accept failure,
// which will repeat in the log if it is the kind that does not clear.
func accept(ctx context.Context, l *protocol.Listener, srv *server.Server, wg *sync.WaitGroup, notice func(string)) {
	for {
		c, err := l.Accept()
		if err != nil {
			// The listener is only ever closed by the shutdown above, so this is the one
			// error that means the loop is done rather than that one connection was.
			if errors.Is(err, net.ErrClosed) {
				return
			}
			notice(err.Error())
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The server never closes a connection it was handed; whoever accepted it owns
			// it. Closing here is also what tells a client whose serve loop just ended with
			// the shutdown that the server is gone, rather than leaving it on a dead socket.
			defer func() { _ = c.Close() }()
			err := srv.Serve(ctx, c)
			// A context that ended and a connection offered mid-shutdown are the shutdown
			// working, not something an operator needs to read.
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, server.ErrShuttingDown) {
				notice(err.Error())
			}
		}()
	}
}
