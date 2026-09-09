package openaichat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/provider/httpx"
	"github.com/guygrigsby/rudy/internal/session"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// serve returns a client pointed at a server that answers every request with
// the given status, content type and body and records the last request seen.
func serve(t *testing.T, status int, contentType string, body []byte) (*Client, *http.Request) {
	t.Helper()
	var last http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = *r
		last.Header = r.Header.Clone()
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	c := New(Options{
		Name:    "aperture",
		BaseURL: srv.URL + "/v1",
		Token:   "tok",
		Headers: map[string]string{"X-Test": "1"},
		HTTP:    httpx.New("test"),
	})
	return c, &last
}

func simpleRequest() provider.Request {
	return provider.Request{
		Model:     session.ModelRef{Provider: "aperture", Model: "cline-pass/kimi-k3"},
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: []session.Block{session.TextBlock("hi")}}},
		MaxTokens: 64,
		SessionID: ulid.Make(),
	}
}

func collect(t *testing.T, c *Client, req provider.Request) []provider.Part {
	t.Helper()
	var parts []provider.Part
	err := c.Complete(context.Background(), req, func(p provider.Part) error {
		parts = append(parts, p)
		return nil
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return parts
}

func withoutThinking(parts []provider.Part) []provider.Part {
	var out []provider.Part
	for _, p := range parts {
		if p.Type != provider.PartThinkingDelta {
			out = append(out, p)
		}
	}
	return out
}

func TestCompleteKimiToolCallStream(t *testing.T) {
	c, last := serve(t, 200, "text/event-stream", fixture(t, "kimi-tool-stream.sse"))
	parts := collect(t, c, simpleRequest())

	if last.URL.Path != "/v1/chat/completions" || last.Method != http.MethodPost {
		t.Fatalf("request went to %s %s", last.Method, last.URL.Path)
	}
	if got := last.Header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := last.Header.Get("X-Test"); got != "1" {
		t.Fatalf("X-Test = %q", got)
	}
	if got := last.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}

	thinking := 0
	for _, p := range parts {
		if p.Type == provider.PartThinkingDelta {
			thinking++
		}
	}
	if thinking == 0 {
		t.Fatal("expected thinking deltas from reasoning_details")
	}
	// Every thinking delta precedes the tool call.
	seenTool := false
	for _, p := range parts {
		if p.Type == provider.PartToolUseStart {
			seenTool = true
		}
		if p.Type == provider.PartThinkingDelta && seenTool {
			t.Fatal("thinking delta after tool_use_start")
		}
	}

	rest := withoutThinking(parts)
	if len(rest) < 5 {
		t.Fatalf("too few non-thinking parts: %+v", rest)
	}
	start := rest[0]
	if start.Type != provider.PartToolUseStart || start.ID != "read_file_0_82619100" || start.Name != "read_file" {
		t.Fatalf("first non-thinking part = %+v", start)
	}
	var args strings.Builder
	i := 1
	for ; i < len(rest) && rest[i].Type == provider.PartToolUseDelta; i++ {
		if rest[i].ID != start.ID {
			t.Fatalf("delta for wrong id %q", rest[i].ID)
		}
		args.WriteString(rest[i].Text)
	}
	if args.String() != `{"path": "go.mod"}` {
		t.Fatalf("arguments = %q", args.String())
	}
	if rest[i].Type != provider.PartToolUseEnd || rest[i].ID != start.ID {
		t.Fatalf("expected tool_use_end, got %+v", rest[i])
	}
	i++
	if rest[i].Type != provider.PartUsage || rest[i].Usage.Output != 126 || rest[i].Usage.Input != 178 {
		t.Fatalf("expected usage 178/126, got %+v", rest[i])
	}
	i++
	stop := rest[i]
	if stop.Type != provider.PartStop || stop.StopReason != session.StopToolUse || stop.StopReasonRaw != "tool_calls" {
		t.Fatalf("expected stop tool_use, got %+v", stop)
	}
	if i != len(rest)-1 {
		t.Fatalf("stop was not the last part: %+v", rest[i+1:])
	}
}

func TestCompleteDeepseekTextStream(t *testing.T) {
	c, _ := serve(t, 200, "text/event-stream", fixture(t, "deepseek-text-stream.sse"))
	parts := collect(t, c, simpleRequest())

	var text strings.Builder
	firstText, lastThinking := -1, -1
	stops := 0
	for i, p := range parts {
		switch p.Type {
		case provider.PartTextDelta:
			text.WriteString(p.Text)
			if firstText < 0 {
				firstText = i
			}
		case provider.PartThinkingDelta:
			lastThinking = i
		case provider.PartStop:
			stops++
			if i != len(parts)-1 {
				t.Fatalf("stop at %d of %d", i, len(parts))
			}
			if p.StopReason != session.StopEndTurn || p.StopReasonRaw != "stop" {
				t.Fatalf("stop = %+v", p)
			}
		case provider.PartUsage:
			if p.Usage.Output != 26 || p.Usage.Input != 9 {
				t.Fatalf("usage = %+v", p.Usage)
			}
		}
	}
	if text.String() != "ok" {
		t.Fatalf("text = %q", text.String())
	}
	if lastThinking < 0 || firstText < 0 || lastThinking > firstText {
		t.Fatalf("thinking must precede text: lastThinking=%d firstText=%d", lastThinking, firstText)
	}
	if stops != 1 {
		t.Fatalf("expected exactly one stop, got %d", stops)
	}
}

func TestCompleteEmptyContent500(t *testing.T) {
	// The fixture is a curl recording: line one is the body, the last line is the status.
	raw := string(fixture(t, "clinepass-empty-500.json"))
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	body := lines[0]
	if lines[len(lines)-1] != "500" {
		t.Fatalf("fixture status line = %q", lines[len(lines)-1])
	}
	c, _ := serve(t, 500, "application/json", []byte(body))
	err := c.Complete(context.Background(), simpleRequest(), func(provider.Part) error { return nil })
	var perr *provider.Error
	if !errors.As(err, &perr) {
		t.Fatalf("want *provider.Error, got %T %v", err, err)
	}
	if perr.Class != session.ErrProvider || perr.Status != 500 || perr.Message != "empty response content" {
		t.Fatalf("got %+v", perr)
	}
	if string(perr.Body) != body {
		t.Fatalf("body not kept verbatim: %q", perr.Body)
	}
}

func TestCompleteOpenAIErrorObject(t *testing.T) {
	c, _ := serve(t, 400, "application/json", []byte(`{"error":{"message":"model not found","type":"invalid_request_error"}}`))
	err := c.Complete(context.Background(), simpleRequest(), func(provider.Part) error { return nil })
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Message != "model not found" || perr.Status != 400 {
		t.Fatalf("got %v", err)
	}
}

func TestCompleteNonStreamingBody(t *testing.T) {
	// The recorded clinepass body wraps the completion in {"data": …}. The plain
	// completion inside it is what a standards-following server would return
	// when it ignores stream: true; the envelope itself is refused without a
	// dialect and unwrapped with one (see the WithDialect tests below).
	raw := fixture(t, "clinepass-nonstream.json")
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Data) == 0 {
		t.Fatalf("fixture has no data envelope: %v", err)
	}

	c, _ := serve(t, 200, "application/json", env.Data)
	parts := collect(t, c, simpleRequest())
	var text strings.Builder
	thinking := 0
	for _, p := range parts {
		switch p.Type {
		case provider.PartTextDelta:
			text.WriteString(p.Text)
		case provider.PartThinkingDelta:
			thinking++
		case provider.PartUsage:
			if p.Usage.Output != 20 || p.Usage.Input != 9 {
				t.Fatalf("usage = %+v", p.Usage)
			}
		}
	}
	if text.String() != "ok" || thinking != 1 {
		t.Fatalf("text=%q thinking=%d", text.String(), thinking)
	}
	if last := parts[len(parts)-1]; last.Type != provider.PartStop || last.StopReason != session.StopEndTurn {
		t.Fatalf("last = %+v", last)
	}

	c2, _ := serve(t, 200, "application/json", raw)
	err := c2.Complete(context.Background(), simpleRequest(), func(provider.Part) error { return nil })
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Class != session.ErrProvider || perr.Message != "response is neither an event stream nor a chat completion" {
		t.Fatalf("envelope should be refused, got %v", err)
	}
}

