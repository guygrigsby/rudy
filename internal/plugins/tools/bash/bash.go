// Package bash is the built-in bash tool: run one shell command in the workspace.
package bash

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/fsroot"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

const schema = `{"type":"object","properties":{"command":{"type":"string","description":"The command to run with bash -c, in the workspace root"},"timeout_ms":{"type":"integer","minimum":1,"maximum":600000,"description":"Wall clock limit, default 120000"}},"required":["command"],"additionalProperties":false}`

const (
	defaultTimeout = 120 * time.Second
	maxTimeout     = 600 * time.Second
	maxOutput      = 30000
	termGrace      = 2 * time.Second
)

type args struct {
	Command   string `json:"command"`
	TimeoutMS int    `json:"timeout_ms"`
}

type bashPlugin struct{}

func New() plugin.Plugin { return bashPlugin{} }

func (bashPlugin) Name() string { return "tools.bash" }

func (bashPlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterTool(tool.Tool{
		Name:        "bash",
		Description: "Run a shell command in the workspace root and return its combined output. Use for builds, tests, git and anything a terminal does. Output is capped at 30000 bytes.",
		Schema:      json.RawMessage(schema),
		Safety:      tool.Unsafe,
		Invoke:      invoke,
	})
}

// cappedBuffer keeps the first maxOutput bytes and remembers that more arrived.
type cappedBuffer struct {
	mu        sync.Mutex
	b         strings.Builder
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := maxOutput - c.b.Len()
	if room <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		c.b.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	c.b.Write(p)
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.truncated {
		return c.b.String() + "\n[output truncated at 30000 bytes]\n"
	}
	return c.b.String()
}

func invoke(ctx context.Context, call tool.Call) (tool.Result, error) {
	var a args
	if err := json.Unmarshal(call.Input, &a); err != nil {
		return fsroot.Fail("bash: bad input: %v", err), nil
	}
	if strings.TrimSpace(a.Command) == "" {
		return fsroot.Fail("bash: command must not be empty"), nil
	}
	timeout := defaultTimeout
	if a.TimeoutMS > 0 {
		timeout = time.Duration(a.TimeoutMS) * time.Millisecond
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out := &cappedBuffer{}
	cmd := exec.CommandContext(runCtx, "bash", "-c", a.Command)
	cmd.Dir = call.Workspace.Root
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = termGrace
	if err := cmd.Start(); err != nil {
		return fsroot.Fail("bash: %v", err), nil
	}
	err := cmd.Wait()
	// Whatever survived SIGTERM and the grace period dies with the group. This always
	// fires, even on a normal exit, so it can in principle kill an unrelated process
	// group that the kernel has already reused this pid for; accepted tradeoff since
	// Wait has already reaped this pid and the window is vanishingly small in practice.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)

	text := out.String()
	if ctx.Err() != nil {
		return tool.Result{Content: []session.Block{session.TextBlock(text + "\n[killed]\n")}, IsError: true}, ctx.Err()
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return fsroot.Fail("%s\n[timed out after %s]", text, timeout), nil
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fsroot.Fail("%sexit status %d", ensureNewline(text), ee.ExitCode()), nil
		}
		return fsroot.Fail("%sbash: %v", ensureNewline(text), err), nil
	}
	return fsroot.Text(text), nil
}

func ensureNewline(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
