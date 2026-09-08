// Package httpx is the one HTTP client every wire codec uses: identifying headers, retry
// with backoff and Retry-After, context cancellation.
package httpx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/oklog/ulid/v2"
)

// Client wraps http.Client. Sleep and Now are replaceable for tests.
type Client struct {
	HTTP    *http.Client
	Version string
	Sleep   func(time.Duration)
	Now     func() time.Time
}

// New builds a client with no overall timeout (responses stream for minutes) and a 60s
// response header timeout so a dead upstream fails fast.
func New(version string) *Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 60 * time.Second
	return &Client{HTTP: &http.Client{Transport: t}, Version: version, Sleep: time.Sleep, Now: time.Now}
}

var backoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}

// Retryable reports whether a status warrants another attempt.
func Retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// Error is returned when every attempt failed at the transport level. Attempts is how
// many were made. Unwrap yields the last transport error.
type Error struct {
	Attempts int
	Err      error
}

func (e *Error) Error() string {
	return fmt.Sprintf("httpx: gave up after %d attempts: %v", e.Attempts, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

type attemptsKey struct{}

// AttemptsOf reports how many attempts Do made before returning resp. It is carried on the
// returned response's request context so the Do signature stays a plain (*http.Response,
// error). Zero for a response that did not come from Do.
func AttemptsOf(resp *http.Response) int {
	if resp == nil || resp.Request == nil {
		return 0
	}
	n, _ := resp.Request.Context().Value(attemptsKey{}).(int)
	return n
}

// withAttempts records n as the attempt count Do made for resp, readable back through
// AttemptsOf.
func withAttempts(resp *http.Response, n int) *http.Response {
	resp.Request = resp.Request.WithContext(context.WithValue(resp.Request.Context(), attemptsKey{}, n))
	return resp
}

// Do sends req with rudy's headers, retrying retryable statuses and transport errors up
// to five attempts. The request body must be replayable through req.GetBody, which
// http.NewRequest sets for bytes.Reader, strings.Reader and bytes.Buffer bodies. On the
// fifth retryable status the response is returned so the caller can classify it, with the
// attempt count readable through AttemptsOf. Transport exhaustion returns *Error.
func (c *Client) Do(ctx context.Context, req *http.Request, sessionID ulid.ULID) (*http.Response, error) {
	req = req.WithContext(ctx)
	req.Header.Set("User-Agent", fmt.Sprintf("rudy/%s (%s/%s)", c.Version, runtime.GOOS, runtime.GOARCH))
	if !sessionID.IsZero() {
		req.Header.Set("X-Rudy-Session", sessionID.String())
	}
	var last error
	var delay time.Duration
	for attempt := range len(backoff) {
		if attempt > 0 {
			if req.GetBody != nil {
				body, err := req.GetBody()
				if err != nil {
					return nil, fmt.Errorf("httpx: replay body: %w", err)
				}
				req.Body = body
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			c.Sleep(delay)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			last = err
			delay = backoff[attempt]
			continue
		}
		if !Retryable(resp.StatusCode) {
			return withAttempts(resp, attempt+1), nil
		}
		if d, ok := RetryAfter(resp.Header, c.Now()); ok {
			delay = d
		} else {
			delay = backoff[attempt]
		}
		if attempt == len(backoff)-1 {
			// Last attempt at a retryable status: still hand the response back so the
			// caller can classify it (a codec reads the body for a provider error
			// message), but only once the body has proven readable. A body that fails
			// to read or close here is a transport failure like any other, so it
			// becomes *Error with the full attempt count rather than a response the
			// caller cannot actually read.
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				_ = resp.Body.Close()
				return nil, &Error{Attempts: len(backoff), Err: fmt.Errorf("httpx: drain body: %w", err)}
			}
			if err := resp.Body.Close(); err != nil {
				return nil, &Error{Attempts: len(backoff), Err: fmt.Errorf("httpx: close body: %w", err)}
			}
			resp.Body = io.NopCloser(bytes.NewReader(body))
			return withAttempts(resp, attempt+1), nil
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			_ = resp.Body.Close()
			last = fmt.Errorf("httpx: drain body: %w", err)
			continue
		}
		if err := resp.Body.Close(); err != nil {
			last = fmt.Errorf("httpx: close body: %w", err)
			continue
		}
		last = fmt.Errorf("httpx: %s %s: status %d", req.Method, req.URL, resp.StatusCode)
	}
	return nil, &Error{Attempts: len(backoff), Err: last}
}

// RetryAfter parses a Retry-After header in delta-seconds or HTTP-date form, clamped to
// [0, 60s]. ok is false when the header is absent or unparseable.
func RetryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	v := h.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	} else {
		return 0, false
	}
	if d < 0 {
		d = 0
	}
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d, true
}
