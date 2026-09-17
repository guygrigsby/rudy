package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// exaEndpoint is Exa's documented search endpoint.
const exaEndpoint = "https://api.exa.ai/search"

// exa is the second backend's anti-corruption layer. Exa answers with page text rather than
// a snippet, so what a result carries here is the head of that text: the same three fields
// Brave's results have, from a different shape.
type exa struct {
	client    *http.Client
	endpoint  string
	key       string
	userAgent string
	limiter   *limiter
}

// exaRequest is the documented body. type "auto" lets Exa pick between its own modes, and
// the text is asked for bounded rather than whole: a snippet is what a model needs to decide
// whether to fetch the page.
type exaRequest struct {
	Query      string      `json:"query"`
	NumResults int         `json:"numResults"`
	Type       string      `json:"type"`
	Contents   exaContents `json:"contents"`
}

type exaContents struct {
	Text exaText `json:"text"`
}

type exaText struct {
	MaxCharacters int `json:"maxCharacters"`
}

type exaResponse struct {
	Results []struct {
		Title string `json:"title"`
		URL   string `json:"url"`
		Text  string `json:"text"`
	} `json:"results"`
}

// exaSnippet is how much of a page's text a result carries.
const exaSnippet = 400

func (e *exa) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	if err := e.limiter.wait(ctx, "search"); err != nil {
		return nil, err
	}
	body, err := json.Marshal(exaRequest{
		Query:      query,
		NumResults: limit,
		Type:       "auto",
		Contents:   exaContents{Text: exaText{MaxCharacters: exaSnippet}},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-api-key", e.key)
	req.Header.Set("User-Agent", e.userAgent)
	res, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("searching: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, statusError(res)
	}
	raw, _, err := readCapped(res.Body, 4<<20)
	if err != nil {
		return nil, fmt.Errorf("searching: %w", err)
	}
	var parsed exaResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("searching: the answer was not the documented JSON")
	}
	out := make([]Result, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		if len(out) == limit {
			break
		}
		out = append(out, Result{
			Title:   strings.TrimSpace(r.Title),
			URL:     strings.TrimSpace(r.URL),
			Snippet: oneLine(r.Text),
		})
	}
	return out, nil
}

// chain asks each backend in turn and returns the first answer. A backend that fails is
// logged and the next one is asked, which is the point: a quota, an outage or a bad gateway
// at one search vendor should cost a turn a few hundred milliseconds, not the capability
// (ADR 0039). An empty result set is an answer, not a failure: the second backend does not
// get to overrule the first about what the web holds.
type chain struct {
	backends []named
}

// named is one backend and the name its failures are logged under.
type named struct {
	name string
	Searcher
}

func (c chain) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	var last error
	for _, b := range c.backends {
		results, err := b.Search(ctx, query, limit)
		if err == nil {
			return results, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		slog.Warn("web: search backend failed", "backend", b.name, "err", err)
		last = fmt.Errorf("%s: %w", b.name, err)
	}
	if last == nil {
		return nil, errors.New("no search backend is configured")
	}
	return nil, last
}

// oneLine is page text as a snippet: whitespace of every kind squeezed to single spaces, so
// a result list stays a list rather than becoming the pages themselves.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
