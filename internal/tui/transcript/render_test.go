// SPDX-License-Identifier: AGPL-3.0-or-later

package transcript

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tui/icons"
	"github.com/guygrigsby/rudy/internal/tui/theme"
)

var update = flag.Bool("update", false, "regenerate the golden files")

// golden compares got against testdata/<name>.golden, or rewrites it under -update. The
// files hold the ANSI bytes verbatim: nothing is stripped.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (run go test -update)", name, err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch\ngot:\n%q\nwant:\n%q", name, got, want)
	}
}

// defaults are the design's ui.transcript defaults, the ones that produce the default
// screen in docs/specs/2026-09-07-rudy-design.md.
func defaults() Options {
	return Options{Width: 80, ToolCollapsed: true, ToolPreviewLines: 2, UserPrefix: "›", BlockGap: 1}
}

// start opens a transcript on one user message and returns it with that turn's id.
func start(t *testing.T, o Options, text string) (*Transcript, string) {
	t.Helper()
	tr := New(o, theme.Default())
	u := entry(t, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock(text)}})
	tr.Apply(u)
	return tr, u.ID.String()
}

// toolTurn is one assistant message carrying one tool_use plus that tool's result.
func toolTurn(t *testing.T, tr *Transcript, id, name, input, result string, outcome session.Outcome) {
	t.Helper()
	tr.Apply(entry(t, session.AssistantMessage{Model: ref, Thinking: session.ThinkingOff, StopReason: session.StopToolUse, Content: []session.Block{
		session.ToolUseBlock(id, name, json.RawMessage(input)),
	}}))
	if result == "" {
		return
	}
	tr.Apply(entry(t, session.ToolResult{ToolUseID: id, Outcome: outcome, Content: []session.Block{session.TextBlock(result)}, DurationMS: 42}))
}

const (
	bashInput  = `{"command":"go test ./internal/session -run TestFork -count=3"}`
	bashOutput = "--- FAIL: TestFork (0.01s)\nfork_test.go:41: want 3 entries, got 2\n"
	editInput  = `{"path":"internal/session/fork.go","old":"\tentries := s.entries[:at]\n","new":"\tentries := s.entries[:at+1]\n"}`
	editOutput = "replaced 1 occurrence in internal/session/fork.go"
)

// screen builds the design's default screen: a user message, an assistant paragraph, a
// failing bash call, the edit that fixes it and the closing paragraph.
func screen(t *testing.T, o Options) string {
	t.Helper()
	tr, turn := start(t, o, "fix the flaky fork test")
	tr.Apply(entry(t, session.AssistantMessage{Model: ref, Thinking: session.ThinkingOff, StopReason: session.StopToolUse, Content: []session.Block{
		session.TextBlock("Looking at the test first."),
		session.ToolUseBlock("t1", "bash", json.RawMessage(bashInput)),
	}}))
	tr.Apply(entry(t, session.ToolResult{ToolUseID: "t1", Outcome: session.OutcomeError, Content: []session.Block{session.TextBlock(bashOutput)}, DurationMS: 120}))
	toolTurn(t, tr, "t2", "edit", editInput, editOutput, session.OutcomeOK)
	tr.Apply(entry(t, session.AssistantMessage{Model: ref, Thinking: session.ThinkingOff, StopReason: session.StopEndTurn, Content: []session.Block{
		session.TextBlock("Off by one in the slice bound. Fixed and green."),
	}}))
	return strings.Join(tr.Commit(turn), "\n") + "\n"
}

func TestGoldenDefaultScreen(t *testing.T) {
	golden(t, "screen_default", screen(t, defaults()))
}

func TestGoldenExpandedScreen(t *testing.T) {
	o := defaults()
	o.ToolCollapsed = false
	golden(t, "screen_expanded", screen(t, o))
}

func TestGoldenDiffBackground(t *testing.T) {
	o := defaults()
	o.DiffBackground = true
	golden(t, "screen_diff_background", screen(t, o))
}

func TestGoldenToolPreviews(t *testing.T) {
	lines := func(n int, f func(int) string) string {
		out := make([]string, n)
		for i := range n {
			out[i] = f(i)
		}
		return strings.Join(out, "\n") + "\n"
	}
	cases := []struct {
		name, tool, input, result string
	}{
		{"tool_read", "read", `{"path":"internal/session/fork.go"}`,
			lines(12, func(i int) string { return "line " + string(rune('a'+i)) })},
		{"tool_grep", "grep", `{"pattern":"TestFork","path":"internal/session"}`,
			lines(3, func(i int) string { return "internal/session/fork_test.go:4" + string(rune('1'+i)) + ":func TestFork" })},
		{"tool_glob", "glob", `{"pattern":"internal/**/*_test.go"}`,
			lines(5, func(i int) string { return "internal/session/f" + string(rune('a'+i)) + "_test.go" })},
		{"tool_write", "write", `{"path":"internal/session/fork.go","content":"package session\n"}`,
			"wrote 16 bytes to internal/session/fork.go"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr, turn := start(t, defaults(), "look around")
			toolTurn(t, tr, "t1", c.tool, c.input, c.result, session.OutcomeOK)
			golden(t, c.name, strings.Join(tr.Commit(turn), "\n")+"\n")
		})
	}
}

