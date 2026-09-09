package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/server"
	"github.com/guygrigsby/rudy/internal/session"
)

const maxStdin = 10 << 20

// ExitError carries a process exit code out of a cobra RunE. main maps it to os.Exit.
type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("exit %d", e.Code) }

type printOptions struct {
	Output   string
	Model    string
	Mode     string
	Thinking string
	Resume   string
	Continue bool
}

// printResult is the --output json shape.
type printResult struct {
	SessionID  string             `json:"session_id"`
	Result     string             `json:"result"`
	Usage      session.Usage      `json:"usage"`
	Cost       string             `json:"cost"`
	StopReason session.StopReason `json:"stop_reason"`
}

// stdinIsTerminal is a variable so tests can pretend.
var stdinIsTerminal = func() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return true
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// registerPrint adds the --print flags and the root run function.
func registerPrint(root *cobra.Command, build buildFunc) {
	var headless bool
	var o printOptions
	f := root.Flags()
	f.BoolVarP(&headless, "print", "p", false, "run one prompt headless and print the result")
	f.StringVar(&o.Output, "output", "text", "text, json or stream-json")
	f.StringVar(&o.Model, "model", "", "provider:id, or a model id unique across providers")
	f.StringVar(&o.Mode, "mode", "", "strict, permissive or off")
	f.StringVar(&o.Thinking, "thinking", "", "off, low, medium or high")
	f.StringVar(&o.Resume, "resume", "", "session id to continue")
	f.BoolVar(&o.Continue, "continue", false, "continue the newest session for this directory")
	root.Args = cobra.ArbitraryArgs
	root.RunE = func(cmd *cobra.Command, args []string) error {
		stderr := cmd.ErrOrStderr()
		if !headless {
			_, _ = fmt.Fprintln(stderr, "the TUI is not built yet; run with --print")
			return ExitError{2}
		}
		switch o.Output {
		case "text", "json", "stream-json":
		default:
			_, _ = fmt.Fprintf(stderr, "unknown --output %q, want text, json or stream-json\n", o.Output)
			return ExitError{2}
		}
		prompt, err := readPrompt(args, cmd.InOrStdin(), stdinIsTerminal())
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return ExitError{2}
		}
		if prompt == "" {
			_, _ = fmt.Fprintln(stderr, "no prompt: pass it as an argument or on stdin")
			return ExitError{2}
		}
		code, err := runPrint(cmd.Context(), o, prompt, build, cmd.OutOrStdout(), stderr)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			if code == 0 {
				code = 1
			}
		}
		if code != 0 {
			return ExitError{code}
		}
		return nil
	}
}

// readPrompt joins the positional words and, when stdin is piped, prepends its content.
func readPrompt(args []string, stdin io.Reader, tty bool) (string, error) {
	prompt := strings.Join(args, " ")
	if tty {
		return prompt, nil
	}
	data, err := io.ReadAll(io.LimitReader(stdin, maxStdin+1))
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	if len(data) > maxStdin {
		return "", errors.New("stdin exceeds 10MB; write it to a file and name the file in the prompt")
	}
	piped := strings.TrimRight(string(data), "\n")
	switch {
	case piped == "":
		return prompt, nil
	case prompt == "":
		return piped, nil
	default:
		return piped + "\n\n" + prompt, nil
	}
}

