package transcript

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tui/theme"
)

var ref = session.ModelRef{Provider: "anthropic", Model: "claude-sonnet-5"}

// TestMain pins the color environment. lipgloss v2 has no SetColorProfile: Style.Render
// always writes the full sequence and downsampling happens only in lipgloss.Writer, at
// print time, which this package never touches; glamour v2 styles through x/ansi the
// same way. The environment is pinned anyway so nothing that consults it can move a
// golden.
func TestMain(m *testing.M) {
	_ = os.Setenv("TERM", "xterm-256color")
	_ = os.Setenv("COLORTERM", "truecolor")
	_ = os.Setenv("CLICOLOR_FORCE", "1")
	os.Exit(m.Run())
}

func entry(t *testing.T, p session.Payload) session.Entry {
	t.Helper()
	if err := session.Validate(p); err != nil {
		t.Fatalf("invalid payload %T: %v", p, err)
	}
	return session.Entry{ID: session.NewID(), At: time.Unix(0, 0).UTC(), Kind: p.Kind(), Payload: p}
}

func TestApplyFoldsToolRows(t *testing.T) {
	tr := New(Options{Width: 80, ToolCollapsed: true, ToolPreviewLines: 2, UserPrefix: "›", BlockGap: 1}, theme.Default())
	u := entry(t, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("fix the flaky fork test")}})
	a := entry(t, session.AssistantMessage{Model: ref, Thinking: session.ThinkingHigh, StopReason: session.StopToolUse, Content: []session.Block{
		{Type: session.BlockThinking, Text: "hidden"},
		session.TextBlock("Looking at the test first."),
		session.ToolUseBlock("t1", "bash", json.RawMessage(`{"command":"go test ./internal/session -run TestFork -count=3"}`)),
	}})
	d := entry(t, session.PermissionDecision{ToolUseID: "t1", Tool: "bash", Mode: session.ModeStrict, Matcher: session.Matcher{Tool: "bash", Prefix: "go test"}, Decision: session.Allow, DecidedBy: session.ByAsker, Scope: session.ScopeOnce, Reason: "asker"})
	r := entry(t, session.ToolResult{ToolUseID: "t1", Outcome: session.OutcomeError, Content: []session.Block{session.TextBlock("--- FAIL: TestFork (0.01s)\n    fork_test.go:41: want 3 entries, got 2\nFAIL\n")}, DurationMS: 120})
	for _, e := range []session.Entry{u, a, d, r} {
		tr.Apply(e)
	}
	rows := tr.Rows()
	kinds := []RowKind{}
	for _, row := range rows {
		kinds = append(kinds, row.Kind)
	}
	if !reflect.DeepEqual(kinds, []RowKind{RowUser, RowAssistant, RowTool}) {
		t.Fatalf("kinds %v", kinds)
	}
	tool := rows[2]
	if tool.Key != "t1" || tool.Decision == nil || tool.Result == nil || tool.Expanded {
		t.Errorf("tool row %+v", tool)
	}
	if tool.TurnID != u.ID.String() || rows[1].TurnID != u.ID.String() {
		t.Errorf("turn ids %q %q, want %q", tool.TurnID, rows[1].TurnID, u.ID.String())
	}
	if rows[1].Key != a.ID.String()+"/1" {
		t.Errorf("assistant key %q, want %q", rows[1].Key, a.ID.String()+"/1")
	}
	if tr.Apply(d) != nil {
		t.Error("re-applying an absorbed entry changes nothing")
	}
	if tr.Apply(u) != nil || tr.Apply(a) != nil || tr.Apply(r) != nil {
		t.Error("re-applying any entry changes nothing")
	}
}

func TestThinkingRowsOnlyWhenShown(t *testing.T) {
	blocks := []session.Block{
		{Type: session.BlockThinking, Text: "weighing the fork bound"},
		session.TextBlock("Looking at the test first."),
	}
	a := entry(t, session.AssistantMessage{Model: ref, Thinking: session.ThinkingHigh, StopReason: session.StopEndTurn, Content: blocks})

	hidden := New(Options{Width: 80}, theme.Default())
	hidden.Apply(a)
	rows := hidden.Rows()
	if len(rows) != 1 || rows[0].Thinking {
		t.Fatalf("hidden rows %+v", rows)
	}

	shown := New(Options{Width: 80, ShowThinking: true}, theme.Default())
	shown.Apply(a)
	rows = shown.Rows()
	if len(rows) != 2 {
		t.Fatalf("shown rows %+v", rows)
	}
	if !rows[0].Thinking || rows[0].Kind != RowAssistant || rows[0].Text != "weighing the fork bound" {
		t.Errorf("thinking row %+v", rows[0])
	}
	if rows[1].Thinking || rows[1].Text != "Looking at the test first." {
		t.Errorf("text row %+v", rows[1])
	}
	if rows[0].Key != a.ID.String()+"/0" || rows[1].Key != a.ID.String()+"/1" {
		t.Errorf("keys %q %q", rows[0].Key, rows[1].Key)
	}
}

