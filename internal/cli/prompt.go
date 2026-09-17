// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/turn"
	"github.com/guygrigsby/rudy/internal/workspace"
)

// newPromptCmd is `rudy prompt`: what the model is told before anything else, and how to
// say it differently. ADR 0024.
func newPromptCmd(build buildFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "prompt",
		Short: "show and override the system prompt",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newPromptShowCmd(build), newPromptExampleCmd(), newPromptPathCmd())
	return cmd
}

// newPromptShowCmd prints the prompt a session in this directory would send, template
// expanded, tools and all. It builds the server the way a session does, because the tool
// list is the plugins' and the AGENTS.md sections are the workspace's.
func newPromptShowCmd(build buildFunc) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "print the system prompt a session here would send",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := build(cmd.Context(), BuildOptions{Stderr: cmd.ErrOrStderr()})
			if err != nil {
				return err
			}
			defer func() { _ = b.Close(context.Background()) }()
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			ws, err := workspace.Detect(cwd)
			if err != nil {
				return err
			}
			template := b.Prompt
			if template == "" {
				template = turn.DefaultTemplate
			}
			out, err := turn.Build(template, turn.Vars{
				Base:      turn.BaseParagraph(ws, b.Version),
				Tools:     turn.ToolList(b.Plugins.Tools()),
				Agents:    turn.AgentsSections(ws),
				Version:   b.Version,
				Workspace: ws.Root,
				Project:   ws.ProjectID,
				Model:     b.Config.Default.Provider + ":" + b.Config.Default.Model,
				Date:      time.Now().Format("2006-01-02"),
				OS:        runtime.GOOS,
			})
			if err != nil {
				return err
			}
			_, _ = fmt.Fprint(cmd.OutOrStdout(), out)
			return nil
		},
	}
}

// newPromptExampleCmd prints the built-in template, which is the file to start from: what
// an operator overrides is a template exactly like this one.
func newPromptExampleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "example",
		Short: "print the built-in template, with the values a template may use",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var b strings.Builder
			b.WriteString("<!-- rudy's built-in system prompt template.\n\n")
			b.WriteString("Save your own as system.md under the config directory, or name one in\n")
			b.WriteString("[prompt] file. The values a template may use, each empty when there is\n")
			b.WriteString("nothing to say:\n\n")
			for _, name := range turn.VarNames() {
				fmt.Fprintf(&b, "  ${%s}%s%s\n", name, strings.Repeat(" ", max(1, 12-len(name))), varHelp[name])
			}
			b.WriteString("\n$${tools} writes a literal ${tools}. A name outside the set is an error\n")
			b.WriteString("naming it, and the session falls back to this prompt. -->\n\n")
			b.WriteString(turn.DefaultTemplate)
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), b.String())
			return nil
		},
	}
}

// newPromptPathCmd prints the file a prompt would be read from, whether or not it exists.
func newPromptPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "print the file the system prompt is read from",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := configPaths()
			if err != nil {
				return err
			}
			cfg, err := config.Load(paths, nil)
			if err != nil {
				return err
			}
			path := config.ExpandHome(cfg.Prompt.File, paths.Home)
			if path == "" {
				path = filepath.Join(paths.Config, "system.md")
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), path)
			return nil
		},
	}
}

// varHelp is one line per template value, for `rudy prompt example`.
var varHelp = map[string]string{
	"base":      "the built-in opening paragraph, or an agent definition's body",
	"tools":     "the tools this turn offers, one line each",
	"agents":    "the workspace's AGENTS.md and ~/.agents/AGENTS.md",
	"version":   "the rudy build",
	"workspace": "the workspace root",
	"project":   "the project id",
	"model":     "provider:model for the session",
	"date":      "today, so the model stops guessing",
	"os":        "the platform the tools run on",
}
