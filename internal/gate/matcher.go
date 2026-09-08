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

// Dangerous reports whether any simple command in a bash input starts with a
// dangerous entry, and which entry matched.
func (g *Gate) Dangerous(tool string, args json.RawMessage) (bool, string) {
	if tool != "bash" {
		return false, ""
	}
	cmd, ok := bashCommand(args)
	if !ok {
		return false, ""
	}
	calls, err := shellCalls(cmd)
	if err != nil || len(calls) == 0 {
		calls = [][]string{strings.Fields(cmd)}
	}
	for _, call := range calls {
		joined := strings.Join(call, " ")
		for _, d := range g.dangerous {
			if joined == d || strings.HasPrefix(joined, d+" ") {
				return true, d
			}
		}
	}
	return false, ""
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
