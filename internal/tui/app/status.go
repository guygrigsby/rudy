package app

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tui/icons"
	"github.com/guygrigsby/rudy/internal/tui/input"
	"github.com/guygrigsby/rudy/internal/tui/theme"
	"github.com/guygrigsby/rudy/internal/tui/transcript"
)

// The seven built-in ui.status.items ids (config.builtinStatusItems). Anything else in
// the list is a plugin's "<owner>:<key>".
const (
	itemVimMode        = "vim_mode"
	itemModel          = "model"
	itemPermissionMode = "permission_mode"
	itemContext        = "context"
	itemCost           = "cost"
	itemWorkspace      = "workspace"
	itemTurn           = "turn"
	itemCat            = "cat"
)

// statusGap separates two items, as the design's screen spaces them.
const statusGap = "  "

// statusLine draws ui.status.items in order, one column in: the design's screen puts the
// status line in the same left gutter every transcript row sits in. An item with nothing
// to say draws nothing and takes no separator with it, so a session with no cost yet does
// not leave a gap where the cost will be.
// statusLine is the line under the composer: ui.status.items, less whatever the line above
// the composer already draws. An item is drawn once, in the first list that names it, so a
// config that carried `turn` before ui.status.above_editor existed does not say it twice
// (ADR 0025). Naming it in both is how a person asks for both.
func (m *Model) statusLine() string {
	above := m.cfg.UI.Status.AboveEditor
	items := make([]string, 0, len(m.cfg.UI.Status.Items))
	for _, id := range m.cfg.UI.Status.Items {
		if slices.Contains(above, id) {
			continue
		}
		items = append(items, id)
	}
	return m.statusLineOf(items)
}

// aboveEditorLine is the status line drawn over the composer, ui.status.above_editor: the
// turn cell by default, which is what a person watches while a turn runs and the last thing
// they should have to look down for.
func (m *Model) aboveEditorLine() string { return m.statusLineOf(m.cfg.UI.Status.AboveEditor) }

