package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	mcpplugin "github.com/guygrigsby/rudy/internal/plugins/mcp"
)

// runMCP executes one rudy invocation and returns everything it printed. Each call gets a
// fresh root: a cobra command remembers the flags it parsed, and these tests run several
// invocations against the same temp tree.
func runMCP(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := NewRoot()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func userMCPPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "rudy", "mcp.toml")
}

func TestMCPAddWritesTheUserFile(t *testing.T) {
	tempXDG(t)
	if out, err := runMCP(t, "mcp", "add", "echo", "/path/echo", "--arg", "one"); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	// Every rudy flag precedes the name: everything after the command belongs to the server.
	if out, err := runMCP(t, "mcp", "add", "--transport", "http", "--header", "Authorization: env:TOKEN", "remote", "https://x/mcp"); err != nil {
		t.Fatalf("add http: %v\n%s", err, out)
	}
	f, err := mcpplugin.ReadFile(userMCPPath(t))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]mcpplugin.ServerConfig{
		"echo": {
			Name:      "echo",
			Transport: mcpplugin.TransportStdio,
			Command:   "/path/echo",
			Args:      []string{"--arg", "one"},
		},
		"remote": {
			Name:      "remote",
			Transport: mcpplugin.TransportHTTP,
			URL:       "https://x/mcp",
			Headers:   map[string]string{"Authorization": "env:TOKEN"},
		},
	}
	if !reflect.DeepEqual(f.Servers, want) {
		t.Errorf("file:\n got %+v\nwant %+v", f.Servers, want)
	}
}

func TestMCPAddProjectScope(t *testing.T) {
	tempXDG(t)
	t.Chdir(t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := runMCP(t, "mcp", "add", "--scope", "project", "local", "/bin/x"); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	f, err := mcpplugin.ReadFile(filepath.Join(cwd, ".rudy", "mcp.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Servers["local"]; got.Command != "/bin/x" || got.Transport != mcpplugin.TransportStdio {
		t.Errorf("project entry %+v", got)
	}
	if _, err := os.Stat(userMCPPath(t)); !os.IsNotExist(err) {
		t.Errorf("the user file was written too: %v", err)
	}
}

func TestMCPListGetRemove(t *testing.T) {
	tempXDG(t)
	t.Chdir(t.TempDir())
	if out, err := runMCP(t, "mcp", "add", "echo", "/path/echo"); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if out, err := runMCP(t, "mcp", "add", "--scope", "project", "local", "/bin/x"); err != nil {
		t.Fatalf("add project: %v\n%s", err, out)
	}

	out, err := runMCP(t, "mcp", "list")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "NAME") ||
		!strings.Contains(lines[0], "SCOPE") || !strings.Contains(lines[0], "TRANSPORT") || !strings.Contains(lines[0], "TARGET") {
		t.Fatalf("list output:\n%s", out)
	}
	if !strings.Contains(lines[1], "echo") || !strings.Contains(lines[1], "user") || !strings.Contains(lines[1], "/path/echo") {
		t.Errorf("user row %q", lines[1])
	}
	if !strings.Contains(lines[2], "local") || !strings.Contains(lines[2], "project") {
		t.Errorf("project row %q", lines[2])
	}

	out, err = runMCP(t, "mcp", "get", "echo")
	if err != nil {
		t.Fatalf("get: %v\n%s", err, out)
	}
	if !strings.Contains(out, "[servers.echo]") || !strings.Contains(out, `transport = 'stdio'`) && !strings.Contains(out, `transport = "stdio"`) {
		t.Errorf("get output:\n%s", out)
	}
	if _, err := runMCP(t, "mcp", "get", "nope"); err == nil {
		t.Error("get of an unknown name returned no error")
	}

	if out, err := runMCP(t, "mcp", "remove", "echo"); err != nil {
		t.Fatalf("remove: %v\n%s", err, out)
	}
	f, err := mcpplugin.ReadFile(userMCPPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, still := f.Servers["echo"]; still {
		t.Errorf("echo survived remove: %+v", f.Servers)
	}
	if _, err := runMCP(t, "mcp", "remove", "nope"); err == nil || err.Error() != "no server named nope" {
		t.Errorf("remove of an unknown name: %v", err)
	}
}