// dialectStub is a test Dialect whose two methods are supplied per test; a nil field means
// that method is never called.
type dialectStub struct {
	unwrap func([]byte) []byte
	errMsg func(int, []byte) string
}

func (d dialectStub) UnwrapJSON(body []byte) []byte {
	if d.unwrap == nil {
		return body
	}
	return d.unwrap(body)
}

func (d dialectStub) ErrorMessage(status int, body []byte) string {
	if d.errMsg == nil {
		return ""
	}
	return d.errMsg(status, body)
}

// unwrapClinepassData is the clinepass dialect's UnwrapJSON logic, duplicated here so the
// codec's test does not import the plugin package that ships it.
func unwrapClinepassData(body []byte) []byte {
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &env) != nil || len(env.Data) == 0 {
		return body
	}
	return env.Data
}

func TestCompleteNonStreamingBodyWithDialectUnwrapsData(t *testing.T) {
	raw := fixture(t, "clinepass-nonstream.json")
	c, _ := serve(t, 200, "application/json", raw)
	c.opts.Dialect = dialectStub{unwrap: unwrapClinepassData}

	parts := collect(t, c, simpleRequest())
	var text strings.Builder
	thinking := 0
	for _, p := range parts {
		switch p.Type {
		case provider.PartTextDelta:
			text.WriteString(p.Text)
		case provider.PartThinkingDelta:
			thinking++
		case provider.PartUsage:
			if p.Usage.Output != 20 || p.Usage.Input != 9 {
				t.Fatalf("usage = %+v", p.Usage)
			}
		}
	}
	if text.String() != "ok" || thinking != 1 {
		t.Fatalf("text=%q thinking=%d", text.String(), thinking)
	}
	if last := parts[len(parts)-1]; last.Type != provider.PartStop || last.StopReason != session.StopEndTurn {
		t.Fatalf("last = %+v", last)
	}
}

