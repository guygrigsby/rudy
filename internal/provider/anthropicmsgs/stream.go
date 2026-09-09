package anthropicmsgs

import (
	"github.com/anthropics/anthropic-sdk-go"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

// streamState turns Messages stream events into parts. Only tool_use blocks need to be
// remembered: their id arrives on content_block_start and every later delta and the stop
// carry only the index. Text and thinking deltas name their own kind, so an index that is not
// in open is a block whose stop emits nothing.
type streamState struct {
	emit    func(provider.Part) error
	open    map[int64]string // content block index to tool_use id
	order   []int64          // indexes in arrival order
	stopRaw string
}

func newStreamState(emit func(provider.Part) error) *streamState {
	return &streamState{emit: emit, open: map[int64]string{}}
}

func (s *streamState) event(ev anthropic.MessageStreamEventUnion) error {
	switch ev.Type {
	case "message_start":
		u := ev.Message.Usage
		return s.emit(provider.Part{Type: provider.PartUsage, Usage: session.Usage{
			Input:      u.InputTokens,
			CacheRead:  u.CacheReadInputTokens,
			CacheWrite: u.CacheCreationInputTokens,
		}})
	case "content_block_start":
		if ev.ContentBlock.Type == "tool_use" {
			s.open[ev.Index] = ev.ContentBlock.ID
			s.order = append(s.order, ev.Index)
			return s.emit(provider.Part{Type: provider.PartToolUseStart, ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name})
		}
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			return s.emit(provider.Part{Type: provider.PartTextDelta, Text: ev.Delta.Text})
		case "thinking_delta":
			return s.emit(provider.Part{Type: provider.PartThinkingDelta, Text: ev.Delta.Thinking})
		case "signature_delta":
			return s.emit(provider.Part{Type: provider.PartThinkingSignature, Signature: ev.Delta.Signature})
		case "input_json_delta":
			if id, ok := s.open[ev.Index]; ok {
				return s.emit(provider.Part{Type: provider.PartToolUseDelta, ID: id, Text: ev.Delta.PartialJSON})
			}
		}
	case "content_block_stop":
		if id, ok := s.open[ev.Index]; ok {
			return s.closeToolUse(ev.Index, id)
		}
	case "message_delta":
		s.stopRaw = string(ev.Delta.StopReason)
		return s.emit(provider.Part{Type: provider.PartUsage, Usage: session.Usage{Output: ev.Usage.OutputTokens}})
	}
	return nil
}

func (s *streamState) closeToolUse(index int64, id string) error {
	delete(s.open, index)
	return s.emit(provider.Part{Type: provider.PartToolUseEnd, ID: id})
}

// finish closes any tool_use the stream never stopped, then emits the one stop part. A stream
// that carried no stop_reason stops as other with an empty raw value.
func (s *streamState) finish() error {
	for _, index := range s.order {
		id, ok := s.open[index]
		if !ok {
			continue
		}
		if err := s.closeToolUse(index, id); err != nil {
			return err
		}
	}
	return s.emit(provider.Part{Type: provider.PartStop, StopReason: mapStop(s.stopRaw), StopReasonRaw: s.stopRaw})
}

func mapStop(raw string) session.StopReason {
	switch raw {
	case "end_turn", "stop_sequence":
		return session.StopEndTurn
	case "tool_use":
		return session.StopToolUse
	case "max_tokens":
		return session.StopMaxTokens
	}
	return session.StopOther
}
