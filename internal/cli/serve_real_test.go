// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/xpty"

	"github.com/guygrigsby/rudy/internal/session"
)

// The serve wave's real path, across processes: a daemon in one process, clients in others,
// the live provider behind all of them. serve_test.go proves runServe against a fake
// builder in one process, which is what pins the wiring; this proves the thing an operator
// actually does works, and it is the only test where a turn survives the death of the
// client that started it because it is the only one where the two are different processes.
//
// It is off unless RUDY_REAL=1 for the same reason TestRealTUIOverPTY is: a provider call
// and a network the rest of the suite does not have. It runs in the same scratch root that
// test uses, with the operator's config.toml copied in and HOME and every XDG variable
// pointed at it, so the real store, the real config and the real socket are untouched.

// The waits this test adds to the pty ones. Starting the daemon covers a plugin load and a
// registry refresh, the same first-frame cost a client pays; resting covers the model
// finishing an answer nobody is reading; stopping covers serveShutdownBudget plus the
// process leaving.
const (
	daemonStartWait = 90 * time.Second
	daemonStopWait  = 30 * time.Second
	restWait        = 180 * time.Second
)

// countPrompt is the turn that has to outlive its client: long enough that killing the
// client the moment the status line says a turn is running lands well before the answer is
// finished, and short enough to fit the emulator's scrollback many times over.
const countPrompt = "Count from 1 to 60 in English words, one per line, nothing else."

// countLast is the answer's last word, and what on the resumed screen proves the daemon
// finished the turn the first client abandoned. A word rather than the digits: the
// transcript wraps a long reply to the terminal's width, so no row of it is predictable,
// and "sixty" is in the answer and nowhere in the prompt that asked for it.
const countLast = "sixty"

func TestRealServeAcrossProcesses(t *testing.T) {
	if os.Getenv(realEnv) != "1" {
		t.Skip("the real serve test runs a daemon and opens live sessions against the configured provider; set " + realEnv + "=1 to run it")
	}
	cfg, path := realConfig(t)
	ref := defaultModel(t, cfg, path)
	root := t.TempDir()
	scratchConfig(t, root, cfg)
	bin := buildRudy(t, root)
	env := serveEnv(t, root, sockDir(t))
	socket := socketIn(t, env)

	d := startDaemon(t, bin, root, env, socket)

	// The headless client, through the daemon it found by probing the default socket.
	out := runClient(t, bin, root, env, "-p", "Reply with exactly: ok")
	if got := strings.TrimSpace(out); got != "ok" {
		t.Fatalf("rudy -p printed %q, want ok", got)
	}

	// The TUI, attached, and a turn that outlives it.
	pty, tui := startTUI(t, bin, root, env)
	log := drain(pty)
	waitFor(t, log, "the status line to show INSERT and "+ref, firstFrameWait, func(s string) bool {
		return strings.Contains(s, "INSERT") && strings.Contains(s, ref)
	})
	typeIn(t, pty, log, countPrompt)
	press(t, pty, keyEnter)
	waitFor(t, log, "the turn cell to say the server is working", turnWait, turnRunning)

	// The session the daemon opened for this client, which is where the answer has to
	// arrive after the client is gone.
	entries := newestEntries(t, root)
	// Killed, not ctrl+d: the editor is empty but this is the point of the test, a client
	// that goes away mid-turn rather than one that asks to leave.
	if err := tui.Process.Kill(); err != nil {
		t.Fatalf("kill the attached client: %v", err)
	}
	if err := tui.Wait(); err == nil {
		t.Fatalf("the killed client exited 0; the terminal read:\n%s", tail(log.text()))
	}
	// Entries are buffered and synced when a turn rests, so an assistant_message on disk
	// means the turn is over. Its absence here is what makes the wait below an assertion
	// about a turn that outlived its client rather than one that beat it.
	if body, ok := logHas(entries, session.KindAssistantMessage); ok {
		t.Fatalf("the turn had already rested when its client died, so nothing outlived it; lengthen %q. The log holds:\n%s", countPrompt, tail(body))
	}
	waitForEntry(t, entries, session.KindAssistantMessage, restWait)

	// A second client, on the session the first one abandoned, hearing the whole answer.
	id := sessionID(entries)
	rpty, resumed := startTUI(t, bin, root, env, "--resume", id)
	rlog := drain(rpty)
	waitFor(t, rlog, "the resumed transcript to carry the answer through "+countLast, firstFrameWait, func(s string) bool {
		return strings.Contains(strings.ToLower(s), countLast)
	})
	// /exit rather than ctrl+d: the client's own command closes the client and nothing
	// else, so the daemon and the session it holds are still there to be stopped below
	// (ADR 0015 decision 3).
	typeIn(t, rpty, rlog, slashCommand)
	press(t, rpty, keyEnter)
	waitExit(t, resumed, rlog)
	if !daemonAlive(d) {
		t.Fatal("/exit closes the client, never the daemon it was attached to")
	}

	// The daemon stops clean on a signal and takes its socket with it.
	stopDaemon(t, d)
	if _, err := os.Stat(socket); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("socket %s is still there after the daemon exited: %v", socket, err)
	}
}