// statusLineOf draws one list of status items.
func (m *Model) statusLineOf(items []string) string {
	parts := make([]string, 0, len(items))
	for _, id := range items {
		if s := m.statusItem(id); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return m.clamp(strings.Repeat(" ", transcript.Gutter) + strings.Join(parts, statusGap))
}

// statusItem renders one ui.status.items entry, styled, or "" when it has nothing to say.
// The vim mode is the one accented cell, as the design's screen reads it; the rest is
// chrome, and a plugin's item paints itself through its own spans' roles.
func (m *Model) statusItem(id string) string {
	switch id {
	case itemVimMode:
		return m.styled(theme.RoleAccent, vimMode(m.ed.Mode()))
	case itemModel:
		// The canonical form, "provider:model": two providers can carry the same model
		// id, and the picker and every log entry name one this way.
		return m.styled(theme.RoleMuted, m.ic.Label(icons.Model, m.session.Model.String()))
	case itemPermissionMode:
		return m.styled(theme.RoleMuted, string(m.session.Mode))
	case itemContext:
		return m.styled(theme.RoleMuted, m.ic.Label(icons.Context, contextPercent(m.model.ContextWindow, m.lastPrompt)))
	case itemCost:
		// No icon: the currency symbol the amount opens with is the icon.
		return m.styled(theme.RoleMuted, cost(m.model.Pricing, m.usage))
	case itemWorkspace:
		return m.styled(theme.RoleMuted, m.workspaceCell())
	case itemTurn:
		return m.styled(theme.RoleMuted, m.turnCell())
	case itemCat:
		// Empty when ui.cats is off, which draws no cell and takes no separator with it,
		// the way vim off leaves no mode cell.
		return m.styled(theme.RoleMuted, m.cat)
	}
	// A plugin's item. One no plugin has set renders nothing rather than a placeholder:
	// the config placed a cell, the plugin decides whether there is anything in it.
	item, ok := m.status[id]
	if !ok {
		return ""
	}
	return renderSpans(m.th, item.Content)
}

// workspaceCell is the workspace item: the repository, then the branch behind the branch
// icon, then the dirty marker. detectWorkspace hands over "<repo> <branch>[*]", which is
// the design's own spelling, so the icon is set in front of the branch rather than in
// front of the whole cell.
func (m *Model) workspaceCell() string {
	repo, branch, ok := strings.Cut(m.workspace, " ")
	if !ok {
		return m.workspace
	}
	return repo + " " + m.ic.Label(icons.Branch, branch)
}

// styled is text in one role, or "" for an item with nothing to say. Built-in text is the
// client's own, but it goes through the same sanitizer a plugin's span does: a title, a
// branch name and a model id all come from somewhere else.
func (m *Model) styled(r theme.Role, text string) string {
	if text == "" {
		return ""
	}
	return m.th.Style(r).Render(spanText(text))
}

// turnCell is the turn item: the spinner's glyph and what the turn is doing, in one word
// (ADR 0013 decision 3, where thinking deltas count toward a spinner). At rest it says
// nothing and the spinner is not turning, so a client that is not working on anything
// draws the same status line it drew before this item existed.
func (m *Model) turnCell() string {
	word := m.turn.word()
	if word == "" {
		return ""
	}
	return m.spin.View() + " " + word
}

// vimMode is the editor's mode as the status line spells it, upper case. Vim disabled has
// no mode and so no cell.
func vimMode(mode input.Mode) string {
	if mode == input.ModeDisabled {
		return ""
	}
	return strings.ToUpper(string(mode))
}

// contextPercent is how much of the model's context window the last prompt filled, as
// "NN%", rounded down. A window the registry does not know (0) has nothing to say.
func contextPercent(window, prompt int64) string {
	if window <= 0 {
		return ""
	}
	return strconv.FormatInt(prompt*100/window, 10) + "%"
}

// cost is the session's running cost as "$X.XX". A session that has spent nothing, and a
// model whose prices the registry does not carry, both have nothing to say.
func cost(p provider.Pricing, u session.Usage) string {
	if u == (session.Usage{}) {
		return ""
	}
	usd, ok := p.Cost(u)
	if !ok || strings.HasPrefix(usd, "-") {
		return ""
	}
	return "$" + cents(usd)
}

// cents rounds Pricing.Cost's six decimals to two, half up, on the decimal string rather
// than through a float: 0.075000 is not a binary fraction, and rounding the float64
// nearest it gives 0.07, a cent short of what the log says was spent.
func cents(usd string) string {
	whole, frac, _ := strings.Cut(usd, ".")
	frac = (frac + "000")[:3]
	hundredths, err := strconv.ParseInt(whole+frac[:2], 10, 64)
	if err != nil {
		return usd
	}
	if frac[2] >= '5' {
		hundredths++
	}
	return strconv.FormatInt(hundredths/100, 10) + "." + fmt.Sprintf("%02d", hundredths%100)
}

// detectWorkspace is the workspace item: the git root's basename, the branch, and a star
// when the tree is dirty, as in the design's "rudy main*". A directory that is not a
// repository, and a machine with no git, both have nothing to say.
//
// root is the git root the session already resolved, so the common case costs two
// subprocesses rather than three. Read once, at New: a status line is redrawn on every
// keystroke and must never fork a process to draw itself.
func detectWorkspace(root, cwd string) string {
	branch := gitLine(cwd, "rev-parse", "--abbrev-ref", "HEAD")
	if branch == "" {
		return ""
	}
	if root == "" {
		root = gitLine(cwd, "rev-parse", "--show-toplevel")
	}
	if root == "" {
		return ""
	}
	name := filepath.Base(root) + " " + branch
	if gitLine(cwd, "status", "--porcelain") != "" {
		return name + "*"
	}
	return name
}

// gitLine runs one git command in dir and returns its trimmed output, or "" on any
// failure, git missing included.
func gitLine(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(ansi.Strip(string(out)))
}
