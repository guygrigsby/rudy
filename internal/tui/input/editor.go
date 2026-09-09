// Package input is the composer: a bubbles v2 textarea carrying pi's tui.editor.*
// bindings (docs/adr/0013-the-tui-wave.md, decision 5), vimbubble v2's modal editing
// over the top of it, and the client-side queue a follow-up message waits in while the
// turn the user already sent is still running.
//
// The editor owns no policy beyond editing. It never decides that a message is sent:
// enter belongs to the app, so Update drops it rather than letting the textarea insert
// a newline for it, and Enqueue, Dequeue and Restore only move text between the draft
// and the queue.
package input

import (
	"slices"
	"strings"

	keybind "charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	vimbubble "github.com/guygrigsby/vimbubble/v2"

	"github.com/guygrigsby/rudy/internal/tui/keys"
	"github.com/guygrigsby/rudy/internal/tui/theme"
)

// Mode is the editor's modal state, vimbubble's mode named in rudy's own vocabulary so
// the status line and the config never see a vendor type. ModeDisabled is vim off, the
// state every key reaches the textarea in.
type Mode string

// The four modes. The strings are what a status widget renders and what a hook payload
// carries, so they are lower case and stable.
const (
	ModeDisabled Mode = "disabled"
	ModeInsert   Mode = "insert"
	ModeNormal   Mode = "normal"
	ModeVisual   Mode = "visual"
)

const (
	// promptFirst prefixes the first line of the buffer, promptRest every line after
	// it. Both must be the same width so the text stays in one column.
	promptFirst = " ┃ "
	promptRest  = "   "

	// maxHeight is how far the editor grows with its content before it scrolls
	// instead. The app may call SetHeight past this; nothing here does.
	maxHeight = 8

	// queueSeparator joins the queue back into the draft: a blank line between
	// messages, so Restore's result reads as the paragraphs it was typed as.
	queueSeparator = "\n\n"
)

// promptWidth is the gutter the textarea reserves for the prompt, taken from the
// prompt itself so the two cannot drift.
var promptWidth = lipgloss.Width(promptFirst)

// Editor is the composer. Zero value is not usable; call New.
type Editor struct {
	ta    textarea.Model
	vim   *vimbubble.Modal
	queue []string
}

// New builds the composer at width columns, painted from th. vim starts the editor in
// Insert (the design opens ready to type, not in Normal as vim itself does); vim false
// leaves vimbubble disabled, so every key reaches the textarea. The editor comes back
// focused: Focus is for the app's Init, which needs the cursor's blink command.
func New(vim bool, th theme.Theme, width int) *Editor {
	e := &Editor{ta: textarea.New()}
	e.ta.Placeholder = ""
	e.ta.ShowLineNumbers = false
	// MaxHeight is the textarea's content limit as well as its viewport height, and a
	// composer must accept more lines than it shows. fit clamps the viewport instead.
	e.ta.MaxHeight = 0
	e.ta.KeyMap = keyMap()
	e.ta.SetStyles(styles(th))
	e.ta.SetPromptFunc(promptWidth, prompt)
	e.ta.SetWidth(width)
	e.fit()
	_ = e.ta.Focus()
	e.vim = vimbubble.New(&e.ta)
	if vim {
		e.vim.SetEnabled(true)
		e.vim.SetMode(vimbubble.Insert)
	}
	return e
}

// prompt draws the bar on the buffer's first line and blank gutter on the rest.
func prompt(info textarea.PromptInfo) string {
	if info.LineNumber == 0 {
		return promptFirst
	}
	return promptRest
}

// styles paints the textarea from th. Foreground only: no role paints a ground, so the
// cursor's line and a visual-mode selection are marked by color, not by a block.
func styles(th theme.Theme) textarea.Styles {
	text := th.Style(theme.RoleText)
	muted := th.Style(theme.RoleMuted)
	accent := th.Style(theme.RoleAccent)
	focused := textarea.StyleState{
		Base:        lipgloss.NewStyle(),
		Text:        text,
		CursorLine:  text,
		EndOfBuffer: muted,
		Placeholder: muted,
		Prompt:      accent,
		Selection:   accent,
	}
	blurred := focused
	blurred.Text = muted
	blurred.CursorLine = muted
	return textarea.Styles{
		Focused: focused,
		Blurred: blurred,
		Cursor: textarea.CursorStyle{
			Color: th.Colors[theme.RoleAccent],
			Shape: tea.CursorBlock,
			Blink: true,
		},
	}
}