// serveEnv is scratchEnv with the runtime directory moved to a short path. The socket lives
// under it, a unix socket path is 104 bytes on darwin, and t.TempDir() under /var/folders
// spends most of that before rudy has named a file. The count is checked so a rename in
// scratchEnv fails here rather than leaving this test on the long path.
func serveEnv(t *testing.T, root, runtime string) []string {
	t.Helper()
	const key = "XDG_RUNTIME_DIR="
	env := scratchEnv(root)
	found := 0
	for i, kv := range env {
		if strings.HasPrefix(kv, key) {
			env[i] = key + runtime
			found++
		}
	}
	if found != 1 {
		t.Fatalf("scratchEnv names %s %d times, want once", key, found)
	}
	return env
}

// socketIn is the socket the daemon and its clients will agree on, resolved from the child's
// own environment through config.XDG rather than assembled here: the path this test watches
// has to be the path the code computes, or the test proves nothing about the default.
func socketIn(t *testing.T, env []string) string {
	t.Helper()
	lookup := func(key string) string {
		for _, kv := range env {
			if name, value, ok := strings.Cut(kv, "="); ok && name == key {
				return value
			}
		}
		return ""
	}
	socket, err := serveSocket("", BuildOptions{Env: lookup, Home: lookup("HOME")})
	if err != nil {
		t.Fatalf("resolve the default socket: %v", err)
	}
	return socket
}

// daemon is a running rudy serve: the process, the stderr it is writing and the exit it
// will report once it is signalled.
type daemon struct {
	cmd    *exec.Cmd
	stderr *watcher
	done   chan error
}

// startDaemon runs rudy serve in its own process and blocks until it says it is serving on
// socket. That line is printed once there is a server behind the socket, so a client that
// dials after it returns is dialing something that will answer.
func startDaemon(t *testing.T, bin, dir string, env []string, socket string) *daemon {
	t.Helper()
	w := newWatcher("serving on " + socket)
	cmd := exec.Command(bin, "serve")
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = io.Discard
	cmd.Stderr = w
	if err := cmd.Start(); err != nil {
		t.Fatalf("start rudy serve: %v", err)
	}
	d := &daemon{cmd: cmd, stderr: w, done: make(chan error, 1)}
	go func() { d.done <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	select {
	case <-w.seen:
	case err := <-d.done:
		t.Fatalf("rudy serve exited %v before it served; stderr:\n%s", err, w.String())
	case <-time.After(daemonStartWait):
		t.Fatalf("rudy serve printed no serving line within %s; stderr:\n%s", daemonStartWait, w.String())
	}
	return d
}

// stopDaemon sends the signal an operator's Ctrl-C sends and takes the exit code.
// daemonAlive reports whether the daemon is still running: its Wait has not answered yet.
// A client that exited is the only thing that could have taken it down, which is what the
// caller is asserting it did not.
func daemonAlive(d *daemon) bool {
	select {
	case err := <-d.done:
		// Put it back, so stopDaemon reads the same answer and reports it.
		d.done <- err
		return false
	default:
		return true
	}
}

func stopDaemon(t *testing.T, d *daemon) {
	t.Helper()
	if err := d.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupt rudy serve: %v", err)
	}
	select {
	case err := <-d.done:
		if err != nil {
			t.Fatalf("rudy serve exited %v on SIGINT, want 0; stderr:\n%s", err, d.stderr.String())
		}
	case <-time.After(daemonStopWait):
		t.Fatalf("rudy serve did not exit within %s of SIGINT; stderr:\n%s", daemonStopWait, d.stderr.String())
	}
}

