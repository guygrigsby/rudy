// SPDX-License-Identifier: AGPL-3.0-or-later

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

	rudy "github.com/guygrigsby/rudy"
	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tui/app"
	"github.com/guygrigsby/rudy/internal/tui/icons"
	"github.com/guygrigsby/rudy/internal/tui/keys"
	"github.com/guygrigsby/rudy/internal/tui/spinner"
	"github.com/guygrigsby/rudy/internal/tui/theme"
)

// look is the theme and key table the client draws and reads keys with, resolved from
// config before anything else happens: a bad ui.theme.name or [keys] entry is a
// configuration error, and it costs a message rather than a half-opened session.
type look struct {
	theme   theme.Theme
	icons   icons.Set
	spinner spinner.Set
	keys    *keys.Table
}

// resolveFunc is how one command decides which session the client opens on, given a
// connection that has already said hello. Root, sessions resume and sessions fork differ in
// this one step and in nothing else.
type resolveFunc func(ctx context.Context, d *dialed, cwd string) (protocol.SessionInfo, int, error)

// clientRun is everything the launcher needs: the greeted connection and the config and
// version that came with it, the resolved look and the session to draw.
type clientRun struct {
	dial *dialed
	look look
	info protocol.SessionInfo
	cwd  string
	// prompt is the positional words of `rudy <prompt>`, seeded into the editor as a
	// draft. Empty for every command that takes no prompt.
	prompt string
	stderr io.Writer
}

// launchFunc draws the client on a greeted connection and an already resolved session, and
// reports whether the session was at rest when the client quit. It is a variable below so
// tests can see what a command wired up without a terminal.
type launchFunc func(ctx context.Context, r clientRun) (resting bool, err error)

// launchTUI is the launcher every command goes through. Tests replace it to assert the
// session a command resolved, the way stdinIsTerminal is replaced to pretend about stdin.
var launchTUI launchFunc = launchApp

// resumeWith resolves the session a command opens on: a new one, or the one --resume or
// --continue names, with --model, --mode and --thinking applied to it. source names the
// flag or verb the session id came from, for the error a bad one gets. stderr is carried in
// rather than added to resolveFunc because it is this resolver's own business: a new session
// over --host moves the working tree first and says so, and a fork has nothing to say.
func resumeWith(o printOptions, source string, stderr io.Writer) resolveFunc {
	return func(ctx context.Context, d *dialed, cwd string) (protocol.SessionInfo, int, error) {
		return openOrResume(ctx, d, o, cwd, source, stderr)
	}
}

// forkAt resolves a fork of id as the session to open on. An empty at forks at the newest
// entry of any kind: the server resolves that default under the parent's own lock
// (internal/server, forkAt), so a command racing on the same session cannot leave the fork
// one entry stale.
func forkAt(id, at string) resolveFunc {
	return func(ctx context.Context, d *dialed, cwd string) (protocol.SessionInfo, int, error) {
		var info protocol.SessionInfo
		if _, err := ulid.Parse(id); err != nil {
			return info, 2, fmt.Errorf("%q is not a session id", id)
		}
		if at != "" {
			if _, err := ulid.Parse(at); err != nil {
				return info, 2, fmt.Errorf("--at %q is not an entry id", at)
			}
		}
		if err := d.Client.Call(ctx, protocol.MethodSessionFork, protocol.SessionForkParams{SessionID: id, AtEntryID: at}, &info); err != nil {
			// A fork reads the parent, so a parent another rudy holds is the same lock and
			// the same answer: the socket that process is serving.
			if hint := heldElsewhere(id, err); hint != nil {
				return info, 1, hint
			}
			return info, 1, fmt.Errorf("fork %s: %w", id, err)
		}
		return info, 0, nil
	}
}

