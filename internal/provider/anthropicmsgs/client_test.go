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
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/provider/httpx"
	"github.com/guygrigsby/rudy/internal/session"
)

func TestCompleteStreamsTextAndToolUse(t *testing.T) {
	var got struct {
		raw     []byte
		body    map[string]any
		headers http.Header
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.headers = r.Header.Clone()
		got.raw, _ = io.ReadAll(r.Body)
		_ = json.Unmarshal(got.raw, &got.body)
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
			{Role: provider.RoleToolResult, Results: []provider.ToolResult{{ToolUseID: "toolu_00", Content: []session.Block{session.TextBlock("contents")}}}},
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
	// The schema and the tool input are asserted on the bytes that left, not on a map they
	// were decoded into: a codec that re-encoded either one would pass that.
	if !strings.Contains(string(got.raw), `"input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`) {
		t.Errorf("schema not verbatim on the wire: %s", got.raw)
	}
	if !strings.Contains(string(got.raw), `{"type":"tool_use","id":"toolu_00","name":"read","input":{"path":"a"}}`) {
		t.Errorf("tool_use input not verbatim on the wire: %s", got.raw)
	}
	tools := got.body["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "read" || tools[0].(map[string]any)["description"] != "Read" {
		t.Errorf("tool %v", tools[0])
	}
	sys := got.body["system"].([]any)
	if len(sys) != 1 || sys[0].(map[string]any)["text"] != "SYS" {
		t.Errorf("system %v", sys)
	}
	msgs := got.body["messages"].([]any)
	asst := msgs[1].(map[string]any)["content"].([]any)
	if asst[0].(map[string]any)["signature"] != "SIG" || asst[0].(map[string]any)["thinking"] != "hm" {
		t.Errorf("assistant content %v", asst)
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

func TestBuildParamsSkipsUnsignedThinking(t *testing.T) {
	req := provider.Request{
		Model: session.ModelRef{Provider: "anth", Model: "claude-sonnet-5"}, MaxTokens: 100,
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []session.Block{session.TextBlock("hi")}},
			{Role: provider.RoleAssistant, Content: []session.Block{
				{Type: session.BlockThinking, Text: "unsigned reasoning"},
				{Type: session.BlockThinking, Text: "signed reasoning", Signature: "SIG"},
				session.TextBlock("answer"),
			}},
		},
	}
	p, err := buildParams(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	// A thinking block with no signature is refused by the API, and a session that moved here
	// from a provider that signs nothing carries one on every request.
	if strings.Contains(string(body), "unsigned reasoning") {
		t.Errorf("unsigned thinking reached the wire: %s", body)
	}
	if !strings.Contains(string(body), `{"signature":"SIG","thinking":"signed reasoning","type":"thinking"}`) {
		t.Errorf("signed thinking must still be sent: %s", body)
	}
	if !strings.Contains(string(body), `{"text":"answer","type":"text"}`) {
		t.Errorf("text must still be sent: %s", body)
	}
}

// TestBuildParamsDropsAnEmptyAssistantMessage: a Ctrl-C during thinking records an assistant
// message whose only block is an unsigned one, the loop above drops that block, and a message
// with no content at all is a 400 from the API. It would be a 400 on every later request too,
// /compact included, since they all replay the same log; the message is dropped instead and
// the ones either side of it still go.
func TestBuildParamsDropsAnEmptyAssistantMessage(t *testing.T) {
	req := provider.Request{
		Model: session.ModelRef{Provider: "anth", Model: "claude-sonnet-5"}, MaxTokens: 100,
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []session.Block{session.TextBlock("first")}},
			{Role: provider.RoleAssistant, Content: []session.Block{{Type: session.BlockThinking, Text: "interrupted reasoning"}}},
			{Role: provider.RoleUser, Content: []session.Block{session.TextBlock("second")}},
		},
	}
	p, err := buildParams(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Messages) != 2 {
		t.Fatalf("sent %d messages, want the two user messages: %+v", len(p.Messages), p.Messages)
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "interrupted reasoning") || strings.Contains(string(body), `"role":"assistant"`) {
		t.Errorf("the empty assistant message reached the wire: %s", body)
	}
	for _, want := range []string{`{"text":"first","type":"text"}`, `{"text":"second","type":"text"}`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("message around the dropped one is missing %s: %s", want, body)
		}
	}
}

