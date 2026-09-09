package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// echoBin is the example server built once for the whole package; "" means go is not on
// PATH and every test that needs a live server skips.
var echoBin string

func TestMain(m *testing.M) {
	os.Exit(func() int {
		if _, err := exec.LookPath("go"); err != nil {
			return m.Run()
		}
		dir, err := os.MkdirTemp("", "rudy-mcp-echo")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer func() { _ = os.RemoveAll(dir) }()
		bin := filepath.Join(dir, "echo")
		build := exec.Command("go", "build", "-o", bin, "./examples/mcp/echo")
		build.Dir = filepath.Join("..", "..", "..")
		if out, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build echo server: %v\n%s", err, out)
			return 1
		}
		echoBin = bin
		return m.Run()
	}())
}

// harness is one loaded plugin and everything it said while loading.
type harness struct {
	t       *testing.T
	reg     *plugin.Registry
	notices func() []string
}

// requireEcho skips a test that needs the built example server.
func requireEcho(t *testing.T) {
	t.Helper()
	if echoBin == "" {
		t.Skip("go is not on PATH, cannot build the example server")
	}
}

// load is the standard two-server file: the built echo example and a command that does not
// exist, so one server is ready and one fails in the same load.
func load(t *testing.T) *harness {
	t.Helper()
	requireEcho(t)
	return loadServers(t, map[string]ServerConfig{
		"echo":   {Transport: TransportStdio, Command: echoBin, Env: map[string]string{"ECHO_TAG": "env:ECHO_TAG_SOURCE"}},
		"broken": {Transport: TransportStdio, Command: filepath.Join(t.TempDir(), "nonexistent")},
	})
}

// loadServers writes an mcp.toml holding servers and loads the plugin over it through a real
// registry, which is what a test asserts registrations and notices against.
func loadServers(t *testing.T, servers map[string]ServerConfig) *harness {
	t.Helper()
	userPath := filepath.Join(t.TempDir(), "mcp.toml")
	if err := WriteFile(userPath, File{Servers: servers}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var notices []string
	reg := plugin.NewRegistry(nil, func(s string) {
		mu.Lock()
		defer mu.Unlock()
		notices = append(notices, s)
	})
	resolve := func(ref string) (string, error) {
		if ref == "env:ECHO_TAG_SOURCE" {
			return "tagged", nil
		}
		return "", fmt.Errorf("unexpected secret reference %q", ref)
	}
	reg.Load(context.Background(), New(userPath, "", 20*time.Second, resolve, "test"))
	t.Cleanup(func() {
		if err := reg.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return &harness{t: t, reg: reg, notices: func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), notices...)
	}}
}

// hasNotice reports whether any notice contains want.
func (h *harness) hasNotice(want string) bool {
	for _, n := range h.notices() {
		if strings.Contains(n, want) {
			return true
		}
	}
	return false
}

