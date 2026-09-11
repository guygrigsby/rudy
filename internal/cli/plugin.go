package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/pluginstore"
)

// newPluginCommand is the "plugin" noun with install, uninstall, list, enable, disable and
// update verbs under it. Like skills and mcp, none of these build a server: they read and
// write plugins.lock.toml and the checkouts beside it under the XDG data root, the same
// files Build's spawnedPlugins reads at boot.
func newPluginCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "plugins",
		// The nouns are plural everywhere else (sessions, skills, models); "plugin" is
		// what this command was called first and still answers to.
		Aliases: []string{"plugin"},
		Short:   "install and manage plugins",
	}
	cmd.AddCommand(
		newInstallCmd(),
		newPluginUninstallCommand(),
		newPluginListCommand(),
		newPluginEnableCommand(),
		newPluginDisableCommand(),
		newPluginUpdateCommand(),
		newPluginsTrustCmd(),
	)
	return cmd
}

// pluginStore is the Store over $XDG_DATA_HOME/rudy: the root rudy plugin install writes
// checkouts and the lock into, and Build's spawnedPlugins reads them from.
func pluginStore() (*pluginstore.Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return pluginstore.New(config.XDG(os.Getenv, home).Data), nil
}

// resolvedAt is what rudy install and rudy plugins update print after "at": the reproducibility
// record of the kind that was installed, since that is the whole point of recording one. git
// resolves to a commit, shortened to the first eight characters; go and https resolve to a
// digest, printed whole, because a truncated sum is not a sum. A path has neither, and a lock
// written before a column existed can be missing one, so both fall back to the source itself.
func resolvedAt(inst pluginstore.Installed) string {
	switch inst.Kind {
	case pluginstore.KindGit:
		if inst.Commit != "" {
			return shortCommit(inst.Commit)
		}
	case pluginstore.KindGo, pluginstore.KindHTTPS:
		if inst.Digest != "" {
			return inst.Digest
		}
	}
	return inst.Source
}

// shortCommit is a commit sha at the length git itself abbreviates to.
func shortCommit(commit string) string {
	if len(commit) > 8 {
		return commit[:8]
	}
	return commit
}

// newInstallCmd is install, mounted twice: as the top-level `rudy install` (ADR 0025
// decision 2, the one verb everybody types) and as `rudy plugins install`, the noun-then-verb
// form. Root and the plugins noun each call this to get their own instance rather than
// sharing one *cobra.Command, since a command remembers a single parent.
func newInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install <source>",
		Short: "install a plugin from a Go module, a git repository, a tarball URL or a local path",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := pluginStore()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			s.Out = out
			_, _ = fmt.Fprintf(out, "installing %s: downloads the source and its manifest may run a build command\n", args[0])
			inst, m, err := s.Install(cmd.Context(), args[0], time.Now())
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(out, "installed %s %s at %s\n", m.Name, m.Version, resolvedAt(inst))
			return err
		},
	}
}

func newPluginUninstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall <name>",
		Short: "remove an installed plugin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := pluginStore()
			if err != nil {
				return err
			}
			if err := s.Uninstall(args[0]); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "uninstalled %s\n", args[0])
			return err
		},
	}
}

func newPluginListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "list installed plugins",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := pluginStore()
			if err != nil {
				return err
			}
			locked, err := s.Read()
			if err != nil {
				return err
			}
			return renderPlugins(cmd.OutOrStdout(), s, locked)
		},
	}
}

// renderPlugins prints one row per installed plugin, name-sorted. The version comes from
// the manifest in the checkout rather than the lock, since the lock does not carry it; a
// checkout whose manifest cannot be read (removed by hand, say) prints "-" rather than
// failing the whole listing.
func renderPlugins(w io.Writer, s *pluginstore.Store, locked map[string]pluginstore.Installed) error {
	names := make([]string, 0, len(locked))
	for name := range locked {
		names = append(names, name)
	}
	sort.Strings(names)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	// DIGEST is a column of its own rather than sharing one with COMMIT: they are what makes a
	// go or https install reproducible, and a listing that shows neither leaves an operator with
	// no way to see what their plugins actually resolved to.
	_, _ = fmt.Fprintln(tw, "NAME\tVERSION\tENABLED\tKIND\tREF\tCOMMIT\tDIGEST\tSOURCE")
	for _, name := range names {
		inst := locked[name]
		version := "-"
		if m, err := plugin.ReadManifest(filepath.Join(s.Root, "plugins", name)); err == nil {
			version = m.Version
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%t\t%s\t%s\t%s\t%s\t%s\n", name, version, inst.Enabled, inst.Kind, inst.Ref, inst.Commit, inst.Digest, inst.Source)
	}
	return tw.Flush()
}

func newPluginEnableCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <name>",
		Short: "enable a disabled plugin",
		Args:  cobra.ExactArgs(1),
		RunE:  pluginSetEnabledRunE(true, "enabled"),
	}
}

func newPluginDisableCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <name>",
		Short: "disable an installed plugin without uninstalling it",
		Args:  cobra.ExactArgs(1),
		RunE:  pluginSetEnabledRunE(false, "disabled"),
	}
}

// pluginSetEnabledRunE is enable and disable's shared body: both flip the same flag through
// the same Store method and differ only in which way and which word they print.
func pluginSetEnabledRunE(on bool, verb string) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		s, err := pluginStore()
		if err != nil {
			return err
		}
		if err := s.SetEnabled(args[0], on); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", verb, args[0])
		return err
	}
}

func newPluginUpdateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "update <name>",
		Short: "re-resolve an installed plugin's source and rebuild it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := pluginStore()
			if err != nil {
				return err
			}
			s.Out = cmd.OutOrStdout()
			inst, changed, err := s.Update(cmd.Context(), args[0], time.Now())
			if err != nil {
				return err
			}
			// "updated" for an https digest that never moved, or a pinned ref that re-resolved
			// to the same commit, claims something happened that did not.
			verb := "unchanged"
			if changed {
				verb = "updated"
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s at %s\n", verb, args[0], resolvedAt(inst))
			return err
		},
	}
}
