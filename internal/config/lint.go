// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// Lint answers the one question validate cannot: what does this file set that rudy never
// reads? viper drops a key it does not know without a word, so a table written one level
// too shallow, or a key spelled almost right, is configuration the operator wrote and the
// harness ignores. Every finding is a warning: the file still loads, and what it loads is
// what the finding describes.
//
// A file that is not there is nothing to say, the same nothing Load makes of it. A file
// that will not parse is the one error: a document TOML cannot read has no keys to reason
// about, every finding this returns is a warning, and the two are not the same news.
func Lint(path string) ([]Finding, error) {
	body, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("config lint: %w", err)
	}
	var doc map[string]any
	if err := toml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("config lint: %s:%d: will not parse: %w", path, parseErrorLine(err), err)
	}
	return lintEntries(entries(strings.Split(strings.TrimSuffix(string(body), "\n"), "\n"))), nil
}

// Finding is one thing a config file says that rudy does not read.
type Finding struct {
	// Line is where it is written, 1-based.
	Line int
	// Text is what to tell the operator, without the file name: the caller knows which
	// file it asked about and prints it the way its own output wants.
	Text string
}

func (f Finding) String() string { return fmt.Sprintf("line %d: %s", f.Line, f.Text) }

// lintEntries is Lint's body once the file is text: every entry the schema cannot resolve,
// with the one suggestion worth making about it.
func lintEntries(es []entry) []Finding {
	root := configSchema()
	var out []Finding
	// An unknown table is one mistake, not one per key under it: the header is where the
	// fix goes, and repeating it for every key buries the line worth reading.
	var skipping []string
	for i, e := range es {
		if skipping != nil {
			if under(e.path, skipping) {
				continue
			}
			skipping = nil
		}
		if root.resolve(e.path) != nil {
			continue
		}
		out = append(out, Finding{Line: e.line, Text: describe(root, es, i)})
		if e.table {
			skipping = e.path
		}
	}
	return out
}

// describe is what to say about the entry at i, which the schema could not resolve.
func describe(root *schema, es []entry, i int) string {
	e := es[i]
	name := strings.Join(e.path, ".")
	if e.table {
		if nest := nesting(root, es, i); nest != "" {
			return fmt.Sprintf("[%s] is not a table rudy reads; did you mean [%s]? TOML reads no indentation, so this is a table of its own and rudy ignores it", name, nest)
		}
		return fmt.Sprintf("[%s] is not a table rudy reads, so everything under it is ignored; rudy config example lists every table", name)
	}
	if near := nearest(root, e.path); near != "" {
		return fmt.Sprintf("%s is not a key rudy reads; did you mean %s?", name, near)
	}
	return fmt.Sprintf("%s is not a key rudy reads, so it does nothing; rudy config example lists every key", name)
}

// nesting is the table this one was meant to be written under: a table whose every key
// belongs to the schema of some table the operator names the children of. It is the
// mis-nested provider, in the general form, and it answers only when the parent's children
// are a closed set, since a table that takes anything (a plugin's own settings) would
// swallow every unknown table on earth and suggest it.
func nesting(root *schema, es []entry, i int) string {
	keys := directKeys(es, i)
	if len(keys) == 0 {
		return ""
	}
	leaf := es[i].path[len(es[i].path)-1]
	for _, parent := range sortedNames(root.fields) {
		open := root.fields[parent].open
		if open == nil || open.wild || len(open.fields) == 0 {
			continue
		}
		fits := true
		for _, k := range keys {
			if open.resolve(k) == nil {
				fits = false
				break
			}
		}
		if fits {
			return parent + "." + leaf
		}
	}
	return ""
}

// directKeys are the keys written under the table at i, as paths relative to it, up to the
// next table header.
func directKeys(es []entry, i int) [][]string {
	var out [][]string
	for _, e := range es[i+1:] {
		if e.table {
			break
		}
		if !under(e.path, es[i].path) {
			break
		}
		out = append(out, e.path[len(es[i].path):])
	}
	return out
}

