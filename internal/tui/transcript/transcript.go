// SPDX-License-Identifier: AGPL-3.0-or-later

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
	// calls maps a subagent's session id to the tool_use id of the agent call that opened
	// it (ADR 0028 decision 6). Set from the child's own session_opened, the first
	// notification that names it, and read by a client deciding whether a notification for
	// a session that is not this one belongs to a subagent it is already showing.
	calls map[string]string
	// toolTurn maps every tool_use id this transcript has ever built a row for to the turn
	// it belongs to, and is never pruned, unlike the row itself: callTurn reads it once a
	// call's own row has gone to scrollback, which is what lets a subagent's straggler
	// entry, arriving after the parent's turn already committed, still carry the right
	// TurnID for CommitLate to find rather than stranding it in the live region forever.
	toolTurn map[string]string
}

// origin is where a row came from when it is not the session this transcript renders: a
// subagent's session id and the tool_use that opened it (ADR 0028 decision 6). The zero
// value is this transcript's own session, which is why every row built without one keeps
// behaving exactly as it did before origin existed.
type origin struct {
	sessionID       string
	parentToolUseID string
}

// key prefixes k with the origin's session, so a subagent's row can never collide with the
// parent's or with a sibling subagent's, even when the id underneath (an entry id, a
// tool_use id) happens to repeat across sessions.
func (o origin) key(k string) string {
	if o.sessionID == "" {
		return k
	}
	return o.sessionID + "/" + k
}

// stamp marks r with the origin, which is what layout indents by and Commit sweeps by.
func (o origin) stamp(r *Row) *Row {
	r.SessionID, r.ParentToolUseID = o.sessionID, o.parentToolUseID
	return r
}

// New opens an empty transcript.
func New(o Options, th theme.Theme) *Transcript {
	return &Transcript{opts: o, th: th, byKey: make(map[string]*Row), committed: make(map[string]bool)}
}

// Rows are the transcript's rows, in order. The Row pointers are the transcript's own:
// a caller may read them and set Expanded, and must not reorder the slice.
func (t *Transcript) Rows() []*Row { return slices.Clone(t.rows) }

// RecordAgentCall remembers that sessionID is the subagent toolUseID's agent call opened, so
// a later notification tagged with sessionID can be routed to that call's own rows
// (ADR 0028 decision 6). A session already recorded keeps its first mapping: the call that
// opened a session cannot change once it has.
func (t *Transcript) RecordAgentCall(sessionID, toolUseID string) {
	if _, ok := t.calls[sessionID]; ok {
		return
	}
	if t.calls == nil {
		t.calls = make(map[string]string)
	}
	t.calls[sessionID] = toolUseID
}