func TestDeltaThenCommitReplacesTheLiveRow(t *testing.T) {
	tr := New(Options{Width: 80}, theme.Default())
	tr.Delta("turn1", provider.Part{Type: provider.PartTextDelta, Text: "Looking "})
	tr.Delta("turn1", provider.Part{Type: provider.PartTextDelta, Text: "at the test."})
	tr.Delta("turn1", provider.Part{Type: provider.PartToolUseStart, ID: "t1", Name: "bash"})
	tr.Delta("turn1", provider.Part{Type: provider.PartToolUseDelta, ID: "t1", Text: `{"command":"ls"}`})
	rows := tr.Rows()
	if len(rows) != 2 || !rows[0].Live || rows[0].Text != "Looking at the test." || !rows[1].Live || rows[1].ToolUse.Name != "bash" {
		t.Fatalf("live rows %+v", rows)
	}
	if string(rows[1].ToolUse.Input) != `{"command":"ls"}` {
		t.Errorf("live input %q", rows[1].ToolUse.Input)
	}
	tr.Delta("turn1", provider.Part{Type: provider.PartToolUseEnd, ID: "t1"})
	rows[1].Expanded = true
	a := entry(t, session.AssistantMessage{Model: ref, Thinking: session.ThinkingOff, StopReason: session.StopToolUse, Content: []session.Block{session.TextBlock("Looking at the test."), session.ToolUseBlock("t1", "bash", json.RawMessage(`{"command":"ls"}`))}})
	tr.Apply(a)
	rows = tr.Rows()
	if len(rows) != 2 || rows[0].Live || rows[1].Live || rows[0].Key != a.ID.String()+"/0" || rows[1].Key != "t1" {
		t.Errorf("committed rows %+v", rows)
	}
	if !rows[1].Expanded {
		t.Error("the committed tool row keeps the live row's expansion")
	}
	if rows[0].TurnID != "turn1" || rows[1].TurnID != "turn1" {
		t.Errorf("turn ids %q %q, want turn1", rows[0].TurnID, rows[1].TurnID)
	}
}

func TestPromptTakesTheToolRowsPlace(t *testing.T) {
	tr := New(Options{Width: 80, ToolCollapsed: true, ToolPreviewLines: 2}, theme.Default())
	u := entry(t, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("run it")}})
	tr.Apply(u)
	a := entry(t, session.AssistantMessage{Model: ref, Thinking: session.ThinkingOff, StopReason: session.StopToolUse, Content: []session.Block{
		session.TextBlock("Running the suite."),
		session.ToolUseBlock("t1", "bash", json.RawMessage(`{"command":"go test ./..."}`)),
	}})
	tr.Apply(a)
	tr.Prompt(protocol.PermissionRequested{
		SessionID: "s1", TurnID: u.ID.String(), ToolUseID: "t1", Tool: "bash",
		Input:   json.RawMessage(`{"command":"go test ./..."}`),
		Matcher: session.Matcher{Tool: "bash", Prefix: "go test"},
	})
	rows := tr.Rows()
	if len(rows) != 4 {
		t.Fatalf("rows %+v", rows)
	}
	if rows[2].Kind != RowPrompt || rows[2].Key != "prompt/t1" || rows[2].Prompt == nil {
		t.Fatalf("prompt row %+v", rows[2])
	}
	if rows[3].Kind != RowTool || rows[3].Key != "t1" {
		t.Fatalf("tool row moved: %+v", rows[3])
	}
	if rows[2].TurnID != u.ID.String() {
		t.Errorf("prompt turn %q", rows[2].TurnID)
	}
	tr.Answered("t1")
	rows = tr.Rows()
	if len(rows) != 3 || rows[2].Kind != RowTool || rows[2].Key != "t1" {
		t.Fatalf("after Answered %+v", rows)
	}
}

