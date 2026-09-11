package openaichat

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

func compact(t *testing.T, s string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		t.Fatalf("compact: %v", err)
	}
	return buf.String()
}

func TestBuildRequestGolden(t *testing.T) {
	req := provider.Request{
		Model:  session.ModelRef{Provider: "aperture", Model: "cline-pass/kimi-k3"},
		System: "You are rudy.",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []session.Block{session.TextBlock("read go.mod")}},
			{Role: provider.RoleAssistant, Content: []session.Block{
				{Type: session.BlockThinking, Text: "I should read it.", Signature: "sig"},
				session.ToolUseBlock("call_1", "read", json.RawMessage(`{"path":"go.mod"}`)),
			}},
			{Role: provider.RoleToolResult, Results: []provider.ToolResult{{ToolUseID: "call_1", Content: []session.Block{session.TextBlock("module x")}}}},
		},
		Tools: []provider.ToolDef{{
			Name:        "read",
			Description: "Read a file",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}},
		MaxTokens: 512,
	}
	got, err := buildRequest(req)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	want := compact(t, `{
	  "model": "cline-pass/kimi-k3",
	  "messages": [
	    {"role": "system", "content": "You are rudy."},
	    {"role": "user", "content": "read go.mod"},
	    {"role": "assistant", "content": "", "tool_calls": [
	      {"id": "call_1", "type": "function", "function": {"name": "read", "arguments": "{\"path\":\"go.mod\"}"}}
	    ]},
	    {"role": "tool", "content": "module x", "tool_call_id": "call_1"}
	  ],
	  "tools": [
	    {"type": "function", "function": {
	      "name": "read", "description": "Read a file",
	      "parameters": {"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}
	    }}
	  ],
	  "stream": true,
	  "stream_options": {"include_usage": true},
	  "max_tokens": 512
	}`)
	if string(got) != want {
		t.Fatalf("body mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildRequestToolInputIsVerbatim(t *testing.T) {
	raw := json.RawMessage(`{"b":  1, "a": [1,2 ]}`)
	req := provider.Request{
		Model: session.ModelRef{Provider: "p", Model: "m"},
		Messages: []provider.Message{
			{Role: provider.RoleAssistant, Content: []session.Block{session.ToolUseBlock("c", "t", raw)}},
		},
	}
	got, err := buildRequest(req)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	var w struct {
		Messages []struct {
			ToolCalls []struct {
				Function struct{ Arguments string } `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &w); err != nil {
		t.Fatal(err)
	}
	if a := w.Messages[0].ToolCalls[0].Function.Arguments; a != string(raw) {
		t.Fatalf("arguments were re-marshaled: %q", a)
	}
}

func TestBuildRequestRefusesImages(t *testing.T) {
	req := provider.Request{
		Model: session.ModelRef{Provider: "p", Model: "m"},
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []session.Block{{Type: session.BlockImage, MediaType: "image/png", SHA256: "abc"}}},
		},
	}
	_, err := buildRequest(req)
	if !errors.Is(err, errImageUnsupported) {
		t.Fatalf("want errImageUnsupported, got %v", err)
	}
}

// TestBuildRequestDropsAnEmptyAssistantMessage: a Ctrl-C during thinking records an assistant
// message whose only block is a thinking block, which this codec never sends back, leaving a
// message with neither content nor tool calls. The endpoint refuses that, and would refuse it
// on every later request too, /compact included, since they all replay the same log; the
// message is dropped instead and the ones either side of it still go.
func TestBuildRequestDropsAnEmptyAssistantMessage(t *testing.T) {
	req := provider.Request{
		Model: session.ModelRef{Provider: "p", Model: "m"},
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []session.Block{session.TextBlock("first")}},
			{Role: provider.RoleAssistant, Content: []session.Block{{Type: session.BlockThinking, Text: "interrupted reasoning"}}},
			{Role: provider.RoleUser, Content: []session.Block{session.TextBlock("second")}},
		},
	}
	got, err := buildRequest(req)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	want := compact(t, `{
	  "model": "m",
	  "messages": [
	    {"role": "user", "content": "first"},
	    {"role": "user", "content": "second"}
	  ],
	  "stream": true,
	  "stream_options": {"include_usage": true}
	}`)
	if string(got) != want {
		t.Fatalf("body mismatch\n got: %s\nwant: %s", got, want)
	}
}

// TestParallelToolResultsAreOneMessageEach is the mirror of the Messages API's rule, not a
// copy of it. Chat completions keys each answer to its own "tool" message by tool_call_id, so
// the group the kernel hands over (provider.Message.Results, one per concurrent call) is fanned
// back out here. Merging them the way the Anthropic codec must would send one answer for two
// calls and leave the second call unanswered.
func TestParallelToolResultsAreOneMessageEach(t *testing.T) {
	req := provider.Request{
		Model: session.ModelRef{Provider: "aperture", Model: "m"}, MaxTokens: 10,
		Messages: []provider.Message{
			{Role: provider.RoleAssistant, Content: []session.Block{
				session.ToolUseBlock("call_1", "read", json.RawMessage(`{}`)),
				session.ToolUseBlock("call_2", "read", json.RawMessage(`{}`)),
			}},
			{Role: provider.RoleToolResult, Results: []provider.ToolResult{
				{ToolUseID: "call_1", Content: []session.Block{session.TextBlock("one")}},
				{ToolUseID: "call_2", Content: []session.Block{session.TextBlock("two")}},
			}},
		},
	}
	got, err := buildRequest(req)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	var w wireRequest
	if err := json.Unmarshal(got, &w); err != nil {
		t.Fatalf("unmarshal %s: %v", got, err)
	}
	var tools []wireMessage
	for _, m := range w.Messages {
		if m.Role == "tool" {
			tools = append(tools, m)
		}
	}
	if len(tools) != 2 {
		t.Fatalf("tool messages = %d, want one per call: %s", len(tools), got)
	}
	for i, want := range []struct{ id, content string }{{"call_1", "one"}, {"call_2", "two"}} {
		if tools[i].ToolCallID != want.id || tools[i].Content != want.content {
			t.Fatalf("tool message %d = %+v, want %s answered with %q", i, tools[i], want.id, want.content)
		}
	}
}
