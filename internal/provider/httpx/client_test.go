package httpx_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/provider/httpx"
)

func newClient(t *testing.T) (*httpx.Client, *[]time.Duration) {
	t.Helper()
	slept := &[]time.Duration{}
	c := httpx.New("0.1.0")
	c.Sleep = func(d time.Duration) { *slept = append(*slept, d) }
	c.Now = func() time.Time { return time.Date(2026, 9, 7, 20, 0, 0, 0, time.UTC) }
	return c, slept
}

func TestDoRetriesThenSucceedsAndReplaysBody(t *testing.T) {
	var attempts atomic.Int32
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	c, slept := newClient(t)
	req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader([]byte(`{"x":1}`)))
	resp, err := c.Do(context.Background(), req, ulid.ULID{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 || attempts.Load() != 3 {
		t.Fatalf("status %d attempts %d", resp.StatusCode, attempts.Load())
	}
	if len(*slept) != 2 || (*slept)[0] != time.Second || (*slept)[1] != 2*time.Second {
		t.Fatalf("backoff %v", *slept)
	}
	for i, b := range bodies {
		if b != `{"x":1}` {
			t.Fatalf("attempt %d body %q", i, b)
		}
	}
}

func TestDoGivesUpAfterFiveAttemptsReturningTheLastResponse(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c, _ := newClient(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := c.Do(context.Background(), req, ulid.ULID{})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 502 || attempts.Load() != 5 {
		t.Fatalf("status %d attempts %d", resp.StatusCode, attempts.Load())
	}
}

func TestDoRetriesWhenDrainingARetryableBodyFails(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			// Content-Length overstates the bytes actually written; the server closes
			// the connection when the handler returns, so the client's drain of the
			// retryable body hits an unexpected EOF.
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "short")
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c, _ := newClient(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := c.Do(context.Background(), req, ulid.ULID{})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || httpx.AttemptsOf(resp) != 2 {
		t.Fatalf("status %d attempts %d", resp.StatusCode, httpx.AttemptsOf(resp))
	}
	if attempts.Load() != 2 {
		t.Fatalf("server saw %d attempts", attempts.Load())
	}
}

func TestDoGivesUpAfterFiveAttemptsWhenDrainFailsEveryTime(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "short")
	}))
	defer srv.Close()
	c, _ := newClient(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	_, err := c.Do(context.Background(), req, ulid.ULID{})
	var herr *httpx.Error
	if !errors.As(err, &herr) || herr.Attempts != 5 {
		t.Fatalf("want *httpx.Error with Attempts=5, got %v", err)
	}
	if attempts.Load() != 5 {
		t.Fatalf("server saw %d attempts", attempts.Load())
	}
}

func TestAttemptsOfIsOneWhenTheFirstAttemptSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c, _ := newClient(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := c.Do(context.Background(), req, ulid.ULID{})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := httpx.AttemptsOf(resp); got != 1 {
		t.Fatalf("want 1 attempt, got %d", got)
	}
}

func TestDoHonorsRetryAfterSecondsAndDate(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch attempts.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.Header().Set("Retry-After", "Mon, 07 Sep 2026 20:00:07 GMT")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	c, slept := newClient(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := c.Do(context.Background(), req, ulid.ULID{})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(*slept) != 2 || (*slept)[0] != 3*time.Second || (*slept)[1] != 7*time.Second {
		t.Fatalf("slept %v", *slept)
	}
}

func TestDoSetsHeaders(t *testing.T) {
	var ua, sid string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua, sid = r.Header.Get("User-Agent"), r.Header.Get("X-Rudy-Session")
	}))
	defer srv.Close()
	c, _ := newClient(t)
	id := ulid.Make()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := c.Do(context.Background(), req, id)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !strings.HasPrefix(ua, "rudy/0.1.0 (") || !strings.HasSuffix(ua, ")") {
		t.Errorf("user agent %q", ua)
	}
	if sid != id.String() {
		t.Errorf("session header %q", sid)
	}
	req, _ = http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err = c.Do(context.Background(), req, ulid.ULID{})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if sid != "" {
		t.Errorf("zero session id must send no header, got %q", sid)
	}
}

func TestDoStopsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c, _ := newClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	c.Sleep = func(time.Duration) { cancel() }
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	if _, err := c.Do(ctx, req, ulid.ULID{}); err != context.Canceled {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 7, 20, 0, 0, 0, time.UTC)
	h := http.Header{}
	if _, ok := httpx.RetryAfter(h, now); ok {
		t.Error("absent header")
	}
	h.Set("Retry-After", "120")
	if d, ok := httpx.RetryAfter(h, now); !ok || d != 60*time.Second {
		t.Errorf("seconds capped at 60: %v %v", d, ok)
	}
	h.Set("Retry-After", "Mon, 07 Sep 2026 19:59:00 GMT")
	if d, ok := httpx.RetryAfter(h, now); !ok || d != 0 {
		t.Errorf("past date clamps to zero: %v %v", d, ok)
	}
	h.Set("Retry-After", "soon")
	if _, ok := httpx.RetryAfter(h, now); ok {
		t.Error("garbage must be ignored")
	}
}

func TestDoDoesNotOutliveContextDuringBackoffSleep(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	// Real Sleep, real backoff (1s, 2s, 4s, 8s, 16s): a context that expires well inside the
	// first backoff delay must cut Do short instead of letting it block for the full sleep.
	c := httpx.New("0.1.0")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	start := time.Now()
	_, err := c.Do(ctx, req, ulid.ULID{})
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Do outlived its context: took %v against a 50ms timeout and a 1s backoff", elapsed)
	}
}
