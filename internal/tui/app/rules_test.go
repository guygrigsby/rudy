// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/session"
)

// ruleLines are the frame's lines that are rules, as plain text.
func ruleLines(h *harness) []string {
	var out []string
	for _, l := range h.lines() {
		if s := ansi.Strip(l); strings.Contains(s, "─") {
			out = append(out, strings.TrimRight(s, " "))
		}
	}
	return out
}

// TestTheComposerSitsBetweenTwoRules is the shape ADR 0017 gives the input slot: a line
// above it and a line below it, both the width of the terminal, in the gutter every other
// line sits in.
func TestTheComposerSitsBetweenTwoRules(t *testing.T) {
	h := newHarness(t, nil)
	rules := ruleLines(h)
	if len(rules) != 2 {
		t.Fatalf("two rules bracket the composer, got %d:\n%s", len(rules), ansi.Strip(h.view()))
	}
	for _, r := range rules {
		if w := ansi.StringWidth(r); w != h.m.width {
			t.Errorf("a rule is the width of the terminal, got %d of %d in %q", w, h.m.width, r)
		}
		if !strings.HasPrefix(r, " ─") {
			t.Errorf("a rule sits in the gutter: %q", r)
		}
	}
	lines := h.lines()
	var editor int
	for i, l := range lines {
		if strings.Contains(ansi.Strip(l), "┃") {
			editor = i
		}
	}
	if editor == 0 {
		t.Fatalf("no composer on screen:\n%s", ansi.Strip(h.view()))
	}
	if !strings.Contains(ansi.Strip(lines[editor-1]), "─") || !strings.Contains(ansi.Strip(lines[editor+1]), "─") {
		t.Errorf("the rules are the lines either side of the composer:\n%s", ansi.Strip(h.view()))
	}
}

// TestTheLowerRuleCarriesTheContextPercentage: the number is drawn once, labelled, and only
// once there is something to say about it.
func TestTheLowerRuleCarriesTheContextPercentage(t *testing.T) {
	h := newHarness(t, nil)
	if got := ruleLines(h)[1]; strings.Contains(got, "context") || strings.Contains(got, "◔") {
		t.Errorf("a session that has sent nothing says nothing about its context, icon included: %q", got)
	}
	// The context percentage is the last request's prompt against the model's window,
	// which is what an assistant message carries.
	h.appended(session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopEndTurn,
		Content: []session.Block{session.TextBlock("done")},
		Usage:   session.Usage{Input: 20000, Output: 1000, CacheRead: 22000},
	})
	below := ruleLines(h)[1]
	if !strings.Contains(below, "42% context") {
		t.Errorf("the lower rule carries the percentage, labelled: %q", below)
	}
	if !strings.HasSuffix(below, "──") {
		t.Errorf("and the rule runs on past it: %q", below)
	}
	if status := ansi.Strip(h.m.statusLine()); strings.Contains(status, "42%") {
		t.Errorf("and it is not also in the status line: %q", status)
	}
}

// TestTheRulesAreOffByConfig is the escape.
func TestTheRulesAreOffByConfig(t *testing.T) {
	h := newHarness(t, map[string]any{"ui.input.rules": false})
	if got := ruleLines(h); len(got) != 0 {
		t.Errorf("no rules were asked for: %q", got)
	}
}

// TestANarrowTerminalDrawsThePlainRule: too narrow for the label and the rule is still a
// rule, never a truncated sentence.
func TestANarrowTerminalDrawsThePlainRule(t *testing.T) {
	h := newHarness(t, nil)
	h.appended(session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopEndTurn,
		Content: []session.Block{session.TextBlock("done")},
		Usage:   session.Usage{Input: 20000, Output: 1000, CacheRead: 22000},
	})
	h.update(tea.WindowSizeMsg{Width: 14, Height: 24})
	for _, r := range ruleLines(h) {
		if strings.ContainsAny(r, "context0123456789") {
			t.Errorf("a rule with no room for the label is a plain rule: %q", r)
		}
		if w := ansi.StringWidth(r); w != 14 {
			t.Errorf("and still the width of the terminal, got %d in %q", w, r)
		}
	}
}

// TestTheUpperRuleCarriesTheSessionName is what /rename shows for itself: the header's box
// has scrolled away by the time anybody renames a session, so the name lands on the rule
// over the composer (ADR 0019).
func TestTheUpperRuleCarriesTheSessionName(t *testing.T) {
	h := newHarness(t, nil)
	if got := ruleLines(h)[0]; strings.ContainsAny(got, "abcdefghijklmnopqrstuvwxyz") {
		t.Errorf("an unnamed session leaves the rule plain: %q", got)
	}
	h.appended(session.TitleChange{Title: "the flaky fork test"})
	if got := ruleLines(h)[0]; !strings.Contains(got, "the flaky fork test") {
		t.Errorf("a named one says so: %q", got)
	}
}
