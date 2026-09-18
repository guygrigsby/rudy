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

// bashCall makes the fake endpoint answer with one bash tool call carrying cmd, then stop.
// The payload of every command below is a touch, so the question a case asks is only
// whether the gate read the shape, never whether something destructive ran.
func bashCall(cmd string) func(w http.ResponseWriter, r *http.Request) {
	args, _ := json.Marshal(map[string]string{"command": cmd})
	call, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
			"tool_calls": []any{map[string]any{"index": 0, "id": "c1", "type": "function",
				"function": map[string]any{"name": "bash", "arguments": string(args)}}},
		}}},
	})
	return func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, string(call),
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`)
	}
}

// modeConfig is a working config in one permission mode.
//
// The evasion battery runs permissive on purpose. Strict would prove nothing: bash is an
// unsafe tool, so strict with no asker denies every bash call whatever it says, and a table
// of clever spellings would all come back "caught" without the dangerous set being read at
// all. Permissive runs an unsafe tool and stops only for the dangerous set, which is the
// one thing under test here, and headless has no asker so that stop is a deny.
func modeConfig(url, mode string) string {
	return "[default]\nprovider = \"fake\"\nmodel = \"m1\"\nthinking = \"off\"\n\n[permissions]\nmode = \"" + mode + "\"\n\n[memory]\nenabled = false\n\n[providers.fake]\nwire = \"openai_chat\"\nbase_url = \"" + url + "/v1\"\n"
}

// TestTheGateReadsThroughAWrapper is the evasion battery: a command that is dangerous in
// shape, dressed up, with a harmless touch chained onto it.
//
// The chain is what makes the result mean anything. Two earlier versions of this test
// measured nothing at all. Using sudo as the payload proved only that sudo needs a password
// on a CI box, and using rm as the payload proved only that rm deletes the marker it was
// supposed to leave. Here the dangerous part aims at a path that does not exist, so it is
// inert either way, and the marker appears if and only if the gate let the whole call
// through: a deny stops the touch with it.
//
// caught false is not a pass mark. It is a spelling the dangerous set does not see, written
// down so the next person knows.
func TestTheGateReadsThroughAWrapper(t *testing.T) {
	const gone = "/tmp/rudy-integration-not-a-path"
	cases := []struct {
		name   string
		cmd    string // the dangerous half; the touch is chained on
		caught bool
	}{
		{"plainly dangerous", "rm -rf " + gone, true},
		{"through sh -c", "sh -c 'rm -rf " + gone + "'", true},
		{"through bash -lc", "bash -lc 'rm -rf " + gone + "'", true},
		{"through env", "env FOO=1 rm -rf " + gone, true},
		{"through nohup", "nohup rm -rf " + gone, true},
		{"through command", "command rm -rf " + gone, true},
		{"two wrappers deep", "env sh -c 'rm -rf " + gone + "'", true},
		{"three wrappers deep", "env nohup sh -c 'rm -rf " + gone + "'", true},
		{"after a semicolon", "echo hi; rm -rf " + gone, true},
		{"after an and", "true && rm -rf " + gone, true},
		{"inside a subshell", "(rm -rf " + gone + ")", true},
		{"sudo, which is its own entry", "sudo -n true", true},
		{"git push force", "git push --force", true},
		{"flags apart", "rm -r -f " + gone, true},
		{"flags swapped", "rm -fr " + gone, false},
		{"flags spelled out", "rm --recursive --force " + gone, false},
		{"by absolute path", "/bin/rm -rf " + gone, true},
		{"escaped name", `\rm -rf ` + gone, true},
		{"split by quotes", `r"m" -rf ` + gone, true},
		{"through a variable", "R=rm; $R -rf " + gone, false},
		{"four wrappers deep, past the bound", "env env env sh -c 'rm -rf " + gone + "'", false},
		{"piped into a shell", "echo 'rm -rf " + gone + "' | sh", false},
		{"through find", `find . -maxdepth 0 -exec rm -rf {} \;`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHome(t)
			p := h.withProvider(t)
			h.writeConfig(t, modeConfig(p.URL(), "permissive"))
			marker := filepath.Join(h.root, "ran-"+strings.ReplaceAll(c.name, " ", "-"))
			// The touch runs only if the whole call was allowed, whatever the first half did.
			p.onCompletion(bashCall(c.cmd + " ; touch " + marker))

			r := h.run(t, 60*time.Second, "-p", "do the thing")
			assertNoPanic(t, r.out())

			_, err := os.Stat(marker)
			ran := err == nil
			switch {
			case c.caught && ran:
				t.Errorf("the dangerous set never saw %q, which ran with nobody to ask\n%s", c.cmd, r.out())
			case !c.caught && !ran:
				t.Errorf("%q was caught after all, so this table is out of date and the entry should say true", c.cmd)
			}
		})
	}
}

// TestTheGateNeverRunsAnUnsafeToolWithNobodyToAsk: the invariant under all of the above. In
// strict mode a headless run has no asker, and no asker means deny (ADR 0023, the README's
// trust model). A tool that runs anyway is the whole permission system failing open.
func TestTheGateNeverRunsAnUnsafeToolWithNobodyToAsk(t *testing.T) {
	h := newHome(t)
	p := h.withProvider(t)
	h.writeConfig(t, modeConfig(p.URL(), "strict"))
	marker := filepath.Join(h.root, "unsafe-ran")
	p.onCompletion(bashCall("touch " + marker))

	r := h.run(t, 60*time.Second, "-p", "do the thing")
	assertNoPanic(t, r.out())
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("bash ran in strict mode with no asker attached\n%s", r.out())
	}
	if !strings.Contains(strings.ToLower(r.out()), "deni") && !strings.Contains(strings.ToLower(r.out()), "permission") {
		t.Logf("the denial is not visible in the output, which a person debugging would want:\n%s", r.out())
	}
}
