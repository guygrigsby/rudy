package openaichat

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

var errImageUnsupported = errors.New("openaichat: image blocks are not supported in this plan")

// errNothingToSend is buildMessage reporting a message with nothing left to put on the wire.
// buildRequest drops such a message: an assistant turn with neither content nor tool calls is
// refused, and since every later request replays the same log (a /compact summary included),
// one of them would fail the rest of the session rather than just this request.
var errNothingToSend = errors.New("openaichat: message has no content to send")

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
		wms, err := buildMessage(m)
		if errors.Is(err, errNothingToSend) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("openaichat: message %d: %w", i, err)
		}
		w.Messages = append(w.Messages, wms...)
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

// buildMessage turns one domain message into the wire messages it becomes. Every role but
// tool_result is one message; a tool_result message is one "tool" message per result, since
// chat completions answers each call in its own message keyed by tool_call_id. That is the
// opposite of what the Messages API wants for the same group, which is why the grouping
// travels in provider.Message and the shape is decided here.
func buildMessage(m provider.Message) ([]wireMessage, error) {
	switch m.Role {
	case provider.RoleUser:
		text, err := textOf(m.Content)
		if err != nil {
			return nil, err
		}
		return []wireMessage{{Role: "user", Content: text}}, nil
	case provider.RoleAssistant:
		var calls []wireToolCall
		for _, b := range m.Content {
			switch b.Type {
			case session.BlockText:
				// Joined by session.TextOf below rather than gathered here.
			case session.BlockToolUse:
				calls = append(calls, wireToolCall{
					ID:       b.ID,
					Type:     "function",
					Function: wireFunction{Name: b.Name, Arguments: string(b.Input)},
				})
			case session.BlockThinking:
				// Not sent back in this plan.
			case session.BlockImage:
				return nil, errImageUnsupported
			default:
				return nil, fmt.Errorf("openaichat: block type %q in assistant message", b.Type)
			}
		}
		text := session.TextOf(m.Content)
		if text == "" && len(calls) == 0 {
			// Only reachable for an assistant message whose blocks were all thinking,
			// which this codec never sends back: a Ctrl-C during thinking records exactly
			// that. A message carrying a tool call or any text still has something here.
			return nil, errNothingToSend
		}
		return []wireMessage{{Role: "assistant", Content: text, ToolCalls: calls}}, nil
	case provider.RoleToolResult:
		out := make([]wireMessage, 0, len(m.Results))
		for _, r := range m.Results {
			text, err := textOf(r.Content)
			if err != nil {
				return nil, err
			}
			out = append(out, wireMessage{Role: "tool", Content: text, ToolCallID: r.ToolUseID})
		}
		if len(out) == 0 {
			return nil, errNothingToSend
		}
		return out, nil
	}
	return nil, fmt.Errorf("openaichat: unknown role %q", m.Role)
}

// textOf is session.TextOf behind the codec's own refusal: an image is refused until the
// image plan lands, and anything but text is a bug upstream. The assistant branch does not
// come through here, since a tool_use block is exactly what it expects to see.
func textOf(blocks []session.Block) (string, error) {
	for _, b := range blocks {
		switch b.Type {
		case session.BlockText:
		case session.BlockImage:
			return "", errImageUnsupported
		default:
			return "", fmt.Errorf("openaichat: block type %q not allowed here", b.Type)
		}
	}
	return session.TextOf(blocks), nil
}
