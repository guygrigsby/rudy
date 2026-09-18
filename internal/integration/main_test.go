// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// Package integration drives the built binary and the running daemon with inputs nobody
// would type on purpose: malformed config, corrupt logs, hostile provider streams, raw
// bytes on the socket. The point is not coverage, it is the crash, the hang and the lie.
//
// A test here asserts one of three things about every input: rudy refuses it and says why,
// rudy handles it, or rudy is the one that ends the process. Never a panic, never a hang,
// never a zero exit on a failure.
//
// Run with: make integration
package integration_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// rudyBin builds the command once per package run and returns the path. Every test drives
// this binary rather than the packages behind it: a panic in a library is a test failure
// somewhere else, a panic in the binary is what a person sees.
func rudyBin(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "rudy-integration")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "rudy")
		cmd := exec.Command("go", "build", "-o", binPath, "../../cmd/rudy")
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

// home is a throwaway XDG environment: config, data, cache and runtime under one temp dir,
// so nothing a test does can reach the operator's own rudy.
type home struct {
	root     string
	config   string
	env      []string
	provider *fakeProvider
}

func newHome(t *testing.T) *home {
	t.Helper()
	root := t.TempDir()
	h := &home{
		root:   root,
		config: filepath.Join(root, "config", "rudy", "config.toml"),
	}
	if err := os.MkdirAll(filepath.Dir(h.config), 0o700); err != nil {
		t.Fatal(err)
	}
	// The runtime dir holds the socket, and a unix path is 104 bytes: t.TempDir spends most
	// of that on the test's name, so the socket lives under the OS temp dir instead.
	runtime, err := os.MkdirTemp("", "rudy-rt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtime) })
	h.env = append(os.Environ(),
		"XDG_CONFIG_HOME="+filepath.Join(root, "config"),
		"XDG_DATA_HOME="+filepath.Join(root, "data"),
		"XDG_CACHE_HOME="+filepath.Join(root, "cache"),
		"XDG_RUNTIME_DIR="+runtime,
		"HOME="+root,
		"NO_COLOR=1",
	)
	return h
}

// withProvider starts a fake OpenAI-compatible endpoint and writes a config that points at
// it, so a test can drive a real turn without a real model.
func (h *home) withProvider(t *testing.T) *fakeProvider {
	t.Helper()
	h.provider = newFakeProvider(t)
	h.writeConfig(t, fmt.Sprintf(`[default]
provider = "fake"
model = "m1"
thinking = "off"

[permissions]
mode = "off"

[memory]
enabled = false

[providers.fake]
wire = "openai_chat"
base_url = "%s/v1"
`, h.provider.URL()))
	return h.provider
}

func (h *home) writeConfig(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(h.config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// result is one run of the binary.
type result struct {
	code   int
	stdout string
	stderr string
	took   time.Duration
}

func (r result) out() string { return r.stdout + r.stderr }

// run executes rudy with args and fails the test if it outlives budget: a hang is a finding,
// not a reason for the suite to sit there.
func (h *home) run(t *testing.T, budget time.Duration, args ...string) result {
	t.Helper()
	r, hung := h.runMaybeHanging(t, budget, args...)
	if hung {
		t.Fatalf("rudy %s hung past %s\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), budget, r.stdout, r.stderr)
	}
	return r
}

// runMaybeHanging is run for the one test that is pinning a hang rather than reporting one:
// it says whether the budget ran out instead of failing. Everything else calls run, where a
// hang is a finding.
func (h *home) runMaybeHanging(t *testing.T, budget time.Duration, args ...string) (result, bool) {
	t.Helper()
	cmd := exec.Command(rudyBin(t), args...)
	cmd.Env = h.env
	cmd.Dir = h.root
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(budget):
		_ = cmd.Process.Kill()
		<-done
		return result{stdout: stdout.String(), stderr: stderr.String(), took: time.Since(start)}, true
	}
	r := result{stdout: stdout.String(), stderr: stderr.String(), took: time.Since(start)}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		r.code = ee.ExitCode()
	default:
		t.Fatalf("wait: %v", err)
	}
	return r, false
}

// refuses is the shape every hostile input has to produce: a non-zero exit, something said
// about why, and no panic. A Go panic reaching the terminal is the failure this suite is
// looking for.
func (h *home) refuses(t *testing.T, r result, args ...string) {
	t.Helper()
	if r.code == 0 {
		t.Errorf("rudy %s exited 0 on an input it should refuse\n%s", strings.Join(args, " "), r.out())
	}
	assertNoPanic(t, r.out())
	if strings.TrimSpace(r.out()) == "" {
		t.Errorf("rudy %s refused silently", strings.Join(args, " "))
	}
}

func assertNoPanic(t *testing.T, out string) {
	t.Helper()
	for _, tell := range []string{"panic:", "runtime error:", "goroutine 1 [running]", "SIGSEGV"} {
		if strings.Contains(out, tell) {
			t.Errorf("the binary panicked: %s", out)
			return
		}
	}
}

// fakeProvider is an OpenAI-compatible endpoint a test can make behave badly: a stream that
// stops mid-frame, JSON that is not, a tool call whose input is garbage.
type fakeProvider struct {
	srv *httptest.Server
	mu  sync.Mutex
	// completion writes the body of /v1/chat/completions. Nil sends one short answer.
	completion func(w http.ResponseWriter, r *http.Request)
	// models writes the body of /v1/models. Nil sends one model.
	models   func(w http.ResponseWriter, r *http.Request)
	requests int
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	p := &fakeProvider{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.requests++
		models, completion := p.models, p.completion
		p.mu.Unlock()
		if r.Method == http.MethodGet {
			if models != nil {
				models(w, r)
				return
			}
			writeJSON(w, map[string]any{"data": []any{map[string]any{
				"id": "m1", "object": "model", "context_window": 100000, "max_output_tokens": 4096,
			}}})
			return
		}
		if completion != nil {
			completion(w, r)
			return
		}
		writeSSE(w, `{"choices":[{"index":0,"delta":{"content":"ok"}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, "[DONE]")
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeProvider) URL() string { return p.srv.URL }

func (p *fakeProvider) onCompletion(f func(w http.ResponseWriter, r *http.Request)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.completion = f
}

func (p *fakeProvider) onModels(f func(w http.ResponseWriter, r *http.Request)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.models = f
}

func writeJSON(w http.ResponseWriter, v any) {
	body, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	_, _ = w.Write(body)
}

// writeSSE writes each frame as one data: event, the way a streaming endpoint does.
func writeSSE(w http.ResponseWriter, frames ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, f := range frames {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", f)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
}

// dialSocket connects to a daemon's socket, for the tests that speak raw bytes at it rather
// than going through the client.
func dialSocket(t *testing.T, path string, budget time.Duration) net.Conn {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		c, err := net.Dial("unix", path)
		if err == nil {
			return c
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no socket at %s within %s", path, budget)
	return nil
}

// toolResults are the tool_result entries a --output stream-json run emitted, in order.
// Asserting on stdout alone proves nothing about a tool: text mode prints the final answer
// and nothing else, so a test that greps it for a leak passes whether or not the tool
// leaked. The stream carries every entry, which is where the outcome actually is.
func toolResults(t *testing.T, r result) []toolResult {
	t.Helper()
	var out []toolResult
	for _, line := range strings.Split(r.stdout, "\n") {
		if !strings.Contains(line, `"kind":"tool_result"`) {
			continue
		}
		var n struct {
			Params struct {
				Entry toolResult `json:"entry"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &n); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		out = append(out, n.Params.Entry)
	}
	return out
}

// toolResult is the part of a tool_result entry a boundary test reads.
type toolResult struct {
	Outcome string `json:"outcome"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}

func (r toolResult) text() string {
	var b strings.Builder
	for _, c := range r.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}
