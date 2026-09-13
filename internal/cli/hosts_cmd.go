package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

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

// onHost is what both verbs do around their one leg: resolve the host, greet it and place this
// directory. The greeting is what says where the host's home is, and the placement is computed
// from it by the same rule a session open uses, so the directory these act on is the directory
// a session would open on.
func onHost(ctx context.Context, build buildFunc, o BuildOptions, d dialOptions, leg func(ctx context.Context, d *dialed, cwd, placement string) error) (int, error) {
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
