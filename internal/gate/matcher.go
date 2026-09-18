// SPDX-License-Identifier: AGPL-3.0-or-later

package gate

import (
	"encoding/json"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/guygrigsby/rudy/internal/session"
)

// MatcherFor classifies a tool input into the key allowances are matched on.
// Only bash has a prefix: the first simple command's first two words.
func (g *Gate) MatcherFor(tool string, args json.RawMessage) session.Matcher {
	if tool != "bash" {
		return session.Matcher{Tool: tool}
	}
	cmd, ok := bashCommand(args)
	if !ok {
		return session.Matcher{Tool: tool}
	}
	calls, err := shellCalls(cmd)
	if err != nil || len(calls) == 0 {
		return session.Matcher{Tool: tool, Prefix: firstWords(strings.Fields(cmd), 2)}
	}
	return session.Matcher{Tool: tool, Prefix: firstWords(calls[0], 2)}
}

// Dangerous reports whether this call matches an entry of the dangerous set, and which
// entry it was. An entry takes one of three forms:
//
//	rm -rf        a bash command prefix, which is what every default is
//	web_fetch:    a tool, dangerous whatever its input
//	bash:git push a tool and, for bash, a command prefix
//
// For bash, every simple command in the input is checked, and so is every command a
// wrapper runs: the set is what forces a question in permissive mode, and an entry that
// `sh -c` walks around is an entry that does nothing (rudy-k0.34, rudy-y3d).
func (g *Gate) Dangerous(tool string, args json.RawMessage) (bool, string) {
	var calls [][]string
	if tool == "bash" {
		if cmd, ok := bashCommand(args); ok {
			calls = expand(shellCallsOrFields(cmd), 0)
		}
	}
	for _, d := range g.dangerous {
		entryTool, prefix, scoped := strings.Cut(d, ":")
		switch {
		case scoped && entryTool != tool:
			continue
		case scoped && prefix == "":
			// A tool with no prefix: every call of it is dangerous, which is the only
			// way to say "always ask" for a tool that is not bash.
			return true, d
		case tool != "bash":
			// Every other entry is a bash command prefix, and no other tool has one.
			continue
		}
		want := d
		if scoped {
			want = prefix
		}
		for _, call := range calls {
			for _, joined := range callForms(call) {
				if joined == want || strings.HasPrefix(joined, want+" ") {
					return true, d
				}
			}
		}
	}
	return false, ""
}

// callForms is the spellings of one call the set is read against: the words as written,
// and, when the command was named by path or escaped, the same call under the name the
// shell will actually run. unwrap already reads /bin/sh as the sh wrapper, and the entries
// have to see /bin/rm as rm for the same reason: a set that any path walks around is a set
// that does nothing. Both forms are kept, so an operator whose entry names a full path
// still matches the command written that way.
func callForms(call []string) []string {
	if len(call) == 0 {
		return nil
	}
	joined := strings.Join(call, " ")
	head := shellName(call[0])
	if head == call[0] {
		return []string{joined}
	}
	normalized := append([]string{head}, call[1:]...)
	return []string{joined, strings.Join(normalized, " ")}
}

