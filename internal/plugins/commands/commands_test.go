// SPDX-License-Identifier: AGPL-3.0-or-later

package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugin/plugintest"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

// register loads the plugin into a fresh host and returns its registered commands by name, so a
// test can call the one it needs without hardcoding registration order.
func register(t *testing.T, h *plugintest.Host) map[string]plugin.Command {
	t.Helper()
	if err := New().Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if len(h.RegisteredCommands) != 6 {
		t.Fatalf("registered %d commands, want 6: %+v", len(h.RegisteredCommands), h.RegisteredCommands)
	}
	byName := map[string]plugin.Command{}
	for _, c := range h.RegisteredCommands {
		byName[c.Name] = c
	}
	return byName
}

func TestModelCommand(t *testing.T) {
	cmds := register(t, &plugintest.Host{Name: "commands"})
	act, err := cmds["model"].Run(context.Background(), plugin.CommandCall{Args: "p:x"})
	if err != nil {
		t.Fatal(err)
	}
	sm, ok := act.(plugin.SetModel)
	if !ok || sm.Model != "p:x" {
		t.Fatalf("action %#v", act)
	}

}

// TestBareModelListsWhatThereIs: /model with nothing after it used to answer with its own
// usage line, which tells a person the syntax and not the one thing they asked for. It
// lists the registry the way /permissions with no argument lists the modes, and marks the
// one the session is on.
func TestBareModelListsWhatThereIs(t *testing.T) {
	h := &plugintest.Host{Name: "commands", ModelSet: []provider.Model{
		{Ref: session.ModelRef{Provider: "aperture", Model: "kimi"}, DisplayName: "Kimi"},
		{Ref: session.ModelRef{Provider: "anthropic", Model: "claude-opus-5"}, DisplayName: "Opus 5"},
		{Ref: session.ModelRef{Provider: "aperture", Model: "claude-opus-5"}, DisplayName: "Opus 5"},
	}}
	cmds := register(t, h)
	act, err := cmds["model"].Run(context.Background(), plugin.CommandCall{
		Model: session.ModelRef{Provider: "aperture", Model: "kimi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	n, ok := act.(plugin.Notice)
	if !ok {
		t.Fatalf("bare /model action %#v", act)
	}
	for _, want := range []string{"aperture:kimi", "anthropic:claude-opus-5", "aperture:claude-opus-5"} {
		if !strings.Contains(n.Text, want) {
			t.Errorf("the listing leaves out %s:\n%s", want, n.Text)
		}
	}
	// The one in force is marked, since the whole point of asking is to see where you are.
	for _, line := range strings.Split(n.Text, "\n") {
		if strings.Contains(line, "aperture:kimi") && !strings.Contains(line, "*") {
			t.Errorf("the model in force is not marked:\n%s", n.Text)
		}
	}
	if !strings.Contains(n.Text, "/model <provider:id>") {
		t.Errorf("the listing never says how to switch:\n%s", n.Text)
	}
}

// TestBareModelWithAnEmptyRegistry: a registry that answered nothing is a different thing
// from a model nobody named, and saying so beats printing an empty list.
func TestBareModelWithAnEmptyRegistry(t *testing.T) {
	cmds := register(t, &plugintest.Host{Name: "commands"})
	act, err := cmds["model"].Run(context.Background(), plugin.CommandCall{})
	if err != nil {
		t.Fatal(err)
	}
	n, ok := act.(plugin.Notice)
	if !ok || !strings.Contains(n.Text, "no models") {
		t.Fatalf("bare /model with an empty registry: %#v", act)
	}
}

func TestHelpCommandListsTheRegistryInOrder(t *testing.T) {
	cmds := register(t, &plugintest.Host{Name: "commands"})
	act, err := cmds["help"].Run(context.Background(), plugin.CommandCall{})
	if err != nil {
		t.Fatal(err)
	}
	n, ok := act.(plugin.Notice)
	if !ok {
		t.Fatalf("action %T", act)
	}
	want := "/model  List the models, or switch this session's: /model [provider:id or unique id]\n" +
		"/help  List the slash commands\n" +
		"/permissions  Show or set how rudy asks before an unsafe tool: /permissions [strict|permissive|off]\n" +
		"/rename  Name this session: /rename <name>\n" +
		"/fork  Fork this session at an entry: /fork [entry id], default the newest\n" +
		"/plugins  List loaded plugins and their state"
	if n.Text != want {
		t.Fatalf("/help text =\n%s\nwant\n%s", n.Text, want)
	}
}

// TestRenameCommand is /rename's own half: the trimmed name as a SetTitle, and the usage
// notice when there is nothing to name it.
func TestRenameCommand(t *testing.T) {
	cmds := register(t, &plugintest.Host{Name: "commands"})
	act, err := cmds["rename"].Run(context.Background(), plugin.CommandCall{Args: "  the flaky fork test "})
	if err != nil {
		t.Fatal(err)
	}
	st, ok := act.(plugin.SetTitle)
	if !ok || st.Title != "the flaky fork test" {
		t.Fatalf("action %#v", act)
	}
	act, err = cmds["rename"].Run(context.Background(), plugin.CommandCall{})
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := act.(plugin.Notice); !ok || n.Text != "usage: /rename <name>" {
		t.Fatalf("no name is a usage notice, got %#v", act)
	}
}

// TestPermissionsCommand: no argument reports the mode in force and what the others mean,
// an argument sets it, and anything else is refused naming what was typed.
func TestPermissionsCommand(t *testing.T) {
	cmds := register(t, &plugintest.Host{Name: "commands"})
	act, err := cmds["permissions"].Run(context.Background(), plugin.CommandCall{Mode: session.ModeStrict})
	if err != nil {
		t.Fatal(err)
	}
	n, ok := act.(plugin.Notice)
	if !ok || !strings.HasPrefix(n.Text, "permissions: strict") {
		t.Fatalf("no argument reports the mode: %#v", act)
	}
	for _, want := range []string{"permissive", "off", "dangerous set"} {
		if !strings.Contains(n.Text, want) {
			t.Errorf("and says what the others do, missing %q:\n%s", want, n.Text)
		}
	}
	act, err = cmds["permissions"].Run(context.Background(), plugin.CommandCall{Args: " off "})
	if err != nil {
		t.Fatal(err)
	}
	if sm, ok := act.(plugin.SetMode); !ok || sm.Mode != session.ModeOff {
		t.Fatalf("action %#v", act)
	}
	if _, err := cmds["permissions"].Run(context.Background(), plugin.CommandCall{Args: "yolo"}); err == nil || !strings.Contains(err.Error(), "yolo") {
		t.Errorf("an unknown mode names itself: %v", err)
	}
}

func TestForkCommand(t *testing.T) {
	cmds := register(t, &plugintest.Host{Name: "commands"})
	act, err := cmds["fork"].Run(context.Background(), plugin.CommandCall{})
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := act.(plugin.Fork); !ok || f.AtEntryID != "" {
		t.Fatalf("bare /fork action %#v", act)
	}

	const id = "01K4M0A7Q8ZJ3N6R9T2V5X8B1D"
	act, err = cmds["fork"].Run(context.Background(), plugin.CommandCall{Args: id})
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := act.(plugin.Fork); !ok || f.AtEntryID != id {
		t.Fatalf("/fork %s action %#v", id, act)
	}

	if _, err := cmds["fork"].Run(context.Background(), plugin.CommandCall{Args: "nope"}); err == nil {
		t.Fatal("/fork nope: want an error, got nil")
	}
}

func TestPluginsCommand(t *testing.T) {
	h := &plugintest.Host{Name: "commands"}
	h.PluginStatuses = []plugin.Status{
		{Name: "commands", State: plugin.StateReady},
		{Name: "openai_chat", State: plugin.StateFailed, Reason: "no api key"},
	}
	cmds := register(t, h)
	act, err := cmds["plugins"].Run(context.Background(), plugin.CommandCall{})
	if err != nil {
		t.Fatal(err)
	}
	n, ok := act.(plugin.Notice)
	if !ok {
		t.Fatalf("action %T", act)
	}
	want := "commands  ready\nopenai_chat  failed  no api key"
	if n.Text != want {
		t.Fatalf("/plugins text =\n%s\nwant\n%s", n.Text, want)
	}
}
