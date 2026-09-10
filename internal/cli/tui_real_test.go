package cli

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
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
	keyCtrlP = "\x10"
)

// altScreenEnter is the sequence a client sends to take the whole terminal, which is what
// ui.render = "altscreen" means on the wire and what the default now does (ADR 0015).
const altScreenEnter = "\x1b[?1049h"

// settledMarkRow is the cat's second row, which carries the rightmost character in the
// drawing and so is whole only once the reveal has passed the last column. A row still
// being revealed carries the shimmer characters instead.
const settledMarkRow = `/ o o \ \ \`

// slashCommand is the one this run completes and then runs: /exit is the client's own, so
// it needs no provider and it is what ends the process.
const slashCommand = "/exit"

// sentinel is the word the long answer ends with, on a line of its own: the model is
// asked for it so the run can tell a turn that is still streaming from one that is over.
const sentinel = "FINISHED"

// screenTail is how much of the terminal a failed wait prints.
const screenTail = 4000

// scrollbackLines is how much of what has scrolled off the emulated terminal is kept. A
// rested turn's rows are committed into the scrollback by the inline client, so an answer
// leaves the screen as soon as enough happens after it; this run never gets near the cap.
const scrollbackLines = 2000

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

	// The startup header, and the mark settled: the reveal runs on its own timer and the
	// picture's rightmost column arrives last, so a row that is whole is the animation
	// having finished rather than a frame of it (ADR 0016, ADR 0017).
	waitFor(t, log, "the settled cat and the release notes", drawWait, func(s string) bool {
		return strings.Contains(s, settledMarkRow) && strings.Contains(s, "What's new in")
	})
	if s := tail(log.text()); strings.ContainsAny(s, "▓▒") {
		t.Errorf("the reveal left nothing in flight; the terminal read:\n%s", s)
	}

	// Full screen: the client took the alternate buffer, which is what every other
	// harness does on startup and what the default ui.render now asks for.
	if !log.wrote(altScreenEnter) {
		t.Fatalf("the client opens full screen; it never entered the alternate buffer. the terminal read:\n%s", tail(log.text()))
	}

	// ctrl+p steps the session's model to the next one in the registry, which the status
	// line carries. The header names the model the session opened on and stays on screen,
	// so the status line is read on its own. A registry that listed one id twice stepped
	// from the first copy to the second, set the model it was already on, and never moved
	// (rudy-aol). Neither this nor /model asks a provider anything, so both run before the
	// turn does.
	press(t, pty, keyCtrlP)
	waitFor(t, log, "the status line to show another model", drawWait, func(s string) bool {
		st := statusLine(s)
		return strings.Contains(st, "INSERT") && strings.Contains(st, ":") && !strings.Contains(st, ref)
	})
	// And back to the one the config names, so the turn below runs on the operator's own.
	typeIn(t, pty, log, "/model "+ref)
	press(t, pty, keyEnter)
	waitFor(t, log, "the model set back to "+ref, drawWait, func(s string) bool {
		return strings.Contains(statusLine(s), ref)
	})

	const prompt = "Reply with exactly: ok"
	typeIn(t, pty, log, prompt)
	press(t, pty, keyEnter)
	// The answer is its own row, so a line that is nothing but "ok" is the model's, not
	// the prompt that asked for it: the prompt carries the same two letters at the end of
	// a longer line.
	waitFor(t, log, "the answer row to read ok", turnWait, hasLine("ok"))

	// A command runs while the model does (ADR 0026): the answer is long enough to still
	// be streaming when /permissions is typed, and its notice landing before the sentinel
	// is the command having run rather than having waited for the turn. The sentinel goes
	// after a blank line because the transcript renders markdown, where a lone newline is
	// a soft break and a count would come back as one wrapped paragraph.
	typeIn(t, pty, log, "Count from 1 to 100. Then, after a blank line, write only: "+sentinel)
	press(t, pty, keyEnter)
	waitFor(t, log, "the count to start", turnWait, func(s string) bool {
		return strings.Contains(s, "1 2 3")
	})
	typeIn(t, pty, log, "/permissions")
	press(t, pty, keyEnter)
	waitFor(t, log, "the /permissions notice mid-turn", drawWait, func(s string) bool {
		return strings.Contains(s, "ask before an unsafe tool")
	})
	if hasLine(sentinel)(log.text()) {
		t.Errorf("the answer was already finished, so nothing was proven about a command mid-turn")
	}
	waitFor(t, log, "the count to finish", turnWait, hasLine(sentinel))

	typeIn(t, pty, log, "/compact")
	press(t, pty, keyEnter)
	// Either notice is the command having run: what separates them is how many entries
	// there were to cover, which is the server's count, not this test's business.
	waitFor(t, log, "the /compact notice", turnWait, func(s string) bool {
		return strings.Contains(s, "compacted ") || strings.Contains(s, "nothing to compact")
	})

	// The slash menu: a lone slash lists every registered command with what it does, and
	// the client's own two under them (ADR 0015 decision 4).
	typeIn(t, pty, log, "/")
	waitFor(t, log, "the slash menu", drawWait, func(s string) bool {
		return strings.Contains(s, "/help") && strings.Contains(s, "List the slash commands")
	})
	// Typing the rest filters it to one, and Enter on a name already whole runs it rather
	// than completing what is already complete. ctrl+d is the other way out and is proven
	// in this package's unit tests; a run has only one exit to spend.
	typeIn(t, pty, log, strings.TrimPrefix(slashCommand, "/"))
	waitFor(t, log, "the menu filtered to "+slashCommand, drawWait, func(s string) bool {
		return strings.Contains(s, "Close the client") && !strings.Contains(s, "List the slash commands")
	})
	press(t, pty, keyEnter)
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

// ptyLog is a terminal emulator fed the client's own output, which is what the test reads
// its assertions off. Stripping the escape sequences out of the raw stream is not the same
// thing: Bubble Tea's renderer repaints the live region as a diff, emitting a character
// here and a cursor motion there, so a prompt the user typed is almost never a contiguous
// run of bytes on the wire. On a screen those cells sit next to each other, which is what
// a person reading the terminal sees and what a test about the real path should match.
type ptyLog struct {
	mu  sync.Mutex
	emu *vt.Emulator
	// raw is every byte the client wrote, kept beside the decoded screen for the
	// assertions a screen cannot make: entering the alternate buffer is a sequence, not
	// a character anybody can read off the terminal.
	raw []byte
}

func newPtyLog() *ptyLog {
	emu := vt.NewEmulator(ptyWidth, ptyHeight)
	emu.SetScrollbackSize(scrollbackLines)
	log := &ptyLog{emu: emu}
	// The emulator answers the mode and device queries a client sends by writing to an
	// unbuffered pipe of its own, and blocks in Write until somebody reads it. Nothing
	// here wants those answers: the client under test is talking to a pty, which is what
	// it hears from, and this emulator is only reading over its shoulder. So the replies
	// are drained and dropped, and the client sees exactly what a pty with nobody
	// answering shows it, which is what it saw before this emulator existed. Without the
	// drain the first query deadlocks the reader against every screen the test reads.
	go func() { _, _ = io.Copy(io.Discard, emu) }()
	return log
}

func (l *ptyLog) write(b []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.raw = append(l.raw, b...)
	_, _ = l.emu.Write(b)
}

// text is the whole terminal as plain text: what has scrolled off it, oldest first, then
// what is on it. Both halves matter, because inline rendering commits a rested turn's rows
// above the live region and they scroll away as the session goes on.
func (l *ptyLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	sb := l.emu.Scrollback()
	for i := range sb.Len() {
		b.WriteString(sb.Line(i).String())
		b.WriteByte('\n')
	}
	b.WriteString(l.emu.String())
	return b.String()
}

// wrote reports whether the client has written this sequence, whatever the screen made
// of it.
func (l *ptyLog) wrote(seq string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(string(l.raw), seq)
}

// drain reads the pty into the emulator until the client closes it, which is what ends the
// goroutine: a terminal nobody reads fills its buffer and stalls the process writing to it.
func drain(pty xpty.Pty) *ptyLog {
	log := newPtyLog()
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
			t.Fatalf("waited %s for %s, never saw it; the terminal read:\n%s", d, want, tail(log.text()))
		}
		time.Sleep(pollEvery)
	}
}

// hasLine matches a terminal showing want as a whole line. Trimmed, because every row is
// drawn in the design's left gutter.
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

// statusLine is the bottom row the client draws, which is the one that carries the
// session's model: the startup header names the model too and stays on screen, so a test
// about what the model is now has to read the status line rather than the terminal.
func statusLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// tail is the end of the terminal, for a failure message: the screen and the last of what
// scrolled off it, which is what a person would have been looking at.
func tail(s string) string {
	if len(s) <= screenTail {
		return s
	}
	return "..." + s[len(s)-screenTail:]
}

// typeIn types text and waits for the editor to draw it on the terminal, so the Enter that
// follows lands on a draft the client has already read and redrawn.
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
			t.Fatalf("rudy exited %v after the exit command; the terminal read:\n%s", err, tail(log.text()))
		}
	case <-time.After(exitWait):
		t.Fatalf("rudy did not exit within %s of the exit command; the terminal read:\n%s", exitWait, tail(log.text()))
	}
}
