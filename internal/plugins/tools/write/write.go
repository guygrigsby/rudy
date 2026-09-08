// Package write is the built-in write tool: create or replace one file.
package write

import (
	"context"
	"encoding/json"
	"os"
	"strconv"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/fsroot"
	"github.com/guygrigsby/rudy/internal/tool"
)

const schema = `{"type":"object","properties":{"path":{"type":"string","description":"File path, relative to the workspace root or absolute inside it"},"content":{"type":"string","description":"The complete new file content"}},"required":["path","content"],"additionalProperties":false}`

type args struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type writePlugin struct{}

func New() plugin.Plugin { return writePlugin{} }

func (writePlugin) Name() string { return "tools.write" }

func (writePlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterTool(tool.Tool{
		Name:        "write",
		Description: "Create or overwrite a file in the workspace with the given content. Parent directories are created. Prefer edit for changes to an existing file.",
		Schema:      json.RawMessage(schema),
		Safety:      tool.Unsafe,
		Invoke:      invoke,
	})
}

func invoke(ctx context.Context, call tool.Call) (tool.Result, error) {
	var a args
	if err := json.Unmarshal(call.Input, &a); err != nil {
		return fsroot.Fail("write: bad input: %v", err), nil
	}
	rel, err := fsroot.Rel(call.Workspace.Root, a.Path)
	if err != nil {
		return fsroot.Fail("write: %v", err), nil
	}
	root, err := os.OpenRoot(call.Workspace.Root)
	if err != nil {
		return tool.Result{}, err
	}
	defer func() { _ = root.Close() }()
	if err := fsroot.WriteAtomic(root, rel, []byte(a.Content)); err != nil {
		return fsroot.Fail("write: %v", err), nil
	}
	return fsroot.Text("wrote " + strconv.Itoa(len(a.Content)) + " bytes to " + a.Path), nil
}
