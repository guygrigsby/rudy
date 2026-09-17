// SPDX-License-Identifier: AGPL-3.0-or-later

package input

import (
	"reflect"
	"strings"
	"testing"

	keybind "charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"

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
	e := New(true, theme.Default(), 80, nil)
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
	off := New(false, theme.Default(), 80, nil)
	if off.Mode() != ModeDisabled {
		t.Error("vim off is disabled")
	}
	off.Update(key("escape"))
	if off.Mode() != ModeDisabled {
		t.Error("escape with vim off changes nothing")
	}
}

func TestEmacsEditsInInsert(t *testing.T) {
	e := New(true, theme.Default(), 80, nil)
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
			e := New(false, theme.Default(), 80, nil)
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
			e := New(false, theme.Default(), 80, nil)
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
		e := New(false, theme.Default(), 80, nil)
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
	e := New(true, theme.Default(), 80, nil)
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
	e := New(true, theme.Default(), 80, nil)
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
	e.Restore()
	if e.Text() != "" || len(e.Queue()) != 0 {
		t.Errorf("restore with nothing queued and nothing drafted: %q %v", e.Text(), e.Queue())
	}
	typeText(e, "queued")
	e.Enqueue()
	e.Restore()
	if e.Text() != "queued" {
		t.Errorf("restore into an empty draft: %q", e.Text())
	}
}

func TestNewlineAndSubmitKeysAreNotConsumed(t *testing.T) {
	e := New(true, theme.Default(), 80, nil)
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
	e := New(true, theme.Default(), 80, nil)
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

// mustTable resolves a [keys] table the test wrote itself.
func mustTable(t *testing.T, overrides map[string][]string) *keys.Table {
	t.Helper()
	tb, err := keys.New(overrides)
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

// TestOverriddenKeysReachTheTextarea is the whole point of taking a table rather than
// reading keys.Defaults: a [keys] table that moves an editor action moves the key the
// composer acts on, and frees the key it moved off.
func TestOverriddenKeysReachTheTextarea(t *testing.T) {
	tb := mustTable(t, map[string][]string{
		"tui.editor.deleteToLineEnd": {"ctrl+e"},
		"tui.editor.cursorLineEnd":   {"end"},
	})
	e := New(false, theme.Default(), 80, tb)
	e.SetText("one two")
	e.ta.MoveToBegin()
	e.Update(key("ctrl+e"))
	if e.Text() != "" {
		t.Errorf("ctrl+e is deleteToLineEnd now: %q", e.Text())
	}
	// ctrl+k is bound to nothing at all now, so it edits nothing: a ctrl combo carries
	// no text for the textarea's default branch to insert either.
	e.SetText("one two")
	e.ta.MoveToBegin()
	e.Update(key("ctrl+k"))
	if e.Text() != "one two" {
		t.Errorf("ctrl+k is unbound now: %q", e.Text())
	}
	// The default table still has it the other way round.
	d := New(false, theme.Default(), 80, nil)
	d.SetText("one two")
	d.ta.MoveToBegin()
	d.Update(key("ctrl+k"))
	if d.Text() != "" {
		t.Errorf("ctrl+k is deleteToLineEnd by default: %q", d.Text())
	}
}

// TestOverriddenShiftedLetterMatches covers the spelling keys.Normalize owns: a shifted
// letter is the one binding whose canonical form is neither what a [keys] table writes
// nor what every terminal reports.
func TestOverriddenShiftedLetterMatches(t *testing.T) {
	tb := mustTable(t, map[string][]string{"tui.editor.cursorLineEnd": {"shift+l"}})
	presses := map[string]tea.KeyPressMsg{
		"as a table spells it":    key("shift+l"),
		"as a kitty terminal is":  tea.KeyPressMsg{Code: 'l', Mod: tea.ModShift, ShiftedCode: 'L', Text: "L"},
		"as a legacy terminal is": tea.KeyPressMsg{Code: 'L', Text: "L"},
	}
	for name, press := range presses {
		e := New(false, theme.Default(), 80, tb)
		e.SetText("one two")
		e.ta.MoveToBegin()
		e.Update(press)
		typeText(e, "X")
		if got := e.Text(); got != "one twoX" {
			t.Errorf("%s: %q, want %q", name, got, "one twoX")
		}
	}
}

// TestHeightCountsWrappedRows: a line too long for the width takes as many rows as it
// wraps into, not the one row a logical-line count would give it.
func TestHeightCountsWrappedRows(t *testing.T) {
	e := New(true, theme.Default(), 40, nil)
	e.SetText(strings.Repeat("x", 100))
	if got := viewHeight(e); got != 3 {
		t.Errorf("100 columns of text at width 40: %d rows, want 3", got)
	}
	// Narrowing rewraps the same text into more rows, and widening back into fewer.
	e.SetWidth(20)
	if got := viewHeight(e); got != 6 {
		t.Errorf("the same text at width 20: %d rows, want 6", got)
	}
	e.SetWidth(120)
	if got := viewHeight(e); got != 1 {
		t.Errorf("the same text at width 120: %d rows, want 1", got)
	}
	// Wrapped rows are clamped the same way logical ones are.
	e.SetWidth(40)
	e.SetText(strings.Repeat("x", 1000))
	if got := viewHeight(e); got != maxHeight {
		t.Errorf("1000 columns at width 40: %d rows, want %d", got, maxHeight)
	}
	// A wide rune counts by its display width, not its rune count.
	e.SetText(strings.Repeat("漢", 40))
	if got := viewHeight(e); got != 3 {
		t.Errorf("40 double-width runes at width 40: %d rows, want 3", got)
	}
}

// TestSelectionIsReverseVideo: the selection has to read as a block, and reverse video
// is the one way to get one without choosing a background color.
func TestSelectionIsReverseVideo(t *testing.T) {
	s := styles(theme.Default())
	for name, state := range map[string]textarea.StyleState{"focused": s.Focused, "blurred": s.Blurred} {
		if !state.Selection.GetReverse() {
			t.Errorf("%s selection is not reverse video", name)
		}
		if _, ok := state.Selection.GetBackground().(lipgloss.NoColor); !ok {
			t.Errorf("%s selection paints a ground: %#v", name, state.Selection.GetBackground())
		}
		for role, style := range map[string]lipgloss.Style{
			"base": state.Base, "text": state.Text, "cursor line": state.CursorLine,
			"prompt": state.Prompt, "placeholder": state.Placeholder,
			"end of buffer": state.EndOfBuffer,
		} {
			if _, ok := style.GetBackground().(lipgloss.NoColor); !ok {
				t.Errorf("%s %s paints a ground: %#v", name, role, style.GetBackground())
			}
		}
	}
}
