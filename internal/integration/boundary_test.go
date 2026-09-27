// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/agentruntime"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/codex"
	"github.com/guygrigsby/rudy/internal/provider/codexapp"
	"github.com/guygrigsby/rudy/internal/session"
)

// These drift checks run in make test despite the rest of this package's
// integration tag. The contracts, adapter and shipped wire catalogue must move
// together when the Codex minimum version or runtime port changes.
func TestRuntimeDriftMethods(t *testing.T) {
	contract := runtimeContractRows(t, "### Runtime plugin requests", "| method |")
	want := map[string]string{
		"Account": "runtime.account.read", "StartLogin": "runtime.login.start",
		"CancelLogin": "runtime.login.cancel", "ListModels": "runtime.model.list",
		"StartThread": "runtime.thread.start", "ResumeThread": "runtime.thread.resume",
		"ForkThread": "runtime.thread.fork", "ReadThread": "runtime.thread.read",
		"StartTurn": "runtime.turn.start", "SteerTurn": "runtime.turn.steer",
		"InterruptTurn": "runtime.turn.interrupt",
	}
	iface := reflect.TypeOf((*agentruntime.Runtime)(nil)).Elem()
	for i := range iface.NumMethod() {
		name := iface.Method(i).Name
		if name == "Name" || name == "SetSink" {
			continue
		}
		if _, ok := want[name]; !ok {
			t.Errorf("agentruntime.Runtime.%s has no spawned method contract; update docs/specs/rudy-contracts.md and this mapping", name)
		}
	}
	var expected []string
	for name, method := range want {
		if _, ok := iface.MethodByName(name); !ok {
			t.Errorf("spawned method %s has no agentruntime.Runtime.%s operation", method, name)
		}
		expected = append(expected, method)
	}
	slices.Sort(expected)
	if !slices.Equal(contract, expected) {
		t.Errorf("runtime method contract %v, want Runtime port %v; update docs/specs/rudy-contracts.md", contract, expected)
	}
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "protocol", "methods.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var protocolMethods []string
	ast.Inspect(f, func(n ast.Node) bool {
		v, ok := n.(*ast.ValueSpec)
		if !ok || len(v.Names) != 1 || !strings.HasPrefix(v.Names[0].Name, "MethodRuntime") || len(v.Values) != 1 {
			return true
		}
		lit, ok := v.Values[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err == nil && strings.HasPrefix(value, "runtime.") && !strings.HasPrefix(value, "runtime.approval.") {
			protocolMethods = append(protocolMethods, value)
		}
		return true
	})
	slices.Sort(protocolMethods)
	if !slices.Equal(contract, protocolMethods) {
		t.Errorf("runtime contract %v, protocol constants %v; update internal/protocol/methods.go and docs/specs/rudy-contracts.md together", contract, protocolMethods)
	}
}

func TestRuntimeDriftCodexRegistration(t *testing.T) {
	client := codexapp.NewClient(codexapp.Command{Path: "/does-not-start"})
	r := plugin.NewRegistry(nil, nil)
	r.Load(context.Background(), codex.New(client))
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	if got, ok := r.Runtime("codex"); !ok || got != client {
		t.Fatal("Codex plugin must register its AgentRuntime through plugin.Host")
	}
	if len(r.Providers()) != 0 {
		t.Fatal("Codex App Server must register as a runtime, not a native provider")
	}
	command, ok := r.Command("login")
	if !ok || command.Owner != "codex" || command.Description == "" || command.Run == nil {
		t.Fatalf("/login command metadata = %+v, registered = %v; update Codex plugin registration", command, ok)
	}
	for _, mode := range []agentruntime.LoginMode{"browser", "device"} {
		action, err := command.Run(context.Background(), plugin.CommandCall{Args: string(mode)})
		if err != nil {
			t.Fatal(err)
		}
		challenge, ok := action.(plugin.AuthChallenge)
		if !ok || challenge.Runtime != "codex" || challenge.Mode != mode {
			t.Errorf("/login %s = %#v; want Codex AuthChallenge", mode, action)
		}
	}
}

func TestRuntimeDriftCodexMethodCatalogue(t *testing.T) {
	catalogue, err := os.ReadFile(filepath.Join("..", "provider", "codexapp", "testdata", "methods-0.155.1.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// Changing the upstream method set needs a reviewed minimum-version update.
	const reviewedSHA256 = "c97ac1d0202c51b1b4ca85deb0d62c276b02480492ab28f19af6b1cb18b7bc21"
	if got := fmt.Sprintf("%x", sha256.Sum256(catalogue)); got != reviewedSHA256 {
		t.Errorf("Codex 0.155.1 method catalogue changed: SHA256 %s; review upstream schema and update this hash", got)
	}
	spec, err := os.ReadFile(filepath.Join("..", "..", "docs", "specs", "codex-app-server-runtime.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(spec), "0.155.1") {
		t.Error("Codex minimum version in docs/specs/codex-app-server-runtime.md must match method catalogue 0.155.1")
	}
	methods := strings.Fields(string(catalogue))
	if !slices.IsSorted(methods) || len(methods) == 0 {
		t.Fatal("Codex method catalogue must be nonempty and sorted")
	}
	known := make(map[string]bool, len(methods))
	for _, method := range methods {
		if known[method] {
			t.Errorf("duplicate Codex catalogue method %q", method)
		}
		known[method] = true
	}
	root := filepath.Join("..")
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path == filepath.Join("..", "provider", "codexapp") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err == nil && strings.Contains(value, "/") && known[value] {
				t.Errorf("Codex App Server method %q leaked into %s; keep wire tokens in internal/provider/codexapp", value, path)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeDriftLinkKeys(t *testing.T) {
	contract := runtimeContractRows(t, "### runtime.toml", "| key |")
	if session.RuntimeLinkFile != "runtime.toml" {
		t.Fatalf("RuntimeLinkFile = %q; update runtime.toml contract", session.RuntimeLinkFile)
	}
	typ := reflect.TypeOf(session.RuntimeLink{})
	var tags []string
	for i := range typ.NumField() {
		field := typ.Field(i)
		if field.Type.Kind() != reflect.String {
			t.Errorf("runtime link %s is not a string; update runtime.toml contract", field.Name)
		}
		tags = append(tags, field.Tag.Get("toml"))
	}
	slices.Sort(tags)
	if !slices.Equal(contract, tags) {
		t.Errorf("runtime.toml contract keys %v, RuntimeLink tags %v; update docs/specs/rudy-contracts.md", contract, tags)
	}
	store := session.NewRuntimeLinkStore(t.TempDir())
	id := session.NewID()
	if err := store.Write(id, session.RuntimeLink{Runtime: "codex", ThreadID: "thr_1"}); err != nil {
		t.Fatal(err)
	}
	path := store.Path(id)
	if filepath.Base(path) != session.RuntimeLinkFile {
		t.Errorf("runtime link path = %s, want %s", path, session.RuntimeLinkFile)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var actual []string
	for _, line := range strings.Split(string(body), "\n") {
		if key, _, ok := strings.Cut(line, " = "); ok {
			actual = append(actual, key)
		}
	}
	slices.Sort(actual)
	if !slices.Equal(contract, actual) {
		t.Errorf("runtime.toml contract keys %v, persisted keys %v; update session.RuntimeLinkStore", contract, actual)
	}
}

func runtimeContractRows(t *testing.T, heading, header string) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "docs", "specs", "rudy-contracts.md"))
	if err != nil {
		t.Fatal(err)
	}
	section := strings.SplitN(string(body), heading, 2)
	if len(section) != 2 {
		t.Fatalf("missing %s in docs/specs/rudy-contracts.md", heading)
	}
	table := strings.SplitN(section[1], header, 2)
	if len(table) != 2 {
		t.Fatalf("missing %s table in %s", header, heading)
	}
	var rows []string
	for _, line := range strings.Split(table[1], "\n") {
		if !strings.HasPrefix(line, "| ") {
			if len(rows) > 0 {
				break
			}
			continue
		}
		cell := strings.TrimSpace(strings.SplitN(line, "|", 3)[1])
		if strings.HasPrefix(cell, "`") && strings.HasSuffix(cell, "`") {
			rows = append(rows, strings.Trim(cell, "`"))
		}
	}
	if len(rows) == 0 {
		t.Fatalf("empty %s table in docs/specs/rudy-contracts.md", heading)
	}
	slices.Sort(rows)
	return rows
}

// toolCall makes the fake endpoint answer with one call of tool carrying args, then stop.
func toolCall(tool string, args map[string]any) func(w http.ResponseWriter, r *http.Request) {
	raw, _ := json.Marshal(args)
	frame, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
			"tool_calls": []any{map[string]any{"index": 0, "id": "c1", "type": "function",
				"function": map[string]any{"name": tool, "arguments": string(raw)}}},
		}}},
	})
	return func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, string(frame),
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, `[DONE]`)
	}
}

