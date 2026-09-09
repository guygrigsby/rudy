package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

func TestStatusLineIsTheDesignScreen(t *testing.T) {
	h := newHarness(t, nil)
	h.appended(session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopEndTurn,
		Content: []session.Block{session.TextBlock("done")},
		Usage:   session.Usage{Input: 20000, Output: 1000, CacheRead: 22000},
	})
	got := ansi.Strip(h.m.statusLine())
	want := "INSERT  fake:m1  strict  42%  $0.08  rudy main*"
	if got != want {
		t.Fatalf("status %q want %q", got, want)
	}
	if !strings.HasSuffix(strings.TrimRight(h.view(), "\n"), h.m.statusLine()) {
		t.Error("the status line is the last thing drawn")
	}
}

func TestStatusItemsRenderOnlyWhatConfigPlaced(t *testing.T) {
	h := newHarness(t, map[string]any{"ui.status.items": []string{"workspace", "vim_mode"}})
	if got := ansi.Strip(h.m.statusLine()); got != "rudy main*  INSERT" {
		t.Fatalf("status %q", got)
	}
}

func TestVimModeItem(t *testing.T) {
	h := newHarness(t, map[string]any{"ui.status.items": []string{"vim_mode", "model"}})
	if got := ansi.Strip(h.m.statusLine()); got != "INSERT  fake:m1" {
		t.Fatalf("insert %q", got)
	}
	h.press("escape") // vim insert to normal
	if got := ansi.Strip(h.m.statusLine()); got != "NORMAL  fake:m1" {
		t.Fatalf("normal %q", got)
	}
	off := newHarness(t, map[string]any{"ui.vim": false, "ui.status.items": []string{"vim_mode", "model"}})
	if got := ansi.Strip(off.m.statusLine()); got != "fake:m1" {
		t.Fatalf("vim off must leave no cell and no separator: %q", got)
	}
}

func TestPluginStatusItem(t *testing.T) {
	h := newHarness(t, map[string]any{"ui.status.items": []string{"memory:servers", "model"}})
	if got := ansi.Strip(h.m.statusLine()); got != "fake:m1" {
		t.Fatalf("an item no plugin set renders nothing: %q", got)
	}
	h.notify(protocol.NotifyStatusUpdated, protocol.StatusUpdated{Items: []protocol.StatusItem{
		{Owner: "memory", Key: "servers", Content: []protocol.Span{
			{Text: "2", Role: "success"}, {Text: " servers", Role: "muted"},
		}},
	}})
	if got := ansi.Strip(h.m.statusLine()); got != "2 servers  fake:m1" {
		t.Fatalf("status %q", got)
	}
	// A span is data: escapes and newlines a plugin sends never reach the screen.
	h.notify(protocol.NotifyStatusUpdated, protocol.StatusUpdated{Items: []protocol.StatusItem{
		{Owner: "memory", Key: "servers", Content: []protocol.Span{{Text: "\x1b[31mred\nnext", Role: "muted"}}},
	}})
	if got := ansi.Strip(h.m.statusLine()); got != "rednext  fake:m1" {
		t.Fatalf("unsanitized span %q", got)
	}
}

func TestContextPercent(t *testing.T) {
	for _, c := range []struct {
		name   string
		window int64
		prompt int64
		want   string
	}{
		{"unknown window", 0, 42000, ""},
		{"none used", 100000, 0, "0%"},
		{"the design screen", 100000, 42000, "42%"},
		{"truncates down", 100000, 42999, "42%"},
		{"full", 100000, 100000, "100%"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := contextPercent(c.window, c.prompt); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestCost(t *testing.T) {
	priced := testModels()[0].Pricing
	for _, c := range []struct {
		name    string
		pricing provider.Pricing
		usage   session.Usage
		want    string
	}{
		{"nothing spent", priced, session.Usage{}, ""},
		{"no prices", provider.Pricing{}, session.Usage{Input: 10}, ""},
		{"input and output", priced, session.Usage{Input: 20000, Output: 1000}, "$0.08"},
		{"cache counts", priced, session.Usage{Input: 20000, Output: 1000, CacheRead: 22000}, "$0.08"},
		{"rounds to cents", priced, session.Usage{Input: 1000000, Output: 100000}, "$4.50"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := cost(c.pricing, c.usage); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestWorkspaceItem(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("commit", "-q", "--allow-empty", "-m", "root")
	want := filepath.Base(dir) + " main"
	if got := detectWorkspace(dir, dir); got != want {
		t.Fatalf("clean %q want %q", got, want)
	}
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := detectWorkspace(dir, dir); got != want+"*" {
		t.Fatalf("dirty %q want %q", got, want+"*")
	}
	// The git root the session already detected is used as is; only the branch and the
	// dirty flag cost a subprocess.
	if got := detectWorkspace("", dir); got != want+"*" {
		t.Fatalf("no root given %q", got)
	}
	if got := detectWorkspace("", t.TempDir()); got != "" {
		t.Fatalf("outside a repository %q", got)
	}
}