// runPrint runs one turn against an embedded server and prints it. It returns the process
// exit code: 0 completed, 1 failed, 2 usage, 130 interrupted.
func runPrint(ctx context.Context, o printOptions, prompt string, build buildFunc, stdout, stderr io.Writer) (int, error) {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	b, err := build(ctx, stderr)
	if err != nil {
		return 1, err
	}
	srvCtx, cancelSrv := context.WithCancel(context.Background())
	defer func() {
		cancelSrv()
		shutdownCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = b.Close(shutdownCtx)
	}()
	clientConn, serverConn := protocol.Pipe()
	served := make(chan struct{})
	go func() { defer close(served); _ = b.Server.Serve(srvCtx, serverConn) }()
	client := protocol.NewClient(clientConn)
	// Closing the client is what ends the Serve loop, and that loop is what detaches the
	// session and closes it, releasing the store's flock. Wait for it here, before the
	// deferred Shutdown below and before returning, so nothing this process started is
	// still holding the session when the next command opens it.
	defer func() {
		_ = client.Close()
		<-served
	}()

	// Calls use a background context so an interrupt can still be delivered after ctx ends.
	bg := context.Background()
	var hello protocol.ClientHelloResult
	if err := client.Call(bg, protocol.MethodClientHello, protocol.ClientHelloParams{Client: "rudy-print", Version: b.Version, Asker: false}, &hello); err != nil {
		return 1, fmt.Errorf("hello: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 1, err
	}
	info, code, err := openOrResume(bg, client, b, o, cwd)
	if err != nil {
		// Printed here, not just returned: runPrint is called directly (bypassing
		// registerPrint's own error printing) by tests and, once the TUI exists, other
		// commands that only care about the exit code.
		_, _ = fmt.Fprintln(stderr, err)
		return code, nil
	}

	turnID, code, err := submit(bg, client, info.SessionID, prompt, stdout, stderr)
	if err != nil || code != 0 {
		return code, err
	}
	if turnID == "" {
		// The command produced no turn (plugin.Notice, whose text submit already printed
		// above, or plugin.NoAction): there is nothing for the loop below to wait on. A
		// turn.state for this turn id will never arrive, so waiting here would block
		// until SIGINT. Report a completed no-op immediately instead.
		return noOpResult(o, info, stdout)
	}

	turnULID, err := ulid.Parse(turnID)
	if err != nil {
		return 1, fmt.Errorf("turn id %q: %w", turnID, err)
	}
	out := &turnOutput{turnID: turnID, turnULID: turnULID}
	enc := json.NewEncoder(stdout)
	for {
		select {
		case <-ctx.Done():
			ictx, done := context.WithTimeout(bg, 5*time.Second)
			_ = client.Call(ictx, protocol.MethodSessionInterrupt, protocol.SessionInterruptParams{SessionID: info.SessionID, How: session.InterruptCancel}, nil)
			done()
			return 130, nil
		case n, ok := <-client.Notifications():
			if !ok {
				return 1, errors.New("server closed the connection")
			}
			if o.Output == "stream-json" {
				if err := enc.Encode(map[string]any{"method": n.Method, "params": json.RawMessage(n.Params)}); err != nil {
					return 1, err
				}
			}
			finished, err := out.observe(n)
			if err != nil {
				return 1, err
			}
			if !finished {
				continue
			}
			return out.finish(o, b, info, stdout, stderr)
		}
	}
}

// noOpResult reports a command that produced no turn as a completed no-op. Text and
// stream-json print nothing beyond what submit already wrote (a plugin.Notice's text, if
// any); json prints a zero-result shape with stop_reason "none", a value outside
// session.StopReason's own vocabulary since no turn ran to report a real one.
func noOpResult(o printOptions, info protocol.SessionInfo, stdout io.Writer) (int, error) {
	if o.Output == "json" {
		res := printResult{SessionID: info.SessionID, Result: "", StopReason: session.StopReason("none")}
		if err := json.NewEncoder(stdout).Encode(res); err != nil {
			return 1, err
		}
	}
	return 0, nil
}

// openOrResume opens a new session or resumes one named by --resume or --continue and applies
// the --model, --mode and --thinking flags to it.
func openOrResume(ctx context.Context, client *protocol.Client, b *Built, o printOptions, cwd string) (protocol.SessionInfo, int, error) {
	var info protocol.SessionInfo
	id := o.Resume
	if o.Continue && id == "" {
		newest, err := newestFor(b.Store, cwd)
		if err != nil {
			return info, 2, err
		}
		id = newest
	}
	if id == "" {
		err := client.Call(ctx, protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: cwd, Model: o.Model, Mode: o.Mode, Thinking: o.Thinking}, &info)
		if err != nil {
			return info, 1, fmt.Errorf("open session: %w", err)
		}
		return info, 0, nil
	}
	if _, err := ulid.Parse(id); err != nil {
		return info, 2, fmt.Errorf("--resume %q is not a session id", id)
	}
	if err := client.Call(ctx, protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: id}, &info); err != nil {
		return info, 1, fmt.Errorf("resume %s: %w", id, err)
	}
	// The set_* methods answer server.EntryIDResult, not SessionInfo, so info is updated here
	// from the same values the server accepted.
	var set server.EntryIDResult
	if o.Model != "" {
		if err := client.Call(ctx, protocol.MethodSessionSetModel, protocol.SessionSetModelParams{SessionID: id, Model: o.Model}, &set); err != nil {
			return info, 1, err
		}
		m, err := b.Registry.Resolve(o.Model)
		if err != nil {
			return info, 1, err
		}
		info.Model = m.Ref
	}
	if o.Mode != "" {
		if err := client.Call(ctx, protocol.MethodSessionSetMode, protocol.SessionSetModeParams{SessionID: id, Mode: session.Mode(o.Mode)}, &set); err != nil {
			return info, 1, err
		}
		info.Mode = session.Mode(o.Mode)
	}
	if o.Thinking != "" {
		if err := client.Call(ctx, protocol.MethodSessionSetThinking, protocol.SessionSetThinkingParams{SessionID: id, Thinking: session.ThinkingLevel(o.Thinking)}, &set); err != nil {
			return info, 1, err
		}
		info.Thinking = session.ThinkingLevel(o.Thinking)
	}
	return info, 0, nil
}

// newestFor returns the newest session opened on cwd.
func newestFor(st *session.Store, cwd string) (string, error) {
	want, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		want = cwd
	}
	list, err := st.List()
	if err != nil {
		return "", err
	}
	for _, s := range list {
		root, err := filepath.EvalSymlinks(s.Workspace.Root)
		if err != nil {
			root = s.Workspace.Root
		}
		if root == want {
			return s.ID.String(), nil
		}
	}
	return "", fmt.Errorf("no session for %s; drop --continue to start one", cwd)
}