// nearest is the key the operator probably meant: the closest name at the same level, when
// it is close enough that a typo is the likelier story than a key from somewhere else.
func nearest(root *schema, path []string) string {
	parent := root.resolve(path[:len(path)-1])
	if parent == nil || len(parent.fields) == 0 {
		return ""
	}
	want := strings.ToLower(path[len(path)-1])
	best, bestAt := "", 0
	for _, name := range sortedNames(parent.fields) {
		d := distance(want, name)
		if best == "" || d < bestAt {
			best, bestAt = name, d
		}
	}
	// Two edits on a short name is a different key, not a slip of the fingers.
	if bestAt > 2 || bestAt >= len(want) {
		return ""
	}
	return strings.Join(append(append([]string{}, path[:len(path)-1]...), best), ".")
}

// distance is Levenshtein, for names short enough that the full table costs nothing.
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range b {
		prev[j+1] = j + 1
	}
	for i := range a {
		cur[0] = i + 1
		for j := range b {
			cost := 1
			if a[i] == b[j] {
				cost = 0
			}
			cur[j+1] = min(prev[j]+cost, min(prev[j+1]+1, cur[j]+1))
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// under reports whether path lies at or below prefix.
func under(path, prefix []string) bool {
	if len(path) < len(prefix) {
		return false
	}
	for i, p := range prefix {
		if !strings.EqualFold(path[i], p) {
			return false
		}
	}
	return true
}

// parseErrorLine is the line a TOML parse error happened on, or 0 when the error does not
// carry one.
func parseErrorLine(err error) int {
	if de, ok := errors.AsType[*toml.DecodeError](err); ok {
		row, _ := de.Position()
		return row
	}
	return 0
}

// schema is the shape rudy reads, derived from Config itself: every key the decoder can
// land a value in, and no second list to keep in step with it. A struct field is a key or a
// table; a map is a table whose children the operator names, and whose values are a closed
// schema again unless they are themselves open-ended.
type schema struct {
	// fields are the names written in the file, lowercased, because viper lowercases every
	// key it reads and Max_Tokens really does set max_tokens.
	fields map[string]*schema
	// open is the schema of a child the operator names, nil when the children are fixed.
	open *schema
	// wild is a value rudy takes as it comes (a plugin's own settings): anything below,
	// at any depth.
	wild bool
}

// resolve is the schema at path, or nil when rudy reads nothing there.
func (s *schema) resolve(path []string) *schema {
	cur := s
	for _, seg := range path {
		switch {
		case cur.wild:
			return cur
		case cur.fields[strings.ToLower(seg)] != nil:
			cur = cur.fields[strings.ToLower(seg)]
		case cur.open != nil:
			cur = cur.open
		default:
			return nil
		}
	}
	return cur
}

// configSchema is Config's own shape. Derived rather than written down: the rule is one
// sentence, so a field added to Config is a key the linter knows without anybody
// remembering to say so twice.
func configSchema() *schema { return schemaOf(reflect.TypeFor[Config]()) }

func schemaOf(t reflect.Type) *schema {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		s := &schema{fields: map[string]*schema{}}
		for f := range t.Fields() {
			if name := fieldName(f); name != "" {
				s.fields[strings.ToLower(name)] = schemaOf(f.Type)
			}
		}
		return s
	case reflect.Map:
		return &schema{open: schemaOf(t.Elem())}
	case reflect.Interface:
		return &schema{wild: true}
	default:
		return &schema{}
	}
}

// fieldName is the name a field carries in the file: its mapstructure tag, or its toml tag
// for the two tables Load fills by hand from the raw document. A field with neither is not
// written in config.toml at all.
func fieldName(f reflect.StructField) string {
	for _, tag := range []string{"mapstructure", "toml"} {
		name, _, _ := strings.Cut(f.Tag.Get(tag), ",")
		if name != "" && name != "-" {
			return name
		}
	}
	return ""
}

// sortedNames keeps every suggestion the same from one run to the next, since a map's order
// is not.
func sortedNames(m map[string]*schema) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
