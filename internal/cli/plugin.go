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
		Use:   "plugin",
		Short: "install and manage plugins",
	}
	cmd.AddCommand(
		newPluginInstallCommand(),
		newPluginUninstallCommand(),
		newPluginListCommand(),
		newPluginEnableCommand(),
		newPluginDisableCommand(),
		newPluginUpdateCommand(),
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

// commitOrSource is what rudy plugin install and update print after "at": the first eight
// characters of the checked-out commit for a git source, or the source itself when inst has
// no commit because it was copied from a plain directory rather than cloned.
func commitOrSource(inst pluginstore.Installed) string {
	if inst.Commit == "" {
		return inst.Source
	}
	if len(inst.Commit) > 8 {
		return inst.Commit[:8]
	}
	return inst.Commit
}

func newPluginInstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "install <source>",
		Short: "install a plugin from a git URL or a local path",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := pluginStore()
			if err != nil {
				return err
			}
			inst, m, err := s.Install(cmd.Context(), args[0], time.Now())
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "installed %s %s at %s\n", m.Name, m.Version, commitOrSource(inst))
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
	_, _ = fmt.Fprintln(tw, "NAME\tVERSION\tENABLED\tCOMMIT\tSOURCE")
	for _, name := range names {
		inst := locked[name]
		version := "-"
		if m, err := plugin.ReadManifest(filepath.Join(s.Root, "plugins", name)); err == nil {
			version = m.Version
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%t\t%s\t%s\n", name, version, inst.Enabled, inst.Commit, inst.Source)
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
		Short: "pull the latest commit, or re-copy the source, for an installed plugin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := pluginStore()
			if err != nil {
				return err
			}
			inst, err := s.Update(cmd.Context(), args[0], time.Now())
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "updated %s at %s\n", args[0], commitOrSource(inst))
			return err
		},
	}
}
