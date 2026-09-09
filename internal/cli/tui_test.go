package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

// launched is what the fake launcher was handed. The launcher is the seam every command
// that draws the client goes through, so what it saw is what the command resolved.
type launched struct {
	called bool
	info   protocol.SessionInfo
	look   look
	prompt string
}

// fakeLauncher swaps the client launcher for one that records and draws nothing, so a
// command's session resolution can be tested without a terminal.
func fakeLauncher(t *testing.T) *launched {
	t.Helper()
	got := &launched{}
	prev := launchTUI
	launchTUI = func(ctx context.Context, r clientRun) error {
		got.called, got.info, got.look, got.prompt = true, r.info, r.look, r.prompt
		return nil
	}
	t.Cleanup(func() { launchTUI = prev })
	return got
}

// fakeTerminal makes stdinIsTerminal answer tty for the rest of the test. Under go test
// stdin is /dev/null, which is a character device and so reads as a terminal; every test
// here says what it means rather than inheriting that.
func fakeTerminal(t *testing.T, tty bool) {
	t.Helper()
	prev := stdinIsTerminal
	stdinIsTerminal = func() bool { return tty }
	t.Cleanup(func() { stdinIsTerminal = prev })
}

// writeConfig writes body to the config.toml testBuilder pointed the XDG config root at.
func writeConfig(t *testing.T, body string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "rudy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// testStore opens the session store testBuilder pointed the XDG data root at.
func testStore(t *testing.T) *session.Store {
	t.Helper()
	st, err := session.OpenStore(filepath.Join(os.Getenv("XDG_DATA_HOME"), "rudy", "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// runRoot executes the root command with args and returns its output and error.
func runRoot(t *testing.T, build buildFunc, args ...string) (string, error) {
	t.Helper()
	root := newRoot("test", build)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	var ee ExitError
	if !errorsAs(err, &ee) {
		t.Fatalf("not an ExitError: %v", err)
	}
	return ee.Code
}

func TestTUIRefusesANonTerminal(t *testing.T) {
	fakeTerminal(t, false)
	fakeLauncher(t)
	out, err := runRoot(t, testBuilder(t, &fakeProvider{}))
	if got := exitCode(t, err); got != 2 {
		t.Fatalf("exit %d, out %q", got, out)
	}
	if !strings.Contains(out, "the TUI needs a terminal; use --print") {
		t.Fatalf("stderr %q", out)
	}
}

func TestTUIOpensANewSessionOnTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	fakeTerminal(t, true)
	got := fakeLauncher(t)
	if out, err := runRoot(t, testBuilder(t, &fakeProvider{})); err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	if !got.called {
		t.Fatal("the client was never launched")
	}
	if got.info.SessionID == "" {
		t.Fatalf("no session: %+v", got.info)
	}
	if got.look.keys == nil {
		t.Fatal("the launcher must be handed a resolved key table")
	}
}

// TestTUIPositionalPromptSeedsTheEditor pins `rudy "fix the flaky fork test"` as a draft
// handed to the client, not a prompt dropped on the floor.
func TestTUIPositionalPromptSeedsTheEditor(t *testing.T) {
	t.Chdir(t.TempDir())
	fakeTerminal(t, true)
	got := fakeLauncher(t)
	if out, err := runRoot(t, testBuilder(t, &fakeProvider{}), "fix", "the", "flaky", "fork", "test"); err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	if got.prompt != "fix the flaky fork test" {
		t.Fatalf("the launcher was handed prompt %q", got.prompt)
	}
}

// TestTUIDialDeclaresAsker drives an unsafe tool through the real dial and expects the
// question to reach this client. The hello has to say asker: true; without it the gate
// denies every unsafe call by no-asker and no question is ever sent, which nothing else
// here would notice.
func TestTUIDialDeclaresAsker(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{script: [][]provider.Part{
		callTool("t1", "bash", `{"command":"echo hi"}`),
		say("done"),
	}}
	build := testBuilderOver(t, fp, map[string]any{"permissions.mode": "strict"})
	fakeTerminal(t, true)

	var asked protocol.PermissionRequested
	prev := launchTUI
	launchTUI = func(ctx context.Context, r clientRun) error {
		var res protocol.SessionSubmitResult
		params := protocol.SessionSubmitParams{
			SessionID: r.info.SessionID,
			Content:   []session.Block{session.TextBlock("run it")},
			Source:    session.SourceTyped,
		}
		if err := r.client.Call(context.Background(), protocol.MethodSessionSubmit, params, &res); err != nil {
			return err
		}
		deadline := time.After(10 * time.Second)
		for {
			select {
			case n, ok := <-r.client.Notifications():
				if !ok {
					return errors.New("server closed the connection")
				}
				switch n.Method {
				case protocol.NotifyPermissionRequested:
					if err := json.Unmarshal(n.Params, &asked); err != nil {
						return err
					}
					// Denied rather than left standing: the turn has to come to rest
					// before this returns, or the shutdown below waits out its budget
					// with a tool still asking.
					answer := protocol.SessionAnswerParams{
						SessionID: r.info.SessionID, ToolUseID: asked.ToolUseID,
						Decision: session.Deny, Scope: session.ScopeOnce, Reason: "asker",
					}
					if err := r.client.Call(context.Background(), protocol.MethodSessionAnswer, answer, nil); err != nil {
						return err
					}
				case protocol.NotifyTurnState:
					var ts protocol.TurnStateChanged
					if err := json.Unmarshal(n.Params, &ts); err != nil {
						return err
					}
					switch ts.State {
					case "completed", "failed", "idle":
						return nil
					}
				}
			case <-deadline:
				return errors.New("no permission question arrived")
			}
		}
	}
	t.Cleanup(func() { launchTUI = prev })

	if out, err := runRoot(t, build); err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	if asked.Tool != "bash" || asked.ToolUseID == "" {
		t.Fatalf("the client must be asked about the unsafe call, got %+v", asked)
	}
}

// TestTUIInterruptCancelsTheRunAndExits130 raises SIGINT while the client is drawing. The
// signal handler runTUI installs is what keeps the default disposition from killing the
// process outright, so the unwind (the client close, then b.Close) still runs: the session
// this run opened is resumable afterwards, which it would not be with its flock still held.
// tea.ErrInterrupted is 130 and prints nothing, as --print reports the same thing.
func TestTUIInterruptCancelsTheRunAndExits130(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	build := testBuilder(t, &fakeProvider{})
	fakeTerminal(t, true)

	var opened string
	prev := launchTUI
	launchTUI = func(ctx context.Context, r clientRun) error {
		opened = r.info.SessionID
		self, err := os.FindProcess(os.Getpid())
		if err != nil {
			return err
		}
		if err := self.Signal(os.Interrupt); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return tea.ErrInterrupted
		case <-time.After(10 * time.Second):
			return errors.New("SIGINT did not cancel the run context")
		}
	}
	t.Cleanup(func() { launchTUI = prev })

	out, err := runRoot(t, build)
	if code := exitCode(t, err); code != 130 {
		t.Fatalf("exit %d, out %q", code, out)
	}
	if strings.Contains(out, "interrupted") || strings.Contains(out, "killed") {
		t.Fatalf("an interrupt is not news: %q", out)
	}
	// The session opened above is resumable, so the unwind released its flock.
	got := fakeLauncher(t)
	if out, err := runRoot(t, build, "--resume", opened); err != nil {
		t.Fatalf("resume after the interrupt: %v\n%s", err, out)
	}
	if got.info.SessionID != opened {
		t.Fatalf("resumed %q want %q", got.info.SessionID, opened)
	}
}

func TestTUIResumeAndContinueResolveTheNamedSession(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	build := testBuilder(t, &fakeProvider{})
	st := testStore(t)
	first := openTestSession(t, st, dir)
	_ = first.Close()
	second := openTestSession(t, st, dir)
	_ = second.Close()
	fakeTerminal(t, true)

	got := fakeLauncher(t)
	if out, err := runRoot(t, build, "--resume", first.ID().String()); err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	if got.info.SessionID != first.ID().String() {
		t.Fatalf("--resume opened %q want %q", got.info.SessionID, first.ID())
	}

	got = fakeLauncher(t)
	if out, err := runRoot(t, build, "--continue"); err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	if got.info.SessionID != second.ID().String() {
		t.Fatalf("--continue opened %q want the newest, %q", got.info.SessionID, second.ID())
	}
}

func TestSessionsResumeLaunchesOnThatSession(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	build := testBuilder(t, &fakeProvider{})
	s := openTestSession(t, testStore(t), dir)
	_ = s.Close()
	fakeTerminal(t, true)
	got := fakeLauncher(t)
	if out, err := runRoot(t, build, "sessions", "resume", s.ID().String()); err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	if got.info.SessionID != s.ID().String() {
		t.Fatalf("resumed %q want %q", got.info.SessionID, s.ID())
	}
}

// TestSessionsForkLaunchesOnTheFork pins both halves of --at: an empty one is the newest
// entry, which the server resolves under the parent's lock, and a named one is the entry
// the fork_point records.
func TestSessionsForkLaunchesOnTheFork(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	build := testBuilder(t, &fakeProvider{})
	st := testStore(t)
	parent := openTestSession(t, st, dir)
	opened, err := session.ReadLog(st.Dir(parent.ID()))
	if err != nil {
		t.Fatal(err)
	}
	note, err := parent.Append(session.Note{Plugin: "test", Text: "a second entry to fork before", Role: session.NoteInfo})
	if err != nil {
		t.Fatal(err)
	}
	_ = parent.Close()
	fakeTerminal(t, true)

	got := fakeLauncher(t)
	if out, err := runRoot(t, build, "sessions", "fork", parent.ID().String()); err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	if got.info.SessionID == parent.ID().String() || got.info.SessionID == "" {
		t.Fatalf("fork opened %q, the parent is %q", got.info.SessionID, parent.ID())
	}
	if at := forkedAt(t, st, got.info.SessionID); at != note.ID.String() {
		t.Fatalf("an empty --at forks at the newest entry, got %q want %q", at, note.ID)
	}

	got = fakeLauncher(t)
	first := opened[0].ID.String()
	if out, err := runRoot(t, build, "sessions", "fork", parent.ID().String(), "--at", first); err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	if at := forkedAt(t, st, got.info.SessionID); at != first {
		t.Fatalf("--at %s forked at %q", first, at)
	}
}

// forkedAt is the parent entry id the session's own fork_point records.
func forkedAt(t *testing.T, st *session.Store, id string) string {
	t.Helper()
	sid, err := ulid.Parse(id)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := session.ReadLog(st.Dir(sid))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if fp, ok := e.Payload.(session.ForkPoint); ok {
			return fp.ParentEntryID.String()
		}
	}
	t.Fatalf("session %s has no fork_point: %+v", id, entries)
	return ""
}

func TestSessionsForkRejectsAnIDThatIsNotOne(t *testing.T) {
	t.Chdir(t.TempDir())
	fakeTerminal(t, true)
	fakeLauncher(t)
	out, err := runRoot(t, testBuilder(t, &fakeProvider{}), "sessions", "fork", "nope")
	if got := exitCode(t, err); got != 2 {
		t.Fatalf("exit %d, out %q", got, out)
	}
	if !strings.Contains(out, `"nope" is not a session id`) {
		t.Fatalf("stderr %q", out)
	}
}

// TestSessionsResumeNamesTheVerbNotTheFlag: the id came from a positional argument, so the
// message says what the operator typed rather than a --resume they did not.
func TestSessionsResumeNamesTheVerbNotTheFlag(t *testing.T) {
	t.Chdir(t.TempDir())
	fakeTerminal(t, true)
	fakeLauncher(t)
	out, err := runRoot(t, testBuilder(t, &fakeProvider{}), "sessions", "resume", "nope")
	if got := exitCode(t, err); got != 2 {
		t.Fatalf("exit %d, out %q", got, out)
	}
	if !strings.Contains(out, `sessions resume "nope" is not a session id`) || strings.Contains(out, "--resume") {
		t.Fatalf("stderr %q", out)
	}
}

func TestTUIThemeErrorIsFatalBeforeAnySessionOpens(t *testing.T) {
	t.Chdir(t.TempDir())
	build := testBuilderOver(t, &fakeProvider{}, map[string]any{"ui.theme.name": "moonlight"})
	fakeTerminal(t, true)
	got := fakeLauncher(t)
	out, err := runRoot(t, build)
	if code := exitCode(t, err); code != 2 {
		t.Fatalf("exit %d, out %q", code, out)
	}
	if !strings.Contains(out, filepath.Join("themes", "moonlight.toml")) {
		t.Fatalf("the message must name the theme file: %q", out)
	}
	if got.called {
		t.Fatal("a theme that will not load must not reach the client")
	}
	list, err := testStore(t).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("no session may be opened before the theme loads: %+v", list)
	}
}

