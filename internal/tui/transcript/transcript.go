package transcript

import (
	"fmt"
	"slices"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tui/theme"
)

// Key prefixes. A live row's key is provisional: it holds the row's place until the
// entry that commits it arrives. Tool ids come from a provider and prompt keys from a
// tool id, so neither can collide with these.
const (
	livePrefix     = "live/"
	thinkingSuffix = "/thinking"
	promptPrefix   = "prompt/"
)

func liveKey(turnID string) string      { return livePrefix + turnID }
func thinkingKey(turnID string) string  { return livePrefix + turnID + thinkingSuffix }
func promptKey(toolUseID string) string { return promptPrefix + toolUseID }

// Transcript holds the rows of a session in order and folds entries, stream deltas and
// permission questions into them. It is owned by one goroutine, the client's update
// loop, and takes no lock.
type Transcript struct {
	opts  Options
	th    theme.Theme
	rows  []*Row
	byKey map[string]*Row
	// turn is the id of the nearest preceding user_message, the turn every entry-derived
	// row after it belongs to.
	turn string
	// liveTurn is the turn the deltas seen so far were announced with. It wins over turn
	// for a message that commits a live row, so the committed rows land in the turn the
	// stream said they were in.
	liveTurn string
	// committed are the turns Commit has already taken off screen. A row that arrives
	// after its turn was committed (a note appended between turns) is one nothing would
	// ever print otherwise, and this is what lets a client recognize it.
	committed map[string]bool
	// md is the glamour renderer for the current width, built on first use and dropped
	// by SetWidth.
	md mdCache
}

// New opens an empty transcript.
func New(o Options, th theme.Theme) *Transcript {
	return &Transcript{opts: o, th: th, byKey: make(map[string]*Row), committed: make(map[string]bool)}
}

// Rows are the transcript's rows, in order. The Row pointers are the transcript's own:
// a caller may read them and set Expanded, and must not reorder the slice.
func (t *Transcript) Rows() []*Row { return slices.Clone(t.rows) }

// Turn is the turn rows are being added to: the nearest preceding user_message, or ""
// before the first one. A steer message continues the turn it was sent into, so this is
// not always the last entry's own id.
func (t *Transcript) Turn() string { return t.turn }

// Turns are the turn ids on screen, oldest first, one entry per turn. It is what lets a
// client commit the turns behind the one in flight without knowing how rows are keyed.
func (t *Transcript) Turns() []string {
	var out []string
	for _, r := range t.rows {
		if !slices.Contains(out, r.TurnID) {
			out = append(out, r.TurnID)
		}
	}
	return out
}

// SetShowThinking changes whether a model's thinking text becomes a row, for the rows
// built from here on. Rows already on screen keep what they were built with and rows
// already committed to scrollback cannot be redrawn at all, so this reads forward only:
// turning thinking on shows the next block the model thinks, not the ones it already did.
func (t *Transcript) SetShowThinking(show bool) { t.opts.ShowThinking = show }

// SetWidth changes the width rows render to.
func (t *Transcript) SetWidth(w int) {
	if w == t.opts.Width {
		return
	}
	t.opts.Width = w
	t.md = mdCache{}
}

// Toggle expands or collapses the tool row named by key and reports whether it is now
// expanded. Any other key, or none, is false.
func (t *Transcript) Toggle(key string) bool {
	r := t.byKey[key]
	if r == nil || r.Kind != RowTool {
		return false
	}
	r.Expanded = !r.Expanded
	return r.Expanded
}

