// Package transcript turns a session's entries and a turn's stream deltas into the rows
// the client draws, and renders one row to styled lines. It is the client side of ADR
// 0013 decisions 2 and 3: a row is derived from entries, one entity per thing on screen,
// and text streams raw but renders through glamour once its entry arrives.
//
// The package speaks the protocol and the log only: session, provider, protocol and the
// theme. Nothing here reaches into the server. A Transcript is owned by the Bubble Tea
// update loop and is not safe for concurrent use.
package transcript

import (
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tui/icons"
)

// RowKind is what a row is. The set is closed.
type RowKind string

const (
	RowUser      RowKind = "user"
	RowAssistant RowKind = "assistant"
	RowTool      RowKind = "tool"
	RowPrompt    RowKind = "prompt"
	RowMarker    RowKind = "marker"
)

// Row is one thing on screen.
//
// Keys are stable so replay and reattach never duplicate a row: a user row and a marker
// row are keyed by entry id, an assistant row by "<entry id>/<block index>", a tool row
// by its tool_use id and a prompt row by "prompt/<tool_use id>". A live row, one that
// exists only because a turn is streaming, is keyed "live/<turn id>" (or that plus
// "/thinking") until its entry arrives and replaces it.
type Row struct {
	Key string
	// Kind is the row's kind. A thinking block is an assistant row with Thinking set,
	// not a kind of its own: it renders in the same place, muted and unformatted.
	Kind RowKind
	// TurnID is the id of the user_message that started the turn this row belongs to,
	// which is what Commit removes by. Rows built from deltas carry the turn id the
	// delta was announced with.
	TurnID string
	// Live marks a row built from stream deltas, with no entry behind it yet.
	Live bool
	// Expanded is a tool row the user opened. Options.ToolCollapsed decides the default,
	// so a row with Expanded false still renders open when ToolCollapsed is false.
	Expanded bool
	// Thinking marks an assistant row carrying thinking rather than answer text. On a
	// live row it is also the spinner flag: it is set as soon as thinking deltas arrive,
	// whether or not ShowThinking lets the text through.
	Thinking bool
	// Entry is the entry the row came from, zero while the row is live.
	Entry session.Entry
	// Text is the live text so far, or the block's text once committed.
	Text string
	// ToolUse is the tool_use block of a tool row. Its Input is the provider's bytes,
	// verbatim; nothing here rewrites them.
	ToolUse  session.Block
	Decision *session.PermissionDecision
	Result   *session.ToolResult
	Prompt   *protocol.PermissionRequested
}

// Options are the [ui.transcript] and [ui.diff] render choices, one field per config key
// (docs/specs/2026-09-07-rudy-design.md, Client).
type Options struct {
	Width            int
	ToolCollapsed    bool
	ToolPreviewLines int
	ShowThinking     bool
	UserPrefix       string
	BlockGap         int
	// Icons is the glyph set ui.icons resolved: a tool row opens with the icon for its
	// own tool, or the generic one for a tool the set does not name.
	Icons icons.Set
	// DiffBackground is ui.diff.style == "background": the diff roles paint the line's
	// background instead of its text. It is the one place the design allows a painted
	// background.
	DiffBackground bool
}
