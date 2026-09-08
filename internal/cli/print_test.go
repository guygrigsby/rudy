package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

type helloCommandPlugin struct{}

func (helloCommandPlugin) Name() string { return "hello" }

func (helloCommandPlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterCommand(plugin.Command{
		Name:        "hello",
		Description: "submit a greeting",
		Run: func(ctx context.Context, call plugin.CommandCall) (plugin.Action, error) {
			return plugin.SubmitPrompt{Text: "hi " + call.Args}, nil
		},
	})
}

// noopCommandPlugin registers a command that does nothing: no prompt submitted, no
// notice shown. This is the shape a plugin.NoAction command takes.
type noopCommandPlugin struct{}

func (noopCommandPlugin) Name() string { return "noop" }

func (noopCommandPlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterCommand(plugin.Command{
		Name:        "noop",
		Description: "do nothing",
		Run: func(ctx context.Context, call plugin.CommandCall) (plugin.Action, error) {
			return plugin.NoAction{}, nil
		},
	})
}

func TestReadPrompt(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		stdin string
		tty   bool
		want  string
	}{
		{"args only on a tty", []string{"fix", "it"}, "", true, "fix it"},
		{"stdin only", nil, "log line\n", false, "log line"},
		{"stdin then args", []string{"explain"}, "boom\n", false, "boom\n\nexplain"},
		{"empty stdin keeps args", []string{"x"}, "", false, "x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := readPrompt(c.args, strings.NewReader(c.stdin), c.tty)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestReadPromptRejectsOversizedStdin(t *testing.T) {
	big := strings.NewReader(strings.Repeat("x", maxStdin+1))
	if _, err := readPrompt(nil, big, false); err == nil {
		t.Fatal("expected an error for stdin over 10MB")
	}
}

func TestPrintText(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{script: [][]provider.Part{say("ok")}}
	var out bytes.Buffer
	code, err := runPrint(context.Background(), printOptions{Output: "text"}, "Reply with exactly: ok", testBuilder(t, fp), &out, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if out.String() != "ok\n" {
		t.Fatalf("stdout %q", out.String())
	}
	req := fp.request(0)
	if len(req.Messages) != 1 || req.Messages[0].Role != provider.RoleUser {
		t.Fatalf("first request messages: %+v", req.Messages)
	}
}

func TestPrintJSON(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{script: [][]provider.Part{say("ok")}}
	var out bytes.Buffer
	code, err := runPrint(context.Background(), printOptions{Output: "json"}, "hi", testBuilder(t, fp), &out, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	var res printResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("not JSON: %v: %s", err, out.String())
	}
	if res.SessionID == "" || res.Result != "ok" || res.StopReason != session.StopEndTurn {
		t.Fatalf("result %+v", res)
	}
	if res.Usage.Input != 10 || res.Usage.Output != 2 {
		t.Fatalf("usage %+v", res.Usage)
	}
	if res.Cost != "0.000014" {
		t.Fatalf("cost %q", res.Cost)
	}
}

func TestPrintStreamJSON(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{script: [][]provider.Part{say("ok")}}
	var out bytes.Buffer
	code, err := runPrint(context.Background(), printOptions{Output: "stream-json"}, "hi", testBuilder(t, fp), &out, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	methods := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var n struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &n); err != nil {
			t.Fatalf("line %q is not JSON: %v", line, err)
		}
		methods[n.Method]++
	}
	if methods["entry.appended"] < 2 || methods["turn.state"] == 0 || methods["stream.delta"] == 0 {
		t.Fatalf("methods seen: %v", methods)
	}
}

func TestPrintToolRoundTrip(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(ws)
	fp := &fakeProvider{script: [][]provider.Part{
		callTool("tu_1", "glob", `{"pattern":"*.txt"}`),
		say("done"),
	}}
	var out bytes.Buffer
	code, err := runPrint(context.Background(), printOptions{Output: "text"}, "list files", testBuilder(t, fp), &out, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if out.String() != "done\n" {
		t.Fatalf("stdout %q", out.String())
	}
	second := fp.request(1)
	last := second.Messages[len(second.Messages)-1]
	if last.Role != provider.RoleToolResult || last.ToolUseID != "tu_1" {
		t.Fatalf("last message %+v", last)
	}
	if !strings.Contains(textOf(last.Content), "a.txt") {
		t.Fatalf("tool result %q", textOf(last.Content))
	}
}

func TestPrintContinueReusesSession(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{script: [][]provider.Part{say("one"), say("two")}}
	build := testBuilder(t, fp)
	var out1, out2 bytes.Buffer
	if code, err := runPrint(context.Background(), printOptions{Output: "json"}, "first", build, &out1, io.Discard); err != nil || code != 0 {
		t.Fatalf("first: code %d err %v", code, err)
	}
	if code, err := runPrint(context.Background(), printOptions{Output: "json", Continue: true}, "second", build, &out2, io.Discard); err != nil || code != 0 {
		t.Fatalf("second: code %d err %v", code, err)
	}
	var r1, r2 printResult
	if err := json.Unmarshal(out1.Bytes(), &r1); err != nil {
		t.Fatalf("unmarshal first: %v", err)
	}
	if err := json.Unmarshal(out2.Bytes(), &r2); err != nil {
		t.Fatalf("unmarshal second: %v", err)
	}
	if r1.SessionID == "" || r1.SessionID != r2.SessionID {
		t.Fatalf("session ids %q %q", r1.SessionID, r2.SessionID)
	}
	if got := len(fp.request(1).Messages); got != 3 {
		t.Fatalf("second request carries %d messages, want 3", got)
	}
}

func TestPrintContinueWithNoSession(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{}
	var errb bytes.Buffer
	code, _ := runPrint(context.Background(), printOptions{Output: "text", Continue: true}, "x", testBuilder(t, fp), io.Discard, &errb)
	if code != 2 || !strings.Contains(errb.String(), "no session") {
		t.Fatalf("code %d stderr %q", code, errb.String())
	}
}

func TestPrintSlashCommand(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{script: [][]provider.Part{say("ok")}}
	build := testBuilder(t, fp, helloCommandPlugin{})
	var out bytes.Buffer
	code, err := runPrint(context.Background(), printOptions{Output: "text"}, "/hello there", build, &out, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if out.String() != "ok\n" {
		t.Fatalf("stdout %q", out.String())
	}
	if got := textOf(fp.request(0).Messages[0].Content); got != "hi there" {
		t.Fatalf("submitted prompt %q", got)
	}
}

func TestPrintUnknownSlashCommand(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{}
	var errb bytes.Buffer
	code, _ := runPrint(context.Background(), printOptions{Output: "text"}, "/nope", testBuilder(t, fp), io.Discard, &errb)
	if code != 2 || !strings.Contains(errb.String(), "unknown command /nope") {
		t.Fatalf("code %d stderr %q", code, errb.String())
	}
}

// TestPrintNoActionCommandCompletesWithoutWaiting proves the bug the coordinator flagged:
// a command that returns plugin.NoAction produces no turn id, and runPrint used to enter
// its notification loop and block until SIGINT, since no turn.state for "" would ever
// arrive. runPrint must instead recognize the no-op and return immediately.
func TestPrintNoActionCommandCompletesWithoutWaiting(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{}
	build := testBuilder(t, fp, noopCommandPlugin{})
	var out bytes.Buffer
	done := make(chan struct{})
	var code int
	var err error
	go func() {
		code, err = runPrint(context.Background(), printOptions{Output: "text"}, "/noop", build, &out, io.Discard)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runPrint did not return within a second")
	}
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if out.String() != "" {
		t.Fatalf("stdout %q, want empty for a no-op command", out.String())
	}
}

// TestPrintNoActionCommandJSON pins the --output json shape for a no-op command: an
// otherwise-empty result with stop_reason "none", a value outside session.StopReason's
// own vocabulary since no turn ran to report a real one.
func TestPrintNoActionCommandJSON(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{}
	build := testBuilder(t, fp, noopCommandPlugin{})
	var out bytes.Buffer
	code, err := runPrint(context.Background(), printOptions{Output: "json"}, "/noop", build, &out, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	var res printResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("not JSON: %v: %s", err, out.String())
	}
	if res.SessionID == "" || res.Result != "" || res.StopReason != session.StopReason("none") {
		t.Fatalf("result %+v", res)
	}
}

// TestPrintCancelReturns130 drives the interrupt path in runPrint's own select loop
// directly, using a pre-cancelled context: fp.block makes Complete hang on its own ctx
// until the interrupt (or the deferred Server.Shutdown, once runPrint returns) cancels
// it, so no turn.state can complete before ctx.Done() fires. That keeps the assertion
// deterministic rather than racing wall-clock timing against the fake provider.
func TestPrintCancelReturns130(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{block: true}
	build := testBuilder(t, fp)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	done := make(chan struct{})
	var code int
	var err error
	go func() {
		code, err = runPrint(ctx, printOptions{Output: "text"}, "hi", build, &out, io.Discard)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runPrint did not return after cancellation")
	}
	if err != nil || code != 130 {
		t.Fatalf("code %d err %v", code, err)
	}
}

func TestPrintProviderFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{fail: &provider.Error{Class: session.ErrProvider, Status: 500, Message: "boom"}}
	var errb bytes.Buffer
	code, _ := runPrint(context.Background(), printOptions{Output: "text"}, "hi", testBuilder(t, fp), io.Discard, &errb)
	if code != 1 || !strings.Contains(errb.String(), "boom") {
		t.Fatalf("code %d stderr %q", code, errb.String())
	}
}

func TestRootPrintFlag(t *testing.T) {
	t.Chdir(t.TempDir())
	old := stdinIsTerminal
	stdinIsTerminal = func() bool { return true }
	t.Cleanup(func() { stdinIsTerminal = old })
	fp := &fakeProvider{script: [][]provider.Part{say("ok")}}
	root := newRoot("test", testBuilder(t, fp))
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs([]string{"-p", "--output", "json", "hello"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute: %v stderr %s", err, errb.String())
	}
	var res printResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.Result != "ok" {
		t.Fatalf("stdout %q err %v", out.String(), err)
	}
}

func TestRootWithoutPrintExplains(t *testing.T) {
	root := newRoot("test", testBuilder(t, &fakeProvider{}))
	var errb bytes.Buffer
	root.SetErr(&errb)
	root.SetArgs([]string{"hello"})
	err := root.ExecuteContext(context.Background())
	var ee ExitError
	if !errorsAs(err, &ee) || ee.Code != 2 || !strings.Contains(errb.String(), "--print") {
		t.Fatalf("err %v stderr %q", err, errb.String())
	}
}

func errorsAs(err error, target *ExitError) bool {
	for err != nil {
		if e, ok := err.(ExitError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestPrintReleasesTheSessionLock proves runPrint leaves nothing holding the session
// directory's flock once it returns. The session is detached and closed by the Serve
// goroutine, not by runPrint itself, so runPrint has to wait for that goroutine before
// handing control back; otherwise the next `rudy --continue` races a lock the previous
// process has not let go of yet. session.Load here stands in for that next process.
func TestPrintReleasesTheSessionLock(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := &fakeProvider{script: [][]provider.Part{say("ok")}}
	build := testBuilder(t, fp)
	var out bytes.Buffer
	code, err := runPrint(context.Background(), printOptions{Output: "json"}, "hi", build, &out, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	var res printResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("not JSON: %v: %s", err, out.String())
	}
	id, err := ulid.Parse(res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := build(context.Background(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Server.Shutdown(context.Background()) }()
	s, err := session.Load(b.Store, id)
	if err != nil {
		t.Fatalf("the session is still locked after runPrint returned: %v", err)
	}
	_ = s.Close()
}
