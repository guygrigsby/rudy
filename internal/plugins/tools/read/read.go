// Package read is the built-in read tool: a numbered window of a text file.
package read

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/fsroot"
	"github.com/guygrigsby/rudy/internal/tool"
)

const schema = `{"type":"object","properties":{"path":{"type":"string","description":"File path, relative to the workspace root or absolute inside it"},"offset":{"type":"integer","minimum":1,"description":"First line to return, 1-based, default 1"},"limit":{"type":"integer","minimum":1,"description":"Maximum lines to return, default 2000"}},"required":["path"],"additionalProperties":false}`

const (
	defaultLimit = 2000
	maxLineBytes = 2000
)

type args struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

type readPlugin struct{}

func New() plugin.Plugin { return readPlugin{} }

func (readPlugin) Name() string { return "tools.read" }

func (readPlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterTool(tool.Tool{
		Name:        "read",
		Description: "Read a text file from the workspace. Output is one line per row as LINE<TAB>TEXT. Use offset and limit to page through large files.",
		Schema:      json.RawMessage(schema),
		Safety:      tool.Safe,
		Invoke:      invoke,
	})
}

func invoke(ctx context.Context, call tool.Call) (tool.Result, error) {
	var a args
	if err := json.Unmarshal(call.Input, &a); err != nil {
		return fsroot.Fail("read: bad input: %v", err), nil
	}
	if a.Offset < 1 {
		a.Offset = 1
	}
	if a.Limit < 1 {
		a.Limit = defaultLimit
	}
	rel, err := fsroot.Rel(call.Workspace.Root, a.Path)
	if err != nil {
		return fsroot.Fail("read: %v", err), nil
	}
	root, err := os.OpenRoot(call.Workspace.Root)
	if err != nil {
		return tool.Result{}, err
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(rel)
	if err != nil {
		return fsroot.Fail("read: %v", err), nil
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 8000)
	n, _ := io.ReadFull(f, head)
	if fsroot.IsBinary(head[:n]) {
		return fsroot.Fail("read: %s is a binary file", a.Path), nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return tool.Result{}, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var b strings.Builder
	line, shown, more := 0, 0, 0
	for sc.Scan() {
		line++
		if line < a.Offset {
			continue
		}
		if shown >= a.Limit {
			more++
			continue
		}
		text := sc.Text()
		if len(text) > maxLineBytes {
			text = text[:maxLineBytes] + " … [line truncated]"
		}
		fmt.Fprintf(&b, "%d\t%s\n", line, text)
		shown++
	}
	if err := sc.Err(); err != nil {
		return fsroot.Fail("read: %v", err), nil
	}
	if shown == 0 && line < a.Offset {
		return fsroot.Fail("read: %s has %d lines, offset %d is past the end", a.Path, line, a.Offset), nil
	}
	if more > 0 {
		fmt.Fprintf(&b, "… %d more lines\n", more)
	}
	return fsroot.Text(b.String()), nil
}
