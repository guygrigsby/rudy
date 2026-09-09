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
