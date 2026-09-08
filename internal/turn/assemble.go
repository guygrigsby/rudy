package turn

import (
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// Assemble builds the provider request from the session's request context. A leading
// compaction entry becomes a user message carrying its summary. Entry kinds that are not
// conversation content are skipped.
func Assemble(s *session.Session, tools []tool.Tool, system string, maxTokens int) provider.Request {
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
	for _, e := range s.RequestContext() {
		switch p := e.Payload.(type) {
		case session.Compaction:
			req.Messages = append(req.Messages, provider.Message{
				Role:    provider.RoleUser,
				Content: []session.Block{session.TextBlock("Summary of the conversation so far:\n" + p.Summary)},
			})
		case session.UserMessage:
			req.Messages = append(req.Messages, provider.Message{Role: provider.RoleUser, Content: p.Content})
		case session.AssistantMessage:
			req.Messages = append(req.Messages, provider.Message{Role: provider.RoleAssistant, Content: p.Content})
		case session.ToolResult:
			req.Messages = append(req.Messages, provider.Message{Role: provider.RoleToolResult, Content: p.Content, ToolUseID: p.ToolUseID})
		}
	}
	return req
}
