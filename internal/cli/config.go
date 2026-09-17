// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/guygrigsby/rudy/internal/config"
)

// newConfigCmd is `rudy config`: where the file is, what a full one looks like, and the one
// thing the harness writes into an operator's own. ADR 0021.
func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "show, generate and sync config.toml",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newConfigPathCmd(), newConfigExampleCmd(), newConfigSyncCmd())
	return cmd
}

func newConfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "print the config file rudy reads",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := configPaths()
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), paths.ConfigFile())
			return nil
		},
	}
}

func newConfigExampleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "example",
		Short: "print a config.toml carrying every key, its default and what it is for",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, _ = fmt.Fprint(cmd.OutOrStdout(), config.Example())
			return nil
		},
	}
}

func newConfigSyncCmd() *cobra.Command {
	var path string
	var dry bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "add missing keys to config.toml, changing no value that is already there",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			file := path
			if file == "" {
				paths, err := configPaths()
				if err != nil {
					return err
				}
				file = paths.ConfigFile()
			}
			res, err := config.Sync(file, dry)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			switch {
			case res.Created && dry:
				_, _ = fmt.Fprintf(out, "would write %s with every key\n", res.Path)
			case res.Created:
				_, _ = fmt.Fprintf(out, "wrote %s with every key\n", res.Path)
			case len(res.Added) == 0:
				_, _ = fmt.Fprintf(out, "%s has every key\n", res.Path)
			default:
				verb := "added"
				if dry {
					verb = "would add"
				}
				_, _ = fmt.Fprintf(out, "%s %d keys to %s:\n", verb, len(res.Added), res.Path)
				for _, k := range res.Added {
					_, _ = fmt.Fprintf(out, "  %s\n", k)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "the file to sync; default the config rudy reads")
	cmd.Flags().BoolVar(&dry, "dry-run", false, "report what would be added and write nothing")
	return cmd
}

// configPaths resolves the XDG roots the same way every other command does, so `rudy config
// path` names the file `rudy` itself would read.
func configPaths() (config.Paths, error) {
	env, home, err := envAndHome(BuildOptions{})
	if err != nil {
		return config.Paths{}, err
	}
	return config.XDG(env, home), nil
}
