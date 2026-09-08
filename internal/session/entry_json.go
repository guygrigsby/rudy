package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// encodeNoEscape marshals v without HTML escaping and without the trailing
// newline json.Encoder adds.
func encodeNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// appendString appends s as a JSON string with minimal escaping.
func appendString(dst []byte, s string) ([]byte, error) {
	b, err := encodeNoEscape(s)
	if err != nil {
		return nil, err
	}
	return append(dst, b...), nil
}

// appendBlocks writes a block array by hand so tool_use input bytes are
// copied verbatim rather than compacted by encoding/json.
func appendBlocks(dst []byte, blocks []Block) ([]byte, error) {
	dst = append(dst, '[')
	for i, b := range blocks {
		if i > 0 {
			dst = append(dst, ',')
		}
		var err error
		switch b.Type {
		case BlockText:
			dst = append(dst, `{"type":"text","text":`...)
			if dst, err = appendString(dst, b.Text); err != nil {
				return nil, err
			}
		case BlockImage:
			dst = append(dst, `{"type":"image","media_type":`...)
			if dst, err = appendString(dst, b.MediaType); err != nil {
				return nil, err
			}
			dst = append(dst, `,"sha256":`...)
			if dst, err = appendString(dst, b.SHA256); err != nil {
				return nil, err
			}
		case BlockThinking:
			dst = append(dst, `{"type":"thinking","text":`...)
			if dst, err = appendString(dst, b.Text); err != nil {
				return nil, err
			}
			dst = append(dst, `,"signature":`...)
			if dst, err = appendString(dst, b.Signature); err != nil {
				return nil, err
			}
		case BlockToolUse:
			dst = append(dst, `{"type":"tool_use","id":`...)
			if dst, err = appendString(dst, b.ID); err != nil {
				return nil, err
			}
			dst = append(dst, `,"name":`...)
			if dst, err = appendString(dst, b.Name); err != nil {
				return nil, err
			}
			dst = append(dst, `,"input":`...)
			if len(b.Input) == 0 {
				dst = append(dst, '{', '}')
			} else {
				if !json.Valid(b.Input) {
					return nil, fmt.Errorf("tool_use %q: input is not valid JSON", b.ID)
				}
				dst = append(dst, b.Input...)
			}
		default:
			return nil, fmt.Errorf("block: invalid type %q", b.Type)
		}
		dst = append(dst, '}')
	}
	return append(dst, ']'), nil
}

// MarshalJSON emits only the variant's fields. Used when a Block travels
// inside a larger json.Marshal, such as a protocol notification; the log
// line path uses appendBlocks instead.
func (b Block) MarshalJSON() ([]byte, error) {
	type alias Block
	out := alias{Type: b.Type}
	switch b.Type {
	case BlockText:
		out.Text = b.Text
	case BlockImage:
		out.MediaType, out.SHA256 = b.MediaType, b.SHA256
	case BlockThinking:
		out.Text, out.Signature = b.Text, b.Signature
	case BlockToolUse:
		out.ID, out.Name, out.Input = b.ID, b.Name, b.Input
	default:
		return nil, fmt.Errorf("block: invalid type %q", b.Type)
	}
	return json.Marshal(out)
}

// UnmarshalJSON validates the variant tag; Input keeps the source bytes.
func (b *Block) UnmarshalJSON(data []byte) error {
	type alias Block
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	if !a.Type.Valid() {
		return fmt.Errorf("block: invalid type %q", a.Type)
	}
	*b = Block(a)
	return nil
}

// blocksOf returns the block slice of a payload that carries one, and a
// scalar-only view of the rest of its fields. The scalar view is a distinct
// anonymous struct rather than the payload with Content zeroed: the
// payload's own Content tag has no omitempty (interfaces.md pins it that
// way), so zeroing it in place and marshaling the payload would still emit
// "content":null alongside the content array appendBlocks writes by hand,
// producing a line with two "content" keys.
func blocksOf(p Payload) (blocks []Block, rest any, has bool) {
	switch v := p.(type) {
	case UserMessage:
		return v.Content, struct {
			Source Source `json:"source"`
		}{v.Source}, true
	case AssistantMessage:
		return v.Content, struct {
			Model         ModelRef      `json:"model"`
			Thinking      ThinkingLevel `json:"thinking"`
			Usage         Usage         `json:"usage"`
			StopReason    StopReason    `json:"stop_reason"`
			StopReasonRaw string        `json:"stop_reason_raw"`
		}{v.Model, v.Thinking, v.Usage, v.StopReason, v.StopReasonRaw}, true
	case ToolResult:
		return v.Content, struct {
			ToolUseID  string  `json:"tool_use_id"`
			Outcome    Outcome `json:"outcome"`
			DurationMS int64   `json:"duration_ms"`
		}{v.ToolUseID, v.Outcome, v.DurationMS}, true
	}
	return nil, p, false
}

