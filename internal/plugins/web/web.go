package web

import (
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

const searchSchema = `{"type":"object","properties":{"query":{"type":"string","description":"What to search for, as you would type it into a search box"}},"required":["query"],"additionalProperties":false}`

const fetchSchema = `{"type":"object","properties":{"url":{"type":"string","description":"The http or https URL to read"}},"required":["url"],"additionalProperties":false}`

const (
	// requestTimeout bounds one request. The turn's own tool timeout bounds the call; this
	// is so a server that accepts a connection and says nothing cannot hold a worker for
	// the whole of it.
	requestTimeout = 30 * time.Second
	// perHostDelay is the floor between two requests to one host, and between two
	// searches. Politeness is part of correct, not an optimization (ADR 0039).
	perHostDelay = 250 * time.Millisecond
)

// webPlugin registers the two web tools when a search key resolves.
type webPlugin struct {
	cfg     config.WebConfig
	version string
	// resolveSecret turns the configured ref into the key, the same way a provider's key is
	// resolved. Nil means no key is available, which registers nothing.
	resolveSecret func(ref string) (string, error)
	// searcher, endpoint and exaEndpoint are the seams a test uses: a fake Searcher, or a
	// real backend pointed at an httptest server. All three are unset in production.
	searcher    Searcher
	endpoint    string
	exaEndpoint string
	// resolve is the policy's lookup, injected for the same reason.
	resolve func(ctx context.Context, host string) ([]net.IP, error)
}

// New returns the plugin. The key is read from the environment at Init rather than here, so
// a kernel started by a daemon that gained the variable later picks it up on its next start
// rather than needing a different build.
func New(cfg config.WebConfig, version string, resolveSecret func(string) (string, error)) plugin.Plugin {
	return &webPlugin{cfg: cfg, version: version, resolveSecret: resolveSecret}
}

func (*webPlugin) Name() string { return "tools.web" }

func (p *webPlugin) Init(ctx context.Context, h plugin.Host) error {
	client := &http.Client{
		Timeout: requestTimeout,
		// Redirects are followed by the fetcher, not by the client: every hop has to pass
		// the policy again, and a client that follows them itself takes the second hop
		// before anything has looked at it (ADR 0039).
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	agent := "rudy/" + p.version + " (+https://github.com/guygrigsby/rudy)"
	lim := newLimiter(perHostDelay)

	search := p.searcher
	if search == nil {
		backends := p.backends(client, agent, lim)
		if len(backends) == 0 {
			// Registering nothing, and not failing either: a model that can see web_search
			// and cannot use it spends a turn discovering that, and an operator who set no
			// key never asked for the tool, so a failed plugin in the status line would be
			// rudy complaining about a choice (ADR 0039).
			slog.Info("web: tools not registered", "reason", "no search backend has a key")
			return nil
		}
		search = chain{backends: backends}
	}
	f := &fetcher{
		client:    client,
		policy:    policy{allowPrivate: p.cfg.AllowPrivateHosts, resolve: p.resolve},
		limiter:   lim,
		maxBytes:  p.cfg.FetchMaxBytes,
		userAgent: agent,
	}
	if err := h.RegisterTool(tool.Tool{
		Name:        "web_search",
		Description: "Search the web and return titles, URLs and snippets. Use it to find pages; use web_fetch to read one.",
		Schema:      json.RawMessage(searchSchema),
		Safety:      tool.Safe,
		Invoke:      p.searchTool(search),
	}); err != nil {
		return err
	}
	return h.RegisterTool(tool.Tool{
		Name:        "web_fetch",
		Description: "Read an http or https URL and return the page as text. HTML is reduced to text and long pages are truncated.",
		Schema:      json.RawMessage(fetchSchema),
		Safety:      tool.Unsafe,
		Invoke:      p.fetchTool(f),
	})
}

type searchArgs struct {
	Query string `json:"query"`
}

type fetchArgs struct {
	URL string `json:"url"`
}

// searchTool is the search tool's Invoke. A failed search is a tool error rather than a
// plugin fault: the model can try another query or read a page it already has.
func (p *webPlugin) searchTool(s Searcher) func(context.Context, tool.Call) (tool.Result, error) {
	return func(ctx context.Context, c tool.Call) (tool.Result, error) {
		var a searchArgs
		if err := json.Unmarshal(c.Input, &a); err != nil {
			return errResult("web_search takes a query string"), nil
		}
		if strings.TrimSpace(a.Query) == "" {
			return errResult("web_search needs a query"), nil
		}
		results, err := s.Search(ctx, a.Query, p.cfg.MaxResults)
		if err != nil {
			return errResult(err.Error()), nil
		}
		if len(results) == 0 {
			return tool.Result{Content: []session.Block{session.TextBlock("no results")}}, nil
		}
		var b strings.Builder
		for i, r := range results {
			if i > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(r.Title + "\n" + r.URL)
			if r.Snippet != "" {
				b.WriteString("\n" + r.Snippet)
			}
		}
		return tool.Result{Content: []session.Block{session.TextBlock(b.String())}}, nil
	}
}

// fetchTool is the fetch tool's Invoke. What comes back is somebody else's text going into
// the model's context, and rudy puts no fence around it: the README's trust model says so.
func (p *webPlugin) fetchTool(f *fetcher) func(context.Context, tool.Call) (tool.Result, error) {
	return func(ctx context.Context, c tool.Call) (tool.Result, error) {
		var a fetchArgs
		if err := json.Unmarshal(c.Input, &a); err != nil {
			return errResult("web_fetch takes a url string"), nil
		}
		page, err := f.fetch(ctx, a.URL)
		if err != nil {
			return errResult(err.Error()), nil
		}
		var b strings.Builder
		if page.Title != "" {
			b.WriteString(page.Title + "\n")
		}
		b.WriteString(page.URL + "\n\n" + page.Text)
		if page.Truncated {
			b.WriteString("\n\n[truncated]")
		}
		return tool.Result{Content: []session.Block{session.TextBlock(b.String())}}, nil
	}
}

func errResult(text string) tool.Result {
	return tool.Result{IsError: true, Content: []session.Block{session.TextBlock(text)}}
}

// backends is the search chain this config asks for, in the order it is asked: Brave first
// because it is the one most operators key, then Exa, so one vendor's quota or outage costs
// a turn a retry rather than the capability (ADR 0039).
func (p *webPlugin) backends(client *http.Client, agent string, lim *limiter) []named {
	var out []named
	if key := p.key(p.cfg.BraveAPIKey); key != "" {
		out = append(out, named{name: "brave", Searcher: &brave{
			client: client, endpoint: cmp.Or(p.endpoint, braveEndpoint), key: key, userAgent: agent, limiter: lim,
		}})
	}
	if key := p.key(p.cfg.ExaAPIKey); key != "" {
		out = append(out, named{name: "exa", Searcher: &exa{
			client: client, endpoint: cmp.Or(p.exaEndpoint, exaEndpoint), key: key, userAgent: agent, limiter: lim,
		}})
	}
	return out
}

// key resolves one backend's reference, or reports why it could not and leaves that backend
// out of the chain.
func (p *webPlugin) key(ref string) string {
	if ref == "" || p.resolveSecret == nil {
		return ""
	}
	v, err := p.resolveSecret(ref)
	if err != nil {
		slog.Info("web: search backend has no key", "ref", ref, "err", err)
		return ""
	}
	return strings.TrimSpace(v)
}
