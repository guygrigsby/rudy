// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Sync adds the keys a config file is missing and changes nothing else. ADR 0021.
//
// It is text, not a re-marshal: the file keeps its own order, its own spacing and every
// comment its owner wrote, and a key that is already there is left exactly as it is,
// whatever its value. A missing key is written under the table it belongs to, with the
// comment from the catalogue above it; a missing table is appended whole.
//
// A file that is not there yet is written as the example, which is every key with its
// default under the comment that says what it is for.
type SyncResult struct {
	// Path is the file that was read and written.
	Path string
	// Created is the file having not existed, in which case Added is every key.
	Created bool
	// Added are the keys written, in the order the catalogue lists them.
	Added []string
}

// Sync merges the missing keys into path. dry reports what it would do and writes nothing.
func Sync(path string, dry bool) (SyncResult, error) {
	res := SyncResult{Path: path}
	body, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		res.Created, res.Added = true, DocumentedKeys()
		if dry {
			return res, nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return res, fmt.Errorf("config sync: %w", err)
		}
		if err := os.WriteFile(path, []byte(Example()), 0o600); err != nil {
			return res, fmt.Errorf("config sync: %w", err)
		}
		return res, nil
	case err != nil:
		return res, fmt.Errorf("config sync: %w", err)
	}

	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	have := keysIn(lines)
	merged, added := merge(lines, have)
	res.Added = added
	if len(added) == 0 || dry {
		return res, nil
	}
	out := strings.Join(merged, "\n") + "\n"
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		return res, fmt.Errorf("config sync: %w", err)
	}
	return res, nil
}

// keysIn is every key the file already sets, as full dotted keys.
func keysIn(lines []string) map[string]bool {
	have := map[string]bool{}
	for _, e := range entries(lines) {
		if !e.table {
			have[strings.Join(e.path, ".")] = true
		}
	}
	return have
}

// entry is a line that names something: a table header, or a key the file sets. Both carry
// the line they are written on, which is what a lint finding points at.
type entry struct {
	line  int
	path  []string
	table bool
}

// entries is every table header and key in the file, in the order they are written.
// Comments and blank lines say nothing, and a value spread over several lines (an array) is
// skipped whole so its contents are never read as keys.
func entries(lines []string) []entry {
	var out []entry
	var table []string
	depth := 0
	for i, raw := range lines {
		line := strings.TrimSpace(stripComment(raw))
		if depth > 0 {
			depth += brackets(line)
			continue
		}
		switch {
		case line == "":
		case strings.HasPrefix(line, "["):
			end := strings.LastIndex(line, "]")
			if end <= 0 {
				continue
			}
			table = splitKey(strings.Trim(line[:end+1], "[]"))
			out = append(out, entry{line: i + 1, path: table, table: true})
		default:
			name, _, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key := append(append([]string{}, table...), splitKey(strings.TrimSpace(name))...)
			out = append(out, entry{line: i + 1, path: key})
			depth += brackets(line)
		}
	}
	return out
}

// splitKey is a dotted key as its segments, leaving a quoted one whole and unquoted: a
// [keys] entry writes an action id like "app.model.select", which is one name and not three.
func splitKey(s string) []string {
	var out []string
	var cur strings.Builder
	quote := rune(0)
	for _, r := range s {
		switch {
		case quote == 0 && (r == '"' || r == '\''):
			quote = r
		case quote == r:
			quote = 0
		case r == '.' && quote == 0:
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(out, strings.TrimSpace(cur.String()))
}

// merge writes the missing keys into the lines and answers the new file and what it added.
func merge(lines []string, have map[string]bool) ([]string, []string) {
	var added []string
	for _, s := range Sections {
		var missing []Doc
		for _, d := range s.Keys {
			if !have[d.Key] {
				missing = append(missing, d)
			}
		}
		if len(missing) == 0 {
			continue
		}
		for _, d := range missing {
			added = append(added, d.Key)
		}
		lines = insert(lines, s, missing)
	}
	return lines, added
}

// insert puts a section's missing keys where they belong: into the table if the file has
// one, at the end of the file as a new table if it does not, and above the first table
// header for the keys that live above every table.
func insert(lines []string, s Section, missing []Doc) []string {
	block := written(missing)
	at, found := tableEnd(lines, s.Table)
	if !found {
		// A table the file has never had: its own header and comment, at the end.
		var head []string
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			head = append(head, "")
		}
		for _, c := range s.Comment {
			head = append(head, comment(c))
		}
		if s.Table != "" {
			head = append(head, "["+s.Table+"]")
		}
		return append(lines, append(head, block...)...)
	}
	out := make([]string, 0, len(lines)+len(block))
	out = append(out, lines[:at]...)
	out = append(out, block...)
	return append(out, lines[at:]...)
}

// written is the lines one section's missing keys become: the catalogue's comment, an
// example when there is one, and the key with its default.
func written(missing []Doc) []string {
	defaults := Defaults()
	out := make([]string, 0, 3*len(missing))
	for _, d := range missing {
		out = append(out, comment(d.Comment))
		if d.Example != "" {
			out = append(out, comment("e.g. "+d.Example))
		}
		out = append(out, fmt.Sprintf("%s = %s", leaf(d.Key), Literal(defaults[d.Key])))
	}
	return out
}

// tableEnd is the line a table's keys end at, and whether the file has that table at all.
// The end is after its last key rather than at the next header, so a key lands under the
// table it belongs to and above whatever blank line or comment introduces the next one.
func tableEnd(lines []string, table string) (int, bool) {
	current := ""
	depth := 0
	last := -1
	found := false
	for i, raw := range lines {
		line := strings.TrimSpace(stripComment(raw))
		if depth > 0 {
			depth += brackets(line)
			if current == table {
				last = i
			}
			continue
		}
		if strings.HasPrefix(line, "[") {
			if end := strings.Index(line, "]"); end > 0 {
				current = strings.TrimSpace(line[1:end])
				if current == table {
					found, last = true, i
				}
			}
			continue
		}
		if line == "" {
			continue
		}
		if current == table {
			found, last = true, i
		}
		depth += brackets(line)
	}
	if !found {
		// The keys above every table are the file's own opening, and a file with a table
		// on its first line still has that place to put them.
		if table == "" {
			return 0, true
		}
		return 0, false
	}
	return last + 1, true
}

// comment is one comment line.
func comment(text string) string {
	if text == "" {
		return "#"
	}
	return "# " + text
}

// stripComment drops a trailing comment, leaving quoted hashes alone.
func stripComment(line string) string {
	inQuote := false
	for i, r := range line {
		switch r {
		case '"':
			inQuote = !inQuote
		case '#':
			if !inQuote {
				return line[:i]
			}
		}
	}
	return line
}

// brackets is how far a line opens or closes an array, for the values written over more
// than one line.
func brackets(line string) int {
	depth := 0
	inQuote := false
	for _, r := range line {
		switch r {
		case '"':
			inQuote = !inQuote
		case '[':
			if !inQuote {
				depth++
			}
		case ']':
			if !inQuote {
				depth--
			}
		}
	}
	return depth
}
