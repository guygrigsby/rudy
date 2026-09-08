package openaichat

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

var errImageUnsupported = errors.New("openaichat: image blocks are not supported in this plan")

type wireRequest struct {
	Model         string             `json:"model"`
	Messages      []wireMessage      `json:"messages"`
	Tools         []wireTool         `json:"tools,omitempty"`
	Stream        bool               `json:"stream"`
	StreamOptions *wireStreamOptions `json:"stream_options,omitempty"`
	MaxTokens     int                `json:"max_tokens,omitempty"`
}

type wireStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string           `json:"type"`
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// buildRequest translates a domain request into the chat-completions wire shape.
// Thinking blocks on assistant messages are dropped and req.Thinking is not sent;
// both are provider-plugin work in the next plan. Tool inputs pass through as the
// verbatim bytes the model produced.
func buildRequest(req provider.Request) ([]byte, error) {
	w := wireRequest{
		Model:         req.Model.Model,
		Stream:        true,
		StreamOptions: &wireStreamOptions{IncludeUsage: true},
		MaxTokens:     req.MaxTokens,
	}
	if req.System != "" {
		w.Messages = append(w.Messages, wireMessage{Role: "system", Content: req.System})
	}
	for i, m := range req.Messages {
		wm, err := buildMessage(m)
		if err != nil {
			return nil, fmt.Errorf("openaichat: message %d: %w", i, err)
		}
		w.Messages = append(w.Messages, wm)
	}
	for _, t := range req.Tools {
		w.Tools = append(w.Tools, wireTool{Type: "function", Function: wireToolFunction{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Schema,
		}})
	}
	return json.Marshal(w)
}

func buildMessage(m provider.Message) (wireMessage, error) {
	switch m.Role {
	case provider.RoleUser:
		text, err := textOf(m.Content)
		if err != nil {
			return wireMessage{}, err
		}
		return wireMessage{Role: "user", Content: text}, nil
	case provider.RoleAssistant:
		var texts []string
		var calls []wireToolCall
		for _, b := range m.Content {
			switch b.Type {
			case session.BlockText:
				texts = append(texts, b.Text)
			case session.BlockToolUse:
				calls = append(calls, wireToolCall{
					ID:       b.ID,
					Type:     "function",
					Function: wireFunction{Name: b.Name, Arguments: string(b.Input)},
				})
			case session.BlockThinking:
				// Not sent back in this plan.
			case session.BlockImage:
				return wireMessage{}, errImageUnsupported
			default:
				return wireMessage{}, fmt.Errorf("openaichat: block type %q in assistant message", b.Type)
			}
		}
		return wireMessage{Role: "assistant", Content: strings.Join(texts, "\n"), ToolCalls: calls}, nil
	case provider.RoleToolResult:
		text, err := textOf(m.Content)
		if err != nil {
			return wireMessage{}, err
		}
		return wireMessage{Role: "tool", Content: text, ToolCallID: m.ToolUseID}, nil
	}
	return wireMessage{}, fmt.Errorf("openaichat: unknown role %q", m.Role)
}

// textOf joins text blocks with newlines. Anything else is refused.
func textOf(blocks []session.Block) (string, error) {
	var texts []string
	for _, b := range blocks {
		switch b.Type {
		case session.BlockText:
			texts = append(texts, b.Text)
		case session.BlockImage:
			return "", errImageUnsupported
		default:
			return "", fmt.Errorf("openaichat: block type %q not allowed here", b.Type)
		}
	}
	return strings.Join(texts, "\n"), nil
}
