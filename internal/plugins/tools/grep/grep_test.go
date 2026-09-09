package grep_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/grep"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

type captureHost struct{ tools []tool.Tool }

func (h *captureHost) RegisterTool(t tool.Tool) error           { h.tools = append(h.tools, t); return nil }
func (h *captureHost) RegisterCommand(plugin.Command) error     { return nil }
func (h *captureHost) RegisterProvider(provider.Provider) error { return nil }
func (h *captureHost) RegisterHook(plugin.HookHandler) error    { return nil }
func (h *captureHost) Config() map[string]any                   { return nil }
func (h *captureHost) Notice(string)                            {}

func load(t *testing.T) tool.Tool {
	t.Helper()
	h := &captureHost{}
	if err := grep.New().Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	return h.tools[0]
}

func call(t *testing.T, tl tool.Tool, root, input string) tool.Result {
	t.Helper()
	res, err := tl.Invoke(context.Background(), tool.Call{Input: json.RawMessage(input), Workspace: session.Workspace{Root: root}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func seed(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	files := map[string]string{
		"a.go":        "package a\nfunc Hello() {}\n",
		"sub/b.go":    "package sub\n// Hello again\n",
		"sub/c.txt":   "hello lowercase\n",
		".git/config": "Hello in git\n",
		"bin.dat":     "Hello\x00binary",
	}
	for name, content := range files {
		p := filepath.Join(ws, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

func TestGrepFindsMatchesSkippingGitAndBinary(t *testing.T) {
	ws := seed(t)
	tl := load(t)
	if tl.Name != "grep" || tl.Safety != tool.Safe {
		t.Fatalf("tool = %+v", tl)
	}
	res := call(t, tl, ws, `{"pattern":"Hello"}`)
	out := res.Content[0].Text
	if res.IsError {
		t.Fatal(out)
	}
	for _, want := range []string{"a.go:2:func Hello() {}", "sub/b.go:2:// Hello again"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	for _, no := range []string{".git/", "bin.dat", "c.txt"} {
		if strings.Contains(out, no) {
			t.Errorf("unexpected %q in %q", no, out)
		}
	}
}

func TestGrepGlobPathAndMax(t *testing.T) {
	ws := seed(t)
	tl := load(t)
	res := call(t, tl, ws, `{"pattern":"(?i)hello","glob":"**/*.txt"}`)
	if out := res.Content[0].Text; !strings.Contains(out, "sub/c.txt:1:") || strings.Contains(out, "a.go") {
		t.Fatalf("glob: %q", out)
	}
	res = call(t, tl, ws, `{"pattern":"Hello","path":"sub"}`)
	if out := res.Content[0].Text; !strings.Contains(out, "sub/b.go:2:") || strings.Contains(out, "a.go") {
		t.Fatalf("path: %q", out)
	}
	res = call(t, tl, ws, `{"pattern":"(?i)hello","max":1}`)
	if out := res.Content[0].Text; strings.Count(out, "\n") != 2 || !strings.Contains(out, "truncated at 1 matches") {
		t.Fatalf("max: %q", out)
	}
	res = call(t, tl, ws, `{"pattern":"nomatch"}`)
	if res.IsError || res.Content[0].Text != "no matches\n" {
		t.Fatalf("none: %q err=%v", res.Content[0].Text, res.IsError)
	}
}

func TestGrepRefusesBadPatternAndEscape(t *testing.T) {
	ws := seed(t)
	tl := load(t)
	if res := call(t, tl, ws, `{"pattern":"("}`); !res.IsError {
		t.Fatal("bad regexp accepted")
	}
	if res := call(t, tl, ws, `{"pattern":"x","path":"../"}`); !res.IsError || !strings.Contains(res.Content[0].Text, "escapes") {
		t.Fatalf("escape: %q", res.Content[0].Text)
	}
}
