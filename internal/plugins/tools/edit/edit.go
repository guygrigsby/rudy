// Package edit is the built-in edit tool: replace an exact substring in one file.
package edit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/fsroot"
	"github.com/guygrigsby/rudy/internal/tool"
)

const schema = `{"type":"object","properties":{"path":{"type":"string","description":"File path, relative to the workspace root or absolute inside it"},"old":{"type":"string","description":"Exact text to replace; must occur exactly once unless replace_all"},"new":{"type":"string","description":"Replacement text"},"replace_all":{"type":"boolean","description":"Replace every occurrence"}},"required":["path","old","new"],"additionalProperties":false}`

type args struct {
	Path       string `json:"path"`
	Old        string `json:"old"`
	New        string `json:"new"`
	ReplaceAll bool   `json:"replace_all"`
}

type editPlugin struct{}

func New() plugin.Plugin { return editPlugin{} }

func (editPlugin) Name() string { return "tools.edit" }

func (editPlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterTool(tool.Tool{
		Name:        "edit",
		Description: "Replace exact text in a file. old must match exactly once; pass replace_all to change every occurrence. Read the file first so old matches byte for byte.",
		Schema:      json.RawMessage(schema),
		Safety:      tool.Unsafe,
		Invoke:      invoke,
	})
}

func invoke(ctx context.Context, call tool.Call) (tool.Result, error) {
	var a args
	if err := json.Unmarshal(call.Input, &a); err != nil {
		return fsroot.Fail("edit: bad input: %v", err), nil
	}
	if a.Old == "" {
		return fsroot.Fail("edit: old must not be empty"), nil
	}
	rel, err := fsroot.Rel(call.Workspace.Root, a.Path)
	if err != nil {
		return fsroot.Fail("edit: %v", err), nil
	}
	root, err := os.OpenRoot(call.Workspace.Root)
	if err != nil {
		return tool.Result{}, err
	}
	defer func() { _ = root.Close() }()
	b, err := root.ReadFile(rel)
	if err != nil {
		return fsroot.Fail("edit: %v", err), nil
	}
	if fsroot.IsBinary(b) {
		return fsroot.Fail("edit: %s is a binary file", a.Path), nil
	}
	content := string(b)
	count := strings.Count(content, a.Old)
	switch {
	case count == 0:
		return fsroot.Fail("edit: old text not found in %s", a.Path), nil
	case count > 1 && !a.ReplaceAll:
		return fsroot.Fail("edit: old text occurs %d times in %s; make it unique or pass replace_all", count, a.Path), nil
	}
	if a.ReplaceAll {
		content = strings.ReplaceAll(content, a.Old, a.New)
	} else {
		content = strings.Replace(content, a.Old, a.New, 1)
	}
	if err := fsroot.WriteAtomic(root, rel, []byte(content)); err != nil {
		return fsroot.Fail("edit: %v", err), nil
	}
	noun := "occurrence"
	if count != 1 {
		noun = "occurrences"
	}
	return fsroot.Text(fmt.Sprintf("replaced %d %s in %s", count, noun, a.Path)), nil
}
