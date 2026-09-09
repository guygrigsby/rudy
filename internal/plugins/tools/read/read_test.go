package read_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugin/plugintest"
	"github.com/guygrigsby/rudy/internal/plugins/tools/read"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

func load(t *testing.T, p plugin.Plugin) tool.Tool {
	t.Helper()
	h := &plugintest.Host{}
	if err := p.Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if len(h.Tools) != 1 {
		t.Fatalf("registered %d tools", len(h.Tools))
	}
	return h.Tools[0]
}

func call(t *testing.T, tl tool.Tool, root string, input string) tool.Result {
	t.Helper()
	res, err := tl.Invoke(context.Background(), tool.Call{
		ID: "tu1", Name: tl.Name, Input: json.RawMessage(input),
		Workspace: session.Workspace{Root: root, ProjectID: "local/x"},
	})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	return res
}

func text(r tool.Result) string { return r.Content[0].Text }

func TestReadNumbersLines(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("one\ntwo\nthree\nfour\nfive\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := load(t, read.New())
	if tl.Name != "read" || tl.Safety != tool.Safe {
		t.Fatalf("tool = %+v", tl)
	}
	res := call(t, tl, ws, `{"path":"a.txt"}`)
	if res.IsError || !strings.HasPrefix(text(res), "1\tone\n2\ttwo\n") {
		t.Fatalf("got %q err=%v", text(res), res.IsError)
	}
	res = call(t, tl, ws, `{"path":"a.txt","offset":4,"limit":1}`)
	if text(res) != "4\tfour\n… 1 more lines\n" {
		t.Fatalf("got %q", text(res))
	}
	res = call(t, tl, ws, `{"path":"a.txt","offset":9}`)
	if !res.IsError || !strings.Contains(text(res), "past the end") {
		t.Fatalf("got %q err=%v", text(res), res.IsError)
	}
}

func TestReadRefusesEscapeBinaryAndMissing(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "bin"), []byte("ab\x00cd"), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := load(t, read.New())
	res := call(t, tl, ws, `{"path":"../etc/passwd"}`)
	if !res.IsError || !strings.Contains(text(res), "escapes") {
		t.Fatalf("escape: %q err=%v", text(res), res.IsError)
	}
	res = call(t, tl, ws, `{"path":"bin"}`)
	if !res.IsError || !strings.Contains(text(res), "binary") {
		t.Fatalf("binary: %q", text(res))
	}
	res = call(t, tl, ws, `{"path":"nope.txt"}`)
	if !res.IsError {
		t.Fatal("missing file did not error")
	}
}