// TestTUIThemeRoleErrorNamesTheRole is the other half: the built-in theme with a role
// override that will not resolve. The message names the role, not a themes/default.toml
// that does not exist.
func TestTUIThemeRoleErrorNamesTheRole(t *testing.T) {
	t.Chdir(t.TempDir())
	build := testBuilderOver(t, &fakeProvider{}, map[string]any{"ui.theme.accent": "not-a-color"})
	fakeTerminal(t, true)
	got := fakeLauncher(t)
	out, err := runRoot(t, build)
	if code := exitCode(t, err); code != 2 {
		t.Fatalf("exit %d, out %q", code, out)
	}
	if !strings.Contains(out, `role "accent"`) || strings.Contains(out, "default.toml") {
		t.Fatalf("the message must name the role and no file: %q", out)
	}
	if got.called {
		t.Fatal("a theme role that will not resolve must not reach the client")
	}
}

func TestTUIKeysErrorNamesTheAction(t *testing.T) {
	t.Chdir(t.TempDir())
	build := testBuilder(t, &fakeProvider{})
	// [keys] is read from the raw config.toml, never from an override: viper lowercases map
	// keys, which would corrupt pi's case-sensitive action ids (config.Load).
	writeConfig(t, "[keys]\n\"tui.editor.levitate\" = [\"ctrl+g\"]\n")
	fakeTerminal(t, true)
	got := fakeLauncher(t)
	out, err := runRoot(t, build)
	if code := exitCode(t, err); code != 2 {
		t.Fatalf("exit %d, out %q", code, out)
	}
	if !strings.Contains(out, "tui.editor.levitate") {
		t.Fatalf("the message must name the action id: %q", out)
	}
	if got.called {
		t.Fatal("a [keys] table that will not resolve must not reach the client")
	}
}
