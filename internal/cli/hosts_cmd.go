// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/guygrigsby/rudy/internal/cli/hosts"
)

// newHostsCommand is the "hosts" noun: the machines a kernel can run on, and what this
// machine does to them. push and pull are the two directions of the sync; check and install
// are the rest of the operator's verbs (ADR 0029).
func newHostsCommand(build buildFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hosts",
		Short: "the machines rudy runs on",
	}
	cmd.AddCommand(
		newHostsMoveCommand(build, "push", "move this directory's working tree to the host", runHostsPush),
		newHostsMoveCommand(build, "pull", "bring the host's work back to this directory", runHostsPull),
		newHostsInstallCommand(build),
	)
	return cmd
}

// moveFunc is one direction of the sync, as a verb runs it.
type moveFunc func(ctx context.Context, build buildFunc, o BuildOptions, d dialOptions, stdout io.Writer) (int, error)

// newHostsMoveCommand wires one of the two: the sync on demand, the same move a new session
// over --host makes, typed by an operator who wants it without opening one. Both take the host
// as the one argument and --cwd for a directory the home-relative rule cannot place, so they
// are one command shape with the leg passed in.
func newHostsMoveCommand(build buildFunc, verb, short string, move moveFunc) *cobra.Command {
	var d dialOptions
	cmd := &cobra.Command{
		Use:   verb + " [host]",
		Short: short,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				d.Host = args[0]
			}
			stderr := cmd.ErrOrStderr()
			code, err := move(cmd.Context(), build, BuildOptions{Stderr: stderr}, d, cmd.OutOrStdout())
			if err != nil {
				_, _ = fmt.Fprintln(stderr, err)
				if code == 0 {
					code = 1
				}
			}
			if code != 0 {
				slog.Error("rudy: exit", "command", "hosts "+verb, "code", code, "err", err)
				return ExitError{code}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&d.Cwd, "cwd", "", "workspace path on the host (default: your cwd's path relative to your home, under the host's home)")
	return cmd
}

// runHostsPush moves this directory's tree to the host.
func runHostsPush(ctx context.Context, build buildFunc, o BuildOptions, d dialOptions, stdout io.Writer) (int, error) {
	return onHost(ctx, build, o, d, func(ctx context.Context, d *dialed, cwd, placement string) error {
		return hosts.Sync(ctx, hosts.SSHRunner(d.Host), d.Host, cwd, placement, stdout)
	})
}

// runHostsPull brings the host's work back: the branch fast-forwarded onto this checkout, or
// the placement tarred over a tree no git tracks.
func runHostsPull(ctx context.Context, build buildFunc, o BuildOptions, d dialOptions, stdout io.Writer) (int, error) {
	return onHost(ctx, build, o, d, func(ctx context.Context, d *dialed, cwd, placement string) error {
		return hosts.Pull(ctx, hosts.SSHRunner(d.Host), cwd, placement, stdout)
	})
}

// newHostsInstallCommand is `rudy hosts install [host] [--force]`: build rudy on the box from
// its own checkout, at the commit this binary was built from. The dial itself installs when
// the box has no rudy at all; this is the verb for the rest of it, which is a box whose rudy
// is older than the one in the operator's hand.
func newHostsInstallCommand(build buildFunc) *cobra.Command {
	var d dialOptions
	var force bool
	cmd := &cobra.Command{
		Use:   "install [host]",
		Short: "build rudy on the host at this binary's commit",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				d.Host = args[0]
			}
			stderr := cmd.ErrOrStderr()
			code, err := runHostsInstall(cmd.Context(), build, BuildOptions{Stderr: stderr}, d, force, cmd.OutOrStdout())
			if err != nil {
				_, _ = fmt.Fprintln(stderr, err)
				if code == 0 {
					code = 1
				}
			}
			if code != 0 {
				slog.Error("rudy: exit", "command", "hosts install", "code", code, "err", err)
				return ExitError{code}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "stop the daemon on the host and start a fresh one at the version just installed")
	return cmd
}

