package session

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/oklog/ulid/v2"
)

// ErrInvariant wraps every refusal by Append and Fork.
var ErrInvariant = errors.New("session: invariant")

// ErrClosed wraps every Append or Sync on a session, or a log, that has been closed.
var ErrClosed = errors.New("session: closed")

func invariant(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvariant, fmt.Sprintf(format, args...))
}

// Session is the aggregate over one log. Everything it reports is derived by
// scanning entries; nothing is cached on disk.
type Session struct {
	id        ulid.ULID
	dir       string
	log       *Log
	blobs     *Blobs
	inherited []Entry // from the fork parent chain, read-only
	own       []Entry // this log's entries
	unlock    func()
}

// Open creates a new root session and appends session_opened.
func Open(st *Store, opened SessionOpened) (*Session, error) {
	s, err := create(st, NewID())
	if err != nil {
		return nil, err
	}
	if _, err := s.Append(opened); err != nil {
		_ = s.Close()
		return nil, err
	}
	// session_opened must be visible to a fresh ReadLog the moment Open
	// returns, the same reason Fork syncs fork_point: otherwise a concurrent
	// Load reads an empty file and reports "empty log" instead of ErrLocked.
	if err := s.log.Sync(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func create(st *Store, id ulid.ULID) (*Session, error) {
	dir := st.Dir(id)
	unlock, err := st.lock(id)
	if err != nil {
		return nil, err
	}
	log, err := OpenLog(dir)
	if err != nil {
		unlock()
		return nil, err
	}
	blobs, err := OpenBlobs(dir)
	if err != nil {
		_ = log.Close()
		unlock()
		return nil, err
	}
	return &Session{id: id, dir: dir, log: log, blobs: blobs, unlock: unlock}, nil
}

// Load replays a session, resolves its fork parent chain and runs recovery.
func Load(st *Store, id ulid.ULID) (*Session, error) {
	own, err := ReadLog(st.Dir(id))
	var tr Truncated
	if err != nil && !errors.As(err, &tr) {
		return nil, err
	}
	if len(own) == 0 {
		return nil, fmt.Errorf("session %s: %w", id, errors.New("session: empty log"))
	}
	for i := range own[1:] {
		if own[i+1].ID.Compare(own[i].ID) <= 0 {
			return nil, invariant("ids not increasing at line %d", i+2)
		}
	}
	var inherited []Entry
	switch first := own[0].Payload.(type) {
	case SessionOpened:
	case ForkPoint:
		inherited, err = inheritedEntries(st, first, 0)
		if err != nil {
			return nil, err
		}
	default:
		return nil, invariant("first entry is %s", own[0].Kind)
	}
	s, err := create(st, id)
	if err != nil {
		return nil, err
	}
	s.inherited, s.own = inherited, own
	if _, err := Recover(s); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// inheritedEntries reads the parent chain named by a fork_point, up to and
// including the fork entry, recursing through grandparents.
func inheritedEntries(st *Store, fp ForkPoint, depth int) ([]Entry, error) {
	if depth > 64 {
		return nil, invariant("fork chain deeper than 64")
	}
	parent, err := ReadLog(st.Dir(fp.ParentSessionID))
	var tr Truncated
	if err != nil && !errors.As(err, &tr) {
		return nil, err
	}
	if len(parent) == 0 {
		return nil, invariant("fork parent %s has no log", fp.ParentSessionID)
	}
	var chain []Entry
	if gp, ok := parent[0].Payload.(ForkPoint); ok {
		chain, err = inheritedEntries(st, gp, depth+1)
		if err != nil {
			return nil, err
		}
	}
	all := append(chain, parent...)
	for i, e := range all {
		if e.ID == fp.ParentEntryID {
			return all[:i+1], nil
		}
	}
	return nil, invariant("fork entry %s not in parent %s", fp.ParentEntryID, fp.ParentSessionID)
}

func (s *Session) ID() ulid.ULID { return s.id }
func (s *Session) Dir() string   { return s.dir }
func (s *Session) Blobs() *Blobs { return s.blobs }

// Entries is the inherited chain followed by this log's entries.
func (s *Session) Entries() []Entry {
	out := make([]Entry, 0, len(s.inherited)+len(s.own))
	out = append(out, s.inherited...)
	return append(out, s.own...)
}

func (s *Session) has(id ulid.ULID) bool {
	for _, e := range s.Entries() {
		if e.ID == id {
			return true
		}
	}
	return false
}

// Append validates p against the log's rules, writes it and returns the entry.
// An allow permission_decision is fsynced before Append returns.
func (s *Session) Append(p Payload) (Entry, error) {
	if err := Validate(p); err != nil {
		return Entry{}, invariant("%v", err)
	}
	switch v := p.(type) {
	case SessionOpened:
		if len(s.own) > 0 || len(s.inherited) > 0 {
			return Entry{}, invariant("session_opened after the first entry")
		}
	case ForkPoint:
		if len(s.own) > 0 {
			return Entry{}, invariant("fork_point after the first entry")
		}
	case PermissionDecision:
		if !s.isPending(v.ToolUseID) {
			return Entry{}, invariant("permission_decision for tool_use %q that is not pending", v.ToolUseID)
		}
		if _, ok := s.decisionFor(v.ToolUseID); ok {
			return Entry{}, invariant("second permission_decision for tool_use %q", v.ToolUseID)
		}
	case ToolResult:
		if !s.isPending(v.ToolUseID) {
			return Entry{}, invariant("tool_result for tool_use %q that is not pending", v.ToolUseID)
		}
		if _, ok := s.decisionFor(v.ToolUseID); !ok {
			return Entry{}, invariant("tool_result for tool_use %q before its permission_decision", v.ToolUseID)
		}
	case Compaction:
		if !s.has(v.FirstEntryID) || !s.has(v.LastEntryID) || v.FirstEntryID.Compare(v.LastEntryID) > 0 {
			return Entry{}, invariant("compaction ids must name ordered entries of this session")
		}
	default:
		if len(s.own) == 0 && len(s.inherited) == 0 {
			return Entry{}, invariant("first entry must be session_opened or fork_point, not %s", p.Kind())
		}
	}
	e := Entry{ID: NewID(), At: time.Now(), Kind: p.Kind(), Payload: p}
	if err := s.log.Append(e); err != nil {
		return Entry{}, err
	}
	if pd, ok := p.(PermissionDecision); ok && pd.Decision == Allow {
		if err := s.log.Sync(); err != nil {
			return Entry{}, err
		}
	}
	s.own = append(s.own, e)
	return e, nil
}

// Sync flushes the log's write buffer and fsyncs it. Append leaves most entries in that
// buffer, so nothing a turn wrote is on disk until this runs (or until Close). Callers
// sync at the boundaries that matter to them: the turn runner does it every time a turn
// comes to rest, so a crash between turns cannot lose entries a user has already seen.
func (s *Session) Sync() error { return s.log.Sync() }

// Fork syncs this log, then creates a new session whose first entry is a
// fork_point at `at`. Entries up to and including `at` are inherited by
// reference, so the parent's bytes must be on disk before the child exists.
func (s *Session) Fork(st *Store, at ulid.ULID) (*Session, error) {
	if err := s.log.Sync(); err != nil {
		return nil, err
	}
	all := s.Entries()
	cut := -1
	for i, e := range all {
		if e.ID == at {
			cut = i
		}
	}
	if cut < 0 {
		return nil, invariant("fork at %s: not an entry of this session", at)
	}
	child, err := create(st, NewID())
	if err != nil {
		return nil, err
	}
	child.inherited = append([]Entry(nil), all[:cut+1]...)
	if _, err := child.Append(ForkPoint{ParentSessionID: s.id, ParentEntryID: at}); err != nil {
		_ = child.Close()
		return nil, err
	}
	// fork_point must be visible to a fresh ReadLog the moment Fork returns,
	// not only once the child is later closed or synced.
	if err := child.log.Sync(); err != nil {
		_ = child.Close()
		return nil, err
	}
	return child, nil
}

func (s *Session) opened() SessionOpened {
	for _, e := range s.Entries() {
		if o, ok := e.Payload.(SessionOpened); ok {
			return o
		}
	}
	return SessionOpened{}
}

func (s *Session) Workspace() Workspace { return s.opened().Workspace }
func (s *Session) Agent() string        { return s.opened().Agent }

func (s *Session) Model() ModelRef {
	m := s.opened().Model
	for _, e := range s.Entries() {
		if c, ok := e.Payload.(ModelChange); ok {
			m = c.Model
		}
	}
	return m
}

func (s *Session) Mode() Mode {
	m := s.opened().Mode
	for _, e := range s.Entries() {
		if c, ok := e.Payload.(ModeChange); ok {
			m = c.Mode
		}
	}
	return m
}

func (s *Session) Thinking() ThinkingLevel {
	l := s.opened().Thinking
	for _, e := range s.Entries() {
		if c, ok := e.Payload.(ThinkingChange); ok {
			l = c.Thinking
		}
	}
	return l
}

// Title is the last title_change, or "" when none.
func (s *Session) Title() string {
	t := ""
	for _, e := range s.Entries() {
		if c, ok := e.Payload.(TitleChange); ok {
			t = c.Title
		}
	}
	return t
}

// Usage sums every assistant_message and compaction usage, since both are provider calls
// the session paid for.
func (s *Session) Usage() Usage {
	var u Usage
	for _, e := range s.Entries() {
		switch p := e.Payload.(type) {
		case AssistantMessage:
			u = u.Add(p.Usage)
		case Compaction:
			u = u.Add(p.Usage)
		}
	}
	return u
}

// Allowances are the matchers of allow decisions with session scope.
func (s *Session) Allowances() []Matcher {
	var out []Matcher
	for _, e := range s.Entries() {
		if d, ok := e.Payload.(PermissionDecision); ok && d.Decision == Allow && d.Scope == ScopeSession {
			out = append(out, d.Matcher)
		}
	}
	return out
}

// RequestContext is the last compaction entry followed by everything after
// it, or every entry when there is no compaction.
func (s *Session) RequestContext() []Entry {
	all := s.Entries()
	for i, e := range slices.Backward(all) {
		if e.Kind == KindCompaction {
			return all[i:]
		}
	}
	return all
}

// PendingToolUses returns tool_use blocks that have no tool_result yet, in order.
func (s *Session) PendingToolUses() []Block {
	done := map[string]bool{}
	var uses []Block
	for _, e := range s.Entries() {
		switch v := e.Payload.(type) {
		case AssistantMessage:
			for _, b := range v.Content {
				if b.Type == BlockToolUse {
					uses = append(uses, b)
				}
			}
		case ToolResult:
			done[v.ToolUseID] = true
		}
	}
	var out []Block
	for _, b := range uses {
		if !done[b.ID] {
			out = append(out, b)
		}
	}
	return out
}

func (s *Session) isPending(toolUseID string) bool {
	for _, b := range s.PendingToolUses() {
		if b.ID == toolUseID {
			return true
		}
	}
	return false
}

func (s *Session) decisionFor(toolUseID string) (PermissionDecision, bool) {
	for _, e := range s.Entries() {
		if d, ok := e.Payload.(PermissionDecision); ok && d.ToolUseID == toolUseID {
			return d, true
		}
	}
	return PermissionDecision{}, false
}

// Close syncs and closes the log and releases the lock. The log pointer is kept: anything
// that still holds this session, a turn the server closed out from under after its shutdown
// budget ran out, gets ErrClosed from its next Append or Sync instead of dereferencing a nil
// log and taking the process down. Closing twice is a no-op.
func (s *Session) Close() error {
	err := s.log.Close()
	if s.unlock != nil {
		s.unlock()
		s.unlock = nil
	}
	return err
}
