// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/skills"
)

// newSkillsCommand is the "skills" noun with "list" and "migrate" verbs under it. Neither
// touches the server: like sessions list, they read config.toml directly and act on the
// filesystem, paying for no plugin load or registry refresh.
func newSkillsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "list and migrate skills",
	}
	cmd.AddCommand(newSkillsListCommand(), newSkillsMigrateCommand())
	return cmd
}

func newSkillsListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "list the skills found under skills.dirs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadSkillsConfig()
			if err != nil {
				return err
			}
			got, errs := skills.Load(cfg.Skills.Dirs)
			notice := notices(cmd.ErrOrStderr())
			for _, e := range errs {
				notice(e.Error())
			}
			return renderSkills(cmd.OutOrStdout(), got)
		},
	}
}

// renderSkills prints one row per skill in the order skills.Load returns them.
func renderSkills(w io.Writer, list []skills.Skill) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tDESCRIPTION\tDIR")
	for _, s := range list {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", s.Name, s.Description, s.Dir)
	}
	return tw.Flush()
}

func newSkillsMigrateCommand() *cobra.Command {
	var from []string
	var to string
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "copy skills from legacy directories into a skills.dirs entry",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadSkillsConfig()
			if err != nil {
				return err
			}
			src := from
			if len(src) == 0 {
				src = cfg.Skills.MigrateFrom
			}
			dst := to
			if dst == "" {
				dst = firstAbsolute(cfg.Skills.Dirs)
			}
			if dst == "" {
				return fmt.Errorf("skills migrate: no --to given and no absolute entry in skills.dirs")
			}
			copied, skipped, err := skills.Migrate(src, dst)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, name := range copied {
				_, _ = fmt.Fprintf(out, "copied: %s\n", name)
			}
			for _, name := range skipped {
				_, _ = fmt.Fprintf(out, "skipped: %s (exists)\n", name)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&from, "from", nil, "a source directory to migrate skills from (repeatable); default skills.migrate_from")
	cmd.Flags().StringVar(&to, "to", "", "the destination directory; default the first absolute entry of skills.dirs")
	return cmd
}

// firstAbsolute returns the first absolute entry of dirs, or "".
func firstAbsolute(dirs []string) string {
	for _, d := range dirs {
		if filepath.IsAbs(d) {
			return d
		}
	}
	return ""
}

// loadSkillsConfig reads config.toml the way storeFromEnv reads it for sessions list: no
// server, no plugin load, no registry refresh, since skills list and migrate only touch the
// filesystem.
func loadSkillsConfig() (*config.Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	paths := config.XDG(os.Getenv, home)
	return config.Load(paths, nil)
}
