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
	calls := bashCalls(args)
	if len(calls) == 0 {
		return session.Matcher{Tool: tool, Prefix: firstWords(strings.Fields(cmd), 2)}
	}
	return session.Matcher{Tool: tool, Prefix: calls[0].prefix()}
}

// Dangerous reports whether this call matches an entry of the dangerous set, and why. An
// entry takes one of three forms:
//
//	rm -rf        a bash command name and the tokens it requires, in any order
//	web_fetch:    a tool, dangerous whatever its input
//	bash:git push a tool and, for bash, a command and its tokens
//
// For bash, every simple command in the input is checked, and so is every command a
// wrapper runs: the set is what forces a question in permissive mode, and an entry that
// `sh -c` walks around is an entry that does nothing (rudy-k0.34, rudy-y3d). A call the
// gate cannot read is dangerous without any entry: a command word or flag the shell will
// expand, a shell given a script or its commands on input, source (ADR 0051).
func (g *Gate) Dangerous(tool string, args json.RawMessage) (bool, string) {
	var calls []call
	if tool == "bash" {
		calls = bashCalls(args)
	}
	for _, d := range g.dangerous {
		entryTool, rest, scoped := strings.Cut(d, ":")
		switch {
		case scoped && entryTool != tool:
			continue
		case scoped && rest == "":
			// A tool with no prefix: every call of it is dangerous, which is the only
			// way to say "always ask" for a tool that is not bash.
			return true, d
		case tool != "bash":
			// Every other entry is a bash command, and no other tool has one.
			continue
		}
		want := d
		if scoped {
			want = rest
		}
		e := parseEntry(want)
		for _, c := range calls {
			if e.matches(c) {
				return true, d
			}
		}
	}
	for _, c := range calls {
		if reason, ok := c.refusal(); ok {
			return true, reason
		}
	}
	return false, ""
}

// unreadable is the characters that make a word something other than what it spells: the
// shell expands, escapes, braces or globs it before it runs, so the gate cannot say what
// command or flag it becomes.
const unreadable = "$`\\{*?[;"

// call is one simple command the input runs, after wrapper expansion.
type call struct {
	words []string // as written, quotes dropped, expansions in source form
}

// name is the command the shell will run: the last path element of the first word, with a
// leading backslash dropped.
func (c call) name() string { return shellName(c.words[0]) }

// prefix is the key an allowance is matched on: the name and the second word, when there is one.
func (c call) prefix() string {
	if len(c.words) > 1 {
		return c.name() + " " + c.words[1]
	}
	return c.name()
}

// tokens is the arguments after normalization, so an entry and a command compare on what
// they ask for and not how it is spelled: a bundle -rvf is -r -v -f, -R and --recursive are
// -r, --force is -f, --flag=value is --flag, and a word that is not a flag stays as written.
// A flag the gate cannot read gives no token; refusal reports it.
func (c call) tokens() []string {
	var out []string
	for _, w := range c.words[1:] {
		switch {
		case isFlag(w) && strings.ContainsAny(w, unreadable):
		case strings.HasPrefix(w, "--"):
			name, _, _ := strings.Cut(w, "=")
			switch name {
			case "--recursive":
				name = "-r"
			case "--force":
				name = "-f"
			}
			out = append(out, name)
		case isFlag(w):
			for _, r := range w[1:] {
				if r == 'R' {
					r = 'r'
				}
				out = append(out, "-"+string(r))
			}
		default:
			out = append(out, w)
		}
	}
	return out
}

// isFlag is a word that starts like a flag: a dash and something after it.
func isFlag(w string) bool { return len(w) > 1 && w[0] == '-' }

// unclassifiable is the first word among the command and every flag-like word that the
// shell will rewrite before running, which the gate cannot read.
func (c call) unclassifiable() (string, bool) {
	for i, w := range c.words {
		if (i == 0 || isFlag(w)) && strings.ContainsAny(w, unreadable) {
			return w, true
		}
	}
	return "", false
}

// refusal is why the call is dangerous whatever the set says, if it is.
func (c call) refusal() (string, bool) {
	if w, ok := c.unclassifiable(); ok {
		return "unclassifiable command " + w, true
	}
	name := c.name()
	switch {
	case name == "source" || name == ".":
		return "sources a file", true
	case name == "eval":
		if joined := strings.Join(c.words[1:], " "); strings.ContainsAny(joined, unreadable) {
			return "unclassifiable command " + joined, true
		}
	case shellFlagged[name]:
		if _, ok := shellScript(c.words); ok {
			return "", false
		}
		for _, w := range c.words[1:] {
			if !strings.HasPrefix(w, "-") {
				return "shell runs a script it was given", true
			}
		}
		return "shell reads its commands from input", true
	}
	return "", false
}

// entry is a dangerous-set entry read the way a call is: a command name and the tokens it requires.
type entry struct {
	name   string
	tokens []string
}

func parseEntry(s string) entry {
	words := strings.Fields(s)
	if len(words) == 0 {
		return entry{}
	}
	return entry{name: shellName(words[0]), tokens: call{words: words}.tokens()}
}

