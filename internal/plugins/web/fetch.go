// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Page is one fetched document in rudy's own terms: no header map, no status code, nothing
// of the transport. URL is where the bytes actually came from, which is the last hop of any
// redirect chain rather than the URL the model asked for.
type Page struct {
	URL       string
	Title     string
	Text      string
	Truncated bool
}

// fetcher reads one page. The policy decides what it may reach, the limiter keeps it from
// hammering one host, and maxBytes bounds what it reads before it stops reading.
type fetcher struct {
	client    *http.Client
	policy    policy
	limiter   *limiter
	maxBytes  int
	userAgent string
}

const (
	// maxRedirects is how far a fetch follows. Each hop is checked against the policy
	// again, so this bounds the checking as much as the travelling.
	maxRedirects = 5
	// maxText is what a page contributes to the model's context. Bytes past it are the
	// tail of a page nobody reads, and the model pays for every one of them.
	maxText = 40000
	// retryAfterCap bounds what a server can ask this process to wait. A Retry-After of an
	// hour is a refusal, not a wait.
	retryAfterCap = 30 * time.Second
)

// fetch reads one URL and returns it as text. It follows redirects itself rather than
// letting the client do it, because every hop has to pass the policy: a server that answers
// a public name with a redirect to 169.254.169.254 is the whole reason the policy exists.
func (f *fetcher) fetch(ctx context.Context, raw string) (Page, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return Page{}, fmt.Errorf("%w: not a URL", errRefused)
	}
	for hop := 0; ; hop++ {
		if hop > maxRedirects {
			return Page{}, fmt.Errorf("%w: more than %d redirects", errRefused, maxRedirects)
		}
		if err := f.policy.check(ctx, u); err != nil {
			return Page{}, err
		}
		if err := f.limiter.wait(ctx, hostOf(u)); err != nil {
			return Page{}, err
		}
		res, err := f.get(ctx, u)
		if err != nil {
			return Page{}, err
		}
		if loc := redirect(res); loc != "" {
			next, err := u.Parse(loc)
			_ = res.Body.Close()
			if err != nil {
				return Page{}, fmt.Errorf("%w: a redirect to something that is not a URL", errRefused)
			}
			u = next
			continue
		}
		defer func() { _ = res.Body.Close() }()
		if res.StatusCode != http.StatusOK {
			return Page{}, statusError(res)
		}
		body, truncated, err := readCapped(res.Body, f.maxBytes)
		if err != nil {
			return Page{}, fmt.Errorf("reading %s: %w", u.Host, err)
		}
		title, text := extract(res.Header.Get("Content-Type"), body)
		if len(text) > maxText {
			text, truncated = text[:maxText], true
		}
		return Page{URL: u.String(), Title: title, Text: text, Truncated: truncated}, nil
	}
}

// get makes one request. No cookies, no credentials, no body: a fetch is a read of something
// public, and anything that needs more than that is not this tool's job.
func (f *fetcher) get(ctx context.Context, u *url.URL) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "text/html,text/plain;q=0.9,*/*;q=0.1")
	res, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", u.Host, err)
	}
	return res, nil
}

// redirect is the Location of a redirect response, or empty for anything else.
func redirect(res *http.Response) string {
	switch res.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return res.Header.Get("Location")
	}
	return ""
}

// statusError reports a refusal the way the server meant it, including how long it asked to
// be left alone. Being a good user of somebody else's service is part of being correct.
func statusError(res *http.Response) error {
	if wait, ok := retryAfter(res.Header.Get("Retry-After")); ok {
		return fmt.Errorf("%s asked to be tried again in %s (HTTP %d)", res.Request.URL.Host, wait, res.StatusCode)
	}
	return fmt.Errorf("%s answered HTTP %d", res.Request.URL.Host, res.StatusCode)
}

// retryAfter reads the header in both its forms, the seconds and the HTTP date, and caps
// what either can ask for.
func retryAfter(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
		return min(time.Duration(secs)*time.Second, retryAfterCap), true
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(time.Until(t), 0), retryAfterCap), true
	}
	return 0, false
}

// readCapped reads at most limit bytes and reports whether there were more.
func readCapped(r io.Reader, limit int) ([]byte, bool, error) {
	body, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, false, err
	}
	if len(body) > limit {
		return body[:limit], true, nil
	}
	return body, false, nil
}
