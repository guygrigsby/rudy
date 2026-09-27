// SPDX-License-Identifier: AGPL-3.0-or-later

// Package codex registers the built-in Codex AgentRuntime and its login command.
package codex

import (
	"context"
	"io"
	"strings"

	"github.com/guygrigsby/rudy/internal/agentruntime"
	"github.com/guygrigsby/rudy/internal/plugin"
)

type codexPlugin struct {
	runtime agentruntime.Runtime
}

func New(runtime agentruntime.Runtime) plugin.Plugin { return &codexPlugin{runtime: runtime} }

func (*codexPlugin) Name() string { return "codex" }

func (p *codexPlugin) Init(_ context.Context, host plugin.Host) error {
	if err := host.RegisterRuntime(p.runtime); err != nil {
		return err
	}
	return host.RegisterCommand(plugin.Command{
		Name:        "login",
		Description: "Sign in to Codex with a ChatGPT subscription",
		Run: func(_ context.Context, call plugin.CommandCall) (plugin.Action, error) {
			return plugin.AuthChallenge{Runtime: p.runtime.Name(), Mode: agentruntime.LoginMode(strings.TrimSpace(call.Args))}, nil
		},
	})
}

func (p *codexPlugin) Close() error {
	if closer, ok := p.runtime.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}