func TestCommitReturnsAndRemovesTheTurn(t *testing.T) {
	tr := New(Options{Width: 80, ToolCollapsed: true, ToolPreviewLines: 2, UserPrefix: "›", BlockGap: 1}, theme.Default())
	u1 := entry(t, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("first question")}})
	tr.Apply(u1)
	tr.Apply(entry(t, session.AssistantMessage{Model: ref, Thinking: session.ThinkingOff, StopReason: session.StopEndTurn, Content: []session.Block{session.TextBlock("First answer.")}}))
	u2 := entry(t, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("second question")}})
	tr.Apply(u2)
	tr.Apply(entry(t, session.AssistantMessage{Model: ref, Thinking: session.ThinkingOff, StopReason: session.StopEndTurn, Content: []session.Block{session.TextBlock("Second answer.")}}))

	lines := tr.Commit(u1.ID.String())
	if len(lines) == 0 {
		t.Fatal("no lines for turn 1")
	}
	joined := ansi.Strip(strings.Join(lines, "\n"))
	if !strings.Contains(joined, "first question") || !strings.Contains(joined, "First answer.") {
		t.Errorf("turn 1 lines %q", joined)
	}
	if strings.Contains(joined, "second question") {
		t.Errorf("turn 1 leaked turn 2: %q", joined)
	}
	rows := tr.Rows()
	if len(rows) != 2 || rows[0].TurnID != u2.ID.String() || rows[1].TurnID != u2.ID.String() {
		t.Fatalf("remaining rows %+v", rows)
	}
	if tr.Commit(session.NewID().String()) != nil {
		t.Error("an unknown turn commits nothing")
	}
}

func TestToggleExpandsOnlyToolRows(t *testing.T) {
	tr := New(Options{Width: 80, ToolCollapsed: true, ToolPreviewLines: 2}, theme.Default())
	u := entry(t, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("go")}})
	tr.Apply(u)
	a := entry(t, session.AssistantMessage{Model: ref, Thinking: session.ThinkingOff, StopReason: session.StopToolUse, Content: []session.Block{
		session.ToolUseBlock("t1", "bash", json.RawMessage(`{"command":"ls"}`)),
	}})
	tr.Apply(a)
	if !tr.Toggle("t1") {
		t.Error("Toggle expands")
	}
	if tr.Toggle("t1") {
		t.Error("Toggle collapses again")
	}
	if tr.Toggle(u.ID.String()) {
		t.Error("a user row does not expand")
	}
	if tr.Toggle("nope") {
		t.Error("an unknown key does not expand")
	}
}

func TestUnknownToolUseBecomesAMarker(t *testing.T) {
	tr := New(Options{Width: 80}, theme.Default())
	e := entry(t, session.ToolResult{ToolUseID: "ghost", Outcome: session.OutcomeOK, Content: []session.Block{session.TextBlock("ok")}})
	keys := tr.Apply(e)
	if len(keys) != 1 || keys[0] != e.ID.String() {
		t.Fatalf("keys %v", keys)
	}
	rows := tr.Rows()
	if len(rows) != 1 || rows[0].Kind != RowMarker {
		t.Fatalf("rows %+v", rows)
	}
	if got := strings.Join(tr.Render(rows[0]), "\n"); !strings.Contains(got, "unknown tool_use") {
		t.Errorf("marker render %q", got)
	}
}

func TestSilentEntriesProduceNoRow(t *testing.T) {
	tr := New(Options{Width: 80}, theme.Default())
	for _, p := range []session.Payload{
		session.ModelChange{Model: ref},
		session.ModeChange{Mode: session.ModePermissive},
		session.ThinkingChange{Thinking: session.ThinkingLow},
		session.TitleChange{Title: "t"},
	} {
		if keys := tr.Apply(entry(t, p)); keys != nil {
			t.Errorf("%T produced %v", p, keys)
		}
	}
	if len(tr.Rows()) != 0 {
		t.Errorf("rows %+v", tr.Rows())
	}
}

// assistantWith is one assistant message over the given blocks.
func assistantWith(t *testing.T, blocks ...session.Block) session.Entry {
	t.Helper()
	return entry(t, session.AssistantMessage{Model: ref, Thinking: session.ThinkingOff, StopReason: session.StopToolUse, Content: blocks})
}

func keysOf(rows []*Row) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Key)
	}
	return out
}

