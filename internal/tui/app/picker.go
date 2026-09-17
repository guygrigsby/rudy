// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tui/keys"
	"github.com/guygrigsby/rudy/internal/tui/theme"
)

// The two things a picker picks. The kind is what a confirm reads to know which call the
// chosen row makes; the rows themselves are the same shape either way.
const (
	pickerModel   = "model"
	pickerSession = "session"
	pickerScope   = "scope"
)

// The marks a multi-select picker draws in front of a row, in and out of the set being
// chosen. A cross rather than a colour: nothing under internal/tui paints a background,
// and a set has to be readable in one glance.
const (
	inSet  = "[x] "
	outSet = "[ ] "
)

// pickerMinRows is the shortest list worth drawing. The longest is half the frame, so
// what the picker was opened from stays on screen above it.
const pickerMinRows = 3

// The mark on the row the keyboard is on. Selection is a mark and the accent role rather
// than a painted line: nothing under internal/tui paints a background but the diff.
const (
	selectedMark   = "> "
	unselectedMark = "  "
)

// picker is the list that takes the editor's place while it is up: a title, the rows it
// was built with, the filter typed so far and the row the keyboard is on. It owns the
// keyboard while it stands, so nothing it does not handle reaches the editor.
type picker struct {
	kind  string
	title string
	rows  []pickRow
	// filter is what has been typed, matched case-insensitively against every row's text.
	filter string
	// sel is an index into the filtered rows, not into rows: the filter is what is on
	// screen, and moving through what is not there would look like a stuck key.
	sel int
	// top is the first filtered row drawn, moved only to keep sel on screen.
	top int
	// chosen is the ids picked so far, for a picker that takes a set rather than one row.
	// Nil for the single-row pickers, which is what tells the two apart when drawing.
	chosen map[string]bool
}

// multi reports whether this picker takes a set rather than one row.
func (p *picker) multi() bool { return p.chosen != nil }

// toggle puts the row under the cursor in or out of the set.
func (p *picker) toggle() {
	row, ok := p.current()
	if !ok {
		return
	}
	if p.chosen[row.id] {
		delete(p.chosen, row.id)
		return
	}
	p.chosen[row.id] = true
}

// pickRow is one row: the text it draws and matches on, and the id a confirm sends, a
// model's "provider:id" or a session's ulid.
type pickRow struct {
	text string
	id   string
}

// visible are the rows the filter leaves, in registry or session order.
func (p *picker) visible() []pickRow {
	if p.filter == "" {
		return p.rows
	}
	want := strings.ToLower(p.filter)
	out := make([]pickRow, 0, len(p.rows))
	for _, r := range p.rows {
		if strings.Contains(strings.ToLower(r.text), want) {
			out = append(out, r)
		}
	}
	return out
}

// current is the row under the cursor, false for a filter that matches nothing.
func (p *picker) current() (pickRow, bool) {
	rows := p.visible()
	if p.sel < 0 || p.sel >= len(rows) {
		return pickRow{}, false
	}
	return rows[p.sel], true
}

// move steps the cursor by d rows, clamped at both ends rather than wrapped: a list long
// enough to scroll should not jump from its end to its start under a held key.
func (p *picker) move(d int) {
	n := len(p.visible())
	if n == 0 {
		p.sel = 0
		return
	}
	p.sel = min(max(p.sel+d, 0), n-1)
}

// point puts the cursor on the row with this id, leaving it where it is when there is no
// such row: a model picker opens on the session's own model.
func (p *picker) point(id string) {
	for i, r := range p.visible() {
		if r.id == id {
			p.sel = i
			return
		}
	}
}

// setRows replaces the rows under the filter, keeping the cursor on the same id when that
// row survived: a registry answer that lands while the picker is open must not move the
// selection out from under the keyboard.
func (p *picker) setRows(rows []pickRow) {
	at, ok := p.current()
	p.rows = rows
	p.sel, p.top = 0, 0
	if ok {
		p.point(at.id)
	}
}

// filterKey folds one key that is none of the picker's actions into the filter: printable
// text extends it, backspace shortens it, everything else is swallowed. The cursor goes
// back to the top whenever the filter changes, since the rows under it have changed too.
func (p *picker) filterKey(k tea.Key) {
	n := keys.Normalize(k)
	switch {
	case n.Code == tea.KeyBackspace && n.Mod == 0:
		if p.filter == "" {
			return
		}
		r := []rune(p.filter)
		p.filter = string(r[:len(r)-1])
	case n.Mod == 0 && n.Text != "":
		p.filter += n.Text
	default:
		return
	}
	p.sel, p.top = 0, 0
}

