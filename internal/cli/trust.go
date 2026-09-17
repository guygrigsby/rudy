// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/pluginstore"
)

// askTrust is the prompt a client with a terminal puts to the operator before this
// workspace's own plugins run: what they are, what each will execute, and one keystroke.
// It runs before the TUI takes the terminal, so it is a plain read from stdin (ADR 0025).
//
// Anything but "y" is no, including an error and an empty line: the safe answer is the one
// a hurried person gives.
func askTrust(in io.Reader, out io.Writer) TrustAsker {
	return func(root string, manifests []plugin.Manifest) (bool, error) {
		_, _ = fmt.Fprintf(out, "\n%s ships %d plugin(s), which rudy would run as you:\n\n", root, len(manifests))
		for _, m := range manifests {
			_, _ = fmt.Fprintf(out, "  %s %s\n", m.Name, m.Version)
			if m.Build != "" {
				_, _ = fmt.Fprintf(out, "    build: %s\n", m.Build)
			}
			_, _ = fmt.Fprintf(out, "    runs:  %s %s\n", m.Command, strings.Join(m.Args, " "))
		}
		_, _ = fmt.Fprint(out, "\nRun them in this workspace from now on? [y/N] ")
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && line == "" {
			return false, nil
		}
		return strings.EqualFold(strings.TrimSpace(line), "y"), nil
	}
}

// newPluginsTrustCmd is `rudy plugins trust`: the same yes, given ahead of time or from a
// script, and `--forget` to take it back.
func newPluginsTrustCmd() *cobra.Command {
	var forget bool
	cmd := &cobra.Command{
		Use:   "trust",
		Short: "allow this workspace's own plugins to run, or forget that",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := configPaths()
			if err != nil {
				return err
			}
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			store := pluginstore.New(paths.Data)
			if forget {
				if err := store.Untrust(cwd); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "forgot %s; its plugins will ask again\n", cwd)
				return nil
			}
			manifests, errs := plugin.Discover([]string{cwd + "/.rudy"})
			for _, e := range errs {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), e)
			}
			if len(manifests) == 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s ships no plugins; nothing to trust\n", cwd)
				return nil
			}
			if err := store.Trust(cwd, manifests, time.Now()); err != nil {
				return err
			}
			names := make([]string, 0, len(manifests))
			for _, m := range manifests {
				names = append(names, m.Name)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "trusted %s: %s\n", cwd, strings.Join(names, ", "))
			return nil
		},
	}
	cmd.Flags().BoolVar(&forget, "forget", false, "forget this workspace, so its plugins ask again")
	return cmd
}
