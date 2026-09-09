package app

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tui/input"
	"github.com/guygrigsby/rudy/internal/tui/theme"
)

// The six built-in ui.status.items ids (config.builtinStatusItems). Anything else in the
// list is a plugin's "<owner>:<key>".
const (
	itemVimMode        = "vim_mode"
	itemModel          = "model"
	itemPermissionMode = "permission_mode"
	itemContext        = "context"
	itemCost           = "cost"
	itemWorkspace      = "workspace"
)

// statusGap separates two items, as the design's screen spaces them.
const statusGap = "  "

// statusLine draws ui.status.items in order. An item with nothing to say draws nothing
// and takes no separator with it, so a session with no cost yet does not leave a gap
// where the cost will be.
func (m *Model) statusLine() string {
	parts := make([]string, 0, len(m.cfg.UI.Status.Items))
	for _, id := range m.cfg.UI.Status.Items {
		if s := m.statusItem(id); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return m.clamp(strings.Join(parts, statusGap))
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
		return m.styled(theme.RoleMuted, m.session.Model.String())
	case itemPermissionMode:
		return m.styled(theme.RoleMuted, string(m.session.Mode))
	case itemContext:
		return m.styled(theme.RoleMuted, contextPercent(m.model.ContextWindow, m.lastPrompt))
	case itemCost:
		return m.styled(theme.RoleMuted, cost(m.model.Pricing, m.usage))
	case itemWorkspace:
		return m.styled(theme.RoleMuted, m.workspace)
	}
	// A plugin's item. One no plugin has set renders nothing rather than a placeholder:
	// the config placed a cell, the plugin decides whether there is anything in it.
	item, ok := m.status[id]
	if !ok {
		return ""
	}
	return renderSpans(m.th, item.Content)
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
