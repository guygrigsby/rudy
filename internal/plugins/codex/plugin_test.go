// SPDX-License-Identifier: AGPL-3.0-or-later

package codex_test

import (
	"context"
	"testing"

	"github.com/guygrigsby/rudy/internal/agentruntime"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugin/plugintest"
	"github.com/guygrigsby/rudy/internal/plugins/codex"
	"github.com/guygrigsby/rudy/internal/provider/codexapp"
)

func TestPluginRegistersCodexRuntimeAndLoginCommandWithoutStarting(t *testing.T) {
	client := codexapp.NewClient(codexapp.Command{Path: "/does/not/exist"})
	t.Cleanup(func() { _ = client.Close() })
	h := &plugintest.Host{}
	if err := codex.New(client).Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if len(h.Runtimes) != 1 || h.Runtimes[0].Name() != "codex" {
		t.Fatalf("runtimes = %+v", h.Runtimes)
	}
	if len(h.RegisteredCommands) != 1 || h.RegisteredCommands[0].Name != "login" {
		t.Fatalf("commands = %+v", h.RegisteredCommands)
	}
	action, err := h.RegisteredCommands[0].Run(context.Background(), plugin.CommandCall{Args: "device"})
	if err != nil {
		t.Fatal(err)
	}
	auth, ok := action.(plugin.AuthChallenge)
	if !ok || auth.Runtime != "codex" || auth.Mode != agentruntime.LoginDevice {
		t.Fatalf("action = %#v", action)
	}
}
