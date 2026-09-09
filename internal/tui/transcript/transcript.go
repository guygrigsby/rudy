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
	// md is the glamour renderer for the current width, built on first use and dropped
	// by SetWidth.
	md mdCache
}

// New opens an empty transcript.
func New(o Options, th theme.Theme) *Transcript {
	return &Transcript{opts: o, th: th, byKey: make(map[string]*Row)}
}

// Rows are the transcript's rows, in order. The Row pointers are the transcript's own:
// a caller may read them and set Expanded, and must not reorder the slice.
func (t *Transcript) Rows() []*Row { return slices.Clone(t.rows) }

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
		t.turn, t.liveTurn = e.ID.String(), ""
		return t.addOnce(&Row{
			Key: e.ID.String(), Kind: RowUser, TurnID: e.ID.String(),
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
	if len(built) == 0 || t.settled(built) {
		return nil
	}
	changed := make([]string, 0, len(built))
	insert := make([]*Row, 0, len(built))
	replaced := make(map[string]bool, len(built))
	for _, r := range built {
		changed = append(changed, r.Key)
		old := t.byKey[r.Key]
		if old == nil {
			insert = append(insert, r)
			t.byKey[r.Key] = r
			continue
		}
		r.Expanded, r.Decision, r.Result = old.Expanded, old.Decision, old.Result
		*old = *r
		replaced[r.Key] = true
	}
	t.rows = t.placeCommitted(turn, insert, replaced)
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

// placeCommitted drops the turn's live text and thinking rows and puts insert where the
// first of them stood, or ahead of the first row replaced in place, or at the end.
func (t *Transcript) placeCommitted(turn string, insert []*Row, replaced map[string]bool) []*Row {
	out := make([]*Row, 0, len(t.rows)+len(insert))
	done := false
	for _, r := range t.rows {
		switch {
		case r.Live && r.Kind != RowTool && r.TurnID == turn:
			delete(t.byKey, r.Key)
			if !done {
				out, done = append(out, insert...), true
			}
			continue
		case !done && replaced[r.Key]:
			out, done = append(out, insert...), true
		}
		out = append(out, r)
	}
	if !done {
		out = append(out, insert...)
	}
	return out
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

// Commit renders the rows of turnID in order, removes them and returns the lines, for
// the client to print above the live region. An unknown turn returns nil.
func (t *Transcript) Commit(turnID string) []string {
	var lines []string
	var prev *Row
	kept := make([]*Row, 0, len(t.rows))
	going := make([]*Row, 0, len(t.rows))
	for _, r := range t.rows {
		if r.TurnID != turnID {
			kept = append(kept, r)
			continue
		}
		going = append(going, r)
		// Render before anything leaves the index: a row can look at its neighbours,
		// the way a tool row checks whether a permission question stands in its place.
		rl := t.Render(r)
		if len(rl) == 0 {
			continue
		}
		lines = append(lines, t.gap(prev, r)...)
		lines = append(lines, rl...)
		prev = r
	}
	if len(going) == 0 {
		return nil
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