// window moves the visible slice so the cursor is inside it, h rows tall.
func (p *picker) window(h int) {
	if h <= 0 {
		p.top = 0
		return
	}
	p.top = min(p.top, p.sel)
	if p.sel >= p.top+h {
		p.top = p.sel - h + 1
	}
	p.top = max(min(p.top, max(len(p.visible())-h, 0)), 0)
}

// head is the title line: the title, and the filter beside it once there is one, so what
// is being typed is visible while the editor is hidden.
func (p *picker) head() string {
	if p.filter == "" {
		return p.title
	}
	return p.title + "  " + p.filter
}

// modelRows are the registry's models as the picker draws them: the canonical
// "provider:id" a log entry and the status line both name, then who actually serves it when
// the endpoint is a proxy, then what choosing it costs, the context window and the price of
// a million tokens in and out.
func modelRows(models []provider.Model) []pickRow {
	out := make([]pickRow, 0, len(models))
	for _, mo := range models {
		id := mo.Ref.String()
		// The upstream after the id: an endpoint that fronts several says which is which,
		// and the filter reads the row, so typing "openrouter" narrows to those.
		out = append(out, pickRow{id: id, text: join(id, mo.Upstream, tokenCount(mo.ContextWindow), perMillion(mo.Pricing))})
	}
	return out
}

// sessionRows are session.list as the picker draws it, newest first as the store returns
// it: when it was opened, what it works on, its model and whether it is a fork or the
// child of another session's tool call.
func sessionRows(sums []session.Summary) []pickRow {
	out := make([]pickRow, 0, len(sums))
	for _, s := range sums {
		var tags []string
		if s.Forked {
			tags = append(tags, "fork")
		}
		if s.ParentSessionID != "" {
			tags = append(tags, "child")
		}
		out = append(out, pickRow{
			id:   s.ID.String(),
			text: join(s.OpenedAt.Local().Format("2006-01-02 15:04"), s.Workspace.Root, s.Model.String(), strings.Join(tags, " ")),
		})
	}
	return out
}

// join spaces the parts of a row, dropping the ones with nothing to say so a model with
// no prices does not draw the gap where they would have been.
func join(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "  ")
}

// tokenCount is a context window in the units a model is sold in. Zero is unknown, which
// the row leaves out rather than drawing as none.
func tokenCount(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n >= 1_000_000:
		return strconv.FormatInt(n/1_000_000, 10) + "M ctx"
	case n >= 1000:
		return strconv.FormatInt(n/1000, 10) + "k ctx"
	}
	return strconv.FormatInt(n, 10) + " ctx"
}

// perMillion prices a million input and a million output tokens, which is how a provider
// quotes a model. It is Pricing.Cost of exactly that usage, so the picker prices a model
// the same way the status line prices a session. Prices the registry does not carry leave
// the row without them.
func perMillion(p provider.Pricing) string {
	in, ok := p.Cost(session.Usage{Input: 1_000_000})
	if !ok {
		return ""
	}
	out, ok := p.Cost(session.Usage{Output: 1_000_000})
	if !ok {
		return ""
	}
	return "$" + cents(in) + "/$" + cents(out) + " per Mtok"
}

// openModelPicker opens the model picker on the session's own model and asks for a fresh
// registry: the design discovers models on session open, on picker open and on a
// model-not-found, and never on a timer. The picker opens on the snapshot in hand and
// takes the answer when it lands, so it is never waiting on the network to draw.
func (m *Model) openModelPicker() tea.Cmd {
	if m.offline() {
		return nil
	}
	m.pick = &picker{kind: pickerModel, title: "model", rows: modelRows(m.models)}
	m.pick.point(m.session.Model.String())
	return m.call(protocol.MethodRegistryRefresh, nil)
}

// openSessionPicker opens the session picker on what session.list answered. A store with
// nothing in it says so rather than standing an empty list up with no way out but Esc.
func (m *Model) openSessionPicker(sums []session.Summary) {
	if len(sums) == 0 {
		m.note(levelInfo, "no sessions to resume")
		return
	}
	m.pick = &picker{kind: pickerSession, title: "session", rows: sessionRows(sums)}
	m.pick.point(m.session.SessionID)
}

