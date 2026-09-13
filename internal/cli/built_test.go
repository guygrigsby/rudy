package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

var (
	builtOnce sync.Once
	builtDir  string
	builtPath string
	builtErr  error
)

// TestMain removes the binary builtRudy compiled. t.TempDir is per test and the binary is
// per package run, so the directory is this package's to clean and nothing else's: without
// this every `go test ./internal/cli/` leaves 41MB under the OS temp dir, for good.
func TestMain(m *testing.M) {
	code := m.Run()
	if builtDir != "" {
		_ = os.RemoveAll(builtDir)
	}
	os.Exit(code)
}

// builtRudy builds ./cmd/rudy once per package run and returns the binary. Tests that run
// the real command line (the bridge, the ssh shim) use it; os.Executable() in a test is the
// test binary and would not serve. Skips when go is not on PATH.
func builtRudy(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	builtOnce.Do(func() {
		dir, err := os.MkdirTemp("", "rudy-built-")
		if err != nil {
			builtErr = err
			return
		}
		builtDir = dir // TestMain removes it once every test that runs the binary is done
		builtPath = filepath.Join(dir, "rudy")
		// The version is pinned rather than left at "dev" because the tests assert on it: the
		// bridge test reads it back out of the hello, and task 7's install check compares it
		// against what the box reports.
		cmd := exec.Command("go", "build", "-ldflags", "-X github.com/guygrigsby/rudy/internal/cli.version=v0.0.0-1-gtest001", "-o", builtPath, "../../cmd/rudy")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			builtErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if builtErr != nil {
		t.Fatal(builtErr)
	}
	return builtPath
}

// boxHome is a fresh home for a pretend box: XDG dirs under it, rudy linked into
// ~/.local/bin, an empty config. Returns the home and the env a process on that box runs
// with. RUDY_HOME_OVERRIDE is not a thing; HOME is what XDG and os.UserHomeDir read.
//
// A box this bare has no provider, so a daemon started on it fails to build: the registry
// comes up empty and wire.go refuses to serve a process no session could open. That is the
// box a test wants when it is testing what happens without one; a box meant to serve gets
// boxWithProvider.
func boxHome(t *testing.T, bin string) (string, []string) {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".local", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bin, filepath.Join(home, ".local", "bin", "rudy")); err != nil {
		t.Fatal(err)
	}
	runtime := sockDir(t) // the short-path helper serve_test.go already has
	env := []string{
		"HOME=" + home,
		"PATH=/usr/bin:/bin",
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"XDG_RUNTIME_DIR=" + runtime,
	}
	return home, env
}

// boxWithProvider is boxHome plus the config an operator would have put on a box they meant
// to run rudy on: one provider pointed at upstream, a default model from it and permissions
// off, since nothing on the far end of a test is there to answer a prompt. The provider is
// where a box's ability to serve comes from, so a box without one is a box that is not set
// up rather than a case the daemon should tolerate.
func boxWithProvider(t *testing.T, bin, upstream string) (string, []string) {
	t.Helper()
	home, env := boxHome(t, bin)
	dir := filepath.Join(home, ".config", "rudy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[default]\nprovider = \"fake\"\nmodel = \"m\"\n\n" +
		"[permissions]\nmode = \"off\"\n\n" +
		"[providers.fake]\nwire = \"openai_chat\"\nbase_url = \"" + upstream + "/v1\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, env
}

// fakeOpenAI is an openai_chat endpoint with one model and one canned reply: enough for a
// daemon to build a registry and run a turn, and nothing more. A real provider would put a
// network and a key between the test and its assertion.
func fakeOpenAI(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"m","object":"model"}]}`)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Flushed per event: a stream delivered in one write would test the decoder against a
		// body it will never see from a real endpoint.
		flush, _ := w.(http.Flusher)
		for _, ev := range []string{
			`{"choices":[{"delta":{"content":` + quoteJSON(reply) + `}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			"[DONE]",
		} {
			_, _ = io.WriteString(w, "data: "+ev+"\n\n")
			if flush != nil {
				flush.Flush()
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// quoteJSON is the reply as a JSON string. json.Marshal on a string cannot fail.
func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