// submit sends the prompt, routing a leading slash word to command.run. It returns the turn id,
// or exit code 2 for an unknown command, or 0 with an empty turn id when a command produced no turn.
func submit(ctx context.Context, client *protocol.Client, sessionID, prompt string, stdout, stderr io.Writer) (string, int, error) {
	if strings.HasPrefix(prompt, "/") {
		name, args, _ := strings.Cut(strings.TrimPrefix(prompt, "/"), " ")
		var res protocol.CommandRunResult
		err := client.Call(ctx, protocol.MethodCommandRun, protocol.CommandRunParams{SessionID: sessionID, Name: name, Args: strings.TrimSpace(args)}, &res)
		var perr *protocol.Error
		if errors.As(err, &perr) && perr.Code == protocol.CodeNotFound {
			_, _ = fmt.Fprintf(stderr, "unknown command /%s\n", name)
			return "", 2, nil
		}
		if err != nil {
			return "", 1, fmt.Errorf("/%s: %w", name, err)
		}
		if res.TurnID == "" {
			if res.Notice != "" {
				// A fork's new session id travels in Notice text, not a separate print path:
				// res.SessionID names it structurally for a caller that wants the id alone, but
				// this print path always prints the notice as it would for any other command.
				_, _ = fmt.Fprintln(stdout, res.Notice)
			}
			return "", 0, nil
		}
		return res.TurnID, 0, nil
	}
	var res protocol.SessionSubmitResult
	params := protocol.SessionSubmitParams{SessionID: sessionID, Content: []session.Block{session.TextBlock(prompt)}, Source: session.SourceTyped}
	if err := client.Call(ctx, protocol.MethodSessionSubmit, params, &res); err != nil {
		return "", 1, fmt.Errorf("submit: %w", err)
	}
	return res.TurnID, 0, nil
}

// turnOutput folds notifications into what the printer reports.
type turnOutput struct {
	turnID     string
	turnULID   ulid.ULID // parsed turnID; entries below it belong to earlier turns
	text       string
	usage      session.Usage
	stopReason session.StopReason
	failure    session.TurnFailed // zero until a turn_failed entry arrives
	state      string
}

// observe folds one notification in and reports whether the turn is over. Entries from
// before this turn are ignored: resume and fork replay the whole log, and folding those
// replayed assistant messages in would report every earlier turn's tokens and cost as this
// run's. A turn id is the ULID of the user_message that started it, so every entry of this
// turn sorts at or above it and every earlier entry sorts below.
func (t *turnOutput) observe(n protocol.Notification) (bool, error) {
	switch n.Method {
	case protocol.NotifyEntryAppended:
		var ea protocol.EntryAppended
		if err := json.Unmarshal(n.Params, &ea); err != nil {
			return false, fmt.Errorf("entry.appended: %w", err)
		}
		if ea.Entry.ID.Compare(t.turnULID) < 0 {
			return false, nil
		}
		switch p := ea.Entry.Payload.(type) {
		case session.AssistantMessage:
			t.text = textOf(p.Content)
			t.usage = t.usage.Add(p.Usage)
			t.stopReason = p.StopReason
		case session.TurnFailed:
			t.failure = p
		}
	case protocol.NotifyTurnState:
		var ts protocol.TurnStateChanged
		if err := json.Unmarshal(n.Params, &ts); err != nil {
			return false, fmt.Errorf("turn.state: %w", err)
		}
		if ts.TurnID != t.turnID {
			return false, nil
		}
		t.state = ts.State
		switch ts.State {
		case "completed", "failed", "idle":
			return true, nil
		}
	}
	return false, nil
}

// finish prints the result in the requested shape and returns the exit code.
func (t *turnOutput) finish(o printOptions, b *Built, info protocol.SessionInfo, stdout, stderr io.Writer) (int, error) {
	if t.state == "failed" {
		_, _ = fmt.Fprintf(stderr, "turn failed (%s, %d retries): %s\n", t.failure.Class, t.failure.Retries, t.failure.Message)
		return 1, nil
	}
	switch o.Output {
	case "text":
		if t.text != "" {
			_, _ = fmt.Fprintln(stdout, t.text)
		}
	case "json":
		cost := ""
		if m, err := b.Registry.Resolve(info.Model.String()); err == nil {
			if c, known := m.Pricing.Cost(t.usage); known {
				cost = c
			}
		}
		res := printResult{SessionID: info.SessionID, Result: t.text, Usage: t.usage, Cost: cost, StopReason: t.stopReason}
		if err := json.NewEncoder(stdout).Encode(res); err != nil {
			return 1, err
		}
	}
	if t.state == "idle" {
		return 130, nil
	}
	return 0, nil
}

// textOf joins the text blocks of a message.
func textOf(blocks []session.Block) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == session.BlockText {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
