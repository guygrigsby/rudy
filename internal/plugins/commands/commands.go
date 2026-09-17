// SPDX-License-Identifier: AGPL-3.0-or-later

// Package commands is the kernel's own slash commands: /model, /help, /permissions,
// /rename, /fork and /plugins. They
// have no private path to the server either, each is a plugin.Command registered through the
// same Host interface a spawned plugin's /foo would use.
package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/session"
)

type cmdPlugin struct{}

func New() plugin.Plugin { return cmdPlugin{} }

func (cmdPlugin) Name() string { return "commands" }

func (cmdPlugin) Init(ctx context.Context, h plugin.Host) error {
	cmds := []plugin.Command{
		{Name: "model", Description: "Switch this session's model: /model <provider:id or unique id>", Run: func(ctx context.Context, c plugin.CommandCall) (plugin.Action, error) {
			spec := strings.TrimSpace(c.Args)
			if spec == "" {
				return plugin.Notice{Text: "usage: /model <provider:id>"}, nil
			}
			return plugin.SetModel{Model: spec}, nil
		}},
		{Name: "help", Description: "List the slash commands", Run: func(ctx context.Context, c plugin.CommandCall) (plugin.Action, error) {
			var b strings.Builder
			for _, cmd := range h.Commands() {
				fmt.Fprintf(&b, "/%s  %s\n", cmd.Name, cmd.Description)
			}
			return plugin.Notice{Text: strings.TrimRight(b.String(), "\n")}, nil
		}},
		{Name: "permissions", Description: "Show or set how rudy asks before an unsafe tool: /permissions [strict|permissive|off]", Run: func(ctx context.Context, c plugin.CommandCall) (plugin.Action, error) {
			want := session.Mode(strings.TrimSpace(c.Args))
			if want == "" {
				return plugin.Notice{Text: fmt.Sprintf(
					"permissions: %s\n  strict      ask before an unsafe tool, and deny it when nobody can answer\n  permissive  allow an unsafe tool unless it is in the dangerous set\n  off         allow everything, and record every decision anyway",
					c.Mode)}, nil
			}
			if !want.Valid() {
				return nil, fmt.Errorf("/permissions: %q is not strict, permissive or off", c.Args)
			}
			return plugin.SetMode{Mode: want}, nil
		}},
		{Name: "rename", Description: "Name this session: /rename <name>", Run: func(ctx context.Context, c plugin.CommandCall) (plugin.Action, error) {
			name := strings.TrimSpace(c.Args)
			if name == "" {
				return plugin.Notice{Text: "usage: /rename <name>"}, nil
			}
			return plugin.SetTitle{Title: name}, nil
		}},
		{Name: "fork", Description: "Fork this session at an entry: /fork [entry id], default the newest", Run: func(ctx context.Context, c plugin.CommandCall) (plugin.Action, error) {
			at := strings.TrimSpace(c.Args)
			if at != "" {
				if _, err := ulid.ParseStrict(at); err != nil {
					return nil, fmt.Errorf("/fork: %q is not an entry id", at)
				}
			}
			return plugin.Fork{AtEntryID: at}, nil
		}},
		{Name: "plugins", Description: "List loaded plugins and their state", Run: func(ctx context.Context, c plugin.CommandCall) (plugin.Action, error) {
			var b strings.Builder
			for _, s := range h.Statuses() {
				fmt.Fprintf(&b, "%s  %s", s.Name, s.State)
				if s.Reason != "" {
					fmt.Fprintf(&b, "  %s", s.Reason)
				}
				b.WriteString("\n")
			}
			return plugin.Notice{Text: strings.TrimRight(b.String(), "\n")}, nil
		}},
	}
	for _, c := range cmds {
		if err := h.RegisterCommand(c); err != nil {
			return err
		}
	}
	return nil
}