// Finding 1: the committed rows come out in block order, not bunched at one anchor.
func TestCommittedRowsKeepBlockOrder(t *testing.T) {
	t.Run("text tool text", func(t *testing.T) {
		tr := New(Options{Width: 80}, theme.Default())
		tr.Delta("turn1", provider.Part{Type: provider.PartTextDelta, Text: "first"})
		tr.Delta("turn1", provider.Part{Type: provider.PartToolUseStart, ID: "t1", Name: "bash"})
		a := assistantWith(t,
			session.TextBlock("first"),
			session.ToolUseBlock("t1", "bash", json.RawMessage(`{"command":"ls"}`)),
			session.TextBlock("second"),
		)
		tr.Apply(a)
		want := []string{a.ID.String() + "/0", "t1", a.ID.String() + "/2"}
		if got := keysOf(tr.Rows()); !reflect.DeepEqual(got, want) {
			t.Errorf("keys %v, want %v", got, want)
		}
	})
	t.Run("tool text", func(t *testing.T) {
		tr := New(Options{Width: 80}, theme.Default())
		tr.Delta("turn1", provider.Part{Type: provider.PartToolUseStart, ID: "t1", Name: "bash"})
		tr.Delta("turn1", provider.Part{Type: provider.PartTextDelta, Text: "after"})
		a := assistantWith(t,
			session.ToolUseBlock("t1", "bash", json.RawMessage(`{"command":"ls"}`)),
			session.TextBlock("after"),
		)
		tr.Apply(a)
		want := []string{"t1", a.ID.String() + "/1"}
		if got := keysOf(tr.Rows()); !reflect.DeepEqual(got, want) {
			t.Errorf("keys %v, want %v", got, want)
		}
	})
	t.Run("replay with no live rows", func(t *testing.T) {
		tr := New(Options{Width: 80}, theme.Default())
		a := assistantWith(t,
			session.TextBlock("first"),
			session.ToolUseBlock("t1", "bash", json.RawMessage(`{"command":"ls"}`)),
			session.TextBlock("second"),
		)
		tr.Apply(a)
		want := []string{a.ID.String() + "/0", "t1", a.ID.String() + "/2"}
		if got := keysOf(tr.Rows()); !reflect.DeepEqual(got, want) {
			t.Errorf("keys %v, want %v", got, want)
		}
	})
}

// Minor: a message that draws nothing still ends the turn's live rows.
func TestAssistantWithNoRowsSweepsTheLiveRows(t *testing.T) {
	tr := New(Options{Width: 80}, theme.Default())
	tr.Delta("turn1", provider.Part{Type: provider.PartThinkingDelta, Text: "weighing"})
	if rows := tr.Rows(); len(rows) != 1 || !rows[0].Live {
		t.Fatalf("live rows %+v", rows)
	}
	// Thinking is hidden, so this message builds no rows at all.
	if keys := tr.Apply(assistantWith(t, session.Block{Type: session.BlockThinking, Text: "weighing"})); keys != nil {
		t.Errorf("keys %v", keys)
	}
	if rows := tr.Rows(); len(rows) != 0 {
		t.Errorf("live rows survived: %+v", rows)
	}
}

// Finding 4: a token longer than the width wraps; nothing is truncated away.
func TestLongTokensWrapRatherThanTruncate(t *testing.T) {
	long := strings.Repeat("a", 200)
	tr := New(Options{Width: 40, UserPrefix: "›"}, theme.Default())
	tr.Apply(entry(t, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("see " + long + " end")}}))
	lines := tr.Render(tr.Rows()[0])
	got := ansi.Strip(strings.Join(lines, "\n"))
	if n := strings.Count(got, "a"); n != 200 {
		t.Errorf("kept %d of 200 characters:\n%s", n, got)
	}
	if !strings.Contains(got, "end") {
		t.Errorf("lost the tail:\n%s", got)
	}
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > 40 {
			t.Errorf("line %d cells wide: %q", w, l)
		}
	}
}

// Finding 5: untrusted text draws without the sequences that would move the cursor.
func TestUntrustedTextIsStripped(t *testing.T) {
	const (
		esc  = "\x1b"
		bell = "\a"
	)
	tr := New(Options{Width: 80, ToolCollapsed: true, ToolPreviewLines: 2, UserPrefix: "›"}, theme.Default())
	u := entry(t, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{
		session.TextBlock("look " + esc + "[31mred" + esc + "[m\rnow" + bell),
	}})
	tr.Apply(u)
	// The escape reaches the summary through valid JSON, the way a model would send it.
	tr.Apply(assistantWith(t, session.ToolUseBlock("t1", "bash", json.RawMessage(`{"command":"say \u001b[2Jhi"}`))))
	tr.Apply(entry(t, session.ToolResult{ToolUseID: "t1", Outcome: session.OutcomeOK, Content: []session.Block{
		session.TextBlock(esc + "[31mboom" + esc + "[m\rgone" + bell),
	}}))
	out := strings.Join(tr.Commit(u.ID.String()), "\n")
	for _, bad := range []string{"[31m", "[2J", "\r", bell} {
		if strings.Contains(out, bad) {
			t.Errorf("%q survived: %q", bad, out)
		}
	}
	// A dropped carriage return closes the gap it left: nothing is inserted in its place.
	for _, want := range []string{"look rednow", "boomgone", "say hi"} {
		if !strings.Contains(ansi.Strip(out), want) {
			t.Errorf("lost %q from %q", want, ansi.Strip(out))
		}
	}
}

