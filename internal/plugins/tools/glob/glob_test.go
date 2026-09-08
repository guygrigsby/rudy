package glob_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/glob"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

type captureHost struct{ tools []tool.Tool }

func (h *captureHost) RegisterTool(t tool.Tool) error           { h.tools = append(h.tools, t); return nil }
func (h *captureHost) RegisterCommand(plugin.Command) error     { return nil }
func (h *captureHost) RegisterProvider(provider.Provider) error { return nil }
func (h *captureHost) Config() map[string]any                   { return nil }
func (h *captureHost) Notice(string)                            {}

func load(t *testing.T) tool.Tool {
	t.Helper()
	h := &captureHost{}
	if err := glob.New().Init(context.Background(), h); err != nil {
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
	for _, name := range []string{"a.go", "sub/b.go", "sub/deep/c.go", "sub/d.txt", ".git/HEAD"} {
		p := filepath.Join(ws, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

func TestGlobListsSortedRelativePaths(t *testing.T) {
	ws := seed(t)
	tl := load(t)
	if tl.Name != "glob" || tl.Safety != tool.Safe {
		t.Fatalf("tool = %+v", tl)
	}
	res := call(t, tl, ws, `{"pattern":"**/*.go"}`)
	if res.IsError || res.Content[0].Text != "a.go\nsub/b.go\nsub/deep/c.go\n" {
		t.Fatalf("got %q err=%v", res.Content[0].Text, res.IsError)
	}
	res = call(t, tl, ws, `{"pattern":"*","path":"sub"}`)
	if res.Content[0].Text != "sub/b.go\nsub/d.txt\nsub/deep\n" {
		t.Fatalf("path: %q", res.Content[0].Text)
	}
	res = call(t, tl, ws, `{"pattern":"**/HEAD"}`)
	if res.IsError || res.Content[0].Text != "no matches\n" {
		t.Fatalf(".git leaked: %q", res.Content[0].Text)
	}
}

func TestGlobRefusesBadPatternAndEscape(t *testing.T) {
	ws := seed(t)
	tl := load(t)
	if res := call(t, tl, ws, `{"pattern":"[bad"}`); !res.IsError {
		t.Fatal("bad pattern accepted")
	}
	if res := call(t, tl, ws, `{"pattern":"*","path":"../"}`); !res.IsError || !strings.Contains(res.Content[0].Text, "escapes") {
		t.Fatalf("escape: %q", res.Content[0].Text)
	}
}
