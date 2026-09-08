package edit_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/edit"
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
	if err := edit.New().Init(context.Background(), h); err != nil {
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

func write(t *testing.T, ws, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, ws, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(ws, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEditReplacesUniqueMatch(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "f.go", "a := 1\nb := 2\n")
	tl := load(t)
	if tl.Name != "edit" || tl.Safety != tool.Unsafe {
		t.Fatalf("tool = %+v", tl)
	}
	res := call(t, tl, ws, `{"path":"f.go","old":"b := 2","new":"b := 3"}`)
	if res.IsError || res.Content[0].Text != "replaced 1 occurrence in f.go" {
		t.Fatalf("got %q err=%v", res.Content[0].Text, res.IsError)
	}
	if read(t, ws, "f.go") != "a := 1\nb := 3\n" {
		t.Fatalf("file = %q", read(t, ws, "f.go"))
	}
}

func TestEditRefusesAmbiguousUnlessReplaceAll(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "f.txt", "x x x")
	tl := load(t)
	res := call(t, tl, ws, `{"path":"f.txt","old":"x","new":"y"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "3 times") {
		t.Fatalf("got %q err=%v", res.Content[0].Text, res.IsError)
	}
	res = call(t, tl, ws, `{"path":"f.txt","old":"x","new":"y","replace_all":true}`)
	if res.IsError || res.Content[0].Text != "replaced 3 occurrences in f.txt" || read(t, ws, "f.txt") != "y y y" {
		t.Fatalf("got %q file=%q", res.Content[0].Text, read(t, ws, "f.txt"))
	}
}

func TestEditNotFoundEmptyAndEscape(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "f.txt", "abc")
	tl := load(t)
	res := call(t, tl, ws, `{"path":"f.txt","old":"zzz","new":"y"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "not found") {
		t.Fatalf("not found: %q", res.Content[0].Text)
	}
	res = call(t, tl, ws, `{"path":"f.txt","old":"","new":"y"}`)
	if !res.IsError {
		t.Fatal("empty old accepted")
	}
	res = call(t, tl, ws, `{"path":"../f.txt","old":"a","new":"b"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "escapes") {
		t.Fatalf("escape: %q", res.Content[0].Text)
	}
}
