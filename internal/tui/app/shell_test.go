// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tui/theme"
	"github.com/guygrigsby/rudy/internal/tui/transcript"
)

// TestABangSendsAShellCommand: Enter on a bang draft runs the command instead of saying it,
// and no message is submitted (ADR 0023).
func TestABangSendsAShellCommand(t *testing.T) {
	h := newHarness(t, nil)
	h.typeText("!git status")
	runAll(t, h.press("enter"))
	h.mu.Lock()
	defer h.mu.Unlock()
	var shell, submit int
	for _, r := range h.reqs {
		switch r.Method {
		case protocol.MethodSessionShell:
			var p protocol.SessionShellParams
			if err := json.Unmarshal(r.Params, &p); err != nil {
				t.Fatal(err)
			}
			if p.Command != "git status" {
				t.Errorf("the bang is not part of the command: %q", p.Command)
			}
			shell++
		case protocol.MethodSessionSubmit:
			submit++
		}
	}
	if shell != 1 || submit != 0 {
		t.Errorf("one shell call and no submit, got %d and %d", shell, submit)
	}
	if h.m.ed.Text() != "" {
		t.Errorf("the composer is cleared, holds %q", h.m.ed.Text())
	}
}

// TestABangAloneSendsNothing: a bang with no command is a draft, not a request.
func TestABangAloneSendsNothing(t *testing.T) {
	h := newHarness(t, nil)
	h.typeText("!   ")
	runAll(t, h.press("enter"))
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.reqs {
		if r.Method == protocol.MethodSessionShell {
			t.Errorf("a bang alone runs nothing: %s", r.Params)
		}
	}
}

// TestTheComposerChangesColourForAShellDraft is the tell: the whole input area wears the
// shell role while the draft is a command, and goes back when it is not.
func TestTheComposerChangesColourForAShellDraft(t *testing.T) {
	h := newHarness(t, nil)
	plain := h.m.ed.View()
	h.typeText("!ls")
	shell := h.m.ed.View()
	if shell == plain {
		t.Fatal("a shell draft looks different from a message")
	}
	if !strings.Contains(shell, colorOf(t, h.m.th, theme.RoleShell)) {
		t.Errorf("the composer wears the shell role:\n%q", shell)
	}
	// Backspace over the bang and it is a message again.
	for range 3 {
		h.press("backspace")
	}
	if got := h.m.ed.View(); strings.Contains(got, colorOf(t, h.m.th, theme.RoleShell)) {
		t.Errorf("a draft that is no longer a command is painted back:\n%q", got)
	}
}

// colorOf is the escape sequence a role paints with, which is what a styled view carries.
func colorOf(t *testing.T, th theme.Theme, role theme.Role) string {
	t.Helper()
	rendered := th.Style(role).Render("x")
	i := strings.Index(rendered, "x")
	if i <= 0 {
		t.Fatalf("role %s renders unstyled: %q", role, rendered)
	}
	return rendered[:i]
}

// TestAShellEntryIsItsOwnRow: what came back is not something the operator said, and it does
// not wear the user prefix.
func TestAShellEntryIsItsOwnRow(t *testing.T) {
	h := newHarness(t, nil)
	h.appended(session.UserMessage{
		Source:  session.SourceShell,
		Content: []session.Block{session.TextBlock("$ git status\nOn branch main")},
	})
	rows := h.m.tr.Rows()
	if len(rows) != 1 || rows[0].Kind != transcript.RowShell {
		t.Fatalf("rows %+v", rows)
	}
	view := ansi.Strip(h.view())
	if !strings.Contains(view, "$ git status") || !strings.Contains(view, "On branch main") {
		t.Errorf("the row shows the command and its output:\n%s", view)
	}
	if strings.Contains(view, "› $ git status") {
		t.Errorf("and not as something the operator said:\n%s", view)
	}
}