func TestPluginRegistersOneServerAndNoticesTheOther(t *testing.T) {
	h := load(t)
	if st := h.reg.Statuses()[0]; st.Name != "mcp" || st.State != plugin.StateReady {
		t.Fatalf("plugin status %+v", st)
	}
	echo, ok := h.reg.Tool("mcp__echo__echo")
	if !ok {
		t.Fatalf("no mcp__echo__echo in %v", toolNames(h.reg))
	}
	if echo.Safety != tool.Unsafe {
		t.Errorf("safety %q, want unsafe", echo.Safety)
	}
	if echo.Description == "" {
		t.Error("description not carried from the server")
	}
	var schema struct {
		Type       string `json:"type"`
		Properties struct {
			Text struct {
				Type string `json:"type"`
			} `json:"text"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(echo.Schema, &schema); err != nil {
		t.Fatalf("schema %s: %v", echo.Schema, err)
	}
	if schema.Type != "object" || schema.Properties.Text.Type != "string" {
		t.Errorf("schema is not the server's inputSchema: %s", echo.Schema)
	}
	if _, ok := h.reg.Tool("mcp__echo__fail"); !ok {
		t.Errorf("no mcp__echo__fail in %v", toolNames(h.reg))
	}
	for _, n := range toolNames(h.reg) {
		if strings.HasPrefix(n, "mcp__broken__") {
			t.Errorf("a server that never connected registered %s", n)
		}
	}
	// A tool whose own name would make an ambiguous mcp__a__b__c is skipped, and the rest of
	// that server's tools still register.
	for _, n := range toolNames(h.reg) {
		if strings.Contains(n, "bad__name") {
			t.Errorf("a tool with an ambiguous name registered as %s", n)
		}
	}
	if !h.hasNotice("tool bad__name") {
		t.Errorf("no notice for the skipped tool: %v", h.notices())
	}
	var found []string
	for _, n := range h.notices() {
		if strings.Contains(n, "mcp: server broken:") {
			found = append(found, n)
		}
	}
	if len(found) != 1 {
		t.Errorf("notices for the broken server: %v (all: %v)", found, h.notices())
	}
	items := h.reg.StatusItems()
	if len(items) != 1 || items[0].Key != "servers" || len(items[0].Content) != 1 || items[0].Content[0].Text != "mcp 1/2" {
		t.Errorf("status items %+v", items)
	}
}

func TestPluginInvokesThroughTheServer(t *testing.T) {
	h := load(t)
	echo, ok := h.reg.Tool("mcp__echo__echo")
	if !ok {
		t.Fatal("no echo tool")
	}
	res, err := echo.Invoke(context.Background(), tool.Call{Name: echo.Name, Input: json.RawMessage(`{"text":"hi"}`)})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.IsError || !reflect.DeepEqual(res.Content, []session.Block{session.TextBlock("hi")}) {
		t.Errorf("echo result %+v", res)
	}
	// The example answers with its own ECHO_TAG for this input, which is the only way to
	// see that the resolved env entry reached the child process.
	res, err = echo.Invoke(context.Background(), tool.Call{Name: echo.Name, Input: json.RawMessage(`{"text":"$ECHO_TAG"}`)})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "tagged" {
		t.Errorf("resolved env did not reach the child: %+v", res)
	}
	fail, ok := h.reg.Tool("mcp__echo__fail")
	if !ok {
		t.Fatal("no fail tool")
	}
	res, err = fail.Invoke(context.Background(), tool.Call{Name: fail.Name, Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !res.IsError || len(res.Content) != 1 || res.Content[0].Text != "failed on purpose" {
		t.Errorf("fail result %+v", res)
	}
	// A tool that takes nothing can arrive with no input bytes at all.
	if _, err := fail.Invoke(context.Background(), tool.Call{Name: fail.Name}); err != nil {
		t.Errorf("empty input: %v", err)
	}
	// A cancelled context is a call that could not run at all, not a tool that failed.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := echo.Invoke(ctx, tool.Call{Name: echo.Name, Input: json.RawMessage(`{"text":"hi"}`)}); err == nil {
		t.Error("cancelled context returned no error")
	}
}

// TestCloseEndsTheSessions covers what the registry calls on the way out: without it a stdio
// server's child process outlives the rudy that started it.
func TestCloseEndsTheSessions(t *testing.T) {
	h := load(t)
	echo, ok := h.reg.Tool("mcp__echo__echo")
	if !ok {
		t.Fatal("no echo tool")
	}
	if err := h.reg.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := echo.Invoke(context.Background(), tool.Call{Name: echo.Name, Input: json.RawMessage(`{"text":"hi"}`)}); err == nil {
		t.Error("the session still answered after Close")
	}
}

// TestLiteralValuesReachTheChild covers the contract's third form of an env value: anything
// that is not env: or cache: is the value itself, and only the two reference forms are asked
// of the resolver.
func TestLiteralValuesReachTheChild(t *testing.T) {
	requireEcho(t)
	h := loadServers(t, map[string]ServerConfig{
		"literal": {Transport: TransportStdio, Command: echoBin, Env: map[string]string{"ECHO_TAG": "a literal value"}},
	})
	echo, ok := h.reg.Tool("mcp__literal__echo")
	if !ok {
		t.Fatalf("no echo tool in %v (%v)", toolNames(h.reg), h.notices())
	}
	res, err := echo.Invoke(context.Background(), tool.Call{Name: echo.Name, Input: json.RawMessage(`{"text":"$ECHO_TAG"}`)})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "a literal value" {
		t.Errorf("literal env did not reach the child: %+v", res)
	}
}

// TestServersConnectConcurrently: two servers that each take startupDelay to answer must not
// cost the sum of their handshakes. Serial connects would take at least twice the delay.
func TestServersConnectConcurrently(t *testing.T) {
	requireEcho(t)
	const delay = 250 * time.Millisecond
	slow := func() ServerConfig {
		return ServerConfig{Transport: TransportStdio, Command: echoBin, Env: map[string]string{"ECHO_DELAY_MS": "250"}}
	}
	start := time.Now()
	h := loadServers(t, map[string]ServerConfig{"one": slow(), "two": slow()})
	elapsed := time.Since(start)
	for _, name := range []string{"mcp__one__echo", "mcp__two__echo"} {
		if _, ok := h.reg.Tool(name); !ok {
			t.Fatalf("no %s in %v (%v)", name, toolNames(h.reg), h.notices())
		}
	}
	if elapsed > 2*delay-50*time.Millisecond {
		t.Errorf("two %s connects took %s: they ran serially", delay, elapsed)
	}
}

// TestRegistrationOrderIsStable: whatever order the connects finish in, tools register in the
// order Merged returns the servers.
func TestRegistrationOrderIsStable(t *testing.T) {
	requireEcho(t)
	h := loadServers(t, map[string]ServerConfig{
		"aaa": {Transport: TransportStdio, Command: echoBin, Env: map[string]string{"ECHO_DELAY_MS": "200"}},
		"bbb": {Transport: TransportStdio, Command: echoBin},
	})
	names := toolNames(h.reg)
	if len(names) < 2 || !strings.HasPrefix(names[0], "mcp__aaa__") {
		t.Errorf("tools registered out of Merged order: %v", names)
	}
}

func toolNames(reg *plugin.Registry) []string {
	all := reg.Tools()
	names := make([]string, len(all))
	for i, t := range all {
		names[i] = t.Name
	}
	return names
}