// openScopePicker opens the model list as a set to choose: the models ctrl+p and ctrl+n
// cycle through. It opens on whatever the scope is now, so a person sees what they already
// chose rather than an empty list (ADR 0020).
func (m *Model) openScopePicker(filter string) tea.Cmd {
	if m.offline() {
		return nil
	}
	chosen := make(map[string]bool, len(m.scope))
	for _, ref := range m.scope {
		chosen[ref.String()] = true
	}
	m.pick = &picker{
		kind: pickerScope, title: "cycle these models  (space toggles, enter confirms)",
		rows: modelRows(m.models), chosen: chosen, filter: filter,
	}
	m.pick.point(m.session.Model.String())
	return m.call(protocol.MethodRegistryRefresh, nil)
}

// pickerKey routes one key while a picker is up. The picker owns the keyboard: the
// tui.select actions drive it, anything printable filters it, and everything else is
// swallowed, so no key reaches the turn or the editor behind it. Enter carries both
// tui.input.submit and tui.select.confirm and Esc both app.interrupt and
// tui.select.cancel; the picker's own action is the one that wins while it stands.
func (m *Model) pickerKey(k tea.KeyPressMsg) tea.Cmd {
	for _, a := range m.keys.Match(tea.Key(k)) {
		switch a {
		case keys.TUISelectUp:
			m.pick.move(-1)
			return nil
		case keys.TUISelectDown:
			m.pick.move(1)
			return nil
		case keys.TUISelectPageUp:
			m.pick.move(-m.pickerHeight(len(m.pick.visible())))
			return nil
		case keys.TUISelectPageDown:
			m.pick.move(m.pickerHeight(len(m.pick.visible())))
			return nil
		case keys.TUISelectConfirm:
			return m.confirmPick()
		case keys.TUIInputTab:
			// Tab toggles a row in a picker that takes a set. Space does too, below: it is
			// what pi uses, and a model id never needs one in a filter. Neither is a new
			// action id, so ADR 0013 decision 5's closed set stands.
			if m.pick.multi() {
				m.pick.toggle()
				return nil
			}
		case keys.TUISelectCancel:
			m.pick = nil
			return nil
		}
	}
	if m.pick.multi() && k.Code == ' ' && k.Mod == 0 {
		m.pick.toggle()
		return nil
	}
	m.pick.filterKey(tea.Key(k))
	return nil
}

// confirmPick takes the row under the cursor and closes the picker. A filter that matches
// nothing confirms nothing.
func (m *Model) confirmPick() tea.Cmd {
	p := m.pick
	m.pick = nil
	row, ok := p.current()
	if !ok {
		return nil
	}
	if p.kind == pickerScope {
		// A confirm takes the set as it stands, an empty one included: emptying the scope
		// is how a person goes back to cycling the whole registry.
		m.setScope(p.chosen)
		return nil
	}
	switch p.kind {
	case pickerModel:
		ref, ok := session.ParseModelRef(row.id)
		if !ok {
			return nil
		}
		return m.setSessionModel(ref)
	case pickerSession:
		return m.resumeSession(row.id)
	}
	return nil
}

// pickerHeight is how many rows the list draws: at most half the frame so the transcript
// stays visible above it, at least three so a filtered list still says what it matched,
// and never more rows than there are.
func (m *Model) pickerHeight(n int) int {
	return min(n, max(m.height/2, pickerMinRows))
}

// pickerView is the picker where the editor would have been: the title, then the filtered
// rows with the cursor's own accented. Every line goes through the same sanitizer and the
// same width clamp a widget's line does: a row draws a model id and a workspace path,
// neither of which this client wrote.
func (m *Model) pickerView() []string {
	p := m.pick
	rows := p.visible()
	h := m.pickerHeight(len(rows))
	p.window(h)
	out := make([]string, 0, h+1)
	out = append(out, m.clamp(m.th.Style(theme.RoleMuted).Render(spanText(p.head()))))
	for i := range h {
		j := p.top + i
		if j >= len(rows) {
			break
		}
		role, mark := theme.RoleText, unselectedMark
		if j == p.sel {
			role, mark = theme.RoleAccent, selectedMark
		}
		if p.multi() {
			// The cursor's mark, then whether this row is in the set: a person moving
			// through the list has to see both at once.
			mark += outSet
			if p.chosen[rows[j].id] {
				mark = mark[:len(mark)-len(outSet)] + inSet
			}
		}
		out = append(out, m.clamp(m.th.Style(role).Render(mark+spanText(rows[j].text))))
	}
	return out
}