// MarshalJSON writes the flattened envelope:
// {"id":…,"at":…,"kind":…,<payload fields>} with block arrays copied verbatim.
// For payloads with content the content array is written last.
func (e Entry) MarshalJSON() ([]byte, error) {
	if e.Payload == nil {
		return nil, errors.New("entry: nil payload")
	}
	if e.Kind != e.Payload.Kind() {
		return nil, fmt.Errorf("entry: kind %q does not match payload %q", e.Kind, e.Payload.Kind())
	}
	if err := Validate(e.Payload); err != nil {
		return nil, fmt.Errorf("entry %s: %w", e.Kind, err)
	}
	blocks, rest, has := blocksOf(e.Payload)
	scalars, err := encodeNoEscape(rest)
	if err != nil {
		return nil, err
	}
	// scalars is "{...}" or "{}"; splice its interior after the envelope.
	interior := bytes.TrimSuffix(bytes.TrimPrefix(scalars, []byte("{")), []byte("}"))

	out := make([]byte, 0, 64+len(scalars))
	out = append(out, `{"id":"`...)
	out = append(out, e.ID.String()...)
	out = append(out, `","at":"`...)
	out = append(out, e.At.Format(time.RFC3339Nano)...)
	out = append(out, `","kind":"`...)
	out = append(out, string(e.Kind)...)
	out = append(out, '"')
	if len(interior) > 0 {
		out = append(out, ',')
		out = append(out, interior...)
	}
	if has {
		out = append(out, `,"content":`...)
		if out, err = appendBlocks(out, blocks); err != nil {
			return nil, err
		}
	}
	return append(out, '}'), nil
}

type envelope struct {
	ID   string `json:"id"`
	At   string `json:"at"`
	Kind Kind   `json:"kind"`
}

// UnmarshalJSON reads the envelope, allocates the payload for its kind and
// decodes the same bytes into it. RawMessage fields keep the source bytes.
func (e *Entry) UnmarshalJSON(data []byte) error {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return err
	}
	id, err := ulid.ParseStrict(env.ID)
	if err != nil {
		return fmt.Errorf("entry: id: %w", err)
	}
	at, err := time.Parse(time.RFC3339Nano, env.At)
	if err != nil {
		return fmt.Errorf("entry: at: %w", err)
	}
	var p Payload
	switch env.Kind {
	case KindSessionOpened:
		p, err = decodePayload[SessionOpened](data)
	case KindForkPoint:
		p, err = decodePayload[ForkPoint](data)
	case KindUserMessage:
		p, err = decodePayload[UserMessage](data)
	case KindAssistantMessage:
		p, err = decodePayload[AssistantMessage](data)
	case KindPermissionDecision:
		p, err = decodePayload[PermissionDecision](data)
	case KindToolResult:
		p, err = decodePayload[ToolResult](data)
	case KindModelChange:
		p, err = decodePayload[ModelChange](data)
	case KindModeChange:
		p, err = decodePayload[ModeChange](data)
	case KindThinkingChange:
		p, err = decodePayload[ThinkingChange](data)
	case KindTitleChange:
		p, err = decodePayload[TitleChange](data)
	case KindCompaction:
		p, err = decodePayload[Compaction](data)
	case KindTurnInterrupted:
		p, err = decodePayload[TurnInterrupted](data)
	case KindTurnFailed:
		p, err = decodePayload[TurnFailed](data)
	case KindNote:
		p, err = decodePayload[Note](data)
	default:
		return fmt.Errorf("entry: unknown kind %q", env.Kind)
	}
	if err != nil {
		return fmt.Errorf("entry %s: %w", env.Kind, err)
	}
	if err := Validate(p); err != nil {
		return fmt.Errorf("entry %s: %w", env.Kind, err)
	}
	*e = Entry{ID: id, At: at, Kind: env.Kind, Payload: p}
	return nil
}

func decodePayload[T Payload](data []byte) (Payload, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return v, nil
}
