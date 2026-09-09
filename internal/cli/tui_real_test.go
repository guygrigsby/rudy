package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/xpty"
	"github.com/pelletier/go-toml/v2"
)

// The real path test drives the client the way a person does: a real terminal, the real
// binary, the operator's own config and the live provider behind it. Everything else in
// this package tests the client through a fake launcher or a scripted server, which is
// what proves the wiring; this proves the thing runs.
//
// It is off unless RUDY_REAL=1, because it costs a provider call and a network the rest of
// the suite does not have. The whole run happens in a scratch root: the operator's
// config.toml is copied into it, and HOME with every XDG variable point at it, so the
// session store, the plugin state and the config the real rudy uses are never touched.

// realEnv opts a run into the live path.
const realEnv = "RUDY_REAL"

// The pty is the size the design's screen assumes: wide enough that the status line and a
// short answer never wrap, tall enough for a turn's rows plus the input.
const (
	ptyWidth  = 100
	ptyHeight = 30
)

// The waits, all bounded. The first frame covers the plugin load and the registry refresh
// the client opens on; a turn covers a live model answering; the exit budget is the brief's
// ten seconds.
const (
	firstFrameWait = 90 * time.Second
	drawWait       = 20 * time.Second
	turnWait       = 180 * time.Second
	exitWait       = 10 * time.Second
	pollEvery      = 50 * time.Millisecond
)

// The keystrokes the test sends, as the terminal sends them.
const (
	keyEnter = "\r"
	keyCtrlD = "\x04"
)

// screenTail is how much of the terminal a failed wait prints.
const screenTail = 4000

func TestRealTUIOverPTY(t *testing.T) {
	if os.Getenv(realEnv) != "1" {
		t.Skip("the real path test opens a live session against the configured provider; set " + realEnv + "=1 to run it")
	}
	cfg, path := realConfig(t)
	ref := defaultModel(t, cfg, path)
	root := t.TempDir()
	scratchConfig(t, root, cfg)
	bin := buildRudy(t, root)

	pty, err := xpty.NewPty(ptyWidth, ptyHeight)
	if err != nil {
		t.Fatalf("open pty: %v", err)
	}
	t.Cleanup(func() { _ = pty.Close() })

	cmd := exec.Command(bin)
	// The scratch root is the workspace too, so the run reads no AGENTS.md and no skills
	// from the repository it was built in.
	cmd.Dir = root
	cmd.Env = scratchEnv(root)
	if err := pty.Start(cmd); err != nil {
		t.Fatalf("start rudy under a pty: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	log := drain(pty)

	// The status line is the client drawing: the vim mode it opens in and the model the
	// session resolved, in the "provider:model" form the status item renders.
	waitFor(t, log, "the status line to show INSERT and "+ref, firstFrameWait, func(s string) bool {
		return strings.Contains(s, "INSERT") && strings.Contains(s, ref)
	})

	const prompt = "Reply with exactly: ok"
	typeIn(t, pty, log, prompt)
	press(t, pty, keyEnter)
	// The answer is its own row, so a line that is nothing but "ok" is the model's, not
	// the prompt that asked for it: the prompt carries the same two letters at the end of
	// a longer line.
	waitFor(t, log, "the answer row to read ok", turnWait, hasLine("ok"))

	typeIn(t, pty, log, "/compact")
	press(t, pty, keyEnter)
	// Either notice is the command having run: what separates them is how many entries
	// there were to cover, which is the server's count, not this test's business.
	waitFor(t, log, "the /compact notice", turnWait, func(s string) bool {
		return strings.Contains(s, "compacted ") || strings.Contains(s, "nothing to compact")
	})

	// No Esc pair: the compact notice means the turn rested, and app.exit does not care
	// about the editor's mode, only that it is empty, which submitting left it.
	press(t, pty, keyCtrlD)
	waitExit(t, cmd, log)
}

// realConfig reads the operator's own config.toml, the file this test proves the client
// opens a live session with. A machine without one has nothing to prove against and says
// so rather than inventing a provider.
func realConfig(t *testing.T) ([]byte, string) {
	t.Helper()
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("no home directory to find a config in: %v", err)
		}
		dir = filepath.Join(home, ".config")
	}
	path := filepath.Join(dir, "rudy", "config.toml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no config to run the real path against: %v", err)
	}
	return body, path
}

