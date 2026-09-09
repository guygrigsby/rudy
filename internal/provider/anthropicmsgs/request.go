package anthropicmsgs

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

var errImageUnsupported = errors.New("anthropicmsgs: image blocks are not supported in this plan")

// Thinking budgets, in tokens. A request whose max_tokens does not leave room for the budget
// is refused by the API, so headroom is added rather than sending a request that cannot work.
const (
	budgetLow      int64 = 1024
	budgetMedium   int64 = 4096
	budgetHigh     int64 = 16384
	budgetHeadroom int64 = 1024
)

// rawTool is a tool definition carried to the wire verbatim. The schema is the bytes the tool
// declared: rudy adds, drops and reorders nothing in it.
type rawTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// rawToolUse is an assistant tool_use block carried back verbatim. Input is the bytes the
// model produced; unmarshaling and re-marshaling them would change what the model said.
type rawToolUse struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

func buildParams(req provider.Request) (anthropic.MessageNewParams, error) {
	p := anthropic.MessageNewParams{Model: anthropic.Model(req.Model.Model), MaxTokens: int64(req.MaxTokens)}
	if req.System != "" {
		p.System = []anthropic.TextBlockParam{{Text: req.System}}
	}
	if budget := thinkingBudget(req.Thinking); budget > 0 {
		p.Thinking = anthropic.ThinkingConfigParamOfEnabled(budget)
		if p.MaxTokens <= budget {
			p.MaxTokens = budget + budgetHeadroom
		}
	}
	for _, t := range req.Tools {
		p.Tools = append(p.Tools, param.Override[anthropic.ToolUnionParam](rawTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Schema,
		}))
	}
	for i, m := range req.Messages {
		mp, err := buildMessage(m)
		if err != nil {
			return p, fmt.Errorf("anthropicmsgs: message %d: %w", i, err)
		}
		p.Messages = append(p.Messages, mp)
	}
	return p, nil
}

func thinkingBudget(l session.ThinkingLevel) int64 {
	switch l {
	case session.ThinkingLow:
		return budgetLow
	case session.ThinkingMedium:
		return budgetMedium
	case session.ThinkingHigh:
		return budgetHigh
	}
	return 0
}

// buildMessage turns one domain message into one Messages API turn. A tool result is a user
// message of its own; several results in a row stay several consecutive user messages, which
// the API combines into one turn.
func buildMessage(m provider.Message) (anthropic.MessageParam, error) {
	switch m.Role {
	case provider.RoleUser:
		blocks, err := contentBlocks(m.Content)
		if err != nil {
			return anthropic.MessageParam{}, err
		}
		return anthropic.NewUserMessage(blocks...), nil
	case provider.RoleAssistant:
		var blocks []anthropic.ContentBlockParamUnion
		for _, b := range m.Content {
			switch b.Type {
			case session.BlockText:
				blocks = append(blocks, anthropic.NewTextBlock(b.Text))
			case session.BlockThinking:
				// A thinking block without its signature is refused by the API, and one
				// turns up whenever a session moves here from a provider that signs
				// nothing. Dropping it costs the model that reasoning; sending it costs
				// the whole request.
				if b.Signature == "" {
					continue
				}
				blocks = append(blocks, anthropic.NewThinkingBlock(b.Signature, b.Text))
			case session.BlockToolUse:
				blocks = append(blocks, param.Override[anthropic.ContentBlockParamUnion](rawToolUse{
					Type:  "tool_use",
					ID:    b.ID,
					Name:  b.Name,
					Input: b.Input,
				}))
			case session.BlockImage:
				return anthropic.MessageParam{}, errImageUnsupported
			default:
				return anthropic.MessageParam{}, fmt.Errorf("anthropicmsgs: block type %q in assistant message", b.Type)
			}
		}
		return anthropic.NewAssistantMessage(blocks...), nil
	case provider.RoleToolResult:
		text, err := textOf(m.Content)
		if err != nil {
			return anthropic.MessageParam{}, err
		}
		return anthropic.NewUserMessage(anthropic.NewToolResultBlock(m.ToolUseID, text, false)), nil
	}
	return anthropic.MessageParam{}, fmt.Errorf("anthropicmsgs: unknown role %q", m.Role)
}

// texts is the text of every block, and the one place user-side content is validated:
// images are refused until the image plan lands, anything else is a bug upstream.
func texts(blocks []session.Block) ([]string, error) {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case session.BlockText:
			out = append(out, b.Text)
		case session.BlockImage:
			return nil, errImageUnsupported
		default:
			return nil, fmt.Errorf("anthropicmsgs: block type %q not allowed here", b.Type)
		}
	}
	return out, nil
}

// contentBlocks maps user content one block to one block.
func contentBlocks(blocks []session.Block) ([]anthropic.ContentBlockParamUnion, error) {
	ts, err := texts(blocks)
	if err != nil {
		return nil, err
	}
	out := make([]anthropic.ContentBlockParamUnion, 0, len(ts))
	for _, t := range ts {
		out = append(out, anthropic.NewTextBlock(t))
	}
	return out, nil
}

// textOf joins text blocks with newlines, for the places the API takes a string rather than
// a block list.
func textOf(blocks []session.Block) (string, error) {
	ts, err := texts(blocks)
	if err != nil {
		return "", err
	}
	return strings.Join(ts, "\n"), nil
}
