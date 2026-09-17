// SPDX-License-Identifier: AGPL-3.0-or-later

package bash_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/plugin/plugintest"
	"github.com/guygrigsby/rudy/internal/plugins/tools/bash"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

func load(t *testing.T) tool.Tool {
	t.Helper()
	h := &plugintest.Host{}
	if err := bash.New().Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	return h.RegisteredTools[0]
}

func TestBashRunsInWorkspaceAndReportsExit(t *testing.T) {
	ws := t.TempDir()
	tl := load(t)
	if tl.Name != "bash" || tl.Safety != tool.Unsafe {
		t.Fatalf("tool = %+v", tl)
	}
	res, err := tl.Invoke(context.Background(), tool.Call{Input: json.RawMessage(`{"command":"pwd; echo out; echo err 1>&2"}`), Workspace: session.Workspace{Root: ws}})
	if err != nil {
		t.Fatal(err)
	}
	out := res.Content[0].Text
	if res.IsError || !strings.Contains(out, "out\n") || !strings.Contains(out, "err\n") {
		t.Fatalf("got %q err=%v", out, res.IsError)
	}
	if !strings.HasSuffix(strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]), ws[strings.LastIndex(ws, "/"):]) {
		t.Fatalf("cwd line = %q, want suffix of %q", strings.SplitN(out, "\n", 2)[0], ws)
	}
	res, err = tl.Invoke(context.Background(), tool.Call{Input: json.RawMessage(`{"command":"echo boom; exit 3"}`), Workspace: session.Workspace{Root: ws}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content[0].Text, "exit status 3") || !strings.Contains(res.Content[0].Text, "boom") {
		t.Fatalf("got %q err=%v", res.Content[0].Text, res.IsError)
	}
}

func TestBashCancelKillsProcessGroup(t *testing.T) {
	ws := t.TempDir()
	tl := load(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res, err := tl.Invoke(ctx, tool.Call{Input: json.RawMessage(`{"command":"echo started; sleep 30; echo never"}`), Workspace: session.Workspace{Root: ws}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("took %s to return after cancel", time.Since(start))
	}
	if !strings.Contains(res.Content[0].Text, "started") || strings.Contains(res.Content[0].Text, "never") {
		t.Fatalf("partial output = %q", res.Content[0].Text)
	}
}

func TestBashTimeoutIsAnError(t *testing.T) {
	ws := t.TempDir()
	tl := load(t)
	res, err := tl.Invoke(context.Background(), tool.Call{Input: json.RawMessage(`{"command":"sleep 5","timeout_ms":300}`), Workspace: session.Workspace{Root: ws}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content[0].Text, "timed out after 300ms") {
		t.Fatalf("got %q err=%v", res.Content[0].Text, res.IsError)
	}
}

func TestBashTruncatesOutput(t *testing.T) {
	ws := t.TempDir()
	tl := load(t)
	res, err := tl.Invoke(context.Background(), tool.Call{Input: json.RawMessage(`{"command":"head -c 50000 /dev/zero | tr '\\0' 'a'"}`), Workspace: session.Workspace{Root: ws}})
	if err != nil {
		t.Fatal(err)
	}
	out := res.Content[0].Text
	if len(out) > 31000 || !strings.Contains(out, "[output truncated at 30000 bytes]") {
		t.Fatalf("len=%d truncated marker missing", len(out))
	}
}
