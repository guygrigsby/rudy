// Package provider is the port through which the turn loop reaches a model. Wire formats
// live in subpackages; nothing here knows what an HTTP request looks like.
package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/session"
)

// Role is who a message is from, in domain terms.
type Role string

const (
	RoleUser       Role = "user"
	RoleAssistant  Role = "assistant"
	RoleToolResult Role = "tool_result"
)

// Message is one turn of conversation in the shape the loop keeps it.
type Message struct {
	Role      Role
	Content   []session.Block
	ToolUseID string // tool_result only
}

// ToolDef is a tool as the model sees it.
type ToolDef struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// Request is one completion request.
type Request struct {
	Model     session.ModelRef
	System    string
	Messages  []Message
	Tools     []ToolDef
	Thinking  session.ThinkingLevel
	MaxTokens int
	SessionID ulid.ULID // for the X-Rudy-Session header
}

// PartType tags a streamed Part.
type PartType string

const (
	PartTextDelta     PartType = "text_delta"
	PartThinkingDelta PartType = "thinking_delta"
	PartToolUseStart  PartType = "tool_use_start" // ID, Name
	PartToolUseDelta  PartType = "tool_use_delta" // ID, Input fragment in Text
	PartToolUseEnd    PartType = "tool_use_end"   // ID
	PartUsage         PartType = "usage"
	PartStop          PartType = "stop"
)

// Part is one streamed piece of a completion.
type Part struct {
	Type          PartType           `json:"type"`
	Text          string             `json:"text,omitempty"`
	ID            string             `json:"id,omitempty"`
	Name          string             `json:"name,omitempty"`
	Usage         session.Usage      `json:"usage,omitempty"`
	StopReason    session.StopReason `json:"stop_reason,omitempty"`
	StopReasonRaw string             `json:"stop_reason_raw,omitempty"`
}

// Provider is one configured endpoint.
//
// Complete streams parts by calling emit in order until the stream ends. A non-nil error
// from emit stops the stream and is returned. Context cancellation returns ctx.Err().
type Provider interface {
	Name() string
	Complete(ctx context.Context, req Request, emit func(Part) error) error
	ListModels(ctx context.Context) ([]Model, error)
}

// Error is a provider failure after retries. The turn loop records it as turn_failed with
// Retries set from Attempts.
type Error struct {
	Class    session.ErrorClass
	Status   int
	Message  string
	Body     []byte
	Attempts int // attempts httpx made before giving up; a codec fills it from httpx.AttemptsOf or *httpx.Error
}

func (e *Error) Error() string {
	return fmt.Sprintf("provider: %s %d %s", e.Class, e.Status, e.Message)
}
