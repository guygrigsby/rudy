// SPDX-License-Identifier: AGPL-3.0-or-later

package keys

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// modifierName pairs pi's grammar name for a modifier with the tea.KeyMod flag it sets.
// The same slice drives both directions: Parse looks a segment's flag up in it, and
// Format walks it in order to emit the canonical prefix (ctrl, shift, alt, super), so the
// grammar and its rendering can never drift apart.
type modifierName struct {
	name string
	mod  tea.KeyMod
}

var modifierNames = []modifierName{
	{"ctrl", tea.ModCtrl},
	{"shift", tea.ModShift},
	{"alt", tea.ModAlt},
	{"super", tea.ModSuper},
}

// namedKey pairs pi's name for a non-printable key with the tea.Key code it parses to.
// name is also the string Format renders for that code; aliases are extra spellings Parse
// accepts (escape/esc, enter/return) that Format never produces, keeping Format
// deterministic while Parse stays permissive.
type namedKey struct {
	name    string
	code    rune
	aliases []string
}

var namedKeys = []namedKey{
	{name: "escape", code: tea.KeyEscape, aliases: []string{"esc"}},
	{name: "enter", code: tea.KeyEnter, aliases: []string{"return"}},
	{name: "tab", code: tea.KeyTab},
	{name: "space", code: tea.KeySpace},
	{name: "backspace", code: tea.KeyBackspace},
	{name: "delete", code: tea.KeyDelete},
	{name: "insert", code: tea.KeyInsert},
	{name: "home", code: tea.KeyHome},
	{name: "end", code: tea.KeyEnd},
	{name: "pageUp", code: tea.KeyPgUp},
	{name: "pageDown", code: tea.KeyPgDown},
	{name: "up", code: tea.KeyUp},
	{name: "down", code: tea.KeyDown},
	{name: "left", code: tea.KeyLeft},
	{name: "right", code: tea.KeyRight},
	{name: "f1", code: tea.KeyF1},
	{name: "f2", code: tea.KeyF2},
	{name: "f3", code: tea.KeyF3},
	{name: "f4", code: tea.KeyF4},
	{name: "f5", code: tea.KeyF5},
	{name: "f6", code: tea.KeyF6},
	{name: "f7", code: tea.KeyF7},
	{name: "f8", code: tea.KeyF8},
	{name: "f9", code: tea.KeyF9},
	{name: "f10", code: tea.KeyF10},
	{name: "f11", code: tea.KeyF11},
	{name: "f12", code: tea.KeyF12},
}

// modByName and codeByName index modifierNames and namedKeys (aliases included) for
// Parse; nameByCode indexes namedKeys by code, canonical names only, for Format.
var (
	modByName  = map[string]tea.KeyMod{}
	codeByName = map[string]rune{}
	nameByCode = map[rune]string{}
)

func init() {
	for _, m := range modifierNames {
		modByName[m.name] = m.mod
	}
	for _, k := range namedKeys {
		codeByName[k.name] = k.code
		nameByCode[k.code] = k.name
		for _, alias := range k.aliases {
			codeByName[alias] = k.code
		}
	}
}

// Parse reads one key string in pi's "modifier+modifier+key" grammar: zero or more of
// ctrl, shift, alt, super, then a key name (namedKeys) or a single printable character,
// joined by "+". A single printable character sets Text to the character as given
// (lowercase letters stay lowercase; shift+l is ModShift with Code 'l', not an uppercase
// Code) and Code to its rune; a named key sets only Code. An empty string, an empty
// modifier or key segment, an unknown modifier, or a key name Parse does not recognize
// (and that is not itself a single character) is an error naming the offending string.
func Parse(s string) (tea.Key, error) {
	parts := strings.Split(s, "+")
	keyPart := parts[len(parts)-1]
	if keyPart == "" {
		return tea.Key{}, fmt.Errorf("keys: %q: missing key name", s)
	}
	var mod tea.KeyMod
	for _, seg := range parts[:len(parts)-1] {
		flag, ok := modByName[seg]
		if !ok {
			return tea.Key{}, fmt.Errorf("keys: %q: unknown modifier %q", s, seg)
		}
		mod |= flag
	}
	if code, ok := codeByName[keyPart]; ok {
		return tea.Key{Mod: mod, Code: code}, nil
	}
	if r := []rune(keyPart); len(r) == 1 {
		return tea.Key{Text: keyPart, Mod: mod, Code: r[0]}, nil
	}
	return tea.Key{}, fmt.Errorf("keys: %q: unknown key %q", s, keyPart)
}

// Format renders k in pi's grammar, the inverse of Parse for any k Parse produced:
// modifiers in the fixed order ctrl, shift, alt, super, then the canonical name for k.Code
// (namedKeys) or, absent one, the code's rune itself.
func Format(k tea.Key) string {
	var b strings.Builder
	for _, m := range modifierNames {
		if k.Mod&m.mod != 0 {
			b.WriteString(m.name)
			b.WriteByte('+')
		}
	}
	if name, ok := nameByCode[k.Code]; ok {
		b.WriteString(name)
	} else {
		b.WriteRune(k.Code)
	}
	return b.String()
}