func TestGoldenPendingRow(t *testing.T) {
	tr, turn := start(t, defaults(), "run the suite")
	toolTurn(t, tr, "t1", "bash", bashInput, "", session.OutcomeOK)
	golden(t, "tool_pending", strings.Join(tr.Commit(turn), "\n")+"\n")
}

func TestGoldenDeniedRow(t *testing.T) {
	tr, turn := start(t, defaults(), "delete everything")
	tr.Apply(entry(t, session.AssistantMessage{Model: ref, Thinking: session.ThinkingOff, StopReason: session.StopToolUse, Content: []session.Block{
		session.ToolUseBlock("t1", "bash", json.RawMessage(`{"command":"rm -rf /"}`)),
	}}))
	tr.Apply(entry(t, session.PermissionDecision{
		ToolUseID: "t1", Tool: "bash", Mode: session.ModeStrict,
		Matcher: session.Matcher{Tool: "bash", Prefix: "rm"}, Decision: session.Deny,
		DecidedBy: session.ByAsker, Scope: session.ScopeOnce, Reason: "the asker said no",
	}))
	golden(t, "tool_denied", strings.Join(tr.Commit(turn), "\n")+"\n")
}

func TestGoldenKilledRow(t *testing.T) {
	tr, turn := start(t, defaults(), "run the slow one")
	toolTurn(t, tr, "t1", "bash", `{"command":"sleep 600"}`, "partial output\n", session.OutcomeKilled)
	golden(t, "tool_killed", strings.Join(tr.Commit(turn), "\n")+"\n")
}

func TestGoldenPromptRow(t *testing.T) {
	tr, turn := start(t, defaults(), "run the suite")
	tr.Apply(entry(t, session.AssistantMessage{Model: ref, Thinking: session.ThinkingOff, StopReason: session.StopToolUse, Content: []session.Block{
		session.ToolUseBlock("t1", "bash", json.RawMessage(bashInput)),
	}}))
	tr.Prompt(protocol.PermissionRequested{
		SessionID: "s1", TurnID: turn, ToolUseID: "t1", Tool: "bash",
		Input:   json.RawMessage(bashInput),
		Matcher: session.Matcher{Tool: "bash", Prefix: "go test"},
	})
	golden(t, "prompt", strings.Join(tr.Commit(turn), "\n")+"\n")
}

func TestGoldenMarkerRows(t *testing.T) {
	first, last := session.NewID(), session.NewID()
	cases := []struct {
		name    string
		payload session.Payload
	}{
		{"marker_note", session.Note{Plugin: "memory", Text: "recalled 3 memories for this workspace", Role: session.NoteMuted}},
		{"marker_compaction", session.Compaction{
			Summary:      "The turn found an off by one in fork's slice bound and fixed it.\nTests are green.",
			FirstEntryID: first, LastEntryID: last, Model: ref,
		}},
		{"marker_interrupted", session.TurnInterrupted{TurnID: first, How: session.InterruptSteer}},
		{"marker_failed", session.TurnFailed{TurnID: first, Class: session.ErrProvider, Message: "529 overloaded after 5 attempts", Retries: 5}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr, turn := start(t, defaults(), "keep going")
			tr.Apply(entry(t, c.payload))
			golden(t, c.name, strings.Join(tr.Commit(turn), "\n")+"\n")
		})
	}
}

func TestGoldenLiveAssistantRow(t *testing.T) {
	tr, turn := start(t, defaults(), "explain the bound")
	for _, s := range []string{"The fork copied ", "`s.entries[:at]`, which drops the entry at the fork point. "} {
		tr.Delta(turn, provider.Part{Type: provider.PartTextDelta, Text: s})
	}
	tr.Delta(turn, provider.Part{Type: provider.PartToolUseStart, ID: "t1", Name: "read"})
	tr.Delta(turn, provider.Part{Type: provider.PartToolUseDelta, ID: "t1", Text: `{"path":"internal/session/fork.go"}`})
	golden(t, "live_stream", strings.Join(tr.Commit(turn), "\n")+"\n")
}

// mdDoc exercises every markdown path the theme touches: a heading, a link, an inline
// code span, a highlighted fence and a diff fence, which is the one chroma styles paint
// a background on.
const mdDoc = "## The fork bound\n\n" +
	"See [ADR 0013](https://example.test/adr) and `Row.Key`.\n\n" +
	"```go\nfunc f() int { return 1 }\n```\n\n" +
	"```diff\n-\tentries := s.entries[:at]\n+\tentries := s.entries[:at+1]\n```\n"

