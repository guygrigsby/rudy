// Package glob is the built-in glob tool: list workspace paths matching a pattern.
package glob

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/fsroot"
	"github.com/guygrigsby/rudy/internal/tool"
)

const schema = `{"type":"object","properties":{"pattern":{"type":"string","description":"Glob with ** support, e.g. **/*.go"},"path":{"type":"string","description":"Directory to search, relative to the workspace root; default the root"}},"required":["pattern"],"additionalProperties":false}`

const maxResults = 1000

type args struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

type globPlugin struct{}

func New() plugin.Plugin { return globPlugin{} }

func (globPlugin) Name() string { return "tools.glob" }

func (globPlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterTool(tool.Tool{
		Name:        "glob",
		Description: "List files and directories matching a glob pattern, relative to the workspace root, sorted. Skips .git. Capped at 1000 results.",
		Schema:      json.RawMessage(schema),
		Safety:      tool.Safe,
		Invoke:      invoke,
	})
}

func invoke(ctx context.Context, call tool.Call) (tool.Result, error) {
	var a args
	if err := json.Unmarshal(call.Input, &a); err != nil {
		return fsroot.Fail("glob: bad input: %v", err), nil
	}
	if !doublestar.ValidatePattern(a.Pattern) {
		return fsroot.Fail("glob: bad pattern %q", a.Pattern), nil
	}
	root, err := os.OpenRoot(call.Workspace.Root)
	if err != nil {
		return tool.Result{}, err
	}
	defer func() { _ = root.Close() }()
	fsys := root.FS()
	prefix := ""
	if a.Path != "" {
		rel, err := fsroot.Rel(call.Workspace.Root, a.Path)
		if err != nil {
			return fsroot.Fail("glob: %v", err), nil
		}
		prefix = path.Clean(strings.ReplaceAll(rel, string(os.PathSeparator), "/"))
		if prefix != "." {
			fsys, err = fs.Sub(fsys, prefix)
			if err != nil {
				return fsroot.Fail("glob: %v", err), nil
			}
		} else {
			prefix = ""
		}
	}
	found, err := doublestar.Glob(fsys, a.Pattern)
	if err != nil {
		return fsroot.Fail("glob: %v", err), nil
	}
	out := make([]string, 0, len(found))
	for _, p := range found {
		if p == ".git" || strings.HasPrefix(p, ".git/") || strings.Contains(p, "/.git/") {
			continue
		}
		if prefix != "" {
			p = prefix + "/" + p
		}
		out = append(out, p)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return fsroot.Text("no matches\n"), nil
	}
	truncated := false
	if len(out) > maxResults {
		out = out[:maxResults]
		truncated = true
	}
	text := strings.Join(out, "\n") + "\n"
	if truncated {
		text += "… truncated at 1000 results\n"
	}
	return fsroot.Text(text), nil
}
