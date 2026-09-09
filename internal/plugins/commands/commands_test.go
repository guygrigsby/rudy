package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugin/plugintest"
)

// register loads the plugin into a fresh host and returns its registered commands by name, so a
// test can call the one it needs without hardcoding registration order.
func register(t *testing.T, h *plugintest.Host) map[string]plugin.Command {
	t.Helper()
	if err := New().Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if len(h.RegisteredCommands) != 4 {
		t.Fatalf("registered %d commands, want 4: %+v", len(h.RegisteredCommands), h.RegisteredCommands)
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

	act, err = cmds["model"].Run(context.Background(), plugin.CommandCall{})
	if err != nil {
		t.Fatal(err)
	}
	n, ok := act.(plugin.Notice)
	if !ok || !strings.HasPrefix(n.Text, "usage: /model <provider:id>") {
		t.Fatalf("bare /model action %#v", act)
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
	want := "/model  Switch this session's model: /model <provider:id or unique id>\n" +
		"/help  List the slash commands\n" +
		"/fork  Fork this session at an entry: /fork [entry id], default the newest\n" +
		"/plugins  List loaded plugins and their state"
	if n.Text != want {
		t.Fatalf("/help text =\n%s\nwant\n%s", n.Text, want)
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
