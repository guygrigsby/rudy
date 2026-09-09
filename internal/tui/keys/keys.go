// Package keys resolves the [keys] table (docs/adr/0013-the-tui-wave.md, decision 5)
// against pi's action ids: a closed set of Action constants, pi's default bindings for
// each, and Table, which parses config.Config.Keys' overrides over those defaults and
// matches an incoming tea.Key against them.
package keys

import (
	"errors"
	"fmt"
	"unicode"

	tea "charm.land/bubbletea/v2"
)

// Action is one of the closed set of pi action ids rudy binds. Its string value is the
// id itself, as it appears in a [keys] table and in pi's own keybindings.
type Action string

// The closed set of pi action ids rudy binds, ADR 0013 decision 5. Every other id in a
// [keys] table is a config error at load, naming it.
const (
	AppInterrupt          Action = "app.interrupt"
	AppClear              Action = "app.clear"
	AppExit               Action = "app.exit"
	AppSuspend            Action = "app.suspend"
	AppThinkingCycle      Action = "app.thinking.cycle"
	AppThinkingToggle     Action = "app.thinking.toggle"
	AppModelSelect        Action = "app.model.select"
	AppModelCycleForward  Action = "app.model.cycleForward"
	AppModelCycleBackward Action = "app.model.cycleBackward"
	AppToolsExpand        Action = "app.tools.expand"
	AppMessageFollowUp    Action = "app.message.followUp"
	AppMessageDequeue     Action = "app.message.dequeue"
	AppSessionNew         Action = "app.session.new"
	AppSessionFork        Action = "app.session.fork"
	AppSessionResume      Action = "app.session.resume"
	AppEditorExternal     Action = "app.editor.external"
	TUIEditorCursorUp     Action = "tui.editor.cursorUp"
	TUIEditorCursorDown   Action = "tui.editor.cursorDown"
	TUIEditorCursorLeft   Action = "tui.editor.cursorLeft"
	TUIEditorCursorRight  Action = "tui.editor.cursorRight"

	TUIEditorCursorWordLeft     Action = "tui.editor.cursorWordLeft"
	TUIEditorCursorWordRight    Action = "tui.editor.cursorWordRight"
	TUIEditorCursorLineStart    Action = "tui.editor.cursorLineStart"
	TUIEditorCursorLineEnd      Action = "tui.editor.cursorLineEnd"
	TUIEditorPageUp             Action = "tui.editor.pageUp"
	TUIEditorPageDown           Action = "tui.editor.pageDown"
	TUIEditorDeleteCharBackward Action = "tui.editor.deleteCharBackward"
	TUIEditorDeleteCharForward  Action = "tui.editor.deleteCharForward"
	TUIEditorDeleteWordBackward Action = "tui.editor.deleteWordBackward"
	TUIEditorDeleteWordForward  Action = "tui.editor.deleteWordForward"
	TUIEditorDeleteToLineStart  Action = "tui.editor.deleteToLineStart"
	TUIEditorDeleteToLineEnd    Action = "tui.editor.deleteToLineEnd"
	TUIEditorUndo               Action = "tui.editor.undo"
	TUIInputNewLine             Action = "tui.input.newLine"
	TUIInputSubmit              Action = "tui.input.submit"
	TUIInputTab                 Action = "tui.input.tab"
	TUIInputCopy                Action = "tui.input.copy"
	TUISelectUp                 Action = "tui.select.up"
	TUISelectDown               Action = "tui.select.down"
	TUISelectPageUp             Action = "tui.select.pageUp"
	TUISelectPageDown           Action = "tui.select.pageDown"
	TUISelectConfirm            Action = "tui.select.confirm"
	TUISelectCancel             Action = "tui.select.cancel"
	TUIAltScreenPageUp          Action = "tui.altScreen.pageUp"
	TUIAltScreenPageDown        Action = "tui.altScreen.pageDown"
	TUIAltScreenHalfPageUp      Action = "tui.altScreen.halfPageUp"
	TUIAltScreenHalfPageDown    Action = "tui.altScreen.halfPageDown"
	TUIAltScreenLineUp          Action = "tui.altScreen.lineUp"
	TUIAltScreenLineDown        Action = "tui.altScreen.lineDown"
	TUIAltScreenTop             Action = "tui.altScreen.top"
	TUIAltScreenBottom          Action = "tui.altScreen.bottom"
)