// Apply folds one entry in and returns the keys of the rows it added or changed. An
// entry that changes nothing on screen, a replay of one already folded in included,
// returns nil.
func (t *Transcript) Apply(e session.Entry) []string {
	switch p := e.Payload.(type) {
	case session.UserMessage:
		// A steer message continues the turn it was sent into rather than opening one:
		// the server keeps a steered turn under the id of the user_message that started
		// it, so this row and everything appended after it commit with that turn. A
		// steer arriving with no turn in hand (a log replayed from the middle of one)
		// starts one, since there is nothing else to hang it on.
		if p.Source != session.SourceSteer || t.turn == "" {
			t.turn = e.ID.String()
		}
		t.liveTurn = ""
		return t.addOnce(&Row{
			Key: e.ID.String(), Kind: RowUser, TurnID: t.turn,
			Entry: e, Text: session.TextOf(p.Content),
		})
	case session.AssistantMessage:
		return t.applyAssistant(e, p)
	case session.PermissionDecision:
		r := t.toolRow(p.ToolUseID)
		if r == nil {
			return t.addOnce(t.orphan(e, "decision"))
		}
		if r.Decision != nil {
			return nil
		}
		r.Decision = &p
		return []string{r.Key}
	case session.ToolResult:
		r := t.toolRow(p.ToolUseID)
		if r == nil {
			return t.addOnce(t.orphan(e, "result"))
		}
		if r.Result != nil {
			return nil
		}
		r.Result = &p
		return []string{r.Key}
	case session.Note, session.Compaction, session.TurnInterrupted, session.TurnFailed:
		return t.addOnce(&Row{Key: e.ID.String(), Kind: RowMarker, TurnID: t.turn, Entry: e})
	}
	// session_opened, fork_point and the model, mode, thinking and title changes are
	// state the header and status line read, not things on screen.
	return nil
}

// orphan is the marker row a decision or a result for a tool_use this transcript never
// saw becomes. Dropping it silently would lose the only sign that the log and the screen
// disagree.
func (t *Transcript) orphan(e session.Entry, what string) *Row {
	return &Row{Key: e.ID.String(), Kind: RowMarker, TurnID: t.turn, Entry: e, Text: what}
}

// toolRow is the tool row keyed by a tool_use id, or nil.
func (t *Transcript) toolRow(toolUseID string) *Row {
	if r := t.byKey[toolUseID]; r != nil && r.Kind == RowTool {
		return r
	}
	return nil
}

// applyAssistant turns one assistant_message into rows: one per text block, one per
// thinking block when they are shown, and one per tool_use. The turn's live rows give
// way to them, live tool rows replaced where they stand so an expansion survives.
func (t *Transcript) applyAssistant(e session.Entry, m session.AssistantMessage) []string {
	turn := t.turn
	if t.liveTurn != "" {
		turn = t.liveTurn
	}
	built := make([]*Row, 0, len(m.Content))
	for i, b := range m.Content {
		key := fmt.Sprintf("%s/%d", e.ID, i)
		switch b.Type {
		case session.BlockText:
			built = append(built, &Row{Key: key, Kind: RowAssistant, TurnID: turn, Entry: e, Text: b.Text})
		case session.BlockThinking:
			if t.opts.ShowThinking {
				built = append(built, &Row{Key: key, Kind: RowAssistant, TurnID: turn, Entry: e, Text: b.Text, Thinking: true})
			}
		case session.BlockToolUse:
			built = append(built, &Row{Key: b.ID, Kind: RowTool, TurnID: turn, Entry: e, ToolUse: b})
		}
	}
	if len(built) == 0 {
		// Nothing on screen comes of this message, but the turn's live rows were
		// standing in for it and must not stay Live. They drew nothing (a live text row
		// with text always yields text blocks, and a hidden thinking row renders
		// empty), so sweeping them adds or changes no key.
		t.rows = t.placeCommitted(turn, nil)
		return nil
	}
	if t.settled(built) {
		return nil
	}
	changed := make([]string, 0, len(built))
	for i, r := range built {
		changed = append(changed, r.Key)
		old := t.byKey[r.Key]
		if old == nil {
			t.byKey[r.Key] = r
			continue
		}
		// A live tool row is replaced through its pointer, keeping what the user opened
		// and anything already absorbed into it, and it is that pointer that takes the
		// row's place in the committed order.
		r.Expanded, r.Decision, r.Result = old.Expanded, old.Decision, old.Result
		*old = *r
		built[i] = old
	}
	t.rows = t.placeCommitted(turn, built)
	return changed
}

