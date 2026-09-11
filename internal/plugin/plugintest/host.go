// Package plugintest is the plugin.Host a test hands a plugin's Init to see what it
// registers. It exists because every plugin's test needs the same stub, and plugin.Host grows:
// one implementation here is one place to add a method when it does.
package plugintest

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/agentdef"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// Host records every registration and notice, in order, and accepts all of them. Its zero
// value is ready to use; set Cfg to give the plugin a config table, Name to own what it
// draws and NoteErr to make Note fail.
type Host struct {
	Name               string
	RegisteredTools    []tool.Tool
	RegisteredCommands []plugin.Command
	Providers          []provider.Provider
	Agents             []agentdef.Definition
	Hooks              []plugin.HookHandler
	Notices            []string
	Status             []plugin.StatusItem
	Widgets            []plugin.Widget
	Notes              []session.Note
	PluginStatuses     []plugin.Status
	Cfg                map[string]any
	NoteErr            error
}

var _ plugin.Host = (*Host)(nil)

func (h *Host) RegisterTool(t tool.Tool) error {
	h.RegisteredTools = append(h.RegisteredTools, t)
	return nil
}

func (h *Host) RegisterCommand(c plugin.Command) error {
	h.RegisteredCommands = append(h.RegisteredCommands, c)
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

func (h *Host) RegisterAgent(d agentdef.Definition) error {
	h.Agents = append(h.Agents, d)
	return nil
}

func (h *Host) Config() map[string]any {
	if h.Cfg != nil {
		return h.Cfg
	}
	return map[string]any{}
}

func (h *Host) Notice(text string) { h.Notices = append(h.Notices, text) }

// SetStatus records what the plugin drew, latest last: a test reads the whole sequence, not
// just the current cell.
func (h *Host) SetStatus(key string, content []plugin.Span) {
	h.Status = append(h.Status, plugin.StatusItem{Owner: h.Name, Key: key, Content: content})
}

// SetWidget refuses an unknown slot the way the real host does, so a plugin's own error
// path is exercised here too.
func (h *Host) SetWidget(key string, slot plugin.WidgetSlot, content []plugin.Span) error {
	if !slot.Valid() {
		return fmt.Errorf("plugin %s: unknown widget slot %q", h.Name, slot)
	}
	h.Widgets = append(h.Widgets, plugin.Widget{Owner: h.Name, Key: key, Slot: slot, Content: content})
	return nil
}

// Note records the note. Set NoteErr to make it fail.
func (h *Host) Note(_ ulid.ULID, text string, role session.NoteRole) error {
	if h.NoteErr != nil {
		return h.NoteErr
	}
	h.Notes = append(h.Notes, session.Note{Plugin: h.Name, Text: text, Role: role})
	return nil
}

// Connect has no server behind it: a test that needs one drives the real registry with
// server.PluginServices instead.
func (h *Host) Connect(context.Context) (*protocol.Client, error) { return nil, plugin.ErrNoServer }

func (h *Host) Commands() []plugin.Command { return h.RegisteredCommands }
func (h *Host) Tools() []tool.Tool         { return h.RegisteredTools }
func (h *Host) Statuses() []plugin.Status  { return h.PluginStatuses }

// AgentDefs indexes RegisteredAgents by name, the shape the real registry's getter has.
func (h *Host) AgentDefs() map[string]agentdef.Definition {
	out := make(map[string]agentdef.Definition, len(h.Agents))
	for _, d := range h.Agents {
		out[d.Name] = d
	}
	return out
}
