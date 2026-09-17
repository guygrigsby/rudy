package web

import (
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
	// searcher and endpoint are the seams a test uses: a fake Searcher, or a real brave
	// pointed at an httptest server. Both are nil in every production path.
	searcher Searcher
	endpoint string
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
	key := ""
	if p.cfg.SearchAPIKey != "" && p.resolveSecret != nil {
		v, err := p.resolveSecret(p.cfg.SearchAPIKey)
		if err != nil {
			slog.Info("web: tools not registered", "reason", "the search key did not resolve", "err", err)
		}
		key = strings.TrimSpace(v)
	}
	if key == "" && p.searcher == nil {
		// Registering nothing, and not failing either: a model that can see web_search and
		// cannot use it spends a turn discovering that, and an operator who never set a key
		// never asked for the tool, so a failed plugin in the status line would be rudy
		// complaining about a choice (ADR 0039).
		slog.Info("web: tools not registered", "reason", "no search key", "ref", p.cfg.SearchAPIKey)
		return nil
	}
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
		endpoint := p.endpoint
		if endpoint == "" {
			endpoint = braveEndpoint
		}
		search = &brave{client: client, endpoint: endpoint, key: key, userAgent: agent, limiter: lim}
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
