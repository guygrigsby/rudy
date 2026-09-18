// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