// Finding 2: the list tools' counts come off their real result shapes.
func TestListCountsFollowTheToolShapes(t *testing.T) {
	cases := []struct {
		tool, result, want string
	}{
		{"read", "1\tpackage x\n2\tfunc f()\n", "2 lines"},
		{"read", "1\tpackage x\n", "1 line"},
		{"read", "1\ta\n2\tb\n… 40 more lines\n", "2 lines, 40 more"},
		{"grep", "no matches\n", "no matches"},
		{"grep", "a.go:1:x\n", "1 match"},
		{"grep", "a.go:1:x\na.go:2:y\n… truncated at 200 matches\n", "2 matches, truncated at 200"},
		{"glob", "no matches\n", "no files"},
		{"glob", "a.go\nb.go\n", "2 files"},
		{"glob", "a.go\n… truncated at 1000 results\n", "1 file, truncated at 1000"},
		{"glob", "", "no files"},
	}
	for _, c := range cases {
		t.Run(c.tool+"/"+c.want, func(t *testing.T) {
			tr := New(Options{Width: 80, ToolCollapsed: true, ToolPreviewLines: 2}, theme.Default())
			u := entry(t, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("go")}})
			tr.Apply(u)
			tr.Apply(assistantWith(t, session.ToolUseBlock("t1", c.tool, json.RawMessage(`{"pattern":"x","path":"y"}`))))
			var blocks []session.Block
			if c.result != "" {
				blocks = append(blocks, session.TextBlock(c.result))
			}
			tr.Apply(entry(t, session.ToolResult{ToolUseID: "t1", Outcome: session.OutcomeOK, Content: blocks}))
			got := ansi.Strip(strings.Join(tr.Commit(u.ID.String()), "\n"))
			if !strings.Contains(got, c.want) {
				t.Errorf("preview %q, want %q", got, c.want)
			}
		})
	}
}

// Finding 3: a failed tool shows what it said, in the error role, whatever tool it is.
func TestFailedToolsShowTheirText(t *testing.T) {
	// The error role's opening sequence, to prove the preview carries it.
	errOpen := strings.TrimSuffix(theme.Default().Style(theme.RoleError).Render("x"), "x\x1b[m")
	cases := []struct {
		name, tool, input, result, absent string
	}{
		{"edit", "edit", `{"path":"a.go","old":"x\n","new":"y\n"}`, "edit: old text not found in a.go\n", "-x"},
		{"read", "read", `{"path":"a.go"}`, "read: a.go is a binary file\n", "lines"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := New(Options{Width: 80, ToolCollapsed: true, ToolPreviewLines: 2}, theme.Default())
			u := entry(t, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("go")}})
			tr.Apply(u)
			tr.Apply(assistantWith(t, session.ToolUseBlock("t1", c.tool, json.RawMessage(c.input))))
			tr.Apply(entry(t, session.ToolResult{ToolUseID: "t1", Outcome: session.OutcomeError, Content: []session.Block{session.TextBlock(c.result)}}))
			joined := strings.Join(tr.Commit(u.ID.String()), "\n")
			plain := ansi.Strip(joined)
			if !strings.Contains(plain, strings.TrimSpace(c.result)) {
				t.Errorf("preview %q, want the error text", plain)
			}
			if strings.Contains(plain, c.absent) {
				t.Errorf("preview still shows the clean-run shape %q: %q", c.absent, plain)
			}
			if !strings.Contains(joined, errOpen) {
				t.Errorf("preview is not in the error role: %q", joined)
			}
		})
	}
}

// Minor: a note role the vocabulary does not define falls back to text rather than to no
// color at all.
func TestUnknownNoteRoleFallsBackToText(t *testing.T) {
	tr := New(Options{Width: 80}, theme.Default())
	e := session.Entry{ID: session.NewID(), Kind: session.KindNote, Payload: session.Note{Plugin: "p", Text: "hello", Role: session.NoteRole("shouty")}}
	tr.Apply(e)
	got := strings.Join(tr.Render(tr.Rows()[0]), "\n")
	want := theme.Default().Style(theme.RoleText).Render("hello")
	if got != want {
		t.Errorf("note %q, want %q", got, want)
	}
}
