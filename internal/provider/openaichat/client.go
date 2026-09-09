// Package openaichat is the openai_chat codec: the OpenAI chat-completions wire shape, shared
// by every server that speaks it closely enough (OpenAI itself, aperture, mlx, llama.cpp).
// Requests go out through httpx so they carry rudy's retry policy and headers.
//
// Dialects: an endpoint that bends the shape passes a Dialect through Options.Dialect. The one
// dialect today is clinepass (internal/plugins/clinepass), aperture's cline-pass route, which
// wraps a non-streaming response in {"data": …} and reports an empty completion as a bare 500
// instead of an empty one.
package openaichat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/provider/httpx"
	"github.com/guygrigsby/rudy/internal/session"
)

const (
	defaultIdle  = 120 * time.Second
	maxErrorBody = 1 << 20
	maxJSONBody  = 8 << 20
)

// Dialect adjusts the codec for an endpoint that bends the chat-completions shape. Every
// method has a no-op default when Dialect is nil.
type Dialect interface {
	// UnwrapJSON returns the chat.completion object inside a non-streaming body, or the
	// body unchanged.
	UnwrapJSON(body []byte) []byte
	// ErrorMessage extracts a message from an error body the codec's own parser did not
	// understand; "" defers to the default.
	ErrorMessage(status int, body []byte) string
}

type Options struct {
	Name    string
	BaseURL string            // ends with /v1
	Token   string            // "" sends no Authorization
	Headers map[string]string // extra, verbatim
	HTTP    *httpx.Client
	Dialect Dialect // nil means the plain chat-completions shape
}

type Client struct {
	opts Options
	idle time.Duration
}

func New(o Options) *Client {
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	return &Client{opts: o, idle: defaultIdle}
}

func (c *Client) Name() string { return c.opts.Name }

// newRequest builds one HTTP request. headers, which a before_request hook produced, is
// applied last so a hook can override a configured header but never the ones the transport
// itself depends on being right.
func (c *Client) newRequest(ctx context.Context, method, path string, body []byte, headers map[string]string) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.opts.BaseURL+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "text/event-stream, application/json")
	if c.opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.opts.Token)
	}
	for k, v := range c.opts.Headers {
		req.Header.Set(k, v)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

func (c *Client) Complete(ctx context.Context, req provider.Request, emit func(provider.Part) error) error {
	body, err := buildRequest(req)
	if err != nil {
		return err
	}
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	hreq, err := c.newRequest(ctx, http.MethodPost, "/chat/completions", body, req.Headers)
	if err != nil {
		return err
	}
	resp, err := c.opts.HTTP.Do(ctx, hreq, req.SessionID)
	if err != nil {
		return transportError(parent, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return c.statusError(resp)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return unwrapEmit(c.completeJSON(resp, emit))
	}

	idle := httpx.IdleBody(resp.Body, c.idle, cancel)
	defer func() { _ = idle.Close() }()
	err = readSSE(idle, newStreamState(emit))
	switch {
	case err == nil:
		return nil
	case httpx.IdleFired(idle):
		return &provider.Error{Class: session.ErrTransport, Message: fmt.Sprintf("idle timeout after %s", c.idle), Attempts: httpx.AttemptsOf(resp)}
	case parent.Err() != nil:
		return parent.Err()
	}
	var ee emitError
	if errors.As(err, &ee) {
		return ee.err
	}
	var perr *provider.Error
	if errors.As(err, &perr) {
		return err
	}
	return &provider.Error{Class: session.ErrTransport, Message: err.Error(), Attempts: httpx.AttemptsOf(resp)}
}

// completion is a non-streaming chat.completion. Only the first choice is used.
type completion struct {
	Choices []struct {
		Message struct {
			Content          string            `json:"content"`
			Reasoning        *string           `json:"reasoning"`
			ReasoningDetails []reasoningDetail `json:"reasoning_details"`
			ToolCalls        []chunkToolCall   `json:"tool_calls"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
}

// completeJSON handles a server that ignored stream: true. The body must be a plain
// chat.completion once the dialect, if any, has unwrapped it; a body with no dialect and the
// clinepass {"data": …} envelope is refused.
func (c *Client) completeJSON(resp *http.Response, emit func(provider.Part) error) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBody))
	if err != nil {
		return &provider.Error{Class: session.ErrTransport, Message: err.Error(), Attempts: httpx.AttemptsOf(resp)}
	}
	if c.opts.Dialect != nil {
		body = c.opts.Dialect.UnwrapJSON(body)
	}
	var comp completion
	if err := json.Unmarshal(body, &comp); err != nil || len(comp.Choices) == 0 {
		return &provider.Error{
			Class:    session.ErrProvider,
			Status:   resp.StatusCode,
			Message:  "response is neither an event stream nor a chat completion",
			Body:     body,
			Attempts: httpx.AttemptsOf(resp),
		}
	}
	ch := comp.Choices[0]
	d := chunkDelta{
		Content:          ch.Message.Content,
		Reasoning:        ch.Message.Reasoning,
		ReasoningDetails: ch.Message.ReasoningDetails,
	}
	for i, tc := range ch.Message.ToolCalls {
		tc.Index = i
		d.ToolCalls = append(d.ToolCalls, tc)
	}
	s := newStreamState(emit)
	if err := s.chunk(chunk{Choices: []chunkChoice{{Delta: d, FinishReason: ch.FinishReason}}, Usage: comp.Usage}); err != nil {
		return err
	}
	return s.finish()
}

// transportError classifies a failure from httpx.Do. When httpx gave up after retries the
// attempt count comes from *httpx.Error; a single failed attempt reports 1.
func transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	attempts := 1
	var he *httpx.Error
	if errors.As(err, &he) {
		attempts = he.Attempts
	}
	return &provider.Error{Class: session.ErrTransport, Message: err.Error(), Attempts: attempts}
}

// statusError reads a non-2xx body and extracts the message from either the OpenAI error
// object {"error":{"message":…}} or the aperture envelope {"error":"…","success":false}. A
// dialect's own extraction wins when it returns non-empty, since it understands a body shape
// the default parser does not. The body is kept verbatim. Attempts is how many times httpx
// sent the request before this response came back.
func (c *Client) statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	msg := errorMessage(body)
	if c.opts.Dialect != nil {
		if dm := c.opts.Dialect.ErrorMessage(resp.StatusCode, body); dm != "" {
			msg = dm
		}
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return &provider.Error{Class: session.ErrProvider, Status: resp.StatusCode, Message: msg, Body: body, Attempts: httpx.AttemptsOf(resp)}
}

func errorMessage(body []byte) string {
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&env); err != nil || len(env.Error) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(env.Error, &s) == nil {
		return s
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(env.Error, &obj) == nil {
		return obj.Message
	}
	return ""
}