func TestCompleteEmptyContent500WithDialectNamesTokenBudget(t *testing.T) {
	raw := string(fixture(t, "clinepass-empty-500.json"))
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	body := lines[0]
	if lines[len(lines)-1] != "500" {
		t.Fatalf("fixture status line = %q", lines[len(lines)-1])
	}
	c, _ := serve(t, 500, "application/json", []byte(body))
	c.opts.Dialect = dialectStub{errMsg: func(status int, b []byte) string {
		if status == 500 && strings.Contains(string(b), "empty response content") {
			return "empty response content; max_tokens may be too small for the model's reasoning"
		}
		return ""
	}}

	err := c.Complete(context.Background(), simpleRequest(), func(provider.Part) error { return nil })
	var perr *provider.Error
	if !errors.As(err, &perr) {
		t.Fatalf("want *provider.Error, got %T %v", err, err)
	}
	want := "empty response content; max_tokens may be too small for the model's reasoning"
	if perr.Class != session.ErrProvider || perr.Status != 500 || perr.Message != want {
		t.Fatalf("got %+v", perr)
	}
	if string(perr.Body) != body {
		t.Fatalf("body not kept verbatim: %q", perr.Body)
	}
}

func TestCompleteIdleTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	c := New(Options{Name: "x", BaseURL: srv.URL + "/v1", HTTP: httpx.New("test")})
	c.idle = 50 * time.Millisecond

	var got []provider.Part
	err := c.Complete(context.Background(), simpleRequest(), func(p provider.Part) error {
		got = append(got, p)
		return nil
	})
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Class != session.ErrTransport || !strings.Contains(perr.Message, "idle timeout") {
		t.Fatalf("want idle transport error, got %v", err)
	}
	if len(got) != 1 || got[0].Text != "a" {
		t.Fatalf("parts before the timeout = %+v", got)
	}
}

func TestCompleteCancelFromEmit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	c := New(Options{Name: "x", BaseURL: srv.URL + "/v1", HTTP: httpx.New("test")})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := c.Complete(ctx, simpleRequest(), func(p provider.Part) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestCompleteEmitErrorIsReturnedVerbatim(t *testing.T) {
	c, _ := serve(t, 200, "text/event-stream", fixture(t, "deepseek-text-stream.sse"))
	sentinel := errors.New("stop here")
	err := c.Complete(context.Background(), simpleRequest(), func(p provider.Part) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel, got %v", err)
	}
}
