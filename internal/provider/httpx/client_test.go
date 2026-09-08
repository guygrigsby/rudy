package httpx_test

import (
	"bytes"
	"context"
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
		io.WriteString(w, "ok")
	}))
	defer srv.Close()
	c, slept := newClient(t)
	req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader([]byte(`{"x":1}`)))
	resp, err := c.Do(context.Background(), req, ulid.ULID{})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
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
	resp.Body.Close()
	if resp.StatusCode != 502 || attempts.Load() != 5 {
		t.Fatalf("status %d attempts %d", resp.StatusCode, attempts.Load())
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
	resp.Body.Close()
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
	resp.Body.Close()
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
	resp.Body.Close()
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