// TestToolResultCarriesItsOutcome: the model has to be able to tell a tool that failed from
// one that answered, and is_error is where the Messages API says so.
func TestToolResultCarriesItsOutcome(t *testing.T) {
	req := provider.Request{
		Model: session.ModelRef{Provider: "anth", Model: "claude-sonnet-5"}, MaxTokens: 100,
		Messages: []provider.Message{
			{Role: provider.RoleToolResult, Results: []provider.ToolResult{
				{ToolUseID: "toolu_ok", Content: []session.Block{session.TextBlock("fine")}},
				{ToolUseID: "toolu_bad", Content: []session.Block{session.TextBlock("boom")}, IsError: true},
			}},
		},
	}
	p, err := buildParams(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"tool_use_id":"toolu_bad","is_error":true`) {
		t.Errorf("a failed tool result must be sent with is_error: %s", body)
	}
	if !strings.Contains(string(body), `"tool_use_id":"toolu_ok","is_error":false`) {
		t.Errorf("a clean tool result must not be sent with is_error: %s", body)
	}
}

func TestMapStop(t *testing.T) {
	cases := map[string]session.StopReason{
		"end_turn":                      session.StopEndTurn,
		"stop_sequence":                 session.StopEndTurn,
		"tool_use":                      session.StopToolUse,
		"max_tokens":                    session.StopMaxTokens,
		"refusal":                       session.StopRefused,
		"model_context_window_exceeded": session.StopOther,
		"":                              session.StopOther,
	}
	for raw, want := range cases {
		if got := mapStop(raw); got != want {
			t.Errorf("mapStop(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestStreamRefusalKeepsTheRawStopReason(t *testing.T) {
	var parts []provider.Part
	s := newStreamState(func(p provider.Part) error { parts = append(parts, p); return nil })
	var ev anthropic.MessageStreamEventUnion
	if err := json.Unmarshal([]byte(`{"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":3}}`), &ev); err != nil {
		t.Fatal(err)
	}
	if err := s.event(ev); err != nil {
		t.Fatal(err)
	}
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
	stop := parts[len(parts)-1]
	if stop.Type != provider.PartStop || stop.StopReason != session.StopRefused || stop.StopReasonRaw != "refusal" {
		t.Fatalf("stop %+v", stop)
	}
}

func TestCompleteIdleTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":25}}}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	c := New(Options{Name: "anth", BaseURL: srv.URL, APIKey: "k", HTTP: httpx.New("test")})
	c.idle = 50 * time.Millisecond

	var got []provider.Part
	start := time.Now()
	err := c.Complete(context.Background(), provider.Request{
		Model:     session.ModelRef{Provider: "anth", Model: "claude-sonnet-5"},
		MaxTokens: 100,
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: []session.Block{session.TextBlock("hi")}}},
		SessionID: ulid.Make(),
	}, func(p provider.Part) error { got = append(got, p); return nil })
	if d := time.Since(start); d > time.Second {
		t.Errorf("waited %s for a 50ms idle timeout", d)
	}
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Class != session.ErrTransport || !strings.Contains(perr.Message, "idle timeout") {
		t.Fatalf("want idle transport error, got %v", err)
	}
	if len(got) != 1 || got[0].Type != provider.PartUsage {
		t.Fatalf("parts before the timeout = %+v", got)
	}
}

func TestRetriedStatusCarriesTheAttemptCount(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"Upstream busy"}}`)
	}))
	defer srv.Close()
	h := httpx.New("test")
	h.Sleep = func(time.Duration) {}
	c := New(Options{Name: "anth", BaseURL: srv.URL, APIKey: "k", HTTP: h})

	err := c.Complete(context.Background(), provider.Request{
		Model:     session.ModelRef{Provider: "anth", Model: "claude-sonnet-5"},
		MaxTokens: 100,
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: []session.Block{session.TextBlock("hi")}}},
		SessionID: ulid.Make(),
	}, func(provider.Part) error { return nil })
	var perr *provider.Error
	if !errors.As(err, &perr) {
		t.Fatalf("error %v (%T)", err, err)
	}
	// httpx retries 503 five times and hands back the last response; the SDK adds none.
	if perr.Class != session.ErrProvider || perr.Status != http.StatusServiceUnavailable || perr.Message != "Upstream busy" || perr.Attempts != 5 {
		t.Errorf("provider error %+v", perr)
	}
	if hits != 5 {
		t.Errorf("server saw %d requests, want 5", hits)
	}
}

func TestErrorMessageFallsBackToTheStatus(t *testing.T) {
	cases := []struct {
		body   string
		status int
		want   string
	}{
		{`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, 529, "Overloaded"},
		// 529 is not a registered status, so http.StatusText has nothing to say about it.
		{`{"type":"error","error":{"type":"overloaded_error"}}`, 529, "HTTP 529"},
		{"", 503, "Service Unavailable"},
		{"<html>gateway</html>", 502, "Bad Gateway"},
	}
	for _, c := range cases {
		if got := errorMessage([]byte(c.body), c.status); got != c.want {
			t.Errorf("errorMessage(%q, %d) = %q, want %q", c.body, c.status, got, c.want)
		}
	}
}
