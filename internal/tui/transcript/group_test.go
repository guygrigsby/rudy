// SPDX-License-Identifier: AGPL-3.0-or-later

package transcript

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tui/theme"
)

// groupedTranscript is a transcript with grouping on, and a helper that appends a settled
// tool call: its tool_use, decision and result in one shot, so the row is one grouping
// will take.
type groupedTranscript struct {
	*Transcript
	t   *testing.T
	n   int
	ids []string
}

func newGroupedTranscript(t *testing.T) *groupedTranscript {
	t.Helper()
	return &groupedTranscript{
		Transcript: New(Options{Width: 80, ToolCollapsed: true, ToolGrouped: true, ToolPreviewLines: 2, BlockGap: 1}, theme.Default()),
		t:          t,
	}
}

// tool appends a settled call for name and returns its tool_use id.
func (g *groupedTranscript) tool(name string) string {
	g.t.Helper()
	g.n++
	id := fmt.Sprintf("t%d", g.n)
	g.ids = append(g.ids, id)
	g.Apply(assistantEntry(g.t, session.ToolUseBlock(id, name, json.RawMessage(`{}`))))
	g.Apply(entry(g.t, session.PermissionDecision{ToolUseID: id, Tool: name, Mode: session.ModeStrict, Decision: session.Allow, DecidedBy: session.ByAllowance, Scope: session.ScopeOnce, Reason: "rule"}))
	g.Apply(entry(g.t, session.ToolResult{ToolUseID: id, Outcome: session.OutcomeOK, Content: []session.Block{session.TextBlock("ok")}, DurationMS: 1}))
	return id
}

// laid is Layout's lines stripped of styling, for substring assertions.
func laid(tr *Transcript) string {
	var out []string
	for _, l := range tr.Layout() {
		out = append(out, ansi.Strip(l.Text))
	}
	return strings.Join(out, "\n")
}

func TestToolGroupFoldsARun(t *testing.T) {
	g := newGroupedTranscript(t)
	g.tool("grep")
	g.tool("grep")
	g.tool("read")

	got := laid(g.Transcript)
	if !strings.Contains(got, "grep ×2, read ×1") {
		t.Fatalf("fold line %q", got)
	}
	// The fold line stands for all three rows: none of their own summaries draws.
	if strings.Contains(got, "grep  ") || strings.Contains(got, "read  ") {
		t.Errorf("a folded row still draws its own summary: %q", got)
	}
	lines := g.Layout()
	if len(lines) != 1 {
		t.Fatalf("one fold line, got %d: %q", len(lines), got)
	}
	// The click lands on the run's first row, whose key Toggle reads as the whole run's.
	if lines[0].Row == nil || lines[0].Row.Key != "t1" {
		t.Errorf("fold line belongs to %v, want the run's first row t1", lines[0].Row)
	}
}

func TestToolGroupToggleOpensEveryRow(t *testing.T) {
	g := newGroupedTranscript(t)
	g.tool("grep")
	g.tool("read")

	// A click on the fold line reaches the leader; Toggle opens the whole run, not the
	// leader's own expansion.
	if !g.Toggle("t1") {
		t.Fatal("toggling a folded group reports expanded")
	}
	got := laid(g.Transcript)
	if !strings.Contains(got, "grep") || !strings.Contains(got, "read") {
		t.Fatalf("the run draws its rows: %q", got)
	}
	if strings.Contains(got, "×2") || strings.Contains(got, "×1") {
		t.Errorf("no fold line once open: %q", got)
	}
	rows := g.Rows()
	for _, r := range rows {
		if !r.GroupExpanded {
			t.Errorf("row %q carries the group expansion", r.Key)
		}
		if r.Expanded {
			t.Errorf("row %q is still folded to its own summary", r.Key)
		}
	}
	// Toggling again opens the row itself, the same as a row that never folded.
	if !g.Toggle("t1") || !rowByKey(t, rows, "t1").Expanded {
		t.Error("the next toggle is the row's own expansion")
	}
}

func TestToolGroupLeavesUnsettledRunsAlone(t *testing.T) {
	g := newGroupedTranscript(t)
	g.tool("grep")
	// A call still running has no result yet, so the run does not fold.
	g.Apply(assistantEntry(g.t, session.ToolUseBlock("t2", "read", json.RawMessage(`{"path":"x"}`))))

	got := laid(g.Transcript)
	if strings.Contains(got, "×") {
		t.Errorf("a run with a call in flight does not fold: %q", got)
	}
	if !strings.Contains(got, "running") {
		t.Errorf("the running row says so: %q", got)
	}

	// Once it settles the run folds.
	g.Apply(entry(g.t, session.ToolResult{ToolUseID: "t2", Outcome: session.OutcomeOK, Content: []session.Block{session.TextBlock("ok")}, DurationMS: 1}))
	if got := laid(g.Transcript); !strings.Contains(got, "grep ×1, read ×1") {
		t.Errorf("settled, the run folds: %q", got)
	}
}

