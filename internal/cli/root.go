// Package cli holds the cobra commands. The root carries global flags; every
// subcommand lives in its own file and attaches in NewRoot.
package cli

import (
	"context"
	"io"

	"github.com/spf13/cobra"
)

// version is set by the linker:
// -X github.com/guygrigsby/rudy/internal/cli.version=<v>
var version = "dev"

// Version reports the build version.
func Version() string { return version }

// NewRoot is the rudy command with the real wiring. Same signature as Task 1.
func NewRoot() *cobra.Command {
	return newRoot(version, func(ctx context.Context, stderr io.Writer) (*Built, error) {
		return Build(ctx, BuildOptions{Version: version, Stderr: stderr})
	})
}

// newRoot takes the wiring as a parameter so tests can substitute a fake provider.
func newRoot(version string, build buildFunc) *cobra.Command {
	root := &cobra.Command{
		Use:           "rudy [prompt]",
		Short:         "a coding agent harness",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("rudy {{.Version}}\n")
	registerPrint(root, build)
	root.AddCommand(newModelsCommand(build), newSessionsCommand(), newSkillsCommand())
	return root
}