func TestGoldenMarkdown(t *testing.T) {
	tr, turn := start(t, defaults(), "explain the bound")
	tr.Apply(entry(t, session.AssistantMessage{Model: ref, Thinking: session.ThinkingOff, StopReason: session.StopEndTurn, Content: []session.Block{
		session.TextBlock(mdDoc),
	}}))
	golden(t, "markdown", strings.Join(tr.Commit(turn), "\n")+"\n")
}

// TestNoPaintedBackgrounds is the design's "no painted backgrounds, the terminal's black
// shows through" as a gate over every golden. ui.diff.style = "background" is the one
// exception the design grants, and it is one file.
func TestNoPaintedBackgrounds(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.golden"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no goldens: %v", err)
	}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		painted := strings.Contains(string(body), "48;")
		want := filepath.Base(f) == "screen_diff_background.golden"
		if painted != want {
			t.Errorf("%s paints a background: %v, want %v", f, painted, want)
		}
	}
}

// escapeDoc is an answer carrying the two things a model must never be able to write into
// a terminal: an ANSI sequence, here the one that clears the screen, and a C0 byte.
const escapeDoc = "clear \x1b[2Jthis \aand this\n"

// TestAnAnswerCarriesNoEscapes covers both halves of an assistant row, which take
// different paths: the live one wraps and sanitizes, the committed one renders through
// glamour, whose escape replacer is for markdown syntax and leaves an ANSI sequence and a
// C0 byte exactly as they arrived. The committed row is the one an inline client writes
// into the terminal's own scrollback through tea.Println, so it is the one that would
// clear the screen.
func TestAnAnswerCarriesNoEscapes(t *testing.T) {
	tr, turn := start(t, defaults(), "say something")
	tr.Delta(turn, provider.Part{Type: provider.PartTextDelta, Text: escapeDoc})
	live := strings.Join(tr.Render(tr.Rows()[1]), "\n")
	tr.Apply(entry(t, session.AssistantMessage{Model: ref, Thinking: session.ThinkingOff, StopReason: session.StopEndTurn, Content: []session.Block{
		session.TextBlock(escapeDoc),
	}}))
	committed := strings.Join(tr.Commit(turn), "\n")
	for _, c := range []struct{ name, got string }{{"live", live}, {"committed", committed}} {
		t.Run(c.name, func(t *testing.T) {
			if strings.Contains(c.got, "\x1b[2J") {
				t.Errorf("the row carries the erase sequence: %q", c.got)
			}
			if strings.ContainsRune(c.got, '\a') {
				t.Errorf("the row carries a C0 byte: %q", c.got)
			}
			if plain := ansi.Strip(c.got); !strings.Contains(plain, "clear this and this") {
				t.Errorf("the row lost the text it was safe to draw: %q", plain)
			}
		})
	}
}

// TestAToolRowSaysWhichWayItIs is what makes a click legible: the row opens with a marker
// pointing right while its result is folded and down while it is open, so expanding is
// visible rather than inferred from the lines below (ADR 0025).
func TestAToolRowSaysWhichWayItIs(t *testing.T) {
	tr := New(Options{Width: 80, ToolCollapsed: true, ToolPreviewLines: 2, Icons: icons.Default()}, theme.Default())
	tr.Apply(entry(t, session.AssistantMessage{
		Model: ref, Thinking: session.ThinkingHigh, StopReason: session.StopToolUse,
		Content: []session.Block{session.ToolUseBlock("t1", "bash", json.RawMessage(`{"command":"go test"}`))},
	}))
	rows := tr.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows %+v", rows)
	}
	// Nothing to fold yet: the call has not answered.
	if got := ansi.Strip(tr.Render(rows[0])[0]); strings.HasPrefix(strings.TrimSpace(got), icons.Default().Get(icons.Collapsed)) {
		t.Errorf("a running call has nothing to expand: %q", got)
	}
	tr.Apply(entry(t, session.ToolResult{
		ToolUseID: "t1", Outcome: session.OutcomeOK, DurationMS: 1,
		Content: []session.Block{session.TextBlock("ok\n")},
	}))
	collapsed := ansi.Strip(tr.Render(rows[0])[0])
	if !strings.Contains(collapsed, icons.Default().Get(icons.Collapsed)) {
		t.Errorf("a folded row points right: %q", collapsed)
	}
	tr.Toggle(rows[0].Key)
	expanded := ansi.Strip(tr.Render(rows[0])[0])
	if !strings.Contains(expanded, icons.Default().Get(icons.Expanded)) {
		t.Errorf("an open row points down: %q", expanded)
	}
	if collapsed == expanded {
		t.Error("and the two look different")
	}
}