func TestToolGroupBreaksOnAnotherRow(t *testing.T) {
	g := newGroupedTranscript(t)
	g.tool("grep")
	// An assistant block between two tool calls ends the run: each side is one row, and
	// one row is not a group.
	g.Apply(assistantEntry(g.t, session.TextBlock("checking")))
	g.tool("read")

	got := laid(g.Transcript)
	if strings.Contains(got, "×") {
		t.Errorf("two runs of one fold into nothing: %q", got)
	}
	for _, want := range []string{"grep", "checking", "read"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is missing from %q", want, got)
		}
	}
}

func TestToolGroupCommitsFolded(t *testing.T) {
	g := newGroupedTranscript(t)
	u := entry(g.t, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("go")}})
	g.Apply(u)
	g.tool("grep")
	g.tool("grep")

	lines := g.Commit(u.ID.String())
	joined := ansi.Strip(strings.Join(lines, "\n"))
	if !strings.Contains(joined, "grep ×2") {
		t.Fatalf("scrollback keeps the fold: %q", joined)
	}
	if rows := g.Rows(); len(rows) != 0 {
		t.Errorf("the whole run leaves with its turn: %+v", rows)
	}
}

func TestToolGroupKeepsAnOpenQuestionOut(t *testing.T) {
	g := newGroupedTranscript(t)
	g.tool("bash")
	g.tool("read")
	// A question standing in the read row's place keeps the run ungrouped: folding the
	// row under a fold line would fold the question's summary away.
	g.Prompt(protocol.PermissionRequested{ToolUseID: "t2", Tool: "read", Input: json.RawMessage(`{"path":"x"}`)})

	got := laid(g.Transcript)
	if strings.Contains(got, "×") {
		t.Errorf("a question in a row's place does not fold: %q", got)
	}
	if !strings.Contains(got, "allow once") {
		t.Errorf("the question still draws: %q", got)
	}
}

// TestToolGroupStaysInsideItsOrigin is the ADR 0028 half of grouping: a subagent's own
// run folds under the agent call that opened it, at that call's indent, and never merges
// with the parent's rows either side of it, so a child call sandwiched between two of the
// parent's does not glue them into one run.
func TestToolGroupStaysInsideItsOrigin(t *testing.T) {
	g := newGroupedTranscript(t)
	g.Apply(assistantEntry(g.t, session.ToolUseBlock("tu1", "agent", json.RawMessage(`{"agent":"explorer","prompt":"look"}`))))
	child := func(name string) {
		g.t.Helper()
		g.n++
		id := fmt.Sprintf("c%d", g.n)
		g.ApplyFrom("child", "tu1", assistantEntry(g.t, session.ToolUseBlock(id, name, json.RawMessage(`{}`))))
		g.ApplyFrom("child", "tu1", entry(g.t, session.ToolResult{ToolUseID: id, Outcome: session.OutcomeOK, Content: []session.Block{session.TextBlock("ok")}, DurationMS: 1}))
	}
	child("grep")
	child("grep")
	g.tool("read")
	g.tool("glob")

	lines := g.Layout()
	var folds []Line
	for _, l := range lines {
		if strings.Contains(ansi.Strip(l.Text), "×") {
			folds = append(folds, l)
		}
	}
	// Two runs fold: the child's and the parent's. The agent call itself stands alone
	// between them and joins neither.
	if len(folds) != 2 {
		t.Fatalf("fold lines %q", laid(g.Transcript))
	}
	childFold, parentFold := ansi.Strip(folds[0].Text), ansi.Strip(folds[1].Text)
	if !strings.Contains(childFold, "grep ×2") || !strings.Contains(parentFold, "read ×1, glob ×1") {
		t.Errorf("child fold %q, parent fold %q", childFold, parentFold)
	}
	if indentOf(folds[0]) <= indentOf(folds[1]) {
		t.Errorf("the child's fold indents under its call: child=%d parent=%d", indentOf(folds[0]), indentOf(folds[1]))
	}
}

func TestToolGroupOffDrawsEveryRow(t *testing.T) {
	tr := New(Options{Width: 80, ToolCollapsed: true, ToolPreviewLines: 2, BlockGap: 1}, theme.Default())
	g := &groupedTranscript{Transcript: tr, t: t}
	g.tool("grep")
	g.tool("grep")

	got := laid(tr)
	if strings.Contains(got, "×") {
		t.Errorf("grouping off folds nothing: %q", got)
	}
}
