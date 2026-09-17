// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testEntry(t *testing.T, p Payload) Entry {
	t.Helper()
	return Entry{ID: NewID(), At: time.Now().UTC(), Kind: p.Kind(), Payload: p}
}

func TestLogAppendThenRead(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		testEntry(t, ModeChange{Mode: ModeOff}),
		testEntry(t, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("hi")}}),
		testEntry(t, TitleChange{Title: "t"}),
	}
	for _, e := range want {
		if err := l.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("read %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Kind != want[i].Kind {
			t.Fatalf("entry %d: got %s %s, want %s %s", i, got[i].ID, got[i].Kind, want[i].ID, want[i].Kind)
		}
		if got[i].ID.Compare(want[i].ID) != 0 || (i > 0 && got[i].ID.Compare(got[i-1].ID) <= 0) {
			t.Fatalf("ids not increasing at %d", i)
		}
	}
}

func TestLogSyncMakesEntriesVisible(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if err := l.Append(testEntry(t, ModeChange{Mode: ModeOff})); err != nil {
		t.Fatal(err)
	}
	if err := l.Sync(); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("after Sync read %d entries, want 1", len(got))
	}
}

func TestReadLogTruncatedFinalLine(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append(testEntry(t, ModeChange{Mode: ModeOff})); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, LogFile), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"01K4M0A7Q8ZJ3N6R9T2V5X8B1D","at":"2026-09-07T2`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	got, err := ReadLog(dir)
	var tr Truncated
	if !errors.As(err, &tr) {
		t.Fatalf("want Truncated, got %v", err)
	}
	if tr.Line != 2 {
		t.Fatalf("Truncated.Line = %d, want 2", tr.Line)
	}
	if len(got) != 1 {
		t.Fatalf("read %d entries, want 1", len(got))
	}
}

func TestReadLogMalformedMiddleLine(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, LogFile), []byte("not json\n"+
		`{"id":"01K4M0A7Q8ZJ3N6R9T2V5X8B1D","at":"2026-09-07T20:30:00Z","kind":"mode_change","mode":"off"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadLog(dir)
	var tr Truncated
	if err == nil || errors.As(err, &tr) {
		t.Fatalf("want a hard error, got %v", err)
	}
}

func TestReadLogMissingFile(t *testing.T) {
	got, err := ReadLog(t.TempDir())
	if err != nil || len(got) != 0 {
		t.Fatalf("missing file: got %d entries, err %v; want 0, nil", len(got), err)
	}
}

// TestLogCompactsInputWithNewlines: a raw newline inside a tool input (a before_tool handler
// answering with indented bytes, a provider pretty-printing partial_json) would split a JSONL
// line in two and cost ReadLog the whole session from there on. The write path compacts those
// bytes, and only those; the keys, their order and every string survive it.
func TestLogCompactsInputWithNewlines(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	indented := json.RawMessage("{\n  \"command\": \"go test ./...\",\n  \"note\": \"a\\nb\"\n}")
	const compacted = `{"command":"go test ./...","note":"a\nb"}`
	entries := []Entry{
		testEntry(t, PermissionDecision{ToolUseID: "toolu_01", Tool: "bash", Mode: ModeStrict,
			Matcher: Matcher{Tool: "bash"}, Decision: Allow, DecidedBy: ByHook, Scope: ScopeOnce,
			Reason: "hook", Input: indented}),
		testEntry(t, AssistantMessage{Model: ModelRef{"aperture", "m"}, Thinking: ThinkingOff,
			Content: []Block{ToolUseBlock("toolu_01", "bash", indented)}, StopReason: StopToolUse}),
		testEntry(t, TitleChange{Title: "still readable"}),
	}
	for _, e := range entries {
		if err := l.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, LogFile))
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(raw, []byte("\n")); n != len(entries) {
		t.Fatalf("log has %d lines for %d entries:\n%s", n, len(entries), raw)
	}
	got, err := ReadLog(dir)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if len(got) != len(entries) {
		t.Fatalf("read %d entries, want %d", len(got), len(entries))
	}
	pd, ok := got[0].Payload.(PermissionDecision)
	if !ok {
		t.Fatalf("entry 0 is %T", got[0].Payload)
	}
	if string(pd.Input) != compacted {
		t.Errorf("permission_decision input = %s, want %s", pd.Input, compacted)
	}
	am, ok := got[1].Payload.(AssistantMessage)
	if !ok {
		t.Fatalf("entry 1 is %T", got[1].Payload)
	}
	if string(am.Content[0].Input) != compacted {
		t.Errorf("tool_use input = %s, want %s", am.Content[0].Input, compacted)
	}
	if tc, ok := got[2].Payload.(TitleChange); !ok || tc.Title != "still readable" {
		t.Errorf("entry after the compacted ones = %#v", got[2].Payload)
	}
}
