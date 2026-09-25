// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"fmt"
	"strings"
)

// themeRoleDocs is the fifteen keys of a theme file in the order
// DefaultThemeExample writes them, each with the sentence above it that says
// what it paints. The values live in themeDefaults; this table lives beside it
// for the same reason Sections does: a fact that lives twice has a guard
// (ADR 0021), and drift_test.go fails the pair the day one changes without the
// other.
var themeRoleDocs = []Doc{
	{Key: "accent", Comment: "Headings, links and the picker: the colour that means here."},
	{Key: "text", Comment: "The transcript's base colour, and any span with no role of its own."},
	{Key: "muted", Comment: "Chrome: the frame of a row rather than the news in it."},
	{Key: "user", Comment: "What you typed. A role name copies that role's colour."},
	{Key: "assistant", Comment: "What the agent is saying, streaming or done: the brightest thing on the screen."},
	{Key: "tool", Comment: "What the agent did: the folded tool call and the row it becomes."},
	{Key: "success", Comment: "A check that passed."},
	{Key: "error", Comment: "A failure, in a notice or a row."},
	{Key: "warning", Comment: "Something worth a look, short of an error."},
	{Key: "diff_add", Comment: "An added line in a diff."},
	{Key: "diff_del", Comment: "A deleted line in a diff."},
	{Key: "shell", Comment: "The composer while a draft is a shell command, and the row that command becomes."},
	{Key: "status", Comment: "The status line's cells."},
	{Key: "spinner", Comment: "The spinner in the status line's turn cell: the one cell that moves."},
	{Key: "code", Comment: "Syntax highlighting: a chroma style name, not a colour.", Example: `"chroma:dracula"`},
}

// DefaultThemeExample is the themes/default.toml `rudy config sync` writes
// when none is there: every role at its built-in default, generated from
// themeDefaults and themeRoleDocs so the file cannot drift from the values it
// spells out. Loading treats the name "default" as the built-in, so this file
// is the palette an operator starts from and names their own, not a config the
// harness reads back (ADR 0042).
func DefaultThemeExample() string {
	var b strings.Builder
	writeComment(&b, "The built-in theme, written out so every role is here to change. A value")
	writeComment(&b, `is a hex colour or another role's name; code takes a "chroma:<style>" name.`)
	writeComment(&b, "Loading always treats the name \"default\" as the built-in: copy this file to")
	writeComment(&b, "themes/<name>.toml, edit it, and set ui.theme.name to that name.")
	b.WriteString("\n")
	for _, d := range themeRoleDocs {
		writeComment(&b, d.Comment)
		if d.Example != "" {
			writeComment(&b, "e.g. "+d.Example)
		}
		fmt.Fprintf(&b, "%s = %s\n", d.Key, Literal(themeDefaults[d.Key]))
	}
	return b.String()
}
