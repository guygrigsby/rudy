package anthropicmsgs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/provider/httpx"
	"github.com/guygrigsby/rudy/internal/session"
)

func TestCompleteStreamsTextAndToolUse(t *testing.T) {
	var got struct {
		body    map[string]any
		headers http.Header
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.headers = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got.body)
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := os.ReadFile("testdata/text-tool-stream.sse")
		_, _ = w.Write(f)
	}))
	defer srv.Close()
	c := New(Options{Name: "anth", BaseURL: srv.URL, APIKey: "k", Headers: map[string]string{"X-Extra": "1"}, HTTP: httpx.New("test")})
	req := provider.Request{
		Model: session.ModelRef{Provider: "anth", Model: "claude-sonnet-5"}, System: "SYS", MaxTokens: 100, Thinking: session.ThinkingOff,
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []session.Block{session.TextBlock("hi")}},
			{Role: provider.RoleAssistant, Content: []session.Block{{Type: session.BlockThinking, Text: "hm", Signature: "SIG"}, session.ToolUseBlock("toolu_00", "read", json.RawMessage(`{"path": "a"}`))}},
			{Role: provider.RoleToolResult, ToolUseID: "toolu_00", Content: []session.Block{session.TextBlock("contents")}},
		},
		Tools:     []provider.ToolDef{{Name: "read", Description: "Read", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)}},
		SessionID: ulid.Make(),
	}
	var parts []provider.Part
	if err := c.Complete(context.Background(), req, func(p provider.Part) error { parts = append(parts, p); return nil }); err != nil {
		t.Fatal(err)
	}
	// request shape
	if got.headers.Get("x-api-key") != "k" || got.headers.Get("X-Extra") != "1" || !strings.HasPrefix(got.headers.Get("User-Agent"), "rudy/test") || got.headers.Get("X-Rudy-Session") == "" || got.headers.Get("anthropic-version") == "" {
		t.Errorf("headers %v", got.headers)
	}
	if got.body["stream"] != true || got.body["max_tokens"] != float64(100) || got.body["model"] != "claude-sonnet-5" {
		t.Errorf("body %v", got.body)
	}
	tools := got.body["tools"].([]any)
	schema, _ := json.Marshal(tools[0].(map[string]any)["input_schema"])
	if string(schema) != `{"additionalProperties":false,"properties":{"path":{"type":"string"}},"required":["path"],"type":"object"}` {
		t.Errorf("schema re-encoded differently: %s", schema)
	}
	if tools[0].(map[string]any)["name"] != "read" || tools[0].(map[string]any)["description"] != "Read" {
		t.Errorf("tool %v", tools[0])
	}
	sys := got.body["system"].([]any)
	if len(sys) != 1 || sys[0].(map[string]any)["text"] != "SYS" {
		t.Errorf("system %v", sys)
	}
	msgs := got.body["messages"].([]any)
	asst := msgs[1].(map[string]any)["content"].([]any)
	if asst[0].(map[string]any)["signature"] != "SIG" || asst[1].(map[string]any)["input"].(map[string]any)["path"] != "a" {
		t.Errorf("assistant content %v", asst)
	}
	if asst[1].(map[string]any)["id"] != "toolu_00" || asst[1].(map[string]any)["name"] != "read" || asst[1].(map[string]any)["type"] != "tool_use" {
		t.Errorf("tool_use block %v", asst[1])
	}
	if len(msgs) != 3 || msgs[2].(map[string]any)["role"] != "user" {
		t.Errorf("tool result must be its own user message: %v", msgs)
	}
	res := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if res["type"] != "tool_result" || res["tool_use_id"] != "toolu_00" {
		t.Errorf("tool_result %v", res)
	}
	if got.body["thinking"] != nil {
		t.Errorf("thinking off must send no thinking config: %v", got.body["thinking"])
	}
	// parts
	var types []provider.PartType
	for _, p := range parts {
		types = append(types, p.Type)
	}
	want := []provider.PartType{provider.PartUsage, provider.PartTextDelta, provider.PartToolUseStart, provider.PartToolUseDelta, provider.PartToolUseDelta, provider.PartToolUseEnd, provider.PartUsage, provider.PartStop}
	if !reflect.DeepEqual(types, want) {
		t.Errorf("parts %v\nwant  %v", types, want)
	}
	if parts[2].ID != "toolu_01" || parts[2].Name != "read" || parts[3].Text+parts[4].Text != `{"path":"go.mod"}` {
		t.Errorf("tool use parts %+v", parts[2:5])
	}
	if parts[7].StopReason != session.StopToolUse || parts[7].StopReasonRaw != "tool_use" || parts[6].Usage.Output != 30 || parts[0].Usage.Input != 25 {
		t.Errorf("stop and usage %+v %+v", parts[7], parts[6])
	}
}