// TestWebFetchStaysOffThisMachine: web_fetch puts a page into the model's context, so the
// address it is pointed at is a boundary. ADR 0039 refuses loopback, private and
// link-local, and the spellings below are the ones that get past a naive string check:
// decimal, hex, a short form, IPv6, and the cloud metadata address that is the reason
// anybody cares.
func TestWebFetchStaysOffThisMachine(t *testing.T) {
	for _, url := range []string{
		"http://127.0.0.1:80/",
		"http://localhost/",
		"http://[::1]/",
		"http://0.0.0.0/",
		"http://127.1/",
		"http://2130706433/",
		"http://0x7f000001/",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/",
		"http://192.168.1.1/",
		"http://172.16.0.1/",
		"http://[fd00::1]/",
		"http://user:pass@127.0.0.1/",
		"file:///etc/passwd",
		"gopher://127.0.0.1:70/",
	} {
		t.Run(url, func(t *testing.T) {
			h := newHome(t)
			p := h.withProvider(t)
			// No key anywhere, which is the point: web_fetch registers without one
			// (ADR 0040), and this battery is what proves the address policy still stands
			// in the configuration an operator who never signed up for a search account
			// actually has.
			h.writeConfig(t, modeConfig(p.URL(), "permissive"))
			p.onCompletion(toolCall("web_fetch", map[string]any{"url": url}))

			r := h.run(t, 60*time.Second, "-p", "--output", "stream-json", "read that page")
			assertNoPanic(t, r.out())
			results := toolResults(t, r)
			if len(results) == 0 {
				t.Fatalf("web_fetch never ran, so this case proves nothing:\n%s", r.out())
			}
			for _, res := range results {
				if res.Outcome != "error" {
					t.Errorf("web_fetch reached %s: outcome %q, %s", url, res.Outcome, res.text())
				}
				if !strings.Contains(strings.ToLower(res.text()), "refus") {
					t.Errorf("web_fetch on %s failed without saying it was refused: %s", url, res.text())
				}
			}
		})
	}
}