// Actions lists the closed set in ADR 0013's order. New builds Table's match index by
// walking Actions in this order, so Match returns every action bound to a key in this
// same order.
var Actions = []Action{
	AppInterrupt, AppClear, AppExit, AppSuspend, AppThinkingCycle, AppThinkingToggle,
	AppModelSelect, AppModelCycleForward, AppModelCycleBackward, AppToolsExpand,
	AppMessageFollowUp, AppMessageDequeue, AppSessionNew, AppSessionFork, AppSessionResume,
	AppEditorExternal,
	TUIEditorCursorUp, TUIEditorCursorDown, TUIEditorCursorLeft, TUIEditorCursorRight,
	TUIEditorCursorWordLeft, TUIEditorCursorWordRight, TUIEditorCursorLineStart, TUIEditorCursorLineEnd,
	TUIEditorPageUp, TUIEditorPageDown, TUIEditorDeleteCharBackward, TUIEditorDeleteCharForward,
	TUIEditorDeleteWordBackward, TUIEditorDeleteWordForward, TUIEditorDeleteToLineStart, TUIEditorDeleteToLineEnd,
	TUIEditorUndo,
	TUIInputNewLine, TUIInputSubmit, TUIInputTab, TUIInputCopy,
	TUISelectUp, TUISelectDown, TUISelectPageUp, TUISelectPageDown, TUISelectConfirm, TUISelectCancel,
	TUIAltScreenPageUp, TUIAltScreenPageDown, TUIAltScreenHalfPageUp, TUIAltScreenHalfPageDown,
	TUIAltScreenLineUp, TUIAltScreenLineDown, TUIAltScreenTop, TUIAltScreenBottom,
}

// actionIDs indexes Actions by their string id, for New to validate a [keys] table's
// keys without a linear scan of Actions per entry.
var actionIDs = func() map[string]Action {
	m := make(map[string]Action, len(Actions))
	for _, a := range Actions {
		m[string(a)] = a
	}
	return m
}()

// Defaults are pi's own bindings for darwin and linux, one entry per Action (an unbound
// action's list is empty, never absent), taken from pi's core/keybindings.js and
// pi-tui's keybindings.js. Never mutated; New copies it before applying overrides.
var Defaults = map[Action][]string{
	AppInterrupt:          {"escape"},
	AppClear:              {"ctrl+c"},
	AppExit:               {"ctrl+d"},
	AppSuspend:            {"ctrl+z"},
	AppThinkingCycle:      {"shift+tab"},
	AppThinkingToggle:     {"ctrl+t"},
	AppModelSelect:        {"ctrl+l"},
	AppModelCycleForward:  {"ctrl+p"},
	AppModelCycleBackward: {"ctrl+shift+p"},
	AppToolsExpand:        {"ctrl+o"},
	AppMessageFollowUp:    {"alt+enter"},
	AppMessageDequeue:     {"alt+up"},
	AppSessionNew:         {},
	AppSessionFork:        {},
	AppSessionResume:      {},
	AppEditorExternal:     {"ctrl+g"},

	TUIEditorCursorUp:           {"up"},
	TUIEditorCursorDown:         {"down"},
	TUIEditorCursorLeft:         {"left", "ctrl+b"},
	TUIEditorCursorRight:        {"right", "ctrl+f"},
	TUIEditorCursorWordLeft:     {"alt+left", "ctrl+left", "alt+b"},
	TUIEditorCursorWordRight:    {"alt+right", "ctrl+right", "alt+f"},
	TUIEditorCursorLineStart:    {"home", "ctrl+home", "ctrl+a"},
	TUIEditorCursorLineEnd:      {"end", "ctrl+end", "ctrl+e"},
	TUIEditorPageUp:             {"pageUp", "ctrl+pageUp"},
	TUIEditorPageDown:           {"pageDown", "ctrl+pageDown"},
	TUIEditorDeleteCharBackward: {"backspace"},
	TUIEditorDeleteCharForward:  {"delete", "ctrl+d"},
	TUIEditorDeleteWordBackward: {"ctrl+w", "alt+backspace"},
	TUIEditorDeleteWordForward:  {"alt+d", "alt+delete"},
	TUIEditorDeleteToLineStart:  {"ctrl+u"},
	TUIEditorDeleteToLineEnd:    {"ctrl+k"},
	TUIEditorUndo:               {"ctrl+-"},

	TUIInputNewLine: {"shift+enter", "ctrl+j"},
	TUIInputSubmit:  {"enter"},
	TUIInputTab:     {"tab"},
	TUIInputCopy:    {"ctrl+c"},

	TUISelectUp:       {"up"},
	TUISelectDown:     {"down"},
	TUISelectPageUp:   {"pageUp"},
	TUISelectPageDown: {"pageDown"},
	TUISelectConfirm:  {"enter"},
	TUISelectCancel:   {"escape", "ctrl+c"},

	TUIAltScreenPageUp:       {"pageUp"},
	TUIAltScreenPageDown:     {"pageDown"},
	TUIAltScreenHalfPageUp:   {},
	TUIAltScreenHalfPageDown: {},
	TUIAltScreenLineUp:       {},
	TUIAltScreenLineDown:     {},
	TUIAltScreenTop:          {"home"},
	TUIAltScreenBottom:       {"end"},
}

