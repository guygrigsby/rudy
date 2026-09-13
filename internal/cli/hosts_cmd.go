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
// machine does to them. push is the whole of it this wave; check, install and pull are the
// rest of the operator's verbs (ADR 0029).
func newHostsCommand(build buildFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hosts",
		Short: "the machines rudy runs on",
	}
	cmd.AddCommand(newHostsPushCommand(build))
	return cmd
}

// newHostsPushCommand is the sync on demand: the same move a new session over --host makes,
// typed by an operator who wants the box caught up without opening one.
func newHostsPushCommand(build buildFunc) *cobra.Command {
	var d dialOptions
	cmd := &cobra.Command{
		Use:   "push [host]",
		Short: "move this directory's working tree to the host",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				d.Host = args[0]
			}
			stderr := cmd.ErrOrStderr()
			code, err := runHostsPush(cmd.Context(), build, BuildOptions{Stderr: stderr}, d, cmd.OutOrStdout())
			if err != nil {
				_, _ = fmt.Fprintln(stderr, err)
				if code == 0 {
					code = 1
				}
			}
			if code != 0 {
				slog.Error("rudy: exit", "command", "hosts push", "code", code, "err", err)
				return ExitError{code}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&d.Cwd, "cwd", "", "workspace path on the host (default: your cwd's path relative to your home, under the host's home)")
	return cmd
}

// runHostsPush resolves the host, greets it and moves the tree. The greeting is what says
// where the host's home is, and the placement is computed from it by the same rule a session
// open uses, so the directory this pushes to is the directory a session would open on.
func runHostsPush(ctx context.Context, build buildFunc, o BuildOptions, d dialOptions, stdout io.Writer) (int, error) {
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
	if err := hosts.Sync(ctx, hosts.SSHRunner(dialed.Host), dialed.Host, cwd, placement, stdout); err != nil {
		return 1, err
	}
	return 0, nil
}