// keyMap is bubbles' default key map with every action pi names rebound to pi's keys.
// A pi binding replaces bubbles' default for that action outright rather than joining
// it, so a key pi gives to something else (ctrl+p to app.model.cycleForward, plus
// ctrl+n, ctrl+h, ctrl+m and enter itself) stops reaching the textarea at all.
//
// pi's tui.editor.undo has no field here: the textarea cannot undo, so that action is
// left for the app. Everything the textarea can do that pi does not name (paste, input
// begin and end, the case verbs, the selection motions) keeps bubbles' own default.
func keyMap() textarea.KeyMap {
	km := textarea.DefaultKeyMap()
	km.LinePrevious = binding(keys.TUIEditorCursorUp)
	km.LineNext = binding(keys.TUIEditorCursorDown)
	km.CharacterBackward = binding(keys.TUIEditorCursorLeft)
	km.CharacterForward = binding(keys.TUIEditorCursorRight)
	km.WordBackward = binding(keys.TUIEditorCursorWordLeft)
	km.WordForward = binding(keys.TUIEditorCursorWordRight)
	km.LineStart = binding(keys.TUIEditorCursorLineStart)
	km.LineEnd = binding(keys.TUIEditorCursorLineEnd)
	km.PageUp = binding(keys.TUIEditorPageUp)
	km.PageDown = binding(keys.TUIEditorPageDown)
	km.DeleteCharacterBackward = binding(keys.TUIEditorDeleteCharBackward)
	km.DeleteCharacterForward = binding(keys.TUIEditorDeleteCharForward)
	km.DeleteWordBackward = binding(keys.TUIEditorDeleteWordBackward)
	km.DeleteWordForward = binding(keys.TUIEditorDeleteWordForward)
	km.DeleteBeforeCursor = binding(keys.TUIEditorDeleteToLineStart)
	km.DeleteAfterCursor = binding(keys.TUIEditorDeleteToLineEnd)
	km.InsertNewline = binding(keys.TUIInputNewLine)
	// Four of bubbles' remaining defaults collide with a pi binding for something else,
	// and the switch in textarea.Update would let whichever it tests first win. pi's
	// binding is the one that stands, so bubbles' claim on the key goes: ctrl+home and
	// ctrl+end are pi's line motions, ctrl+g is app.editor.external and ctrl+t is
	// app.thinking.toggle.
	km.InputBegin = keybind.NewBinding(keybind.WithKeys("alt+<"))
	km.InputEnd = keybind.NewBinding(keybind.WithKeys("alt+>"))
	km.SelectAll = keybind.NewBinding()
	km.TransposeCharacterBackward = keybind.NewBinding()
	return km
}

// binding is a's pi default bindings as a keybind.Binding. keybind.Matches compares
// against tea.Key.String, so each key string is parsed in pi's grammar and re-spelled
// in bubbletea's: "pageUp" becomes "pgup", "escape" becomes "esc". keys.Defaults is a
// compile-time table every entry of which parses, so a failure here is a bug in that
// table, not bad input.
func binding(a keys.Action) keybind.Binding {
	spellings := make([]string, 0, len(keys.Defaults[a]))
	for _, s := range keys.Defaults[a] {
		k, err := keys.Parse(s)
		if err != nil {
			panic("input: keys.Defaults[" + string(a) + "] holds an unparsable key: " + err.Error())
		}
		spellings = append(spellings, tea.Key(terminal(k)).String())
	}
	return keybind.NewBinding(keybind.WithKeys(spellings...))
}