// shellName is the command a word runs: its last path element, with a leading backslash
// dropped. \rm is how a shell asks for the binary rather than an alias, and it is still rm.
func shellName(word string) string {
	name := strings.TrimPrefix(word, `\`)
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// shellCallsOrFields is the parsed simple commands of src, or its words when it does not
// parse: an input the shell will refuse is still an input the set should be read against.
func shellCallsOrFields(src string) [][]string {
	calls, err := shellCalls(src)
	if err != nil || len(calls) == 0 {
		return [][]string{strings.Fields(src)}
	}
	return calls
}

// wrapperDepth bounds how far a wrapper inside a wrapper is followed. Three is past
// anything a person writes and short of anything a generated command can spend.
const wrapperDepth = 3

// shellFlagged are the commands that run a script given as one argument, after a flag that
// ends in c: sh -c, bash -lc, zsh -ic.
var shellFlagged = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true}

// prefixWrappers run whatever follows them, once their own flags and assignments are past.
// sudo and doas are here as wrappers as well as being dangerous entries in their own right:
// `env sudo rm -rf` has to match both the sudo entry and the rm one.
var prefixWrappers = map[string]bool{
	"env": true, "xargs": true, "nohup": true, "time": true, "exec": true, "command": true,
	"sudo": true, "doas": true, "nice": true, "ionice": true, "timeout": true, "stdbuf": true,
	"setsid": true, "watch": true,
}

// expand adds, for every call that runs another command, the command it runs. The wrapper
// call itself stays in the set: `sudo ls` is still a sudo.
func expand(calls [][]string, depth int) [][]string {
	if depth >= wrapperDepth {
		return calls
	}
	out := calls
	for _, call := range calls {
		for _, inner := range unwrap(call) {
			if len(inner) == 0 {
				continue
			}
			out = append(out, inner)
			out = append(out, expand([][]string{inner}, depth+1)...)
		}
	}
	return out
}

// unwrap is what one call runs, if it runs anything: the parsed script of a shell's -c, the
// parsed argument of eval, or the remainder of a prefix wrapper.
func unwrap(call []string) [][]string {
	if len(call) < 2 {
		return nil
	}
	head := shellName(call[0]) // /bin/sh and sh are the same wrapper
	switch {
	case head == "find":
		return findExec(call)
	case shellFlagged[head]:
		for i := 1; i < len(call)-1; i++ {
			// -c, and the bundles a login or interactive shell takes: -lc, -ic, -ec.
			if strings.HasPrefix(call[i], "-") && strings.HasSuffix(call[i], "c") {
				return shellCallsOrFields(call[i+1])
			}
		}
		return nil
	case head == "eval":
		return shellCallsOrFields(strings.Join(call[1:], " "))
	case prefixWrappers[head]:
		rest := call[1:]
		for len(rest) > 0 && (strings.HasPrefix(rest[0], "-") || strings.Contains(rest[0], "=")) {
			// env's assignments and a wrapper's own flags are not the command it runs.
			rest = rest[1:]
		}
		if len(rest) == 0 {
			return nil
		}
		return [][]string{rest}
	}
	return nil
}

func bashCommand(args json.RawMessage) (string, bool) {
	var a struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &a); err != nil || a.Command == "" {
		return "", false
	}
	return a.Command, true
}

func firstWords(words []string, n int) string {
	if len(words) > n {
		words = words[:n]
	}
	return strings.Join(words, " ")
}

// shellCalls parses src and returns the words of every simple command in
// source order. Assignments are not words.
func shellCalls(src string) ([][]string, error) {
	f, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		return nil, err
	}
	var calls [][]string
	syntax.Walk(f, func(n syntax.Node) bool {
		if c, ok := n.(*syntax.CallExpr); ok && len(c.Args) > 0 {
			words := make([]string, 0, len(c.Args))
			for _, a := range c.Args {
				words = append(words, wordText(a))
			}
			calls = append(calls, words)
		}
		return true
	})
	return calls, nil
}

func wordText(w *syntax.Word) string {
	var b strings.Builder
	for _, p := range w.Parts {
		b.WriteString(partText(p))
	}
	return b.String()
}

// partText renders a word part: quotes are dropped, expansions keep their
// source form so $HOME stays $HOME.
func partText(p syntax.WordPart) string {
	switch p := p.(type) {
	case *syntax.Lit:
		return p.Value
	case *syntax.SglQuoted:
		return p.Value
	case *syntax.DblQuoted:
		var b strings.Builder
		for _, q := range p.Parts {
			b.WriteString(partText(q))
		}
		return b.String()
	}
	var b strings.Builder
	_ = syntax.NewPrinter().Print(&b, p)
	return b.String()
}

// findExec is the command a find runs, if it runs one: the words after -exec or -execdir up
// to the ; or + that ends them, with the {} placeholder dropped. A cleanup task is exactly
// where a model writes find -exec rm -rf, and find is not a prefix wrapper, so without this
// the set never reads what it runs.
func findExec(call []string) [][]string {
	var out [][]string
	for i := 0; i < len(call); i++ {
		if call[i] != "-exec" && call[i] != "-execdir" && call[i] != "-ok" && call[i] != "-okdir" {
			continue
		}
		var inner []string
		for j := i + 1; j < len(call); j++ {
			w := call[j]
			if w == ";" || w == "+" || w == `\;` {
				break
			}
			if w == "{}" {
				continue
			}
			inner = append(inner, w)
		}
		if len(inner) > 0 {
			out = append(out, inner)
		}
	}
	return out
}
