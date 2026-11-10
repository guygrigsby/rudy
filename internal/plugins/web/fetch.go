// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
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
	// dial is the dialer the transport uses once dialVetted has chosen the address. Nil is a
	// net.Dialer with a 10s timeout; a test sets it to see what the transport was handed.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// vettedKey carries the addresses the policy accepted for the current hop to dialVetted.
type vettedKey struct{}

// dialTimeout bounds one connection attempt, as the default transport's dialer does.
const dialTimeout = 10 * time.Second

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
		ips, err := f.policy.vet(ctx, u)
		if err != nil {
			return Page{}, err
		}
		if err := f.limiter.wait(ctx, hostOf(u)); err != nil {
			return Page{}, err
		}
		res, err := f.get(context.WithValue(ctx, vettedKey{}, ips), u)
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

// dialVetted is the transport's DialContext. It dials the addresses the policy vetted for
// this hop, which fetch put in ctx, and ignores the host in addr: the name was resolved once,
// by the policy, and a second resolution is the window ADR 0053 closes. The port is kept. A
// ctx without vetted addresses is an error, never a fallback to the name.
func (f *fetcher) dialVetted(ctx context.Context, network, addr string) (net.Conn, error) {
	ips, _ := ctx.Value(vettedKey{}).([]net.IP)
	if len(ips) == 0 {
		return nil, errors.New("no vetted address to dial")
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	dial := f.dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: dialTimeout}).DialContext
	}
	var errs []error
	for _, ip := range ips {
		conn, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// The dial was handed a vetted address; the peer must be one too, so a dial that
		// landed elsewhere is refused rather than read.
		if !slices.ContainsFunc(ips, peerIP(conn).Equal) {
			_ = conn.Close()
			errs = append(errs, fmt.Errorf("%w: the connection reached an address the policy did not vet", errRefused))
			continue
		}
		return conn, nil
	}
	return nil, errors.Join(errs...)
}

// peerIP is the IP a connection is actually connected to, or nil when it cannot say.
func peerIP(c net.Conn) net.IP {
	switch a := c.RemoteAddr().(type) {
	case *net.TCPAddr:
		return a.IP
	case *net.IPAddr:
		return a.IP
	}
	host, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
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