// terminal restates k the way a terminal reports it. tea.Key.String, which both
// keybind.Matches and vimbubble read a key through, returns Text when there is any and
// the keystroke otherwise, and a terminal only fills Text for a key that produces text,
// never for one carrying ctrl, alt or super. keys.Parse fills Text from the character
// in a binding string whatever its modifiers, so ctrl+a would otherwise read as a plain
// "a": it would miss the ctrl+a binding and type an "a" into the buffer.
func terminal(k tea.Key) tea.Key {
	if k.Mod&^tea.ModShift != 0 {
		k.Text = ""
	}
	return k
}

// Update routes msg. A key press goes through vim first and stops there if vim consumed
// it; an unmodified enter stops too, since submitting is the app's and the textarea
// would only insert a newline for it; everything else, key press or not, reaches the
// textarea.
func (e *Editor) Update(msg tea.Msg) tea.Cmd {
	press, isKey := msg.(tea.KeyPressMsg)
	if !isKey {
		return e.toTextarea(msg)
	}
	press = tea.KeyPressMsg(terminal(tea.Key(press)))
	if consumed, cmd := e.vim.Update(press); consumed {
		e.fit()
		return cmd
	}
	if press.Mod == 0 && press.Code == tea.KeyEnter {
		return nil
	}
	return e.toTextarea(press)
}

// toTextarea hands msg to the textarea and resizes to whatever it left in the buffer.
func (e *Editor) toTextarea(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	e.ta, cmd = e.ta.Update(msg)
	e.fit()
	return cmd
}

// fit sizes the editor to its content: one line empty, growing a line at a time to
// maxHeight, past which it scrolls. Called after everything that changes the buffer,
// including the vim verbs, which edit it behind the textarea's back.
func (e *Editor) fit() {
	e.ta.SetHeight(min(e.ta.LineCount(), maxHeight))
}

// View renders the composer. The mode label is the app's to place, from Mode.
func (e *Editor) View() string { return e.ta.View() }

// Mode is the editor's modal state.
func (e *Editor) Mode() Mode {
	switch e.vim.Mode() {
	case vimbubble.Normal:
		return ModeNormal
	case vimbubble.Insert:
		return ModeInsert
	case vimbubble.Visual:
		return ModeVisual
	}
	return ModeDisabled
}

// Text is the draft, the queue excluded.
func (e *Editor) Text() string { return e.ta.Value() }

// SetText replaces the draft, leaving the cursor at the end of it.
func (e *Editor) SetText(s string) {
	e.ta.SetValue(s)
	e.fit()
}

// Clear empties the draft. The queue is untouched.
func (e *Editor) Clear() {
	e.ta.Reset()
	e.fit()
}

// Empty reports whether the draft holds nothing worth sending. Whitespace alone counts
// as nothing, so a stray newline neither submits nor enqueues.
func (e *Editor) Empty() bool { return strings.TrimSpace(e.ta.Value()) == "" }

// Queue is the messages waiting behind the running turn, oldest first. The result is a
// copy: appending to it changes nothing here.
func (e *Editor) Queue() []string { return slices.Clone(e.queue) }

// Enqueue moves the draft to the back of the queue and clears it. An empty draft
// enqueues nothing.
func (e *Editor) Enqueue() {
	if e.Empty() {
		return
	}
	e.queue = append(e.queue, e.Text())
	e.Clear()
}

// Dequeue pops the oldest queued message, false when the queue is empty. The draft is
// untouched: the caller sends what it popped, it does not go back into the editor.
func (e *Editor) Dequeue() (string, bool) {
	if len(e.queue) == 0 {
		return "", false
	}
	head := e.queue[0]
	e.queue = slices.Delete(e.queue, 0, 1)
	return head, true
}

// Restore empties the queue back into the draft, oldest first and the draft last, one
// blank line between messages, so a queue the user changed their mind about comes back
// as editable text in the order it would have been sent.
func (e *Editor) Restore() {
	parts := e.queue
	e.queue = nil
	if !e.Empty() {
		parts = append(parts, e.Text())
	}
	e.SetText(strings.Join(parts, queueSeparator))
}

// SetWidth resizes the editor, prompt gutter included.
func (e *Editor) SetWidth(w int) { e.ta.SetWidth(w) }

// Focus focuses the textarea and returns the cursor's blink command.
func (e *Editor) Focus() tea.Cmd { return e.ta.Focus() }
