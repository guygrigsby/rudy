package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"

	tea "charm.land/bubbletea/v2"
	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/tui/app"
	"github.com/guygrigsby/rudy/internal/tui/keys"
	"github.com/guygrigsby/rudy/internal/tui/theme"
)

// look is the theme and key table the client draws and reads keys with, resolved from
// config before anything else happens: a bad ui.theme.name or [keys] entry is a
// configuration error, and it costs a message rather than a half-opened session.
type look struct {
	theme theme.Theme
	keys  *keys.Table
}

// resolveFunc is how one command decides which session the client opens on, given a
// client that has already said hello. Root, sessions resume and sessions fork differ in
// this one step and in nothing else.
type resolveFunc func(ctx context.Context, client *protocol.Client, b *Built, cwd string) (protocol.SessionInfo, int, error)

// clientRun is everything the launcher needs: the wiring, a connection that has already
// said hello, the resolved look and the session to draw.
type clientRun struct {
	built  *Built
	client *protocol.Client
	look   look
	info   protocol.SessionInfo
	cwd    string
	// prompt is the positional words of `rudy <prompt>`, seeded into the editor as a
	// draft. Empty for every command that takes no prompt.
	prompt string
	stderr io.Writer
}

// launchFunc draws the client on a greeted connection and an already resolved session.
// It is a variable below so tests can see what a command wired up without a terminal.
type launchFunc func(ctx context.Context, r clientRun) error

// launchTUI is the launcher every command goes through. Tests replace it to assert the
// session a command resolved, the way stdinIsTerminal is replaced to pretend about stdin.
var launchTUI launchFunc = launchApp

// resumeWith resolves the session a command opens on: a new one, or the one --resume or
// --continue names, with --model, --mode and --thinking applied to it. source names the
// flag or verb the session id came from, for the error a bad one gets.
func resumeWith(o printOptions, source string) resolveFunc {
	return func(ctx context.Context, client *protocol.Client, b *Built, cwd string) (protocol.SessionInfo, int, error) {
		return openOrResume(ctx, client, b, o, cwd, source)
	}
}

// forkAt resolves a fork of id as the session to open on. An empty at forks at the newest
// entry of any kind: the server resolves that default under the parent's own lock
// (internal/server, forkAt), so a command racing on the same session cannot leave the fork
// one entry stale.
func forkAt(id, at string) resolveFunc {
	return func(ctx context.Context, client *protocol.Client, b *Built, cwd string) (protocol.SessionInfo, int, error) {
		var info protocol.SessionInfo
		if _, err := ulid.Parse(id); err != nil {
			return info, 2, fmt.Errorf("%q is not a session id", id)
		}
		if at != "" {
			if _, err := ulid.Parse(at); err != nil {
				return info, 2, fmt.Errorf("--at %q is not an entry id", at)
			}
		}
		if err := client.Call(ctx, protocol.MethodSessionFork, protocol.SessionForkParams{SessionID: id, AtEntryID: at}, &info); err != nil {
			return info, 1, fmt.Errorf("fork %s: %w", id, err)
		}
		return info, 0, nil
	}
}

// runTUI opens the client on an embedded server and returns the process exit code: 0 the
// client exited, 1 failed, 2 usage. Every failure is printed here rather than returned,
// so the three commands that draw the client report one the same way.
//
// The order matters. The terminal is checked before anything is built, so a piped run
// costs no plugin load; the theme and the keys are resolved before the connection, so a
// configuration error never leaves a session open behind a client that cannot draw.
func runTUI(ctx context.Context, build buildFunc, resolve resolveFunc, launch launchFunc, prompt string, stderr io.Writer) int {
	// The TUI takes over the terminal; a pipe or a redirect means the caller wanted the
	// headless client and did not say so.
	if !stdinIsTerminal() {
		_, _ = fmt.Fprintln(stderr, "the TUI needs a terminal; use --print")
		return 2
	}
	// The signal handler covers the windows the program's own does not: the build, which
	// loads plugins and refreshes the registry, and the close budget after the program has
	// given the terminal back. While the program is drawing, the terminal is raw and
	// Ctrl-C is a key (app.clear), not a signal.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	b, err := build(ctx, stderr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	defer func() {
		// stop() first, as --print does: it puts SIGINT back to its default disposition,
		// so a second Ctrl-C during the shutdown kills the process instead of cancelling
		// a context nothing is reading any more.
		stop()
		shutdownCtx, done := context.WithTimeout(context.Background(), clientShutdownBudget)
		defer done()
		_ = b.Close(shutdownCtx)
	}()
	lk, err := loadLook(b)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	client, closeConn, err := serveInMemory(b, "rudy-tui", true)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	defer closeConn()

	cwd, err := os.Getwd()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	// A background context on purpose, as --print does: the session has to be opened and
	// closed even once ctx is the one a signal cancelled.
	info, code, err := resolve(context.Background(), client, b, cwd)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return code
	}
	run := clientRun{built: b, client: client, look: lk, info: info, cwd: cwd, prompt: prompt, stderr: stderr}
	switch err := launch(ctx, run); {
	case err == nil:
		return 0
	case errors.Is(err, tea.ErrInterrupted):
		// The program's own SIGINT handler. Not news worth printing; 130 is what --print
		// reports for the same thing.
		return 130
	case errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil:
		// The other half of the same interrupt: the handler above cancelled ctx, which
		// kills the program. The ctx check is what separates it from everything else that
		// answers ErrProgramKilled, since Bubble Tea wraps every non-nil event-loop error
		// in it (a TTY that could not be read, a recovered panic). A run whose context is
		// still live was killed by a failure, and a failure gets printed.
		return 130
	default:
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
}

// tuiExit carries runTUI's exit code out through cobra. Whatever went wrong has already
// been printed where the operator can read it.
func tuiExit(code int) error {
	if code != 0 {
		return ExitError{code}
	}
	return nil
}

// loadLook resolves ui.theme and [keys]. Neither error is wrapped: theme.Load already
// names the file it could not read or the role it could not resolve, and keys.New names
// every action id and key spelling it refused, so a second copy of the path here would
// only repeat what the message already says, and would name a themes/default.toml that
// does not exist when the built-in theme is the one carrying a bad role override.
func loadLook(b *Built) (look, error) {
	th, err := theme.Load(filepath.Join(b.Paths.Config, "themes"), b.Config.UI.Theme["name"], b.Config.UI.Theme)
	if err != nil {
		return look{}, err
	}
	table, err := keys.New(b.Config.Keys)
	if err != nil {
		return look{}, err
	}
	return look{theme: th, keys: table}, nil
}

// launchApp is the real launcher: the model registry as the client's snapshot of it, then
// the Bubble Tea program, which owns the terminal until it returns.
func launchApp(ctx context.Context, r clientRun) error {
	var reg protocol.RegistryListResult
	if err := r.client.Call(context.Background(), protocol.MethodRegistryList, nil, &reg); err != nil {
		// A registry the client could not read costs the context percent and the cost cell,
		// not the session: the server already resolved the model the session opened on.
		_, _ = fmt.Fprintln(r.stderr, "rudy: registry.list:", err)
	}
	return app.Run(ctx, app.Options{
		Config:  r.built.Config,
		Theme:   r.look.theme,
		Keys:    r.look.keys,
		Client:  r.client,
		Session: r.info,
		Models:  reg.Models,
		Version: r.built.Version,
		Cwd:     r.cwd,
		Prompt:  r.prompt,
	})
}
