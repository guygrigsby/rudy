// SPDX-License-Identifier: AGPL-3.0-or-later

// Package anthropicmsgs is the Messages API codec, built on anthropic-sdk-go. It is the only
// package that may import the SDK: everything above it speaks provider.Request and
// provider.Part. Requests go out through httpx so they carry rudy's headers and its retry
// policy, and the SDK's own retries are off.
package anthropicmsgs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/provider/httpx"
	"github.com/guygrigsby/rudy/internal/session"
)

// defaultIdle is how long a stream may go without a byte before the request is cancelled.
// Same name and value as openaichat's: a stalled provider fails the same way either way.
const defaultIdle = 120 * time.Second

// Options is one configured Messages endpoint.
type Options struct {
	Name    string
	BaseURL string            // e.g. https://api.anthropic.com; the SDK appends /v1/messages
	APIKey  string            // "" sends no x-api-key
	Headers map[string]string // extra, verbatim
	HTTP    *httpx.Client
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

// sdk builds the SDK client for one request, sending through the doer the caller built.
// Every request carries rudy's User-Agent, X-Rudy-Session and retry policy and the SDK
// retries nothing itself. WithoutEnvironmentDefaults turns off the SDK's own credential
// autoload (ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL, ANTHROPIC_PROFILE, the profile files under
// the home directory): what rudy sends is what config.toml configured, never what happens to
// be in the environment. headers are the extra headers a before_request hook produced and are
// applied last, so a hook can override a configured header.
func (c *Client) sdk(d *doer, headers map[string]string) anthropic.Client {
	opts := []option.RequestOption{
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(c.opts.BaseURL),
		option.WithMaxRetries(0),
		option.WithHTTPClient(d),
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

// doer is the SDK's option.HTTPClient over httpx, and the only place that holds a response:
// the SDK owns everything from there on, so the idle timeout and the attempt count are read
// back off the doer rather than off a response the codec never sees. One doer serves one
// Complete or one ListModels, whose requests it makes in the caller's own goroutine.
type doer struct {
	c    *httpx.Client
	sid  ulid.ULID
	idle time.Duration
	body io.ReadCloser // the last response body, wrapped with the idle timeout
	sent int           // attempts httpx made for the last response
}

// Do sends under a context of its own so the idle timer can cancel this one request without
// touching the caller's. The SDK carries the caller's context on the request it built.
func (d *doer) Do(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	resp, err := d.c.Do(ctx, req, d.sid)
	if err != nil {
		cancel()
		return nil, err
	}
	d.sent = httpx.AttemptsOf(resp)
	d.body = httpx.IdleBody(resp.Body, d.idle, cancel)
	resp.Body = d.body
	return resp, nil
}

// idleFired reports whether the last response died of silence rather than anything the
// provider said.
func (d *doer) idleFired() bool { return httpx.IdleFired(d.body) }

func (c *Client) Complete(ctx context.Context, req provider.Request, emit func(provider.Part) error) error {
	params, err := buildParams(req)
	if err != nil {
		return err
	}
	d := &doer{c: c.opts.HTTP, sid: req.SessionID, idle: c.idle}
	sdk := c.sdk(d, req.Headers)
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
		if d.idleFired() {
			return &provider.Error{
				Class:    session.ErrTransport,
				Message:  fmt.Sprintf("idle timeout after %s", c.idle),
				Attempts: attempts(d.sent),
			}
		}
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
	if text := http.StatusText(status); text != "" {
		return text
	}
	// 529 and the rest of the unregistered statuses have no text at all.
	return fmt.Sprintf("HTTP %d", status)
}
