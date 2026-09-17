// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"

	"github.com/guygrigsby/rudy/internal/config"
	mcpplugin "github.com/guygrigsby/rudy/internal/plugins/mcp"
)

// newMCPCommand is the "mcp" noun with "add", "remove", "list" and "get" under it. None of
// them builds a server or connects to one: they edit mcp.toml, which the mcp plugin reads at
// the next boot.
func newMCPCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "manage MCP servers",
	}
	cmd.AddCommand(newMCPAddCommand(), newMCPRemoveCommand(), newMCPListCommand(), newMCPGetCommand())
	return cmd
}

// mcpPaths are the two files rudy mcp reads and writes. Project is "" when the working
// directory has no workspace, which makes --scope project an error rather than a write to
// nowhere.
type mcpPaths struct {
	user    string
	project string
}

func mcpFilePaths() (mcpPaths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return mcpPaths{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		// No working directory is no project scope, not a reason to refuse the command.
		cwd = ""
	}
	user, project := mcpplugin.Paths(config.XDG(os.Getenv, home).Config, cwd)
	return mcpPaths{user: user, project: project}, nil
}

// path returns the file one scope is kept in.
func (p mcpPaths) path(scope string) (string, error) {
	switch scope {
	case mcpplugin.ScopeUser:
		return p.user, nil
	case mcpplugin.ScopeProject:
		if p.project == "" {
			return "", fmt.Errorf("no workspace for --scope project")
		}
		return p.project, nil
	default:
		return "", fmt.Errorf("unknown scope %q, want user or project", scope)
	}
}

func newMCPAddCommand() *cobra.Command {
	var transport, scope string
	var env, headers []string
	cmd := &cobra.Command{
		Use:   "add <name> <command-or-url> [args...]",
		Short: "add an MCP server to mcp.toml",
		Long: "Add an MCP server to mcp.toml.\n\n" +
			"Every rudy flag goes before the name: everything after the command is passed to the\n" +
			"server as its own arguments.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			known := func(n string) bool { return cmd.Flags().Lookup(n) != nil }
			if err := refuseMisplacedFlag(known, args[1:]); err != nil {
				return err
			}
			paths, err := mcpFilePaths()
			if err != nil {
				return err
			}
			path, err := paths.path(scope)
			if err != nil {
				return err
			}
			cfg := mcpplugin.ServerConfig{Name: args[0], Transport: mcpplugin.Transport(transport)}
			switch cfg.Transport {
			case mcpplugin.TransportHTTP:
				cfg.URL = args[1]
				if len(args) > 2 {
					return fmt.Errorf("an http server takes a url and no arguments, got %v", args[2:])
				}
			default:
				cfg.Command = args[1]
				cfg.Args = args[2:]
			}
			if cfg.Env, err = keyValueFlag(env, "="); err != nil {
				return fmt.Errorf("--env: %w", err)
			}
			if cfg.Headers, err = keyValueFlag(headers, ":"); err != nil {
				return fmt.Errorf("--header: %w", err)
			}
			if err := cfg.Validate(); err != nil {
				return err
			}
			f, err := mcpplugin.ReadFile(path)
			if err != nil {
				return err
			}
			f.Servers[cfg.Name] = cfg
			return mcpplugin.WriteFile(path, f)
		},
	}
	cmd.Flags().StringVar(&transport, "transport", string(mcpplugin.TransportStdio), "stdio or http")
	cmd.Flags().StringVar(&scope, "scope", mcpplugin.ScopeUser, "user or project")
	cmd.Flags().StringArrayVar(&env, "env", nil, "KEY=<secret reference> for a stdio server (repeatable)")
	cmd.Flags().StringArrayVar(&headers, "header", nil, "\"Name: <secret reference>\" for an http server (repeatable)")
	// Everything after the name is the server's own argument list, flags included.
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// refuseMisplacedFlag refuses an argument that names one of add's own long flags.
// Interspersing is off, so a rudy flag written after the server name is silently stored as an
// argument to the server instead; refusing it is the difference between a typo and a server
// started with flags nobody meant. The names come from the flag set itself, so they cannot
// drift from the flags the command actually parses.
func refuseMisplacedFlag(known func(string) bool, args []string) error {
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if !known(name) {
			continue
		}
		return fmt.Errorf("%s would be passed to the server: rudy flags go before the server name", a)
	}
	return nil
}

// keyValueFlag parses repeated "KEY<sep>value" flag values into a table. Values are secret
// references, kept as written: nothing is resolved until a server is connected.
func keyValueFlag(values []string, sep string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(values))
	for _, v := range values {
		k, val, ok := strings.Cut(v, sep)
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("%q is not KEY%svalue", v, sep)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(val)
	}
	return out, nil
}

func newMCPRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "remove an MCP server from mcp.toml",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			paths, err := mcpFilePaths()
			if err != nil {
				return err
			}
			name := args[0]
			removed := false
			// A name is removed from whichever scope holds it: asking for a scope the
			// operator would have to guess at is worse than removing both copies.
			for _, path := range []string{paths.user, paths.project} {
				if path == "" {
					continue
				}
				f, err := mcpplugin.ReadFile(path)
				if err != nil {
					return err
				}
				if _, ok := f.Servers[name]; !ok {
					continue
				}
				delete(f.Servers, name)
				if err := mcpplugin.WriteFile(path, f); err != nil {
					return err
				}
				removed = true
			}
			if !removed {
				return fmt.Errorf("no server named %s", name)
			}
			return nil
		},
	}
}

func newMCPListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "list the configured MCP servers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			configs, scopes, err := mergedMCP()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "NAME\tSCOPE\tTRANSPORT\tTARGET")
			for i, c := range configs {
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.Name, scopes[i], c.Transport, mcpTarget(c))
			}
			return tw.Flush()
		},
	}
}

// mcpTarget is what the server is reached at: the command line for stdio, the endpoint for
// http.
func mcpTarget(c mcpplugin.ServerConfig) string {
	if c.Transport == mcpplugin.TransportHTTP {
		return c.URL
	}
	return strings.Join(append([]string{c.Command}, c.Args...), " ")
}

func newMCPGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "print one server's mcp.toml entry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			configs, _, err := mergedMCP()
			if err != nil {
				return err
			}
			for _, c := range configs {
				if c.Name != args[0] {
					continue
				}
				b, err := toml.Marshal(mcpplugin.File{Servers: map[string]mcpplugin.ServerConfig{c.Name: c}})
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(b)
				return err
			}
			return fmt.Errorf("no server named %s", args[0])
		},
	}
}

// mergedMCP is the two files as one list, the same merge the plugin loads.
func mergedMCP() ([]mcpplugin.ServerConfig, []string, error) {
	paths, err := mcpFilePaths()
	if err != nil {
		return nil, nil, err
	}
	return mcpplugin.Merged(paths.user, paths.project)
}
