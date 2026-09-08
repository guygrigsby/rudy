// Package cli holds the cobra commands. The root carries global flags; every
// subcommand lives in its own file and attaches in NewRoot.
package cli

import (
	"github.com/spf13/cobra"
)

// version is set by the linker:
// -X github.com/guygrigsby/rudy/internal/cli.version=<v>
var version = "dev"

// Version reports the build version.
func Version() string { return version }

// NewRoot builds the root command. Subcommands attach here in later tasks.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "rudy",
		Short:         "A coding agent harness",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("rudy {{.Version}}\n")
	return root
}

// Execute runs the root command against os.Args and reports the error once.
func Execute() error {
	root := NewRoot()
	if err := root.Execute(); err != nil {
		root.PrintErrln("rudy:", err)
		return err
	}
	return nil
}
