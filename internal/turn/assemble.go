// SPDX-License-Identifier: AGPL-3.0-or-later

package turn

import (
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// Assemble builds the provider request from the session's request context. A leading
// compaction entry becomes a user message carrying its summary. Entry kinds that are not
// conversation content are skipped.
//
// overrides, keyed by tool_use id, replaces what the model sees of a tool result without
// touching the log: an after_tool hook may rewrite a result for the model only, so the two
// deliberately disagree and the request, not the entry, is where the substitution happens.
func Assemble(s *session.Session, tools []tool.Tool, system string, maxTokens int, overrides map[string][]session.Block) provider.Request {
	req := provider.Request{
		Model:     s.Model(),
		System:    system,
		Thinking:  s.Thinking(),
		MaxTokens: maxTokens,
		SessionID: s.ID(),
	}
	for _, t := range tools {
		req.Tools = append(req.Tools, provider.ToolDef{Name: t.Name, Description: t.Description, Schema: t.Schema})
	}
	req.Messages = messagesOf(s.RequestContext(), overrides)
	return req
}

// messagesOf is the entry to message conversion: the compaction summary as a user message,
// the conversation entries as themselves, everything else skipped. Both the turn's request
// and the Compactor's summary request are built from it, so the model reads a compacted
// conversation the same way in each.
//
// A run of tool results becomes one message carrying all of them, not one message each. The
// calls of an assistant message run at once and their results land together, so together is
// what they are: Anthropic's parallel tool use wants every tool_result of a turn in a single
// user message, and splitting them is accepted on the wire while teaching the model to stop
// asking for tools in parallel, which is the whole point of the fan-out (ADR 0028, the
// Turn.RunTool contracts row). A run is contiguous because nothing else can be appended
// between the results of one message: the entries in between are permission decisions, which
// are skipped here, and a steer's user_message is appended by the next Run, not this one.
func messagesOf(entries []session.Entry, overrides map[string][]session.Block) []provider.Message {
	var out []provider.Message
	var results []provider.ToolResult
	// flush closes the run of tool results built up so far, so it lands before whatever
	// entry ended it rather than after.
	flush := func() {
		if len(results) > 0 {
			out = append(out, provider.Message{Role: provider.RoleToolResult, Results: results})
			results = nil
		}
	}
	for _, e := range entries {
		switch p := e.Payload.(type) {
		case session.Compaction:
			flush()
			out = append(out, provider.Message{
				Role:    provider.RoleUser,
				Content: []session.Block{session.TextBlock("Summary of the conversation so far:\n" + p.Summary)},
			})
		case session.UserMessage:
			flush()
			out = append(out, provider.Message{Role: provider.RoleUser, Content: p.Content})
		case session.AssistantMessage:
			flush()
			out = append(out, provider.Message{Role: provider.RoleAssistant, Content: p.Content})
		case session.ToolResult:
			content := p.Content
			if over, ok := overrides[p.ToolUseID]; ok {
				content = over
			}
			// Anything but a clean run is an error to the model: a tool that failed, one
			// killed by a steer, and one whose result was lost to a crash all say so on
			// the wire rather than arriving as an ordinary answer.
			results = append(results, provider.ToolResult{
				ToolUseID: p.ToolUseID,
				Content:   content,
				IsError:   p.Outcome != session.OutcomeOK,
			})
		}
	}
	flush()
	return out
}
