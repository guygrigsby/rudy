// SPDX-License-Identifier: AGPL-3.0-or-later

// Package cli holds the cobra commands. The root carries global flags; every
// subcommand lives in its own file and attaches in NewRoot.
package cli

import (
	"context"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version is set by the linker, by make and by the release workflow:
// -X github.com/guygrigsby/rudy/internal/cli.version=<v>
// Empty is a build nobody stamped, which is what `go install` produces, and Version reads
// what the toolchain recorded instead of naming it after a word.
var version = ""

// unknownVersion is what a binary that carries no version and no commit calls itself. It is
// deliberately not a revision token: hosts.Revision refuses it rather than asking a box to
// check it out.
const unknownVersion = "unknown"

// devSuffix marks a build made from a tree with uncommitted changes. It reads as what it
// is, a build of something that is not any commit, rather than as a complaint about the
// tree being dirty.
const devSuffix = "-dev"

// revisionLen is how much of a stamped commit the version carries. Long enough to be
// unambiguous in this repository and short enough to read in a status line.
const revisionLen = 12

// Version reports the build version: what the linker stamped, else the commit the toolchain
// recorded for a build from a checkout, else the module version a `go install pkg@version`
// carries, else unknown.
func Version() string { return resolveVersion(version, debug.ReadBuildInfo) }

// resolveVersion is Version's rule, with the build info passed in so it can be tested.
//
// The commit is preferred over Main.Version for a checkout build because Main.Version there
// is a pseudo-version (v0.0.0-<time>-<commit>, +dirty when the tree was), which names the
// commit in a form nothing can check out; `rudy hosts install` needs the revision itself.
// A binary installed from the module proxy has no commit stamped and a real version.
func resolveVersion(linker string, read func() (*debug.BuildInfo, bool)) string {
	if linker != "" {
		return linker
	}
	bi, ok := read()
	if !ok {
		return unknownVersion
	}
	var rev string
	var modified bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if rev != "" {
		if len(rev) > revisionLen {
			rev = rev[:revisionLen]
		}
		// The same suffix make asks git describe for, and the same one hosts.Revision
		// refuses: a tree with uncommitted changes names no commit a box can check out.
		if modified {
			return rev + devSuffix
		}
		return rev
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	return unknownVersion
}

// NewRoot is the rudy command with the real wiring. Same signature as Task 1.
func NewRoot() *cobra.Command {
	v := Version()
	return newRoot(v, func(ctx context.Context, o BuildOptions) (*Built, error) {
		o.Version = v
		return Build(ctx, o)
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
	root.AddCommand(newServeCommand(build), newBridgeCommand(build), newModelsCommand(build), newSessionsCommand(build), newSkillsCommand(), newMCPCommand(), newPluginCommand(), newConfigCmd(), newPromptCmd(build), newInstallCmd(), newHostsCommand(build))
	return root
}
