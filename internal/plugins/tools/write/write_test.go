// SPDX-License-Identifier: AGPL-3.0-or-later

package write_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin/plugintest"
	"github.com/guygrigsby/rudy/internal/plugins/tools/write"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

func load(t *testing.T) tool.Tool {
	t.Helper()
	h := &plugintest.Host{}
	if err := write.New().Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	return h.RegisteredTools[0]
}

func call(t *testing.T, tl tool.Tool, root, input string) tool.Result {
	t.Helper()
	res, err := tl.Invoke(context.Background(), tool.Call{ID: "tu1", Name: tl.Name, Input: json.RawMessage(input), Workspace: session.Workspace{Root: root}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestWriteCreatesAndReports(t *testing.T) {
	ws := t.TempDir()
	tl := load(t)
	if tl.Name != "write" || tl.Safety != tool.Unsafe {
		t.Fatalf("tool = %+v", tl)
	}
	res := call(t, tl, ws, `{"path":"sub/dir/new.txt","content":"hello\n"}`)
	if res.IsError || res.Content[0].Text != "wrote 6 bytes to sub/dir/new.txt" {
		t.Fatalf("got %q err=%v", res.Content[0].Text, res.IsError)
	}
	b, err := os.ReadFile(filepath.Join(ws, "sub", "dir", "new.txt"))
	if err != nil || string(b) != "hello\n" {
		t.Fatalf("file = %q, %v", b, err)
	}
	res = call(t, tl, ws, `{"path":"sub/dir/new.txt","content":"replaced"}`)
	b, _ = os.ReadFile(filepath.Join(ws, "sub", "dir", "new.txt"))
	if res.IsError || string(b) != "replaced" {
		t.Fatalf("overwrite: %q err=%v", b, res.IsError)
	}
}

func TestWriteRefusesEscape(t *testing.T) {
	ws := t.TempDir()
	res := call(t, load(t), ws, `{"path":"../escape.txt","content":"x"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "escapes") {
		t.Fatalf("got %q err=%v", res.Content[0].Text, res.IsError)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(ws), "escape.txt")); err == nil {
		t.Fatal("file written outside the workspace")
	}
}