// AgentCallFor is the tool_use id sessionID's rows render under, and whether one is known.
func (t *Transcript) AgentCallFor(sessionID string) (string, bool) {
	id, ok := t.calls[sessionID]
	return id, ok
}

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
		kind := RowUser
		if p.Source == session.SourceShell {
			kind = RowShell
		}
		return t.addOnce(&Row{
			Key: e.ID.String(), Kind: kind, TurnID: t.turn,
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
func (t *Transcript) toolRow(toolUseID string) *Row { return t.rowIfTool(toolUseID) }

// rowIfTool is the row keyed key, if it is a tool row.
func (t *Transcript) rowIfTool(key string) *Row {
	if r := t.byKey[key]; r != nil && r.Kind == RowTool {
		return r
	}
	return nil
}

// ApplyFrom is Apply for an entry that arrived tagged with a subagent's session id
// (ADR 0028 decision 6): every row it makes is stamped with that origin and inserted
// immediately after the last row already carrying the same parentToolUseID, so a child's
// work stays grouped under the call that opened it in the order it arrived rather than at
// the end of the transcript.
//
// A child's own turn is never this transcript's turn: every row here is stamped with the
// agent call's own TurnID, read off the call's own row, rather than anything the child's
// log carries, which is what lets Commit sweep a subagent's rows off screen together with
// the call that opened it. Nothing here reads or writes t.turn or t.liveTurn: those track
// this transcript's own turn, and a child's is not it.
func (t *Transcript) ApplyFrom(sessionID, parentToolUseID string, e session.Entry) []string {
	o := origin{sessionID: sessionID, parentToolUseID: parentToolUseID}
	turn := t.callTurn(parentToolUseID)
	switch p := e.Payload.(type) {
	case session.UserMessage:
		kind := RowUser
		if p.Source == session.SourceShell {
			kind = RowShell
		}
		return t.addChild(o, o.stamp(&Row{
			Key: o.key(e.ID.String()), Kind: kind, TurnID: turn,
			Entry: e, Text: session.TextOf(p.Content),
		}))
	case session.AssistantMessage:
		return t.applyAssistantFrom(o, turn, e, p)
	case session.PermissionDecision:
		r := t.childToolRow(o, p.ToolUseID)
		if r == nil {
			return t.addChild(o, t.orphanFrom(o, turn, e, "decision"))
		}
		if r.Decision != nil {
			return nil
		}
		r.Decision = &p
		return []string{r.Key}
	case session.ToolResult:
		r := t.childToolRow(o, p.ToolUseID)
		if r == nil {
			return t.addChild(o, t.orphanFrom(o, turn, e, "result"))
		}
		if r.Result != nil {
			return nil
		}
		r.Result = &p
		return []string{r.Key}
	case session.Note, session.Compaction, session.TurnInterrupted, session.TurnFailed:
		return t.addChild(o, o.stamp(&Row{Key: o.key(e.ID.String()), Kind: RowMarker, TurnID: turn, Entry: e}))
	}
	// session_opened, fork_point and the model, mode, thinking and title changes are state
	// the model reads off the entry directly (a session_opened is what records the call a
	// child answers in the first place); none of them is a row.
	return nil
}

// orphanFrom is orphan for a subagent's decision or result whose own tool_use this
// transcript never saw.
func (t *Transcript) orphanFrom(o origin, turn string, e session.Entry, what string) *Row {
	return o.stamp(&Row{Key: o.key(e.ID.String()), Kind: RowMarker, TurnID: turn, Entry: e, Text: what})
}

// childToolRow is the tool row a subagent's own tool_use id names, or nil.
func (t *Transcript) childToolRow(o origin, toolUseID string) *Row {
	return t.rowIfTool(o.key(toolUseID))
}

// callTurn is the TurnID a subagent's rows are stamped with: the agent call's own row's, not
// the child's. The row itself may already be gone to scrollback by the time a straggler
// entry asks (a child answering after the parent's turn has committed), so this falls back
// to toolTurn, recorded when the row was built and never pruned, rather than "", which
// CommitLate would never recognize as a turn it has already committed and would strand the
// straggler in the live region for the rest of the session.
func (t *Transcript) callTurn(parentToolUseID string) string {
	if r := t.byKey[parentToolUseID]; r != nil {
		return r.TurnID
	}
	return t.toolTurn[parentToolUseID]
}

// recordToolTurn remembers that toolUseID's row belongs to turn, kept even after Commit
// takes the row itself off screen (see callTurn).
func (t *Transcript) recordToolTurn(toolUseID, turn string) {
	if t.toolTurn == nil {
		t.toolTurn = make(map[string]string)
	}
	t.toolTurn[toolUseID] = turn
}

// addChild inserts r under its own origin unless its key is already on screen, a replayed
// entry, and reports the key it changed.
func (t *Transcript) addChild(o origin, r *Row) []string {
	if t.byKey[r.Key] != nil {
		return nil
	}
	t.insertAfterGroup(o.parentToolUseID, r)
	return []string{r.Key}
}

// insertAfterGroup adds r to the transcript immediately after the last row already carrying
// parentToolUseID: the call's own row, or the last row it has already gained. Appending at
// the end instead would scatter a subagent's rows behind whatever the parent said in
// between. A call whose own row has already gone to scrollback (a straggler entry for a
// turn that already committed) falls back to the end, which CommitLate then walks the same
// as any other late row.
func (t *Transcript) insertAfterGroup(parentToolUseID string, r *Row) {
	t.byKey[r.Key] = r
	end := len(t.rows)
	for i, row := range t.rows {
		if row.Key == parentToolUseID || row.ParentToolUseID == parentToolUseID {
			end = i + 1
		}
	}
	t.rows = slices.Insert(t.rows, end, r)
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
			t.recordToolTurn(b.ID, turn)
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

// applyAssistantFrom is applyAssistant for a subagent's own assistant_message: the same
// per-block row shapes and the same live-tool-row replacement through its pointer, but
// grouped under parentToolUseID (placeChild) rather than anchored on this transcript's own
// turn (placeCommitted). Filtering by turn alone would not do: a sibling agent call
// dispatched in the very same parent turn stamps its own rows with that same TurnID, and a
// turn-only sweep would take its live rows along with this call's.
func (t *Transcript) applyAssistantFrom(o origin, turn string, e session.Entry, m session.AssistantMessage) []string {
	built := make([]*Row, 0, len(m.Content))
	for i, b := range m.Content {
		key := o.key(fmt.Sprintf("%s/%d", e.ID, i))
		switch b.Type {
		case session.BlockText:
			built = append(built, o.stamp(&Row{Key: key, Kind: RowAssistant, TurnID: turn, Entry: e, Text: b.Text}))
		case session.BlockThinking:
			if t.opts.ShowThinking {
				built = append(built, o.stamp(&Row{Key: key, Kind: RowAssistant, TurnID: turn, Entry: e, Text: b.Text, Thinking: true}))
			}
		case session.BlockToolUse:
			built = append(built, o.stamp(&Row{Key: o.key(b.ID), Kind: RowTool, TurnID: turn, Entry: e, ToolUse: b}))
		}
	}
	if len(built) == 0 {
		t.rows = t.placeChild(o.parentToolUseID, nil)
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
		r.Expanded, r.Decision, r.Result = old.Expanded, old.Decision, old.Result
		*old = *r
		built[i] = old
	}
	t.rows = t.placeChild(o.parentToolUseID, built)
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
	return t.placeGroup(built,
		func(r *Row) bool { return r.Live && r.Kind != RowTool && r.TurnID == turn },
		func(out []*Row) int { return len(out) },
	)
}

// placeChild is placeCommitted for a subagent's rows: built goes in immediately after the
// last row of parentToolUseID's own group instead of at the end, and only that group's own
// live placeholder is swept, by ParentToolUseID rather than by turn (see applyAssistantFrom
// for why turn alone is not enough).
func (t *Transcript) placeChild(parentToolUseID string, built []*Row) []*Row {
	return t.placeGroup(built,
		func(r *Row) bool { return r.Live && r.Kind != RowTool && r.ParentToolUseID == parentToolUseID },
		func(out []*Row) int {
			end := len(out)
			for i, r := range out {
				if r.Key == parentToolUseID || r.ParentToolUseID == parentToolUseID {
					end = i + 1
				}
			}
			return end
		},
	)
}

// placeGroup puts built on screen in block order: one message's rows are one contiguous
// group, so any row sweep marks as this group's own live placeholder is lifted out and the
// whole of built goes in at the first position such a row held, placed one at a time so
// [text, tool_use, text] keeps that order rather than being anchored and appended. Nothing
// swept means the transcript had no live placeholder for this group yet, so fallback decides
// where the group lands instead: at the end for the transcript's own turn, right after the
// call's own row for a subagent (see placeCommitted and placeChild).
func (t *Transcript) placeGroup(built []*Row, sweep func(*Row) bool, fallback func(out []*Row) int) []*Row {
	member := make(map[string]bool, len(built))
	for _, r := range built {
		member[r.Key] = true
	}
	out := make([]*Row, 0, len(t.rows)+len(built))
	anchor := -1
	for _, r := range t.rows {
		live := sweep(r)
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
		anchor = fallback(out)
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

// DeltaFrom is Delta for a part streamed by a subagent. The live placeholder it builds is
// keyed by the call it belongs to rather than by a turn id: one agent call has at most one
// turn in flight at a time, since the agent tool submits its prompt once and waits, so the
// call's own tool_use id is a stable key for the whole of it. It is inserted under the call
// the same way ApplyFrom's committed rows are (insertAfterGroup), so a subagent's answer
// shows up where it will land as it streams rather than only once it is whole.
func (t *Transcript) DeltaFrom(sessionID, parentToolUseID string, p provider.Part) {
	o := origin{sessionID: sessionID, parentToolUseID: parentToolUseID}
	turn := t.callTurn(parentToolUseID)
	switch p.Type {
	case provider.PartTextDelta:
		t.liveChild(o, turn, o.key(liveKey(parentToolUseID)), false).Text += p.Text
	case provider.PartThinkingDelta:
		r := t.liveChild(o, turn, o.key(thinkingKey(parentToolUseID)), true)
		if t.opts.ShowThinking {
			r.Text += p.Text
		}
	case provider.PartToolUseStart:
		key := o.key(p.ID)
		if t.byKey[key] != nil {
			return
		}
		t.insertAfterGroup(o.parentToolUseID, o.stamp(&Row{
			Key: key, Kind: RowTool, TurnID: turn, Live: true,
			ToolUse: session.Block{Type: session.BlockToolUse, ID: p.ID, Name: p.Name},
		}))
	case provider.PartToolUseDelta:
		if r := t.byKey[o.key(p.ID)]; r != nil && r.Live {
			r.ToolUse.Input = append(r.ToolUse.Input, p.Text...)
		}
	case provider.PartToolUseEnd, provider.PartThinkingSignature, provider.PartUsage, provider.PartStop:
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

// liveChild is live for a subagent: the row goes in under its call (insertAfterGroup)
// instead of at the end.
func (t *Transcript) liveChild(o origin, turn, key string, thinking bool) *Row {
	if r := t.byKey[key]; r != nil {
		return r
	}
	r := o.stamp(&Row{Key: key, Kind: RowAssistant, TurnID: turn, Live: true, Thinking: thinking})
	t.insertAfterGroup(o.parentToolUseID, r)
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

// FocusedPrompt is the standing question nearest the input: the last prompt row in row order.
// It is the one a client's keyboard answers and the one prompt() draws the keys on, and those
// two must be the same row or a keypress lands on a question the operator was not reading
// (rudy-omc). nil when no question is on screen.
//
// Row order is not arrival order. This session's own question is appended at the end, or stands
// in front of its tool row (Prompt); a subagent's is inserted under the agent call that opened
// it (PromptFrom, insertAfterGroup), so a child's later question lands ABOVE a parent's earlier
// one. What an operator reads as the bottom question is what row order says, never what arrived
// last.
func (t *Transcript) FocusedPrompt() *protocol.PermissionRequested {
	for _, r := range slices.Backward(t.rows) {
		if r.Kind == RowPrompt && r.Prompt != nil {
			return r.Prompt
		}
	}
	return nil
}

// Answered removes a permission question. The tool row it stood in front of stays.
func (t *Transcript) Answered(toolUseID string) {
	if i := t.indexOf(promptKey(toolUseID)); i >= 0 {
		t.removeAt(i)
	}
}

// PromptFrom is Prompt for a subagent's own permission question (rudy-ssz): a child has no
// asker of its own, so its permission.requested reaches the parent's askers carrying the
// child's session id, and answering it is entitled the same way (contracts: session.answer
// is deliberately not subscription gated). The row is keyed and stamped under sessionID and
// parentToolUseID so it renders in front of the child's own tool row, under the agent call
// that opened it, the same as any other of that call's rows.
func (t *Transcript) PromptFrom(sessionID, parentToolUseID string, p protocol.PermissionRequested) {
	o := origin{sessionID: sessionID, parentToolUseID: parentToolUseID}
	key := o.key(promptKey(p.ToolUseID))
	if t.byKey[key] != nil {
		return
	}
	r := o.stamp(&Row{Key: key, Kind: RowPrompt, TurnID: t.callTurn(parentToolUseID), Prompt: &p})
	if i := t.indexOf(o.key(p.ToolUseID)); i >= 0 {
		t.byKey[key] = r
		t.rows = slices.Insert(t.rows, i, r)
		return
	}
	t.insertAfterGroup(parentToolUseID, r)
}

// AnsweredFrom is Answered for a subagent's own question.
func (t *Transcript) AnsweredFrom(sessionID, parentToolUseID, toolUseID string) {
	o := origin{sessionID: sessionID, parentToolUseID: parentToolUseID}
	if i := t.indexOf(o.key(promptKey(toolUseID))); i >= 0 {
		t.removeAt(i)
	}
}

// SetOwnToolState is SetToolState for this transcript's own session, whose rows carry no origin
// prefix (origin.key). It exists so no caller reaches for the session id it is rendering: that
// builds a prefixed key, which matches a subagent's rows and never one of this session's own, so
// every state for the session on screen would be dropped silently.
func (t *Transcript) SetOwnToolState(toolUseID, state string) { t.SetToolState("", toolUseID, state) }

// SetToolState records a subagent's own tool call progress on its row (Row.ToolState), so
// the moment between tool.state(awaiting_permission) and the permission.requested that
// follows it (contracts) shows the operator why the call is not moving, rather than a bare
// "running" for however long that ask takes to reach an asker (rudy-ssz). A tool_use this
// transcript has no row for (an unknown call, or one whose row has already gone) is silently
// ignored: there is nothing to mark.
func (t *Transcript) SetToolState(sessionID, toolUseID, state string) {
	if r := t.rowIfTool(origin{sessionID: sessionID}.key(toolUseID)); r != nil {
		r.ToolState = state
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
