// Package anthropicmsgs is the Messages API codec, built on anthropic-sdk-go. It is the only
// package that may import the SDK: everything above it speaks provider.Request and
// provider.Part. Requests go out through httpx so they carry rudy's headers and its retry
// policy, and the SDK's own retries are off.
package anthropicmsgs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/provider/httpx"
	"github.com/guygrigsby/rudy/internal/session"
)

// Options is one configured Messages endpoint.
type Options struct {
	Name    string
	BaseURL string            // e.g. https://api.anthropic.com; the SDK appends /v1/messages
	APIKey  string            // "" sends no x-api-key
	Headers map[string]string // extra, verbatim
	HTTP    *httpx.Client
}

type Client struct{ opts Options }

func New(o Options) *Client {
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	return &Client{opts: o}
}

func (c *Client) Name() string { return c.opts.Name }

// sdk builds the SDK client for one request. Its transport is httpx bound to the session id,
// so every request carries rudy's User-Agent, X-Rudy-Session and retry policy and the SDK
// retries nothing itself. WithoutEnvironmentDefaults turns off the SDK's own credential
// autoload (ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL, ANTHROPIC_PROFILE, the profile files under
// the home directory): what rudy sends is what config.toml configured, never what happens to
// be in the environment. headers are the extra headers a before_request hook produced and are
// applied last, so a hook can override a configured header.
func (c *Client) sdk(sessionID ulid.ULID, headers map[string]string) anthropic.Client {
	opts := []option.RequestOption{
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(c.opts.BaseURL),
		option.WithMaxRetries(0),
		option.WithHTTPClient(&doer{c: c.opts.HTTP, sid: sessionID}),
	}
	if c.opts.APIKey != "" {
		opts = append(opts, option.WithAPIKey(c.opts.APIKey))
	}
	for k, v := range c.opts.Headers {
		opts = append(opts, option.WithHeader(k, v))
	}
	for k, v := range headers {
		opts = append(opts, option.WithHeader(k, v))
	}
	return anthropic.NewClient(opts...)
}

// doer is the SDK's option.HTTPClient over httpx. The SDK builds the request and carries the
// caller's context on it, which is the context httpx sends it under.
type doer struct {
	c   *httpx.Client
	sid ulid.ULID
}

func (d *doer) Do(req *http.Request) (*http.Response, error) {
	return d.c.Do(req.Context(), req, d.sid)
}

func (c *Client) Complete(ctx context.Context, req provider.Request, emit func(provider.Part) error) error {
	params, err := buildParams(req)
	if err != nil {
		return err
	}
	sdk := c.sdk(req.SessionID, req.Headers)
	stream := sdk.Messages.NewStreaming(ctx, params)
	defer func() { _ = stream.Close() }()
	st := newStreamState(emit)
	for stream.Next() {
		// An error here is the caller's own emit failing; it goes back verbatim.
		if err := st.event(stream.Current()); err != nil {
			return err
		}
	}
	if err := stream.Err(); err != nil {
		return classify(ctx, err)
	}
	return st.finish()
}

// classify turns an SDK failure into the error the turn loop records. A cancelled context
// wins over whatever the stream reported, an API status becomes a provider error with the
// body kept verbatim, and everything else is transport. Attempts is what httpx made: the
// response the SDK error carries came from httpx, so it still knows.
func classify(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		body := []byte(apiErr.RawJSON())
		return &provider.Error{
			Class:    session.ErrProvider,
			Status:   apiErr.StatusCode,
			Message:  errorMessage(body, apiErr.StatusCode),
			Body:     body,
			Attempts: attempts(httpx.AttemptsOf(apiErr.Response)),
		}
	}
	var he *httpx.Error
	if errors.As(err, &he) {
		return &provider.Error{Class: session.ErrTransport, Message: err.Error(), Attempts: attempts(he.Attempts)}
	}
	return &provider.Error{Class: session.ErrTransport, Message: err.Error(), Attempts: 1}
}

// attempts floors a count at one: a failure the client saw took at least one request, and a
// response that did not come from httpx.Do reports zero.
func attempts(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// errorMessage reads the message out of the API's {"error":{"message":…}} body. The status
// text stands in when the body is absent or shaped differently.
func errorMessage(body []byte, status int) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		return env.Error.Message
	}
	return http.StatusText(status)
}
