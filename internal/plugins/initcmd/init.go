// Package initcmd is the /init slash command: create or revise the workspace's AGENTS.md.
package initcmd

import (
	"context"

	"github.com/guygrigsby/rudy/internal/plugin"
)

// Prompt is the user message /init submits. It is the whole behavior of the command.
const Prompt = `Initialize this workspace's AGENTS.md.

Inspect the repository with the glob, grep and read tools: README and other top-level docs; build files such as Makefile, go.mod, package.json, pyproject.toml and Cargo.toml; the directory layout two levels deep; the test layout; CI configuration; and any existing AGENTS.md or CLAUDE.md.

If ./AGENTS.md exists, read it first and revise it: keep what is still true, correct what is not.

Then write ./AGENTS.md with the write tool. Contents, in this order:
1. What the project is, in two or three sentences.
2. How to build, test and lint, with the exact commands.
3. Conventions you observed: language version, formatting, naming, commit style, anything a contributor would be corrected on.
4. Where things are: a short map of the important directories and entry points.

Under 80 lines. No placeholders, no sections you could not fill from the repository, no speculation. When done, reply with one sentence saying what you wrote.`

type initPlugin struct{}

func New() plugin.Plugin { return initPlugin{} }

func (initPlugin) Name() string { return "init" }

func (initPlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterCommand(plugin.Command{
		Name:        "init",
		Description: "Create or revise AGENTS.md for this workspace",
		Run: func(ctx context.Context, call plugin.CommandCall) (plugin.Action, error) {
			return plugin.SubmitPrompt{Text: Prompt}, nil
		},
	})
}
