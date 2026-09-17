// SPDX-License-Identifier: AGPL-3.0-or-later

package anthropicmsgs

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

// wireBody is the request as it would leave, decoded far enough to count turns and the blocks
// in each. Asserting on the marshalled params rather than on the SDK structs is the point: the
// defect this file exists for was invisible in the domain request and visible only in the shape
// that reaches the API.
type wireBody struct {
	Messages []wireTurn `json:"messages"`
}

type wireTurn struct {
	Role    string      `json:"role"`
	Content []wireBlock `json:"content"`
}

// wireBlock is one content block, flat enough for every type this codec emits. Content is the
// nested block list a tool_result carries; the rest leave it empty.
type wireBlock struct {
	Type      string      `json:"type"`
	Text      string      `json:"text"`
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	ToolUseID string      `json:"tool_use_id"`
	IsError   bool        `json:"is_error"`
	Content   []wireBlock `json:"content"`
}

// hasText reports whether any block in the list says text.
func hasText(blocks []wireBlock, text string) bool {
	for _, b := range blocks {
		if b.Text == text {
			return true
		}
	}
	return false
}

func buildWire(t *testing.T, req provider.Request) wireBody {
	t.Helper()
	p, err := buildParams(req)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var w wireBody
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return w
}

func assistantWithCalls(ids ...string) provider.Message {
	m := provider.Message{Role: provider.RoleAssistant, Content: []session.Block{session.TextBlock("doing both")}}
	for _, id := range ids {
		m.Content = append(m.Content, session.ToolUseBlock(id, "read", json.RawMessage(`{}`)))
	}
	return m
}

// TestParallelToolResultsAreOneUserMessage is the shape parallel tool use is specified in, and
// the reason this file exists at all: there was no request_test.go here, so nothing checked it.
// One user message per result is accepted, because the API combines consecutive same-role
// turns, which is exactly why the defect is dangerous: no error ever appears, and the model,
// shown its parallel calls answered one turn at a time, learns to stop making them. The
// fan-out the subagents wave exists for would go quiet with nothing to debug (ADR 0028, the
// Turn.RunTool contracts row).
func TestParallelToolResultsAreOneUserMessage(t *testing.T) {
	w := buildWire(t, provider.Request{
		Model: session.ModelRef{Provider: "anth", Model: "claude-sonnet-5"}, MaxTokens: 100,
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []session.Block{session.TextBlock("read both")}},
			assistantWithCalls("toolu_a", "toolu_b"),
			{Role: provider.RoleToolResult, Results: []provider.ToolResult{
				{ToolUseID: "toolu_a", Content: []session.Block{session.TextBlock("a body")}},
				{ToolUseID: "toolu_b", Content: []session.Block{session.TextBlock("no b")}, IsError: true},
			}},
		},
	})
	if len(w.Messages) != 3 {
		t.Fatalf("messages = %d, want user, assistant and one user turn carrying both results: %+v", len(w.Messages), w.Messages)
	}
	results := w.Messages[2]
	if results.Role != "user" {
		t.Fatalf("tool results go back as a user turn, got %q", results.Role)
	}
	if len(results.Content) != 2 {
		t.Fatalf("the results turn carries %d blocks, want one tool_result per call: %+v", len(results.Content), results.Content)
	}
	for i, want := range []struct {
		id      string
		isError bool
	}{{"toolu_a", false}, {"toolu_b", true}} {
		b := results.Content[i]
		if b.Type != "tool_result" || b.ToolUseID != want.id || b.IsError != want.isError {
			t.Fatalf("block %d = %+v, want tool_result %s is_error=%v", i, b, want.id, want.isError)
		}
	}
}

// TestOneToolResultIsStillOneUserMessage keeps the ordinary case honest: grouping must not
// wrap a single result in anything new.
func TestOneToolResultIsStillOneUserMessage(t *testing.T) {
	w := buildWire(t, provider.Request{
		Model: session.ModelRef{Provider: "anth", Model: "claude-sonnet-5"}, MaxTokens: 100,
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []session.Block{session.TextBlock("read it")}},
			assistantWithCalls("toolu_a"),
			{Role: provider.RoleToolResult, Results: []provider.ToolResult{
				{ToolUseID: "toolu_a", Content: []session.Block{session.TextBlock("a body")}},
			}},
		},
	})
	if len(w.Messages) != 3 || len(w.Messages[2].Content) != 1 {
		t.Fatalf("one result must be one block in one user turn: %+v", w.Messages)
	}
	if w.Messages[2].Content[0].ToolUseID != "toolu_a" {
		t.Fatalf("block %+v", w.Messages[2].Content[0])
	}
}

// TestSeparateTurnsResultsStaySeparateMessages is the other direction: two turns' results must
// not merge, or the second turn's answers would arrive attached to the first turn's calls.
func TestSeparateTurnsResultsStaySeparateMessages(t *testing.T) {
	w := buildWire(t, provider.Request{
		Model: session.ModelRef{Provider: "anth", Model: "claude-sonnet-5"}, MaxTokens: 100,
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []session.Block{session.TextBlock("go")}},
			assistantWithCalls("toolu_a"),
			{Role: provider.RoleToolResult, Results: []provider.ToolResult{{ToolUseID: "toolu_a", Content: []session.Block{session.TextBlock("a")}}}},
			assistantWithCalls("toolu_b"),
			{Role: provider.RoleToolResult, Results: []provider.ToolResult{{ToolUseID: "toolu_b", Content: []session.Block{session.TextBlock("b")}}}},
		},
	})
	if len(w.Messages) != 5 {
		t.Fatalf("messages = %d, want each turn's results in their own: %+v", len(w.Messages), w.Messages)
	}
	for _, i := range []int{2, 4} {
		if len(w.Messages[i].Content) != 1 {
			t.Fatalf("message %d carries %d blocks, want its own turn's one", i, len(w.Messages[i].Content))
		}
	}
}

// TestToolResultMessageWithNoResults is the empty case: nothing to send rather than an empty
// user turn, which the API refuses.
func TestToolResultMessageWithNoResults(t *testing.T) {
	_, err := buildMessage(provider.Message{Role: provider.RoleToolResult})
	if err == nil || !strings.Contains(err.Error(), "no content to send") {
		t.Fatalf("err = %v, want errNothingToSend", err)
	}
}