// runHostsInstall greets the box, builds rudy there and decides what to do with the daemon.
//
// The greeting comes first and may start a daemon, which is accepted: it is the same daemon
// the next session would start, and the version it reports is the one the operator is being
// told about. Without --force the daemon is left exactly as it is, running the binary it was
// started from, and the operator is told so; --force is their word that nothing on the box is
// live.
func runHostsInstall(ctx context.Context, build buildFunc, o BuildOptions, d dialOptions, force bool, stdout io.Writer) (int, error) {
	if code, err := resolveHost(o, &d); err != nil {
		return code, err
	}
	dialed, code, err := dial(ctx, build, o, d, "rudy-hosts", false)
	if err != nil {
		return code, err
	}
	defer dialed.Close()
	wasVersion, wasInstance := dialed.Version, dialed.InstanceID
	host, source := dialed.Host, dialed.Config.Remote.Source
	rev, err := hosts.Revision(Version())
	if err != nil {
		return 1, err
	}
	r := hosts.SSHRunner(host)
	if err := hosts.Install(ctx, r, source, rev, stdout); err != nil {
		return 1, err
	}
	said := notices(stdout)
	said(fmt.Sprintf("installed rudy %s on %s from %s", rev, host, source))
	if !force {
		said(fmt.Sprintf("the daemon on %s is still running rudy %s; rudy hosts install %s --force restarts it, which ends whatever it is running", host, wasVersion, host))
		return 0, nil
	}
	if err := shutdownDialed(ctx, dialed); err != nil {
		return 1, fmt.Errorf("stopping the daemon on %s: %w", host, err)
	}
	dialed.Close()
	replacement, code, err := dial(ctx, build, o, d, "rudy-hosts", false)
	if err != nil {
		return code, fmt.Errorf("starting the replacement daemon on %s: %w", host, err)
	}
	defer replacement.Close()
	if replacement.InstanceID == "" || replacement.InstanceID == wasInstance {
		return 1, fmt.Errorf("daemon on %s did not replace server instance %s", host, wasInstance)
	}
	if replacement.Version != Version() {
		return 1, fmt.Errorf("replacement daemon on %s runs rudy %s, want %s", host, replacement.Version, Version())
	}
	said("restarted the daemon on " + host.String())
	return 0, nil
}

const hostShutdownBudget = 30 * time.Second

// shutdownDialed stops the exact Server that answered this connection. Its response proves
// acceptance; matching server.stopped followed by EOF proves successful runtime cleanup.
func shutdownDialed(ctx context.Context, d *dialed) error {
	stopCtx, cancel := context.WithTimeout(ctx, hostShutdownBudget)
	defer cancel()
	return requestDaemonShutdown(stopCtx, d.Client, d.InstanceID)
}

// resolveHost fills in the machine a hosts verb acts on: the argument, else remote.host, else
// a usage error. Every verb under the noun asks the same question and gets the same answer.
func resolveHost(o BuildOptions, d *dialOptions) (int, error) {
	if d.Host == "" {
		_, cfg, err := localConfig(o)
		if err != nil {
			return 1, err
		}
		d.Host = cfg.Remote.Host
	}
	if d.Host == "" {
		return 2, errors.New("no host: name one as the argument or set remote.host")
	}
	return 0, nil
}

// onHost is what both move verbs do around their one leg: resolve the host, greet it and place
// this directory. The greeting is what says where the host's home is, and the placement is
// computed from it by the same rule a session open uses, so the directory these act on is the
// directory a session would open on.
func onHost(ctx context.Context, build buildFunc, o BuildOptions, d dialOptions, leg func(ctx context.Context, d *dialed, cwd, placement string) error) (int, error) {
	if code, err := resolveHost(o, &d); err != nil {
		return code, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 1, err
	}
	dialed, code, err := dial(ctx, build, o, d, "rudy-hosts", false)
	if err != nil {
		return code, err
	}
	defer dialed.Close()
	placement, err := dialed.place(cwd)
	if err != nil {
		return 2, err
	}
	if err := leg(ctx, dialed, cwd, placement); err != nil {
		return 1, err
	}
	return 0, nil
}