func TestCompleteThinkingSignature(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := os.ReadFile("testdata/thinking-stream.sse")
		_, _ = w.Write(f)
	}))
	defer srv.Close()
	c := New(Options{Name: "anth", BaseURL: srv.URL, APIKey: "k", HTTP: httpx.New("test")})
	req := provider.Request{
		Model: session.ModelRef{Provider: "anth", Model: "claude-sonnet-5"}, MaxTokens: 100, Thinking: session.ThinkingHigh,
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: []session.Block{session.TextBlock("which file pins the sdk?")}}},
		SessionID: ulid.Make(),
	}
	var parts []provider.Part
	if err := c.Complete(context.Background(), req, func(p provider.Part) error { parts = append(parts, p); return nil }); err != nil {
		t.Fatal(err)
	}
	think, _ := got["thinking"].(map[string]any)
	if think == nil || think["type"] != "enabled" || think["budget_tokens"] != float64(16384) {
		t.Errorf("thinking %v", got["thinking"])
	}
	// max_tokens must cover the budget: 100 would be rejected by the API.
	if got["max_tokens"] != float64(16384+1024) {
		t.Errorf("max_tokens %v", got["max_tokens"])
	}
	var types []provider.PartType
	for _, p := range parts {
		types = append(types, p.Type)
	}
	want := []provider.PartType{
		provider.PartUsage, provider.PartThinkingDelta, provider.PartThinkingDelta, provider.PartThinkingSignature,
		provider.PartTextDelta, provider.PartUsage, provider.PartStop,
	}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("parts %v\nwant  %v", types, want)
	}
	if parts[1].Text+parts[2].Text != "The file to read is go.mod." {
		t.Errorf("thinking text %q%q", parts[1].Text, parts[2].Text)
	}
	if parts[3].Signature != "EqQBCgIYAhIM+/abc==" {
		t.Errorf("signature %q", parts[3].Signature)
	}
	if parts[4].Text != "go.mod pins the SDK." {
		t.Errorf("text %q", parts[4].Text)
	}
	if parts[0].Usage.Input != 40 || parts[0].Usage.CacheRead != 12 || parts[0].Usage.CacheWrite != 7 {
		t.Errorf("usage %+v", parts[0].Usage)
	}
	if parts[6].StopReason != session.StopEndTurn || parts[6].StopReasonRaw != "end_turn" {
		t.Errorf("stop %+v", parts[6])
	}
}

func TestListModelsAndErrors(t *testing.T) {
	var messagesHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			f, _ := os.ReadFile("testdata/models.json")
			_, _ = w.Write(f)
		case "/v1/messages":
			messagesHits++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(529)
			f, _ := os.ReadFile("testdata/error-529.json")
			_, _ = w.Write(f)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(Options{Name: "anth", BaseURL: srv.URL, APIKey: "k", HTTP: httpx.New("test")})

	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []provider.Model{
		{
			Ref:           session.ModelRef{Provider: "anth", Model: "claude-haiku-4.5"},
			DisplayName:   "Claude Haiku 4.5",
			ContextWindow: 200000,
			MaxOutput:     8192,
			Capabilities:  provider.Capabilities{Tools: true, Vision: true, Reasoning: true},
		},
		{
			Ref:           session.ModelRef{Provider: "anth", Model: "claude-sonnet-5"},
			DisplayName:   "Claude Sonnet 5",
			ContextWindow: 200000,
			MaxOutput:     64000,
			Capabilities:  provider.Capabilities{Tools: true, Vision: true, Reasoning: true},
		},
	}
	if !reflect.DeepEqual(models, want) {
		t.Errorf("models %+v\nwant   %+v", models, want)
	}

	req := provider.Request{
		Model:     session.ModelRef{Provider: "anth", Model: "claude-sonnet-5"},
		MaxTokens: 100,
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: []session.Block{session.TextBlock("hi")}}},
		SessionID: ulid.Make(),
	}
	err = c.Complete(context.Background(), req, func(provider.Part) error { return nil })
	var perr *provider.Error
	if !errors.As(err, &perr) {
		t.Fatalf("error %v (%T)", err, err)
	}
	// 529 is not in httpx.Retryable, so httpx sends it once and the SDK retries nothing.
	if perr.Class != session.ErrProvider || perr.Status != 529 || perr.Message != "Overloaded" || perr.Attempts != 1 {
		t.Errorf("provider error %+v", perr)
	}
	if !strings.Contains(string(perr.Body), "overloaded_error") {
		t.Errorf("body %s", perr.Body)
	}
	if messagesHits != 1 {
		t.Errorf("messages hit %d times, the SDK must retry nothing", messagesHits)
	}
}

func TestCompleteRefusesImageBlocks(t *testing.T) {
	c := New(Options{Name: "anth", BaseURL: "http://127.0.0.1:1", HTTP: httpx.New("test")})
	req := provider.Request{
		Model:     session.ModelRef{Provider: "anth", Model: "claude-sonnet-5"},
		MaxTokens: 100,
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: []session.Block{{Type: session.BlockImage, MediaType: "image/png", SHA256: "abc"}}}},
		SessionID: ulid.Make(),
	}
	err := c.Complete(context.Background(), req, func(provider.Part) error { return nil })
	if !errors.Is(err, errImageUnsupported) {
		t.Fatalf("error %v", err)
	}
}
