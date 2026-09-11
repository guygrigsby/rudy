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

// Message is one turn of conversation in the shape the loop keeps it. Content and Results are
// both omitempty because exactly one of them is live for a given role, and a message reaching
// a provider plugin over provider.complete carries only the field that says anything.
type Message struct {
	Role    Role            `json:"role"`
	Content []session.Block `json:"content,omitempty"`
	// Results is what a RoleToolResult message carries, and Content is empty for that role.
	// It is a list rather than one result because the calls of one assistant message are
	// answered as a group: they run at once (ADR 0028) and every one of their results belongs
	// to the same reply. Which shape that reply takes on the wire is the codec's to decide,
	// and the two APIs want opposite ones, so the group travels as a group and each codec
	// renders it.
	//
	// The order is the order the results landed, which is the order the log has them in and
	// not the order of the tool_use blocks they answer: the calls run concurrently, so the
	// slowest one's result is last whichever call the model asked for first. Nothing reads
	// the position. Every result names its own ToolUseID and both codecs pair on that, which
	// is what the Turn.RunTool contracts row means by the pairing being per tool_use rather
	// than positional.
	Results []ToolResult `json:"results,omitempty"`
}

// ToolResult is one call's answer inside a RoleToolResult message.
type ToolResult struct {
	ToolUseID string          `json:"tool_use_id"`
	Content   []session.Block `json:"content"`
	// IsError is the result's outcome as the wire spells it: true for anything but a clean
	// run (an error, a killed tool, a result lost to a crash). A provider that has a flag for
	// it says so, which is what lets the model tell a failure from an answer instead of
	// reading the text and guessing.
	IsError bool `json:"is_error,omitempty"`
}

// ToolDef is a tool as the model sees it.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"input_schema"`
}

// Request is one completion request.
type Request struct {
	Model     session.ModelRef
	System    string
	Messages  []Message
	Tools     []ToolDef
	Thinking  session.ThinkingLevel
	MaxTokens int
	SessionID ulid.ULID         // for the X-Rudy-Session header
	Headers   map[string]string // extra request headers a before_request hook added, verbatim
}

// PartType tags a streamed Part.
type PartType string

const (
	PartTextDelta     PartType = "text_delta"
	PartThinkingDelta PartType = "thinking_delta"
	// PartThinkingSignature carries the provider's signature over the thinking block that
	// just streamed, in Signature. It arrives after that block's deltas and its bytes are
	// kept verbatim: a re-encoded signature is rejected when the block is sent back.
	PartThinkingSignature PartType = "thinking_signature"
	PartToolUseStart      PartType = "tool_use_start" // ID, Name
	PartToolUseDelta      PartType = "tool_use_delta" // ID, Input fragment in Text
	PartToolUseEnd        PartType = "tool_use_end"   // ID
	PartUsage             PartType = "usage"
	PartStop              PartType = "stop"
)

// Part is one streamed piece of a completion.
type Part struct {
	Type          PartType           `json:"type"`
	Text          string             `json:"text,omitempty"`
	ID            string             `json:"id,omitempty"`
	Name          string             `json:"name,omitempty"`
	Signature     string             `json:"signature,omitempty"`
	Usage         session.Usage      `json:"usage"`
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
