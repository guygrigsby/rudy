// Package icons is the client's glyph set: one name per thing the screen labels, three
// sets to draw them from, and a config override per name.
//
// rudy ships no font and copies no artwork. A set is a table of codepoints, and what
// arrives on screen is whatever the reader's own terminal font has at them. The nerd set
// uses the ranges Nerd Fonts patches in from Powerline (MIT) and Font Awesome 4 (SIL OFL
// 1.1 for the font, MIT for the code); the unicode set is ordinary Unicode with no font
// requirement at all; the ascii set is plain ASCII. Nothing here is taken from a copyleft
// or share-alike source. ADR 0018.
package icons

import (
	"fmt"
	"slices"
	"strings"
)

// Name is one thing the client labels. The set is closed: a config override naming
// anything else is an error rather than a silently ignored key, the way an unknown theme
// role or key action is.
type Name string

const (
	Branch  Name = "branch"
	Model   Name = "model"
	Context Name = "context"
	Tool    Name = "tool"
	Bash    Name = "bash"
	Edit    Name = "edit"
	Read    Name = "read"
	Write   Name = "write"
	Grep    Name = "grep"
	Glob    Name = "glob"
	Fetch   Name = "fetch"
	Agent   Name = "agent"
	Info    Name = "info"
	Warn    Name = "warn"
	Error   Name = "error"
)

// Names is every icon, in the order the contracts table lists them.
var Names = []Name{
	Branch, Model, Context,
	Tool, Bash, Edit, Read, Write, Grep, Glob, Fetch, Agent,
	Info, Warn, Error,
}

// The three sets a config may name.
const (
	SetNerd    = "nerd"
	SetUnicode = "unicode"
	SetASCII   = "ascii"
)

// nerd is the default: Powerline's branch glyph and Font Awesome 4's codepoints, which is
// what a Nerd Font patches into the private use area. A terminal whose font is not patched
// draws a box for each, which is what ui.icons.set = "unicode" is for.
var nerd = Set{
	Branch:  "", // powerline branch
	Model:   "", // fa cube
	Context: "", // fa tachometer
	Tool:    "", // fa cog
	Bash:    "", // fa terminal
	Edit:    "", // fa pencil
	Read:    "", // fa file-text-o
	Write:   "", // fa floppy-o
	Grep:    "", // fa search
	Glob:    "", // fa folder-o
	Fetch:   "", // fa globe
	Agent:   "", // fa users
	Info:    "", // fa info-circle
	Warn:    "", // fa exclamation-triangle
	Error:   "", // fa times-circle
}

// unicode needs no patched font: every glyph is an ordinary codepoint an everyday font
// has. It is the set to name when the nerd one draws boxes.
var unicode = Set{
	Branch:  "⎇",
	Model:   "◆",
	Context: "◔",
	Tool:    "▸",
	Bash:    "▸",
	Edit:    "✎",
	Read:    "▤",
	Write:   "▣",
	Grep:    "⌕",
	Glob:    "▢",
	Fetch:   "⌂",
	Agent:   "◇",
	Info:    "•",
	Warn:    "!",
	Error:   "✗",
}

// ascii is the last resort, and is what the client drew before it had icons at all.
var ascii = Set{
	Branch:  "on",
	Model:   "",
	Context: "",
	Tool:    ">",
	Bash:    "$",
	Edit:    "~",
	Read:    "<",
	Write:   ">",
	Grep:    "/",
	Glob:    "*",
	Fetch:   "@",
	Agent:   "&",
	Info:    "i",
	Warn:    "!",
	Error:   "x",
}

// Set is the glyph for each name. A name mapped to the empty string draws nothing and
// takes no space with it, which is how a config turns one icon off.
type Set map[Name]string

// sets are the three by name.
var sets = map[string]Set{SetNerd: nerd, SetUnicode: unicode, SetASCII: ascii}

// Load resolves ui.icons: the named set with the per-name overrides applied over it. An
// unknown set or an unknown override name is an error naming what it could not resolve,
// and the client does not open, the same as a bad theme role.
func Load(set string, overrides map[string]string) (Set, error) {
	if set == "" {
		set = SetNerd
	}
	base, ok := sets[set]
	if !ok {
		return nil, fmt.Errorf("config: ui.icons.set %q is not %s", set, strings.Join(setNames(), ", "))
	}
	out := make(Set, len(base))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		if k == "set" {
			continue
		}
		n := Name(k)
		if !slices.Contains(Names, n) {
			return nil, fmt.Errorf("config: ui.icons.%s is not an icon; the set is %s", k, strings.Join(nameStrings(), ", "))
		}
		out[n] = v
	}
	return out, nil
}

// Default is the set a caller with no config uses, for tests and for a client built
// without one.
func Default() Set {
	s, _ := Load(SetNerd, nil)
	return s
}

// Get is the glyph for a name, empty for one the set draws nothing for.
func (s Set) Get(n Name) string { return s[n] }

// Label is the glyph, a space and the text, or just the text when the set draws nothing
// for that name. It is the one place an icon meets what it labels, so an icon turned off
// never leaves the gap where it would have been.
func (s Set) Label(n Name, text string) string {
	glyph := s[n]
	switch {
	case glyph == "":
		return text
	case text == "":
		return glyph
	}
	return glyph + " " + text
}

// ForTool is the icon a tool row opens with: the tool's own when the set names it, the
// generic one otherwise, so a plugin's tool still gets a row that lines up.
func (s Set) ForTool(tool string) string {
	if g, ok := s[Name(tool)]; ok && g != "" {
		return g
	}
	return s[Tool]
}

func setNames() []string {
	out := make([]string, 0, len(sets))
	for k := range sets {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func nameStrings() []string {
	out := make([]string, 0, len(Names))
	for _, n := range Names {
		out = append(out, string(n))
	}
	return out
}
