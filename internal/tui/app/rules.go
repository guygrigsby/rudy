package app

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/tui/icons"
	"github.com/guygrigsby/rudy/internal/tui/theme"
	"github.com/guygrigsby/rudy/internal/tui/transcript"
)

// The composer's rules: a line above it and a line below it, so the input area reads as its
// own region rather than as the next transcript row. The lower one carries how much of the
// context window the last request filled, which is the one number a person checks while
// typing and the only place it is drawn (ADR 0017).
const (
	ruleChar = "─"
	// ruleGap is the space each side of the label sitting in the rule.
	ruleGap = " "
	// ruleTail is how much rule is left after the label, so the number reads as set into
	// the line rather than as the end of it.
	ruleTail = 2
)

// contextLabel is the percentage as the rule says it. Bare digits and a sign say nothing
// about what filled up; a session with no assistant message yet, and a model the registry
// has no window for, both leave the rule plain.
func contextLabel(percent string) string {
	if percent == "" || percent == "0%" {
		return ""
	}
	return percent + " context"
}

// rule is one horizontal line the width of the terminal, drawn in the same left gutter
// every other line sits in, with label set into its right end when there is one.
func (m *Model) rule(label string) string {
	width := m.width - transcript.Gutter
	if width < 1 {
		return ""
	}
	gutter := strings.Repeat(" ", transcript.Gutter)
	if label != "" {
		if w := ansi.StringWidth(label) + 2*len(ruleGap) + ruleTail; w < width {
			line := strings.Repeat(ruleChar, width-w) + ruleGap + label + ruleGap + strings.Repeat(ruleChar, ruleTail)
			return gutter + m.th.Style(theme.RoleMuted).Render(line)
		}
	}
	return gutter + m.th.Style(theme.RoleMuted).Render(strings.Repeat(ruleChar, width))
}

// composerRules are the two lines that bracket the input slot, or nothing when config has
// turned them off. The lower one is where the context percentage lives.
func (m *Model) composerRules() (above, below []string) {
	if !m.cfg.UI.Input.Rules {
		return nil, nil
	}
	top := m.rule("")
	bottom := m.rule(m.ic.Label(icons.Context, contextLabel(contextPercent(m.model.ContextWindow, m.lastPrompt))))
	if top == "" || bottom == "" {
		return nil, nil
	}
	return []string{top}, []string{bottom}
}
