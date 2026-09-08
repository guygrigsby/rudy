package session

import (
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