// settled reports whether every row this message makes is already on screen, committed.
// Replaying a log entry a second time must not move anything.
func (t *Transcript) settled(built []*Row) bool {
	for _, r := range built {
		if old := t.byKey[r.Key]; old == nil || old.Live {
			return false
		}
	}
	return true
}

// placeCommitted puts built on screen in block order. One assistant message's rows are
// one contiguous group, so the turn's live text and thinking rows go, any row a tool row
// is replacing is lifted out of where the stream left it, and the whole of built goes in
// at the first position the group held. Placing each row individually is what keeps
// [text, tool_use, text] in that order: anchoring the group and appending would put both
// text rows in front of the tool row.
//
// built may be empty, which is the sweep on its own: an assistant message that draws
// nothing still ends the turn's live rows.
func (t *Transcript) placeCommitted(turn string, built []*Row) []*Row {
	member := make(map[string]bool, len(built))
	for _, r := range built {
		member[r.Key] = true
	}
	out := make([]*Row, 0, len(t.rows)+len(built))
	anchor := -1
	for _, r := range t.rows {
		live := r.Live && r.Kind != RowTool && r.TurnID == turn
		if !member[r.Key] && !live {
			out = append(out, r)
			continue
		}
		if anchor < 0 {
			anchor = len(out)
		}
		if live {
			delete(t.byKey, r.Key)
		}
	}
	if anchor < 0 {
		anchor = len(out)
	}
	return slices.Insert(out, anchor, built...)
}

// Delta folds one streamed part into the turn's live rows.
func (t *Transcript) Delta(turnID string, p provider.Part) {
	t.liveTurn = turnID
	switch p.Type {
	case provider.PartTextDelta:
		t.live(liveKey(turnID), turnID, false).Text += p.Text
	case provider.PartThinkingDelta:
		// The row exists either way: while thinking is hidden it renders nothing and is
		// only the flag that says the model is thinking.
		r := t.live(thinkingKey(turnID), turnID, true)
		if t.opts.ShowThinking {
			r.Text += p.Text
		}
	case provider.PartToolUseStart:
		if t.byKey[p.ID] != nil {
			return
		}
		t.add(&Row{
			Key: p.ID, Kind: RowTool, TurnID: turnID, Live: true,
			ToolUse: session.Block{Type: session.BlockToolUse, ID: p.ID, Name: p.Name},
		})
	case provider.PartToolUseDelta:
		if r := t.byKey[p.ID]; r != nil && r.Live {
			r.ToolUse.Input = append(r.ToolUse.Input, p.Text...)
		}
	case provider.PartToolUseEnd, provider.PartThinkingSignature, provider.PartUsage, provider.PartStop:
		// Nothing on screen moves. A tool row summarizes the input bytes it has, which
		// the end only declares whole; a signature, a usage total and a stop reason
		// belong to the entry the turn will append, not to a row.
	}
}

// live is the turn's live assistant row of one flavour, created on first use.
func (t *Transcript) live(key, turnID string, thinking bool) *Row {
	if r := t.byKey[key]; r != nil {
		return r
	}
	r := &Row{Key: key, Kind: RowAssistant, TurnID: turnID, Live: true, Thinking: thinking}
	t.add(r)
	return r
}

// Prompt shows a permission question in the place of its tool row, or at the end when
// the tool_use block has not arrived yet.
func (t *Transcript) Prompt(p protocol.PermissionRequested) {
	key := promptKey(p.ToolUseID)
	if t.byKey[key] != nil {
		return
	}
	r := &Row{Key: key, Kind: RowPrompt, TurnID: p.TurnID, Prompt: &p}
	if i := t.indexOf(p.ToolUseID); i >= 0 {
		t.byKey[key] = r
		t.rows = slices.Insert(t.rows, i, r)
		return
	}
	t.add(r)
}

// Answered removes a permission question. The tool row it stood in front of stays.
func (t *Transcript) Answered(toolUseID string) {
	if i := t.indexOf(promptKey(toolUseID)); i >= 0 {
		t.removeAt(i)
	}
}

// Line is one rendered line and the row it came from. A blank line between two blocks
// belongs to no row, so its Row is nil. It is what lets a client map a mouse click at a
// screen line back to the row under it.
type Line struct {
	Text string
	Row  *Row
}