// runTUI opens the client on the server dial reached, a running daemon or one this process
// starts, and returns the process exit code: 0 the client exited, 1 failed, 2 usage. Every
// failure is printed here rather than returned, so the three commands that draw the client
// report one the same way.
//
// The order matters. The terminal is checked before anything is built or dialed, so a piped
// run costs no plugin load; the theme and the keys are resolved before the session, so a
// configuration error never leaves a session open behind a client that cannot draw.
func runTUI(ctx context.Context, build buildFunc, dopts dialOptions, resolve resolveFunc, launch launchFunc, prompt string, stderr io.Writer) int {
	// Whether the box's tree has settled, read by the close-time pull below. False until the
	// client has quit and said so: every way out before that is a client that never drew, and
	// a session that never opened has nothing to bring home anyway.
	resting := false
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
	// The client has a terminal and has not taken it yet, so it is the one caller that can
	// ask whether this workspace's own plugins may run (ADR 0025).
	d, code, err := dial(ctx, build, BuildOptions{Stderr: stderr, Trust: askTrust(os.Stdin, stderr)}, dopts, "rudy-tui", true)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return code
	}
	defer func() {
		// stop() first, as --print does: it puts SIGINT back to its default disposition,
		// so a second Ctrl-C during the shutdown kills the process instead of cancelling
		// a context nothing is reading any more.
		stop()
		d.Close()
		// And then the tree, once the session on the box is closed. The terminal is the
		// client's again by now, so the notices land where the operator can read them.
		pullBack(d, resting, stderr)
	}()
	lk, err := loadLook(d.Paths, d.Config)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}

	cwd, err := os.Getwd()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	// A background context on purpose, as --print does: the session has to be opened and
	// closed even once ctx is the one a signal cancelled.
	info, code, err := resolve(context.Background(), d, cwd)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return code
	}
	run := clientRun{dial: d, look: lk, info: info, cwd: cwd, prompt: prompt, stderr: stderr}
	quiet, err := launch(ctx, run)
	resting = quiet
	switch {
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
func loadLook(paths config.Paths, cfg *config.Config) (look, error) {
	th, err := theme.Load(filepath.Join(paths.Config, "themes"), cfg.UI.Theme["name"], cfg.UI.Theme)
	if err != nil {
		return look{}, err
	}
	table, err := keys.New(cfg.Keys)
	if err != nil {
		return look{}, err
	}
	// The icon set last: a bad set name or an icon nobody has heard of is a configuration
	// error like the other two, and costs a message rather than a half drawn client.
	set, err := icons.Load(cfg.UI.Icons["set"], cfg.UI.Icons)
	if err != nil {
		return look{}, err
	}
	spin, err := spinner.Load(cfg.UI.Spinner.Name, cfg.UI.Spinner.Frames, cfg.UI.Spinner.IntervalMS)
	if err != nil {
		return look{}, err
	}
	return look{theme: th, icons: set, spinner: spin, keys: table}, nil
}

// launchApp is the real launcher: the model registry as the client's snapshot of it, then
// the Bubble Tea program, which owns the terminal until it returns.
func launchApp(ctx context.Context, r clientRun) (bool, error) {
	var reg protocol.RegistryListResult
	if err := r.dial.Client.Call(context.Background(), protocol.MethodRegistryList, nil, &reg); err != nil {
		// A registry the client could not read costs the context percent and the cost cell,
		// not the session: the server already resolved the model the session opened on.
		_, _ = fmt.Fprintln(r.stderr, "rudy: registry.list:", err)
	}
	// The models ctrl+p walks, as the last client to be told left them. A file that will
	// not parse costs the cycle its shortlist and nothing else, so it is printed and the
	// client opens on the whole registry (ADR 0027).
	scope, err := readScope(r.dial.Paths.Data)
	if err != nil {
		_, _ = fmt.Fprintln(r.stderr, err)
	}
	return app.Run(ctx, app.Options{
		Config: r.dial.Config,
		// The binary's own release notes, for the startup header's news (ADR 0016).
		Changelog: rudy.Changelog,
		Theme:     r.look.theme,
		Icons:     r.look.icons,
		Spinner:   r.look.spinner,
		Keys:      r.look.keys,
		Client:    r.dial.Client,
		Session:   r.info,
		Models:    reg.Models,
		Version:   r.dial.Version,
		Cwd:       r.cwd,
		Prompt:    r.prompt,
		Scope:     scope,
		SaveScope: func(refs []session.ModelRef) error { return writeScope(r.dial.Paths.Data, refs) },
	})
}