// TestToolsStayInTheWorkspace: read, write and edit are the tools a model points wherever
// it likes, and the workspace root is the fence. Every path here aims outside it.
func TestToolsStayInTheWorkspace(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(secret, []byte("the quiet part"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"read by absolute path", "read", map[string]any{"path": secret}},
		{"read by dots", "read", map[string]any{"path": "../../../../etc/passwd"}},
		{"read /etc/passwd", "read", map[string]any{"path": "/etc/passwd"}},
		{"read a path with a nul", "read", map[string]any{"path": "/etc/pass\x00wd"}},
		{"write outside", "write", map[string]any{"path": filepath.Join(filepath.Dir(secret), "planted.txt"), "content": "x"}},
		{"write by dots", "write", map[string]any{"path": "../../planted.txt", "content": "x"}},
		{"edit outside", "edit", map[string]any{"path": secret, "old_string": "quiet", "new_string": "loud"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHome(t)
			p := h.withProvider(t)
			h.writeConfig(t, modeConfig(p.URL(), "permissive"))
			p.onCompletion(toolCall(c.tool, c.args))

			r := h.run(t, 60*time.Second, "-p", "--output", "stream-json", "do the thing")
			assertNoPanic(t, r.out())
			results := toolResults(t, r)
			if len(results) == 0 {
				t.Fatalf("%s never ran, so this case proves nothing:\n%s", c.name, r.out())
			}
			for _, res := range results {
				if strings.Contains(res.text(), "the quiet part") {
					t.Errorf("%s read a file outside the workspace: %s", c.name, res.text())
				}
				if res.Outcome != "error" {
					t.Errorf("%s came back %q rather than an error: %s", c.name, res.Outcome, res.text())
				}
			}
			if b, err := os.ReadFile(secret); err == nil && !strings.Contains(string(b), "quiet") {
				t.Errorf("%s changed a file outside the workspace: %q", c.name, b)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(secret), "planted.txt")); err == nil {
				t.Errorf("%s wrote a file outside the workspace", c.name)
			}
		})
	}
}

// TestBashStaysInTheWorkspace: bash runs in the workspace root, and cd is not a fence, so
// this is about where it starts rather than where it can reach. A model that asks for the
// parent directory gets the parent directory; the test pins that the tool at least starts
// where it says it does.
func TestBashStartsInTheWorkspace(t *testing.T) {
	h := newHome(t)
	p := h.withProvider(t)
	h.writeConfig(t, modeConfig(p.URL(), "permissive"))
	p.onCompletion(toolCall("bash", map[string]any{"command": "pwd"}))

	r := h.run(t, 60*time.Second, "-p", "--output", "stream-json", "where are you")
	assertNoPanic(t, r.out())
	results := toolResults(t, r)
	if len(results) == 0 {
		t.Fatalf("bash never ran:\n%s", r.out())
	}
	if got := strings.TrimSpace(results[0].text()); !strings.HasSuffix(got, h.root) && !strings.Contains(got, h.root) {
		t.Errorf("bash started in %q, want the workspace %q", got, h.root)
	}
}
