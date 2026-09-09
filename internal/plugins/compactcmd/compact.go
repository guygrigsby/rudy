// Package compactcmd is the /compact slash command: summarize the session so far and continue
// from the summary. The work is the kernel's Compactor; the command is only how a user asks
// for it, registered through the same plugin interface an external command would use.
package compactcmd

import (
	"context"
	"strings"

	"github.com/guygrigsby/rudy/internal/plugin"
)

type compactPlugin struct{}

func New() plugin.Plugin { return compactPlugin{} }

func (compactPlugin) Name() string { return "compact" }

func (compactPlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterCommand(plugin.Command{
		Name:        "compact",
		Description: "Summarize the conversation so far and continue from the summary",
		Run: func(ctx context.Context, call plugin.CommandCall) (plugin.Action, error) {
			// Arguments are instructions for the summary ("/compact focus on decisions"),
			// which also mean the before_compaction hook is not asked.
			return plugin.Compact{Instructions: strings.TrimSpace(call.Args)}, nil
		},
	})
}