// runClient runs a client to completion in the daemon's environment and answers its stdout.
// Nothing is piped in, so --print takes its prompt from the arguments the way a terminal
// invocation does.
func runClient(t *testing.T, bin, dir string, env []string, args ...string) string {
	t.Helper()
	ctx, done := context.WithTimeout(context.Background(), turnWait)
	defer done()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	var out, errs bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errs
	if err := cmd.Run(); err != nil {
		t.Fatalf("rudy %s exited %v; stdout %q; stderr:\n%s", strings.Join(args, " "), err, out.String(), errs.String())
	}
	return out.String()
}

// startTUI opens a terminal on a client. The pty and the process are both torn down by the
// cleanups, so a test that fails mid-turn leaves neither behind.
func startTUI(t *testing.T, bin, dir string, env []string, args ...string) (xpty.Pty, *exec.Cmd) {
	t.Helper()
	pty, err := xpty.NewPty(ptyWidth, ptyHeight)
	if err != nil {
		t.Fatalf("open pty: %v", err)
	}
	t.Cleanup(func() { _ = pty.Close() })
	cmd := exec.Command(bin, args...)
	// The scratch root is the workspace too, so the run reads no AGENTS.md and no skills
	// from the repository it was built in.
	cmd.Dir = dir
	cmd.Env = env
	if err := pty.Start(cmd); err != nil {
		t.Fatalf("start rudy %s under a pty: %v", strings.Join(args, " "), err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	return pty, cmd
}

// turnRunning matches a status line whose turn cell says the server is working. The client
// mirrors turn.state and both words the cell can carry mean a turn is in flight, which is
// what "mid-turn" has to mean for a test that is about to kill the client.
func turnRunning(s string) bool {
	return strings.Contains(s, "thinking") || strings.Contains(s, "streaming")
}

// newestEntries is the log of the newest session in the scratch store. Session ids are
// ULIDs, so the newest directory is the last in lexical order.
func newestEntries(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "data", "rudy", "sessions")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the scratch session store: %v", err)
	}
	newest := ""
	for _, e := range ents {
		if e.IsDir() && e.Name() > newest {
			newest = e.Name()
		}
	}
	if newest == "" {
		t.Fatalf("no session under %s", dir)
	}
	return filepath.Join(dir, newest, "entries.jsonl")
}

// sessionID is the id of the session a log belongs to: the store names each session's
// directory after it.
func sessionID(entries string) string { return filepath.Base(filepath.Dir(entries)) }

// logHas reports whether a session log on disk carries an entry of this kind, and answers
// what it read for the failure message. A log that is not there yet holds nothing.
func logHas(path string, kind session.Kind) (string, bool) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(body), strings.Contains(string(body), `"kind":"`+string(kind)+`"`)
}

// waitForEntry polls a session log until it carries an entry of this kind, and fails with
// what the log held when the deadline passes first.
func waitForEntry(t *testing.T, path string, kind session.Kind, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		body, ok := logHas(path, kind)
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for a %s entry in %s, never saw one; the log holds:\n%s", d, kind, path, tail(body))
		}
		time.Sleep(pollEvery)
	}
}
