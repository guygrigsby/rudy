package turn

import (
	"context"
	"slices"
	"strings"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

// summarySystem is the whole instruction the summary request carries. It asks for prose plus
// the concrete things a continuation needs, because a summary that keeps only the narrative
// loses the file paths and commands the next step depends on.
const summarySystem = "You summarize a coding session so the assistant can continue it. Write the summary in plain prose, then a list of every file path, command, decision and open task mentioned. Keep tool inputs and outputs that a next step depends on. Do not invent."

// ModelCompactor is the Compactor: it asks the before_compaction hook for a summary and, when
// no handler has one (or the caller gave instructions, which skip the hook), asks the model.
// It is a domain service of the turn, not a plugin: an automatic compaction and an explicit
// session.compact are the same code, so they cover the same entries and fire the same hook.
type ModelCompactor struct {
	Provider provider.Provider
	Model    provider.Model
	Hooks    HookFirer // nil means no hooks
	// Commit enters the owning Server's Session admission fence for the compaction append,
	// the same boundary Config.Commit gives the rest of the turn. The summary request is made
	// outside it. Nil commits directly, which is what a compaction with no Server behind it
	// (a test) wants.
	Commit    func(func() error) error
	MaxTokens int
	// Overrides is what after_tool handlers replaced, by tool_use id, the same map the turn's
	// requests are assembled with (see Config.Overrides). The summary is written from what the
	// model was shown, never from what the log kept: a result a handler redacted must not come
	// back through a compaction that then persists it.
	Overrides map[string][]session.Block
}

// isConversation reports whether an entry is one a compaction covers. The rest of the log,
// the decisions, the mode changes, the notes, is not conversation the model ever saw.
func isConversation(e session.Entry) bool {
	switch e.Kind {
	case session.KindUserMessage, session.KindAssistantMessage, session.KindToolResult, session.KindCompaction:
		return true
	}
	return false
}

// Cover is what a compaction of s before `before` would summarize: the conversation entries of
// the request context that precede it, in log order. A zero `before` covers the whole request
// context. Fewer than two entries is nothing worth compacting.
//
// Exported because the server answers /compact with the number of entries it covered, and the
// count has to be this set, not a second opinion about what a compaction covers.
func Cover(s *session.Session, before ulid.ULID) []session.Entry {
	var cover []session.Entry
	for _, e := range s.RequestContext() {
		// Skipped, not stopped at: the request context is not ordered by id. Its leading
		// compaction was appended after the entries that follow it, the turn that was running
		// when it happened, so a break here would end the walk on that first entry whenever a
		// compaction is younger than before, and a second compaction in one turn would be a
		// silent no-op.
		if !before.IsZero() && e.ID.Compare(before) >= 0 {
			continue
		}
		if isConversation(e) {
			cover = append(cover, e)
		}
	}
	return cover
}

// Compact summarizes everything before `before` and appends the compaction. It returns the
// zero Entry and nil when there is nothing worth covering, which is not an error: a session
// too short to compact is the normal answer to an early /compact.
func (c *ModelCompactor) Compact(ctx context.Context, s *session.Session, before ulid.ULID, instructions string) (session.Entry, error) {
	cover := Cover(s, before)
	if len(cover) < 2 {
		return session.Entry{}, nil
	}
	first, last := cover[0], cover[len(cover)-1]
	firstID := first.ID
	if prev, ok := first.Payload.(session.Compaction); ok {
		// The new summary subsumes the old one's span, so it starts where that one started.
		// The compaction entry's own id will not do: a compaction is appended after the
		// entries it covers, so whenever nothing was said between it and before, its id is
		// younger than every other entry in this set and the log refuses the range.
		firstID = prev.FirstEntryID
	}
	summary, usage := "", session.Usage{}
	// Instructions skip the hook entirely: the caller said what this summary is for, and a
	// handler's canned summary would answer a different question.
	if instructions == "" && c.Hooks != nil {
		for _, res := range c.Hooks.Fire(ctx, plugin.HookCall{Point: plugin.HookBeforeCompaction, SessionID: s.ID().String(), Payload: &plugin.BeforeCompactionPayload{
			SessionID: s.ID().String(), FirstEntryID: firstID.String(), LastEntryID: last.ID.String(), PromptTokens: promptTokensOf(cover), ContextWindow: c.Model.ContextWindow,
		}}) {
			if r, ok := res.(*plugin.BeforeCompactionResult); ok && r.Summary != "" {
				summary = r.Summary
				break
			}
		}
	}
	if summary == "" {
		var err error
		summary, usage, err = c.summarize(ctx, s, cover, instructions)
		if err != nil {
			return session.Entry{}, err
		}
	}
	p := session.Compaction{Summary: summary, FirstEntryID: firstID, LastEntryID: last.ID, Model: c.Model.Ref, Usage: usage}
	if c.Commit == nil {
		return s.Append(p)
	}
	var e session.Entry
	err := c.Commit(func() error {
		var aerr error
		e, aerr = s.Append(p)
		return aerr
	})
	return e, err
}

// summarize sends the covered entries as the conversation and asks for the summary as a final
// user message. No tools are offered: the model is reading, not working. Thinking is off, so
// the budget goes to the summary itself.
func (c *ModelCompactor) summarize(ctx context.Context, s *session.Session, cover []session.Entry, instructions string) (string, session.Usage, error) {
	req := provider.Request{Model: c.Model.Ref, System: summarySystem, Thinking: session.ThinkingOff, MaxTokens: c.MaxTokens, SessionID: s.ID()}
	req.Messages = messagesOf(cover, c.Overrides)
	ask := "Summarize the conversation above for a continuation of this session."
	if instructions != "" {
		ask += " Instructions: " + instructions
	}
	req.Messages = append(req.Messages, provider.Message{Role: provider.RoleUser, Content: []session.Block{session.TextBlock(ask)}})
	var b strings.Builder
	var usage session.Usage
	err := c.Provider.Complete(ctx, req, func(p provider.Part) error {
		switch p.Type {
		case provider.PartTextDelta:
			b.WriteString(p.Text)
		case provider.PartUsage:
			usage = usage.Add(p.Usage)
		}
		return nil
	})
	if err != nil {
		return "", usage, err
	}
	if strings.TrimSpace(b.String()) == "" {
		return "", usage, &provider.Error{Class: session.ErrProvider, Message: "empty summary"}
	}
	return b.String(), usage, nil
}

// promptTokensOf is what the last request over these entries cost to send: the newest
// assistant message's prompt tokens, cached ones included. Zero when nothing in cover came
// back from a provider.
func promptTokensOf(cover []session.Entry) int64 {
	for _, e := range slices.Backward(cover) {
		if am, ok := e.Payload.(session.AssistantMessage); ok {
			return am.Usage.Input + am.Usage.CacheRead + am.Usage.CacheWrite
		}
	}
	return 0
}
