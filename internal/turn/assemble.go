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
func messagesOf(entries []session.Entry, overrides map[string][]session.Block) []provider.Message {
	var out []provider.Message
	for _, e := range entries {
		switch p := e.Payload.(type) {
		case session.Compaction:
			out = append(out, provider.Message{
				Role:    provider.RoleUser,
				Content: []session.Block{session.TextBlock("Summary of the conversation so far:\n" + p.Summary)},
			})
		case session.UserMessage:
			out = append(out, provider.Message{Role: provider.RoleUser, Content: p.Content})
		case session.AssistantMessage:
			out = append(out, provider.Message{Role: provider.RoleAssistant, Content: p.Content})
		case session.ToolResult:
			content := p.Content
			if over, ok := overrides[p.ToolUseID]; ok {
				content = over
			}
			// Anything but a clean run is an error to the model: a tool that failed, one
			// killed by a steer, and one whose result was lost to a crash all say so on
			// the wire rather than arriving as an ordinary answer.
			out = append(out, provider.Message{
				Role:      provider.RoleToolResult,
				Content:   content,
				ToolUseID: p.ToolUseID,
				IsError:   p.Outcome != session.OutcomeOK,
			})
		}
	}
	return out
}