// matchKey is the normalized form Table indexes bindings by: normalize collapses both a
// bound key (from Parse) and an incoming tea.Key (from the terminal) to this shape, so
// the same physical keystroke looks up the same entry regardless of which produced it.
type matchKey struct {
	mod  tea.KeyMod
	code rune
}

// isASCIILetter reports whether r is one Parse's "shift+<letter>" grammar covers.
func isASCIILetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// shiftedLetter reports the uppercase letter k represents as a shift+letter combo, if
// any. Three shapes all count: ModShift with a letter Code (pi's own grammar, and a
// kitty-protocol press, both report Code as the unshifted letter); ModShift with a
// letter ShiftedCode (a decoder that reports the shifted form there instead); and a
// plain uppercase Text with no modifier at all, which is how a terminal without the
// kitty protocol reports shift on a letter: by case alone.
func shiftedLetter(k tea.Key) (rune, bool) {
	if k.Mod&tea.ModShift != 0 {
		if isASCIILetter(k.Code) {
			return unicode.ToUpper(k.Code), true
		}
		if isASCIILetter(k.ShiftedCode) {
			return unicode.ToUpper(k.ShiftedCode), true
		}
	}
	if k.Mod == 0 {
		if r := []rune(k.Text); len(r) == 1 && r[0] >= 'A' && r[0] <= 'Z' {
			return r[0], true
		}
	}
	return 0, false
}

// normalize collapses k to the key Table looks bindings up by. Shift on a letter takes
// priority (see shiftedLetter) and always normalizes to the shifted form with the Shift
// bit stripped, so a shift+l binding and every shape a real shift+l press can take land
// on the same entry. Otherwise a printable key with no modifiers matches by its Text (a
// terminal that reports Code differently for the same character still matches); every
// other key matches by Mod and Code.
func normalize(k tea.Key) matchKey {
	if letter, ok := shiftedLetter(k); ok {
		return matchKey{mod: k.Mod &^ tea.ModShift, code: letter}
	}
	if k.Mod == 0 && k.Text != "" {
		return matchKey{code: []rune(k.Text)[0]}
	}
	return matchKey{mod: k.Mod, code: k.Code}
}

// Table is a resolved [keys] table: Defaults with a config's overrides applied, indexed
// for Match.
type Table struct {
	byKey map[matchKey][]Action
}

// New builds a Table from overrides, a [keys] table as config.Config.Keys holds it: a
// key present in overrides replaces that action's default list entirely (an empty list
// unbinds it); an action absent from overrides keeps its default. Every id in overrides
// must be one of Actions and every key string must Parse; New reports every violation it
// finds, not just the first, joined into one error naming each offending id or key.
func New(overrides map[string][]string) (*Table, error) {
	bindings := make(map[Action][]string, len(Defaults))
	for a, keys := range Defaults {
		cp := make([]string, len(keys))
		copy(cp, keys)
		bindings[a] = cp
	}
	var errs []error
	for id, keys := range overrides {
		a, ok := actionIDs[id]
		if !ok {
			errs = append(errs, fmt.Errorf("keys: unknown action %q", id))
			continue
		}
		bindings[a] = keys
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	byKey := make(map[matchKey][]Action)
	for _, a := range Actions {
		for _, s := range bindings[a] {
			k, err := Parse(s)
			if err != nil {
				errs = append(errs, fmt.Errorf("keys: %s: %w", a, err))
				continue
			}
			nk := normalize(k)
			byKey[nk] = append(byKey[nk], a)
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &Table{byKey: byKey}, nil
}

// Match returns every action k is bound to, in Actions order (so, for instance, a plain
// enter with both tui.input.submit and tui.select.confirm bound to it returns both). k
// is normalized the same way a bound key is, so a raw tea.Key straight from the terminal
// matches regardless of which of Mod, Code, ShiftedCode or Text it populated.
func (t *Table) Match(k tea.Key) []Action {
	return t.byKey[normalize(k)]
}
