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
	start := "."
	if a.Path != "" {
		rel, err := fsroot.Rel(call.Workspace.Root, a.Path)
		if err != nil {
			return fsroot.Fail("glob: %v", err), nil
		}
		start = fsToSlash(rel)
	}
	root, err := os.OpenRoot(call.Workspace.Root)
	if err != nil {
		return tool.Result{}, err
	}
	defer func() { _ = root.Close() }()
	fsys := root.FS()

	// Walk the tree ourselves, the way grep does, so .git is pruned at every depth
	// rather than filtered out of the match list after doublestar has already
	// descended into it.
	var out []string
	walkErr := fs.WalkDir(fsys, start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		if p == start {
			return nil
		}
		matchPath := p
		if start != "." {
			matchPath = strings.TrimPrefix(p, start+"/")
		}
		ok, err := doublestar.Match(a.Pattern, matchPath)
		if err != nil || !ok {
			return nil
		}
		out = append(out, p)
		return nil
	})
	if walkErr != nil {
		return fsroot.Fail("glob: %v", walkErr), nil
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

func fsToSlash(rel string) string {
	s := strings.ReplaceAll(rel, string(os.PathSeparator), "/")
	return path.Clean(s)
}
