package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Result is one search hit in rudy's own terms. Every field is what a model needs to decide
// whether the page is worth fetching, and nothing else the vendor happens to return.
type Result struct {
	Title   string
	URL     string
	Snippet string
}

// Searcher is what the search tool calls. Brave is one implementation; a second backend is
// another implementation of this, not another tool (ADR 0039).
type Searcher interface {
	Search(ctx context.Context, query string, limit int) ([]Result, error)
}

// braveEndpoint is the documented search endpoint. A field on the searcher rather than a
// constant at the call site so a test can point it at its own server.
const braveEndpoint = "https://api.search.brave.com/res/v1/web/search"

// brave is the anti-corruption layer around one vendor's API: it is the only thing in this
// package that knows Brave's JSON, its header name or its endpoint, and it hands back
// rudy's own Result.
type brave struct {
	client    *http.Client
	endpoint  string
	key       string
	userAgent string
	limiter   *limiter
}

// braveResponse is the shape this package reads out of Brave's answer. Everything else in
// the payload is left where it is.
type braveResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
}

func (b *brave) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	if err := b.limiter.wait(ctx, "search"); err != nil {
		return nil, err
	}
	u, err := url.Parse(b.endpoint)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("q", query)
	q.Set("count", strconv.Itoa(limit))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", b.key)
	req.Header.Set("User-Agent", b.userAgent)
	res, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("searching: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		// Reported rather than retried past: a quota is an answer, and hammering it is how
		// a key gets suspended. The response body is not repeated, since a search error can
		// carry the account behind the key.
		return nil, statusError(res)
	}
	body, _, err := readCapped(res.Body, 4<<20)
	if err != nil {
		return nil, fmt.Errorf("searching: %w", err)
	}
	var parsed braveResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("searching: the answer was not the documented JSON")
	}
	out := make([]Result, 0, len(parsed.Web.Results))
	for _, r := range parsed.Web.Results {
		if len(out) == limit {
			break
		}
		out = append(out, Result{
			Title:   strings.TrimSpace(stripTags(r.Title)),
			URL:     strings.TrimSpace(r.URL),
			Snippet: strings.TrimSpace(stripTags(r.Description)),
		})
	}
	return out, nil
}

// stripTags removes the <strong> marks Brave puts around matched words. They are markup for
// a browser and noise in a transcript.
func stripTags(s string) string {
	for {
		open := strings.IndexByte(s, '<')
		if open < 0 {
			return s
		}
		close := strings.IndexByte(s[open:], '>')
		if close < 0 {
			return s
		}
		s = s[:open] + s[open+close+1:]
	}
}

// limiter is a per-key minimum delay between requests, which is the floor of being a decent
// user of someone else's service: a turn that fans out ten fetches at one host should still
// arrive as ten requests spaced apart rather than ten at once.
type limiter struct {
	every time.Duration

	mu   sync.Mutex
	next map[string]time.Time
}

func newLimiter(every time.Duration) *limiter {
	return &limiter{every: every, next: map[string]time.Time{}}
}

// wait blocks until this key's turn, or until the context ends. It reserves the slot before
// sleeping, so concurrent callers queue behind each other instead of all waking at once.
func (l *limiter) wait(ctx context.Context, key string) error {
	l.mu.Lock()
	now := time.Now()
	at := l.next[key]
	if at.Before(now) {
		at = now
	}
	l.next[key] = at.Add(l.every)
	l.mu.Unlock()
	d := time.Until(at)
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
