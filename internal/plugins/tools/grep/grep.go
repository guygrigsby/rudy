// Package grep is the built-in grep tool: regexp search over workspace files.
package grep

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/fsroot"
	"github.com/guygrigsby/rudy/internal/tool"
)

const schema = `{"type":"object","properties":{"pattern":{"type":"string","description":"Go regular expression"},"path":{"type":"string","description":"Directory to search, relative to the workspace root; default the root"},"glob":{"type":"string","description":"Only files whose relative path matches this glob, e.g. **/*.go"},"max":{"type":"integer","minimum":1,"description":"Maximum matches, default 200"}},"required":["pattern"],"additionalProperties":false}`

const (
	defaultMax  = 200
	maxFileSize = 1 << 20
)

type args struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
	Glob    string `json:"glob"`
	Max     int    `json:"max"`
}

type grepPlugin struct{}

func New() plugin.Plugin { return grepPlugin{} }

func (grepPlugin) Name() string { return "tools.grep" }

func (grepPlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterTool(tool.Tool{
		Name:        "grep",
		Description: "Search file contents with a Go regular expression. Output is path:line:text. Skips .git, binaries and files over 1MB.",
		Schema:      json.RawMessage(schema),
		Safety:      tool.Safe,
		Invoke:      invoke,
	})
}

func invoke(ctx context.Context, call tool.Call) (tool.Result, error) {
	var a args
	if err := json.Unmarshal(call.Input, &a); err != nil {
		return fsroot.Fail("grep: bad input: %v", err), nil
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return fsroot.Fail("grep: %v", err), nil
	}
	if a.Max < 1 {
		a.Max = defaultMax
	}
	if a.Glob != "" && !doublestar.ValidatePattern(a.Glob) {
		return fsroot.Fail("grep: bad glob %q", a.Glob), nil
	}
	start := "."
	if a.Path != "" {
		rel, err := fsroot.Rel(call.Workspace.Root, a.Path)
		if err != nil {
			return fsroot.Fail("grep: %v", err), nil
		}
		start = fsToSlash(rel)
	}
	root, err := os.OpenRoot(call.Workspace.Root)
	if err != nil {
		return tool.Result{}, err
	}
	defer root.Close()
	fsys := root.FS()

	var b strings.Builder
	matches := 0
	truncated := false
	walkErr := fs.WalkDir(fsys, start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if a.Glob != "" {
			ok, _ := doublestar.Match(a.Glob, p)
			if !ok {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxFileSize {
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil || fsroot.IsBinary(data) {
			return nil
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 0, 64*1024), maxFileSize+1)
		line := 0
		for sc.Scan() {
			line++
			if !re.Match(sc.Bytes()) {
				continue
			}
			if matches >= a.Max {
				truncated = true
				return fs.SkipAll
			}
			matches++
			fmt.Fprintf(&b, "%s:%d:%s\n", p, line, sc.Text())
		}
		return nil
	})
	if walkErr != nil && walkErr != fs.SkipAll {
		return fsroot.Fail("grep: %v", walkErr), nil
	}
	if matches == 0 {
		return fsroot.Text("no matches\n"), nil
	}
	if truncated {
		fmt.Fprintf(&b, "… truncated at %d matches\n", a.Max)
	}
	return fsroot.Text(b.String()), nil
}

func fsToSlash(rel string) string {
	s := strings.ReplaceAll(rel, string(os.PathSeparator), "/")
	return path.Clean(s)
}
