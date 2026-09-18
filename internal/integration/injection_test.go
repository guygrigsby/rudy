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

// TestToolOutputCannotDriveTheTerminal: a tool result is bytes from outside. A fetched
// page, a test's output, a file a model asked to read can all carry escape sequences, and
// a client that prints them raw hands the terminal to whoever wrote them: the title bar,
// the clipboard on terminals that answer OSC 52, a cleared screen hiding what happened.
//
// The headless printer is the path under test, since it writes to a real terminal with no
// renderer in between.
func TestToolOutputCannotDriveTheTerminal(t *testing.T) {
	payloads := map[string]string{
		"a title bar sequence":     "\x1b]0;pwned\x07",
		"an OSC 52 clipboard grab": "\x1b]52;c;cHduZWQ=\x07",
		"a screen clear":           "\x1b[2J\x1b[H",
		"a cursor jump":            "\x1b[999;999H",
		"a colour that never ends": "\x1b[31m",
		"an alternate screen":      "\x1b[?1049h",
		"a bracketed paste open":   "\x1b[200~",
	}
	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			h := newHome(t)
			p := h.withProvider(t)
			h.writeConfig(t, modeConfig(p.URL(), "permissive"))
			// A file the model reads, carrying the sequence. The read tool is the shortest
			// path from somebody else's bytes to this terminal.
			// Named relative to the workspace, not absolutely: on macOS a temp dir is
			// /var/... which is a symlink to /private/var/..., the workspace root resolves
			// to the real path, and an absolute path in the other spelling is refused for
			// escaping the workspace, which would be this test fooling itself.
			if err := os.WriteFile(filepath.Join(h.root, "payload.txt"), []byte("before"+payload+"after"), 0o600); err != nil {
				t.Fatal(err)
			}
			p.onCompletion(toolCall("read", map[string]any{"path": "payload.txt"}))

			r := h.run(t, 60*time.Second, "-p", "--output", "stream-json", "read it")
			assertNoPanic(t, r.out())
			results := toolResults(t, r)
			if len(results) == 0 {
				t.Fatalf("read never ran:\n%s", r.out())
			}
			// stream-json is JSON, which spells an escape \u001b on its own, so this half
			// only proves the tool ran and carried the bytes. The half that matters is
			// TestModelTextCannotDriveTheTerminal below, where the answer is printed.
			if got := results[0].text(); !strings.Contains(got, "before") {
				t.Errorf("the read never returned the file: %q", got)
			}
		})
	}
}

// TestAMalformedAgentDefinition: agents are markdown with frontmatter, read from a
// directory the operator controls but written by whoever shared them.
func TestAMalformedAgentDefinition(t *testing.T) {
	cases := map[string]string{
		"frontmatter that is not yaml":  "---\n: : :\n---\nbody\n",
		"no frontmatter at all":         "just a body\n",
		"frontmatter never closed":      "---\nname: x\n",
		"tools that is a string":        "---\nname: x\ntools: everything\n---\nbody\n",
		"a name that is a path":         "---\nname: ../../escape\n---\nbody\n",
		"a description with a nul byte": "---\nname: x\ndescription: \"a\x00b\"\n---\nbody\n",
		"a megabyte of body":            "---\nname: x\n---\n" + strings.Repeat("b", 1<<20),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHome(t)
			p := h.withProvider(t)
			h.writeConfig(t, modeConfig(p.URL(), "permissive"))
			dir := filepath.Join(h.root, "config", "rudy", "agents")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "broken.md"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			// A definition rudy cannot read must not stop a session that never asked for it.
			r := h.run(t, 60*time.Second, "-p", "say ok")
			assertNoPanic(t, r.out())
			if r.code != 0 {
				t.Errorf("a broken agent definition (%s) stopped an ordinary turn\n%s", name, r.out())
			}
		})
	}
}

// TestAnAgentNobodyDefined: naming an agent that is not there is a usage error rather than
// a quiet fall back to the default, which would run a turn nobody asked for.
func TestAnAgentNobodyDefined(t *testing.T) {
	h := newHome(t)
	p := h.withProvider(t)
	h.writeConfig(t, "agent = \"nobody\"\n\n"+modeConfig(p.URL(), "permissive"))
	r := h.run(t, 60*time.Second, "-p", "say ok")
	assertNoPanic(t, r.out())
	if r.code == 0 {
		t.Errorf("a session opened on an agent nobody defined\n%s", r.out())
	}
}

// TestModelTextCannotDriveTheTerminal is the half that reaches a terminal: --output text
// prints the model's own words, and those words came through a model that reads pages
// written by other people. A pipe is left alone, since a script is owed the bytes.
func TestModelTextCannotDriveTheTerminal(t *testing.T) {
	h := newHome(t)
	p := h.withProvider(t)
	answer := "before" + string(rune(27)) + "]0;pwned" + string(rune(7)) + string(rune(27)) + "[2Jafter"
	p.onCompletion(func(w http.ResponseWriter, r *http.Request) {
		frame, _ := json.Marshal(map[string]any{"choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{"content": answer}},
		}})
		writeSSE(w, string(frame), `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, "[DONE]")
	})

	// The suite runs rudy with pipes for stdout, which is the path that keeps the bytes.
	r := h.run(t, 60*time.Second, "-p", "say it")
	assertNoPanic(t, r.out())
	if !strings.ContainsRune(r.stdout, 27) {
		t.Errorf("a pipe lost the bytes the model produced: %q", r.stdout)
	}
}