// matches is true when the call runs the entry's command, matching mkfs to mkfs.ext4, and
// holds every token the entry requires, in any order.
func (e entry) matches(c call) bool {
	if e.name == "" {
		return false
	}
	if n := c.name(); n != e.name && !strings.HasPrefix(n, e.name+".") {
		return false
	}
	have := c.tokens()
	for _, t := range e.tokens {
		found := false
		for _, h := range have {
			if h == t {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
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

// bashCalls is every simple command of a bash input plus every command a wrapper runs, as
// calls. An input the shell will refuse is still read, as its words.
func bashCalls(args json.RawMessage) []call {
	cmd, ok := bashCommand(args)
	if !ok {
		return nil
	}
	return expand(shellCallsOrFields(cmd), 0)
}

// shellCallsOrFields is the parsed simple commands of src, or its words when it does not
// parse: an input the shell will refuse is still an input the set should be read against.
func shellCallsOrFields(src string) []call {
	words, err := shellCalls(src)
	if err != nil || len(words) == 0 {
		words = [][]string{strings.Fields(src)}
	}
	out := make([]call, 0, len(words))
	for _, w := range words {
		if len(w) > 0 {
			out = append(out, call{words: w})
		}
	}
	return out
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
	"setsid": true, "watch": true, "caffeinate": true,
}

// wrapperValueFlags is, per wrapper, the short flags that take the next word as their
// value. That word is not the command: `nice -n 10 rm -rf` runs rm. A value attached to its
// flag (-n10) or joined with = (--adjustment=10) is one word and consumes nothing extra.
// timeout's -k and -s are here beyond the table the ADR names, since `timeout -s KILL 5 rm`
// would otherwise read KILL as the duration and 5 as the command.
var wrapperValueFlags = map[string]map[string]bool{
	"nice":       {"-n": true},
	"ionice":     {"-c": true, "-n": true, "-p": true},
	"xargs":      {"-n": true, "-I": true, "-L": true, "-P": true, "-d": true, "-s": true, "-a": true, "-E": true},
	"watch":      {"-n": true},
	"env":        {"-u": true, "-C": true, "-S": true},
	"exec":       {"-a": true},
	"sudo":       {"-u": true, "-g": true, "-C": true, "-D": true, "-h": true, "-p": true, "-r": true, "-t": true, "-U": true},
	"doas":       {"-u": true, "-C": true},
	"stdbuf":     {"-i": true, "-o": true, "-e": true},
	"caffeinate": {"-t": true, "-w": true},
	"timeout":    {"-k": true, "-s": true},
}

// expand adds, for every call that runs another command, the command it runs. The wrapper
// call itself stays in the set: `sudo ls` is still a sudo.
func expand(calls []call, depth int) []call {
	if depth >= wrapperDepth {
		return calls
	}
	out := calls
	for _, c := range calls {
		for _, inner := range unwrap(c) {
			out = append(out, inner)
			out = append(out, expand([]call{inner}, depth+1)...)
		}
	}
	return out
}

// shellScript is the script a shell is given with -c: the first non-flag word after the
// first short-flag bundle that holds c, past a --. Long flags such as --norc are not
// bundles. ok is false when no bundle holds c, which is a shell that runs a file or stdin.
func shellScript(words []string) (string, bool) {
	for i := 1; i < len(words); i++ {
		w := words[i]
		if strings.HasPrefix(w, "--") || !isFlag(w) || !strings.Contains(w, "c") {
			continue
		}
		for _, a := range words[i+1:] {
			if !strings.HasPrefix(a, "-") {
				return a, true
			}
		}
		return "", false
	}
	return "", false
}

// unwrap is what one call runs, if it runs anything: the parsed script of a shell's -c, the
// parsed argument of eval, or the remainder of a prefix wrapper.
func unwrap(c call) []call {
	if len(c.words) < 2 {
		return nil
	}
	head := c.name() // /bin/sh and sh are the same wrapper
	switch {
	case head == "find":
		return findExec(c.words)
	case shellFlagged[head]:
		if script, ok := shellScript(c.words); ok {
			return shellCallsOrFields(script)
		}
		return nil
	case head == "eval":
		return shellCallsOrFields(strings.Join(c.words[1:], " "))
	case prefixWrappers[head]:
		rest := wrapped(head, c.words[1:])
		if len(rest) == 0 {
			return nil
		}
		return []call{{words: rest}}
	}
	return nil
}

// wrapped is the command a prefix wrapper runs: what follows its flags, the values those
// flags take, env's assignments and, for timeout, the duration.
func wrapped(wrapper string, rest []string) []string {
	valued := wrapperValueFlags[wrapper]
	duration := wrapper == "timeout"
	for len(rest) > 0 {
		w := rest[0]
		switch {
		case strings.HasPrefix(w, "-"):
			if valued[w] && len(rest) > 1 {
				rest = rest[1:]
			}
		case strings.Contains(w, "="):
		case duration:
			duration = false
		default:
			return rest
		}
		rest = rest[1:]
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
func findExec(words []string) []call {
	var out []call
	for i := range words {
		if words[i] != "-exec" && words[i] != "-execdir" && words[i] != "-ok" && words[i] != "-okdir" {
			continue
		}
		var inner []string
		for _, w := range words[i+1:] {
			if w == ";" || w == "+" || w == `\;` {
				break
			}
			if w == "{}" {
				continue
			}
			inner = append(inner, w)
		}
		if len(inner) > 0 {
			out = append(out, call{words: inner})
		}
	}
	return out
}
