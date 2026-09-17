package openaichat

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

// chunk is one chat.completion.chunk. Non-streaming completions are folded into
// the same shape by completeJSON so both paths share the part emitter.
type chunk struct {
	Choices []chunkChoice `json:"choices"`
	Usage   *wireUsage    `json:"usage"`
}

type chunkChoice struct {
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chunkDelta struct {
	Content          string            `json:"content"`
	Reasoning        *string           `json:"reasoning"`
	ReasoningDetails []reasoningDetail `json:"reasoning_details"`
	ToolCalls        []chunkToolCall   `json:"tool_calls"`
}

type reasoningDetail struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type chunkToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireUsage struct {
	PromptTokens             int64 `json:"prompt_tokens"`
	CompletionTokens         int64 `json:"completion_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	PromptTokensDetails      *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u *wireUsage) usage() session.Usage {
	out := session.Usage{
		Input:      u.PromptTokens,
		Output:     u.CompletionTokens,
		CacheWrite: u.CacheCreationInputTokens,
	}
	if u.PromptTokensDetails != nil {
		out.CacheRead = u.PromptTokensDetails.CachedTokens
	}
	return out
}

func mapStop(raw string) session.StopReason {
	switch raw {
	case "stop":
		return session.StopEndTurn
	case "tool_calls":
		return session.StopToolUse
	case "length":
		return session.StopMaxTokens
	case "content_filter":
		return session.StopRefused
	}
	return session.StopOther
}

// emitError wraps an error returned by the caller's emit so the client can hand
// it back verbatim instead of classifying it as a transport failure.
type emitError struct{ err error }

func (e emitError) Error() string { return e.err.Error() }
func (e emitError) Unwrap() error { return e.err }

func unwrapEmit(err error) error {
	var ee emitError
	if errors.As(err, &ee) {
		return ee.err
	}
	return err
}

// streamState turns chunks into parts. It tracks open tool calls by index and
// buffers the stop so it is emitted exactly once, last.
type streamState struct {
	emit  func(provider.Part) error
	open  map[int]string // index to the tool_use id currently open at it
	order []int          // indexes with an open call, in arrival order
	stop  *provider.Part
}

func newStreamState(emit func(provider.Part) error) *streamState {
	return &streamState{emit: emit, open: map[int]string{}}
}

func (s *streamState) send(p provider.Part) error {
	if err := s.emit(p); err != nil {
		return emitError{err}
	}
	return nil
}

func (s *streamState) chunk(c chunk) error {
	for _, ch := range c.Choices {
		if err := s.delta(ch.Delta); err != nil {
			return err
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			if err := s.closeToolCalls(); err != nil {
				return err
			}
			if s.stop == nil {
				raw := *ch.FinishReason
				s.stop = &provider.Part{Type: provider.PartStop, StopReason: mapStop(raw), StopReasonRaw: raw}
			}
		}
	}
	if c.Usage != nil {
		if err := s.send(provider.Part{Type: provider.PartUsage, Usage: c.Usage.usage()}); err != nil {
			return err
		}
	}
	return nil
}

func (s *streamState) delta(d chunkDelta) error {
	switch {
	case len(d.ReasoningDetails) > 0:
		for _, r := range d.ReasoningDetails {
			if r.Text == "" {
				continue
			}
			if err := s.send(provider.Part{Type: provider.PartThinkingDelta, Text: r.Text}); err != nil {
				return err
			}
		}
	case d.Reasoning != nil && *d.Reasoning != "":
		if err := s.send(provider.Part{Type: provider.PartThinkingDelta, Text: *d.Reasoning}); err != nil {
			return err
		}
	}
	if d.Content != "" {
		if err := s.send(provider.Part{Type: provider.PartTextDelta, Text: d.Content}); err != nil {
			return err
		}
	}
	for _, tc := range d.ToolCalls {
		id, known := s.open[tc.Index]
		if known && tc.ID != "" && tc.ID != id {
			// A different id at the same index is a different call, not more of this
			// one. Appending its arguments to the open call would assemble an input
			// out of two, and the Gate's consent is given for the bytes of one
			// (rudy-k0.25). Close the one that was open and start the new one.
			if err := s.closeCall(tc.Index); err != nil {
				return err
			}
			known = false
		}
		if !known {
			if tc.ID == "" || tc.Function.Name == "" {
				continue // a fragment for a call that never started; nothing to attach it to
			}
			id = tc.ID
			s.open[tc.Index] = id
			s.order = append(s.order, tc.Index)
			if err := s.send(provider.Part{Type: provider.PartToolUseStart, ID: id, Name: tc.Function.Name}); err != nil {
				return err
			}
		}
		if tc.Function.Arguments != "" {
			if err := s.send(provider.Part{Type: provider.PartToolUseDelta, ID: id, Text: tc.Function.Arguments}); err != nil {
				return err
			}
		}
	}
	return nil
}

// closeCall ends the call open at one index and forgets it, so the index is free for the
// next call the provider puts there and the end is emitted exactly once.
func (s *streamState) closeCall(index int) error {
	id, open := s.open[index]
	if !open {
		return nil
	}
	delete(s.open, index)
	s.order = slices.DeleteFunc(s.order, func(i int) bool { return i == index })
	return s.send(provider.Part{Type: provider.PartToolUseEnd, ID: id})
}

func (s *streamState) closeToolCalls() error {
	for _, idx := range s.order {
		if err := s.send(provider.Part{Type: provider.PartToolUseEnd, ID: s.open[idx]}); err != nil {
			return err
		}
	}
	s.open = map[int]string{}
	s.order = nil
	return nil
}

// finish emits the buffered stop. A stream that never carried a finish_reason
// stops as other with an empty raw value.
func (s *streamState) finish() error {
	if err := s.closeToolCalls(); err != nil {
		return err
	}
	if s.stop == nil {
		s.stop = &provider.Part{Type: provider.PartStop, StopReason: session.StopOther}
	}
	return s.send(*s.stop)
}

// readSSE feeds every data: line to s until [DONE] or EOF, then finishes.
func readSSE(r io.Reader, s *streamState) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var c chunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			return &provider.Error{Class: session.ErrProvider, Message: "malformed stream chunk: " + err.Error(), Body: []byte(data), Attempts: 1}
		}
		if err := s.chunk(c); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return s.finish()
}
