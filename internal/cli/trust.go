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

	"github.com/guygrigsby/rudy/internal/trust"
)

// askTrust is the prompt a client with a terminal puts to the operator before this
// workspace's own plugins run: what they are, what each will execute, and one keystroke.
// It runs before the TUI takes the terminal, so it is a plain read from stdin (ADR 0025).
//
// Anything but "y" is no, including an error and an empty line: the safe answer is the one
// a hurried person gives.
func askTrust(in io.Reader, out io.Writer) TrustAsker {
	return func(root string, decls []trust.Declaration) (bool, error) {
		var plugins, servers, agents int
		for _, d := range decls {
			switch d.Kind {
			case trust.KindPlugin:
				plugins++
			case trust.KindMCP:
				servers++
			case trust.KindAgent:
				agents++
			}
		}
		_, _ = fmt.Fprintf(out, "\n%s ships %d plugin(s), %d MCP server(s) and %d agent definition(s), which rudy would run or speak for as you:\n\n", root, plugins, servers, agents)
		for _, d := range decls {
			_, _ = fmt.Fprintf(out, "  %s\n    %s\n", d, d.Summary)
		}
		_, _ = fmt.Fprint(out, "\nTrust this workspace from now on? [y/N] ")
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
			store := trust.NewStore(paths.Data)
			if forget {
				if err := store.Untrust(cwd); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "forgot %s; its plugins will ask again\n", cwd)
				return nil
			}
			decls, errs := trust.Declarations(cwd)
			for _, e := range errs {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), e)
			}
			if len(decls) == 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s ships nothing under .rudy; nothing to trust\n", cwd)
				return nil
			}
			if err := store.Trust(cwd, decls, time.Now()); err != nil {
				return err
			}
			names := make([]string, 0, len(decls))
			for _, d := range decls {
				names = append(names, d.String())
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "trusted %s: %s\n", cwd, strings.Join(names, ", "))
			return nil
		},
	}
	cmd.Flags().BoolVar(&forget, "forget", false, "forget this workspace, so its plugins ask again")
	return cmd
}
