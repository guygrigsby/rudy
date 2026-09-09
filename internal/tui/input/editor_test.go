package input

import (
	"reflect"
	"strings"
	"testing"

	keybind "charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/guygrigsby/rudy/internal/tui/keys"
	"github.com/guygrigsby/rudy/internal/tui/theme"
)

// key builds the key press pi's grammar names, the same way a [keys] table entry is
// read, so a test presses exactly what a binding binds.
func key(s string) tea.KeyPressMsg {
	k, err := keys.Parse(s)
	if err != nil {
		panic("input: test key " + s + ": " + err.Error())
	}
	return tea.KeyPressMsg(k)
}

// typeText presses one printable key per rune, the shape a terminal reports typing: no
// modifiers, Text carrying the character.
func typeText(e *Editor, s string) {
	for _, r := range s {
		e.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func TestVimModesAndLabel(t *testing.T) {
	e := New(true, theme.Default(), 80)
	if e.Mode() != ModeInsert {
		t.Fatalf("starts in insert: %s", e.Mode())
	}
	e.Update(key("escape"))
	if e.Mode() != ModeNormal {
		t.Error("escape to normal")
	}
	e.Update(key("i"))
	if e.Mode() != ModeInsert {
		t.Error("i to insert")
	}
	typeText(e, "hello")
	e.Update(key("escape"))
	e.Update(key("v"))
	if e.Mode() != ModeVisual {
		t.Error("v to visual")
	}
	e.Update(key("escape"))
	if e.Mode() != ModeNormal || e.Text() != "hello" {
		t.Errorf("visual escape: %s %q", e.Mode(), e.Text())
	}
	off := New(false, theme.Default(), 80)
	if off.Mode() != ModeDisabled {
		t.Error("vim off is disabled")
	}
	off.Update(key("escape"))
	if off.Mode() != ModeDisabled {
		t.Error("escape with vim off changes nothing")
	}
}

func TestEmacsEditsInInsert(t *testing.T) {
	e := New(true, theme.Default(), 80)
	typeText(e, "hello world")
	e.Update(key("ctrl+a"))
	e.Update(key("ctrl+k"))
	if e.Text() != "" {
		t.Errorf("ctrl+a ctrl+k: %q", e.Text())
	}
	typeText(e, "one two")
	e.Update(key("ctrl+w"))
	if e.Text() != "one " {
		t.Errorf("ctrl+w: %q", e.Text())
	}
}

// TestPiEditorKeysActThroughTheTextarea presses every key pi binds to a tui.editor.*
// action and checks the textarea carried the action out. A binding still spelled in
// pi's grammar rather than bubbletea's ("pageUp" for "pgup", a ctrl combo carrying its
// character in Text) matches nothing, and the key silently does nothing at all, so the
// assertion has to be on the effect rather than on the key map.
func TestPiEditorKeysActThroughTheTextarea(t *testing.T) {
	const base = "one two"
	cases := []struct {
		action keys.Action
		atEnd  bool   // press with the cursor at the end of the line, else at its start
		want   string // the buffer after the key press and typing one "X"
	}{
		{keys.TUIEditorCursorLineStart, true, "Xone two"},
		{keys.TUIEditorCursorLineEnd, false, "one twoX"},
		{keys.TUIEditorCursorLeft, true, "one twXo"},
		{keys.TUIEditorCursorRight, false, "oXne two"},
		{keys.TUIEditorCursorWordLeft, true, "one Xtwo"},
		{keys.TUIEditorCursorWordRight, false, "oneX two"},
		{keys.TUIEditorDeleteCharBackward, true, "one twX"},
		{keys.TUIEditorDeleteCharForward, false, "Xne two"},
		{keys.TUIEditorDeleteWordBackward, true, "one X"},
		{keys.TUIEditorDeleteWordForward, false, "X two"},
		{keys.TUIEditorDeleteToLineStart, true, "X"},
		{keys.TUIEditorDeleteToLineEnd, false, "X"},
		{keys.TUIInputNewLine, true, "one two\nX"},
	}
	for _, c := range cases {
		if len(keys.Defaults[c.action]) == 0 {
			t.Errorf("%s has no pi default to press", c.action)
		}
		for _, s := range keys.Defaults[c.action] {
			e := New(false, theme.Default(), 80)
			e.SetText(base)
			if c.atEnd {
				e.ta.MoveToEnd()
			} else {
				e.ta.MoveToBegin()
			}
			e.Update(key(s))
			typeText(e, "X")
			if got := e.Text(); got != c.want {
				t.Errorf("%s %q: %q, want %q", c.action, s, got, c.want)
			}
		}
	}
}

// TestPiVerticalMotionsActThroughTheTextarea covers the four actions whose effect is a
// cursor row rather than a change to the buffer.
func TestPiVerticalMotionsActThroughTheTextarea(t *testing.T) {
	tall := strings.TrimSuffix(strings.Repeat("line\n", 30), "\n")
	cases := []struct {
		action keys.Action
		up     bool
	}{
		{keys.TUIEditorCursorUp, true},
		{keys.TUIEditorCursorDown, false},
		{keys.TUIEditorPageUp, true},
		{keys.TUIEditorPageDown, false},
	}
	for _, c := range cases {
		for _, s := range keys.Defaults[c.action] {
			e := New(false, theme.Default(), 80)
			e.SetText(tall)
			e.ta.SetCursorColumn(0)
			for range 15 {
				e.ta.CursorUp()
			}
			before := e.ta.Line()
			e.Update(key(s))
			after := e.ta.Line()
			if c.up && after >= before {
				t.Errorf("%s %q: row %d to %d, wanted up", c.action, s, before, after)
			}
			if !c.up && after <= before {
				t.Errorf("%s %q: row %d to %d, wanted down", c.action, s, before, after)
			}
		}
	}
}

// TestUnboundActionsLeaveTheKeyAlone covers the pi actions the editor deliberately does
// not hand to the textarea: undo, which the textarea cannot do, and the app keys pi
// binds over one of bubbles' own defaults.
func TestUnboundActionsLeaveTheKeyAlone(t *testing.T) {
	untouched := []string{"ctrl+-", "ctrl+g", "ctrl+t", "ctrl+p", "ctrl+n"}
	for _, s := range untouched {
		e := New(false, theme.Default(), 80)
		e.SetText("one two")
		e.ta.MoveToEnd()
		e.Update(key(s))
		if got := e.Text(); got != "one two" {
			t.Errorf("%q changed the buffer: %q", s, got)
		}
		if e.ta.HasSelection() {
			t.Errorf("%q selected", s)
		}
	}
}

func TestQueue(t *testing.T) {
	e := New(true, theme.Default(), 80)
	typeText(e, "first")
	e.Enqueue()
	typeText(e, "second")
	e.Enqueue()
	if !e.Empty() || !reflect.DeepEqual(e.Queue(), []string{"first", "second"}) {
		t.Errorf("queue %v empty %v", e.Queue(), e.Empty())
	}
	head, ok := e.Dequeue()
	if !ok || head != "first" || len(e.Queue()) != 1 {
		t.Errorf("dequeue %q %v", head, e.Queue())
	}
	typeText(e, "draft")
	e.Restore()
	if e.Text() != "second\n\ndraft" || len(e.Queue()) != 0 {
		t.Errorf("restore %q %v", e.Text(), e.Queue())
	}
}

func TestQueueEdges(t *testing.T) {
	e := New(true, theme.Default(), 80)
	e.Enqueue()
	if len(e.Queue()) != 0 {
		t.Errorf("an empty draft does not enqueue: %v", e.Queue())
	}
	if _, ok := e.Dequeue(); ok {
		t.Error("an empty queue does not dequeue")
	}
	typeText(e, "draft")
	e.Restore()
	if e.Text() != "draft" {
		t.Errorf("restore with an empty queue keeps the draft: %q", e.Text())
	}
	e.Clear()
	if !e.Empty() || e.Text() != "" {
		t.Errorf("clear: %q", e.Text())
	}
	typeText(e, "queued")
	e.Enqueue()
	e.Restore()
	if e.Text() != "queued" {
		t.Errorf("restore into an empty draft: %q", e.Text())
	}
}

func TestNewlineAndSubmitKeysAreNotConsumed(t *testing.T) {
	e := New(true, theme.Default(), 80)
	typeText(e, "one")
	e.Update(key("shift+enter"))
	typeText(e, "two")
	if e.Text() != "one\ntwo" {
		t.Errorf("shift+enter: %q", e.Text())
	}
	e.Update(key("ctrl+j"))
	typeText(e, "three")
	if e.Text() != "one\ntwo\nthree" {
		t.Errorf("ctrl+j: %q", e.Text())
	}
	before := e.Text()
	if cmd := e.Update(key("enter")); cmd != nil {
		t.Error("enter is the app's, so it runs no command")
	}
	if e.Text() != before {
		t.Errorf("enter inserted: %q", e.Text())
	}
	// Submitting stays the app's whatever the key map says. A [keys] table is free to
	// give tui.input.newLine the enter key; the editor still never forwards it, so the
	// textarea cannot insert a newline the app has already read as a submit.
	e.ta.KeyMap.InsertNewline = keybind.NewBinding(keybind.WithKeys("enter"))
	if cmd := e.Update(key("enter")); cmd != nil {
		t.Error("enter runs no command even when a binding claims it")
	}
	if e.Text() != before {
		t.Errorf("enter reached the textarea: %q", e.Text())
	}
}

func TestPromptAndHeight(t *testing.T) {
	e := New(true, theme.Default(), 80)
	if view := e.View(); !strings.Contains(view, "\u2503") {
		t.Errorf("the first line carries the bar: %q", view)
	}
	if got := viewHeight(e); got != 1 {
		t.Errorf("an empty editor is one line, got %d", got)
	}
	e.SetText("a\nb\nc")
	if got := viewHeight(e); got != 3 {
		t.Errorf("three lines of content are three lines tall, got %d", got)
	}
	if got := strings.Count(e.View(), "\u2503"); got != 1 {
		t.Errorf("only the first line carries the bar, got %d: %q", got, e.View())
	}
	e.SetText(strings.Repeat("x\n", 20) + "x")
	if got := viewHeight(e); got != maxHeight {
		t.Errorf("content grows to %d lines, got %d", maxHeight, got)
	}
	e.Clear()
	if got := viewHeight(e); got != 1 {
		t.Errorf("clearing shrinks back to one line, got %d", got)
	}
}

// viewHeight is the number of rendered rows e currently occupies.
func viewHeight(e *Editor) int {
	return len(strings.Split(strings.TrimRight(e.View(), "\n"), "\n"))
}