// defaultModel is the "provider:model" the status line will show, read from the config's
// [default] table. A config with no default has no session to open.
func defaultModel(t *testing.T, cfg []byte, path string) string {
	t.Helper()
	var doc struct {
		Default struct {
			Provider string `toml:"provider"`
			Model    string `toml:"model"`
		} `toml:"default"`
	}
	if err := toml.Unmarshal(cfg, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if doc.Default.Provider == "" || doc.Default.Model == "" {
		t.Skipf("%s names no default provider and model", path)
	}
	return doc.Default.Provider + ":" + doc.Default.Model
}

// scratchConfig puts a copy of the config under the scratch config root. A copy, not the
// original: config.toml is read and never written, and this test is how that is checked in
// the one place the client could break it.
func scratchConfig(t *testing.T, root string, cfg []byte) {
	t.Helper()
	dir := filepath.Join(root, "config", "rudy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
}

// scratchEnv is the child's whole environment: the scratch root as home and as every XDG
// root, a PATH so the workspace tools resolve, and a terminal the client can draw on.
// Nothing else is inherited, so nothing the operator's shell carries can reach the run.
func scratchEnv(root string) []string {
	return []string{
		"HOME=" + root,
		"XDG_CONFIG_HOME=" + filepath.Join(root, "config"),
		"XDG_DATA_HOME=" + filepath.Join(root, "data"),
		"XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"XDG_RUNTIME_DIR=" + filepath.Join(root, "run"),
		"PATH=" + os.Getenv("PATH"),
		"TERM=xterm-256color",
	}
}

// buildRudy builds the binary under test into the scratch root, from the repository this
// test file lives in.
func buildRudy(t *testing.T, root string) string {
	t.Helper()
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "rudy")
	build := exec.Command("go", "build", "-o", bin, "./cmd/rudy")
	build.Dir = repo
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/rudy: %v\n%s", err, out)
	}
	return bin
}

// ptyLog is everything the client has drawn since it started. Inline rendering repaints
// the live region rather than scrolling it, so what matters is not what is on screen at
// one instant but whether the terminal has ever been shown a thing: the log keeps the
// bytes and strips the escape sequences on demand, whole, so a sequence split across two
// reads is never half stripped.
type ptyLog struct {
	mu  sync.Mutex
	raw []byte
}

func (l *ptyLog) write(b []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.raw = append(l.raw, b...)
}

func (l *ptyLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return ansi.Strip(string(l.raw))
}

// drain reads the pty until the client closes it, which is what ends the goroutine: a
// terminal nobody reads fills its buffer and stalls the process writing to it.
func drain(pty xpty.Pty) *ptyLog {
	log := &ptyLog{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := pty.Read(buf)
			if n > 0 {
				log.write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return log
}

// waitFor polls the terminal until match is satisfied, and fails with the tail of what was
// drawn when the deadline passes first. want names what was being waited for, in the words
// of the checklist rather than of the predicate.
func waitFor(t *testing.T, log *ptyLog, want string, d time.Duration, match func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if match(log.text()) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %s, never saw it; the terminal ended with:\n%s", d, want, tail(log.text()))
		}
		time.Sleep(pollEvery)
	}
}

// hasLine matches a terminal that has drawn want as a whole line. Trimmed, because a row
// is indented and a pty line ends in a carriage return.
func hasLine(want string) func(string) bool {
	return func(s string) bool {
		for line := range strings.SplitSeq(s, "\n") {
			if strings.TrimSpace(line) == want {
				return true
			}
		}
		return false
	}
}

// tail is the last of what was drawn, for a failure message: the whole log is every
// repaint since the client started and says less than its end does.
func tail(s string) string {
	if len(s) <= screenTail {
		return s
	}
	return "..." + s[len(s)-screenTail:]
}

// typeIn types text and waits for the editor to draw it, so the Enter that follows lands on
// a draft the client has already read.
func typeIn(t *testing.T, pty xpty.Pty, log *ptyLog, text string) {
	t.Helper()
	press(t, pty, text)
	waitFor(t, log, "the editor to hold "+strconv.Quote(text), drawWait, func(s string) bool {
		return strings.Contains(s, text)
	})
}

// press sends keystrokes as the terminal sends them.
func press(t *testing.T, pty xpty.Pty, keys string) {
	t.Helper()
	if _, err := pty.Write([]byte(keys)); err != nil {
		t.Fatalf("write %q to the pty: %v", keys, err)
	}
}

// waitExit is the last assertion: app.exit quit the program, the program gave the terminal
// back and the process ended 0, inside the budget.
func waitExit(t *testing.T, cmd *exec.Cmd, log *ptyLog) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("rudy exited %v after ctrl+d; the terminal ended with:\n%s", err, tail(log.text()))
		}
	case <-time.After(exitWait):
		t.Fatalf("rudy did not exit within %s of ctrl+d; the terminal ended with:\n%s", exitWait, tail(log.text()))
	}
}