// Layout renders every row on screen in order, gaps included: what a client draws in the
// region it owns, and what it traces a click through.
func (t *Transcript) Layout() []Line { return t.layout(t.rows) }

// Wrap renders text at the transcript's width in one theme role, wrapped and sanitized
// the way a marker row's text is, and adds no row. It is how a client draws its own
// chrome, a notice for instance, in the same column and with the same safety as the rows
// above it.
func (t *Transcript) Wrap(role theme.Role, text string) []string {
	return t.wrap(role, 0, text)
}

// layout renders rows in order with the gaps between them, one Line per drawn line. A row
// that renders nothing takes no line and no gap.
func (t *Transcript) layout(rows []*Row) []Line {
	var out []Line
	var prev *Row
	for _, r := range rows {
		rl := t.Render(r)
		if len(rl) == 0 {
			continue
		}
		for _, blank := range t.gap(prev, r) {
			out = append(out, Line{Text: blank})
		}
		for _, l := range rl {
			out = append(out, Line{Text: l, Row: r})
		}
		prev = r
	}
	return out
}

// Commit renders the rows of turnID in order, removes them and returns the lines, for
// the client to print above the live region. An unknown turn returns nil. The turn is
// remembered as committed either way: what says a later row is late is that its turn has
// already gone, whether or not it had rows of its own at the time.
func (t *Transcript) Commit(turnID string) []string {
	t.committed[turnID] = true
	kept := make([]*Row, 0, len(t.rows))
	going := make([]*Row, 0, len(t.rows))
	for _, r := range t.rows {
		if r.TurnID == turnID {
			going = append(going, r)
			continue
		}
		kept = append(kept, r)
	}
	if len(going) == 0 {
		return nil
	}
	// Render before anything leaves the index: a row can look at its neighbours, the way
	// a tool row checks whether a permission question stands in its place.
	laid := t.layout(going)
	lines := make([]string, 0, len(laid))
	for _, l := range laid {
		lines = append(lines, l.Text)
	}
	for _, r := range going {
		delete(t.byKey, r.Key)
	}
	t.rows = kept
	if t.liveTurn == turnID {
		t.liveTurn = ""
	}
	return lines
}

// CommitLate is Commit for the rows of turns that have already been committed: an entry
// that arrives after its turn went to scrollback, a note appended between turns, a result
// for a tool_use whose row has gone. They are committed in the order they stand in, so
// they reach scrollback behind the turn they belong to rather than sitting in the live
// region for the rest of the session. Nothing to commit returns nil, which is every call
// while a turn is still on screen.
func (t *Transcript) CommitLate() []string {
	var late []string
	for _, r := range t.rows {
		if t.committed[r.TurnID] && !slices.Contains(late, r.TurnID) {
			late = append(late, r.TurnID)
		}
	}
	var out []string
	for _, id := range late {
		out = append(out, t.Commit(id)...)
	}
	return out
}

// gap is the blank lines between two adjacent rows. The design puts one blank line
// around an assistant block and nothing between two tool rows, so the gap belongs to a
// pair where either side is an assistant row.
func (t *Transcript) gap(prev, next *Row) []string {
	if prev == nil || t.opts.BlockGap <= 0 {
		return nil
	}
	if prev.Kind != RowAssistant && next.Kind != RowAssistant {
		return nil
	}
	return make([]string, t.opts.BlockGap)
}

// addOnce adds r unless its key is already on screen, and returns the keys it changed.
func (t *Transcript) addOnce(r *Row) []string {
	if t.byKey[r.Key] != nil {
		return nil
	}
	t.add(r)
	return []string{r.Key}
}

func (t *Transcript) add(r *Row) {
	t.byKey[r.Key] = r
	t.rows = append(t.rows, r)
}

func (t *Transcript) indexOf(key string) int {
	return slices.IndexFunc(t.rows, func(r *Row) bool { return r.Key == key })
}

func (t *Transcript) removeAt(i int) {
	delete(t.byKey, t.rows[i].Key)
	t.rows = slices.Delete(t.rows, i, i+1)
}
