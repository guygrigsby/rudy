// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/tui/keys"
	"github.com/guygrigsby/rudy/internal/tui/theme"
)

// The commands the client answers itself. A quit is the client's own business: no Action a
// registered command may return closes a client, and a server that could close one would
// not know whose, since a running rudy serve has several attached. ADR 0015 decision 3.
const (
	exitCommand = "exit"
	quitCommand = "quit"
	// scopedModelsCommand opens the set ctrl+p and ctrl+n cycle through. The cycle is the
	// client's own state, never the session's, so the command is the client's too
	// (ADR 0020).
	scopedModelsCommand = "scoped-models"
)

// clientCommands are those two as the menu lists them, under everything the server
// registered: they are the client's vocabulary, and they never reach a session log.
var clientCommands = []protocol.CommandInfo{
	{Name: scopedModelsCommand, Description: "Choose the models ctrl+p cycles through"},
	{Name: exitCommand, Description: "Close the client; a session on a running rudy serve keeps its place"},
	{Name: quitCommand, Description: "Close the client, the same as /exit"},
}

// localCommand reports whether the client answers name itself. It is read ahead of
// command.run, so a plugin registering either name finds it unreachable from this client.
func localCommand(name string) bool {
	switch name {
	case exitCommand, quitCommand, scopedModelsCommand:
		return true
	}
	return false
}

// runLocal runs one of the client's own commands. The editor has already been cleared by
// the submit that got here, the way it is for a command the server runs.
func (m *Model) runLocal(name, args string) tea.Cmd {
	switch name {
	case exitCommand, quitCommand:
		return tea.Quit
	case scopedModelsCommand:
		// An argument is the filter the picker opens under, so /scoped-models kimi lands
		// on the models worth choosing between rather than on the whole registry.
		return m.openScopePicker(strings.TrimSpace(args))
	}
	return nil
}

// shellPrefix opens a draft that is a shell command rather than a message.
const shellPrefix = "!"

// menuState is the slash menu's own state: the row the keyboard is on, the draft that
// selection was made under, and the draft an Esc dismissed the menu for. What is on screen
// is derived from the draft itself, so nothing here has to be kept in step with it.
type menuState struct {
	sel int
	// filter is the draft sel was chosen under. A draft that has moved on starts at the
	// top again, since the rows under it have changed.
	filter string
	// off is the draft the menu was dismissed for. Typing anything else opens it again,
	// which is what makes Esc a dismissal rather than a mode.
	off string
}

// slashWord is the command name a draft is typing, and whether a menu belongs on screen at
// all. The draft has to open with a slash and carry no whitespace yet: a space means the
// name is finished and what is being typed is an argument, which has no completion source.
func slashWord(draft string) (string, bool) {
	if !strings.HasPrefix(draft, "/") {
		return "", false
	}
	word := strings.TrimPrefix(draft, "/")
	if strings.ContainsFunc(word, unicode.IsSpace) {
		return "", false
	}
	return word, true
}

// menuRows are the commands the draft matches, in the order the menu lists them: what
// command.list answered, in registration order, then the client's own. Nothing when the
// draft is not a command name, when it was dismissed, or when nothing matches.
func (m *Model) menuRows() []protocol.CommandInfo {
	draft := m.ed.Text()
	word, ok := slashWord(draft)
	if !ok || m.menu.off == draft {
		return nil
	}
	want := strings.ToLower(word)
	var out []protocol.CommandInfo
	for _, c := range m.commands {
		if strings.HasPrefix(strings.ToLower(c.Name), want) {
			out = append(out, c)
		}
	}
	return out
}

// menuSel is the row the keyboard is on, clamped into the rows the draft matches now. A
// selection made under an older draft starts over at the top: the rows moved under it.
func (m *Model) menuSel(rows []protocol.CommandInfo) int {
	if len(rows) == 0 {
		return 0
	}
	if m.menu.filter != m.ed.Text() {
		return 0
	}
	return min(max(m.menu.sel, 0), len(rows)-1)
}

// setMenuSel puts the keyboard on a row and remembers the draft it belongs to.
func (m *Model) setMenuSel(i int) {
	m.menu.sel, m.menu.filter = i, m.ed.Text()
}

// menuKey routes one key while the menu stands, and reports whether it took it. It takes
// only the keys that mean something to a list; everything else reaches the editor, which
// is what keeps the draft being typed while the menu is up. The menu is a completion, not
// a picker: it never owns the keyboard.
func (m *Model) menuKey(k tea.KeyPressMsg) bool {
	rows := m.menuRows()
	if len(rows) == 0 {
		return false
	}
	word, _ := slashWord(m.ed.Text())
	sel := m.menuSel(rows)
	for _, a := range m.keys.Match(tea.Key(k)) {
		switch a {
		case keys.TUISelectUp:
			m.setMenuSel(max(sel-1, 0))
			return true
		case keys.TUISelectDown:
			m.setMenuSel(min(sel+1, len(rows)-1))
			return true
		case keys.TUIInputSubmit:
			if rows[sel].Name == word {
				// A name that is already complete has nothing to complete, so Enter is the
				// submit it always was: typing a command out in full and pressing it once
				// still runs the command.
				return false
			}
			m.completeCommand(rows[sel].Name)
			return true
		case keys.TUIInputTab:
			m.completeCommand(rows[sel].Name)
			return true
		case keys.TUISelectCancel:
			// Dismissed for this draft only. Esc reaches the editor's mode again on the
			// next press, and typing another letter brings the menu back.
			m.menu = menuState{off: m.ed.Text()}
			return true
		}
	}
	return false
}

// completeCommand puts the name in the editor with the space an argument goes after. The
// space is also what closes the menu, since a draft past the name is no longer one.
func (m *Model) completeCommand(name string) {
	m.ed.SetText("/" + name + " ")
	m.menu = menuState{}
}

// menuLines is the menu as it draws, between a plugin's above_editor widgets and the
// editor itself: the matching names with what each does, the keyboard's row accented. Each
// line goes through the sanitizer and the width clamp a widget's line does, since a
// description is a plugin's own text.
func (m *Model) menuLines() []string {
	rows := m.menuRows()
	if len(rows) == 0 {
		return nil
	}
	h := m.pickerHeight(len(rows))
	sel := m.menuSel(rows)
	top := 0
	if sel >= h {
		top = sel - h + 1
	}
	out := make([]string, 0, h)
	for i := range h {
		j := top + i
		if j >= len(rows) {
			break
		}
		role, mark := theme.RoleMuted, unselectedMark
		if j == sel {
			role, mark = theme.RoleAccent, selectedMark
		}
		out = append(out, m.clamp(m.th.Style(role).Render(mark+spanText(join("/"+rows[j].Name, rows[j].Description)))))
	}
	return out
}
