// Package plugintest is the plugin.Host a test hands a plugin's Init to see what it
// registers. It exists because every plugin's test needs the same stub, and plugin.Host grows:
// one implementation here is one place to add a method when it does.
package plugintest

import (
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/tool"
)

// Host records every registration and notice, in order, and accepts all of them. Its zero
// value is ready to use; set Cfg to give the plugin a config table.
type Host struct {
	Tools     []tool.Tool
	Commands  []plugin.Command
	Providers []provider.Provider
	Hooks     []plugin.HookHandler
	Notices   []string
	Cfg       map[string]any
}

var _ plugin.Host = (*Host)(nil)

func (h *Host) RegisterTool(t tool.Tool) error {
	h.Tools = append(h.Tools, t)
	return nil
}

func (h *Host) RegisterCommand(c plugin.Command) error {
	h.Commands = append(h.Commands, c)
	return nil
}

func (h *Host) RegisterProvider(p provider.Provider) error {
	h.Providers = append(h.Providers, p)
	return nil
}

func (h *Host) RegisterHook(hh plugin.HookHandler) error {
	h.Hooks = append(h.Hooks, hh)
	return nil
}

func (h *Host) Config() map[string]any {
	if h.Cfg != nil {
		return h.Cfg
	}
	return map[string]any{}
}

func (h *Host) Notice(text string) { h.Notices = append(h.Notices, text) }
