package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// load runs the plugin through a real registry, which is the only thing that builds a
// plugin.Host, and returns what it registered.
func load(t *testing.T, p plugin.Plugin) map[string]tool.Tool {
	t.Helper()
	reg := plugin.NewRegistry(nil, func(string) {})
	reg.Load(context.Background(), p)
	t.Cleanup(func() { _ = reg.Close(context.Background()) })
	out := map[string]tool.Tool{}
	for _, tl := range reg.Tools() {
		out[tl.Name] = tl
	}
	return out
}

// testConfig is the shipped defaults, with the seams a test needs.
func testConfig() config.WebConfig {
	return config.WebConfig{BraveAPIKey: "env:RUDY_TEST_WEB_KEY", MaxResults: 3, FetchMaxBytes: 1 << 20}
}

// loopbackResolver lets a test reach its own httptest server, which lives at exactly the
// address the policy refuses by default. Only loopback is pretended public: everything else
// resolves to itself, so a redirect to the metadata endpoint is still the real thing.
func loopbackResolver(_ context.Context, host string) ([]net.IP, error) {
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	return nil, fmt.Errorf("no test address for %s", host)
}

func fetchToolFor(t *testing.T, p *webPlugin) tool.Tool {
	t.Helper()
	f, ok := load(t, p)["web_fetch"]
	if !ok {
		t.Fatal("web_fetch was not registered")
	}
	return f
}

func invoke(t *testing.T, tl tool.Tool, input string) tool.Result {
	t.Helper()
	res, err := tl.Invoke(context.Background(), tool.Call{ID: "tu1", Name: tl.Name, Input: json.RawMessage(input)})
	if err != nil {
		t.Fatalf("%s: %v", tl.Name, err)
	}
	return res
}

func text(res tool.Result) string { return session.TextOf(res.Content) }

// TestNoKeyRegistersNothing keeps a tool the operator never configured out of the model's
// view, without making an unconfigured key look like a broken plugin.
func TestNoKeyRegistersNothing(t *testing.T) {
	tools := load(t, &webPlugin{cfg: testConfig(), version: "test"})
	if len(tools) != 0 {
		t.Errorf("registered %d tools with no key", len(tools))
	}
}

// TestTheToolsCarryTheirSafetyClasses is the Gate's half of ADR 0039: a search reaches the
// one endpoint the operator configured, a fetch reaches wherever the model says.
func TestTheToolsCarryTheirSafetyClasses(t *testing.T) {
	tools := load(t, &webPlugin{cfg: testConfig(), version: "test", searcher: fakeSearcher{}})
	if got := tools["web_search"].Safety; got != tool.Safe {
		t.Errorf("web_search safety = %s, want safe", got)
	}
	if got := tools["web_fetch"].Safety; got != tool.Unsafe {
		t.Errorf("web_fetch safety = %s, want unsafe", got)
	}
}

type fakeSearcher struct {
	results []Result
	err     error
}

func (f fakeSearcher) Search(_ context.Context, _ string, limit int) ([]Result, error) {
	return f.results, f.err
}

// TestSearchReturnsWhatTheModelNeeds covers the shape of a result: a title to judge it by, a
// URL to fetch and a snippet to skip it on.
func TestSearchReturnsWhatTheModelNeeds(t *testing.T) {
	s := fakeSearcher{results: []Result{
		{Title: "Go 1.26 release notes", URL: "https://go.dev/doc/go1.26", Snippet: "What changed"},
	}}
	tools := load(t, &webPlugin{cfg: testConfig(), version: "test", searcher: s})
	got := text(invoke(t, tools["web_search"], `{"query":"go 1.26"}`))
	for _, want := range []string{"Go 1.26 release notes", "https://go.dev/doc/go1.26", "What changed"} {
		if !strings.Contains(got, want) {
			t.Errorf("search result %q does not carry %q", got, want)
		}
	}
}

// TestSearchReportsItsFailureToTheModel: a quota or a network failure is something the model
// can work around, so it is a tool error rather than a failed turn.
func TestSearchReportsItsFailureToTheModel(t *testing.T) {
	tools := load(t, &webPlugin{cfg: testConfig(), version: "test", searcher: fakeSearcher{err: fmt.Errorf("api.search.brave.com answered HTTP 429")}})
	res := invoke(t, tools["web_search"], `{"query":"anything"}`)
	if !res.IsError || !strings.Contains(text(res), "429") {
		t.Errorf("search failure = %+v, want a tool error naming the status", res)
	}
}

// TestFetchReadsAPageAsText is the ordinary case: the model gets the title and the prose,
// and none of the markup.
func TestFetchReadsAPageAsText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "rudy/") {
			t.Errorf("User-Agent = %q, want one that names rudy", ua)
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>A page</title><style>p{color:red}</style></head>
			<body><script>alert(1)</script><p>First line.</p><p>Second line.</p></body></html>`))
	}))
	defer srv.Close()

	p := &webPlugin{cfg: testConfig(), version: "test", searcher: fakeSearcher{}, resolve: loopbackResolver}
	res := invoke(t, fetchToolFor(t, p), `{"url":"`+srv.URL+`"}`)
	got := text(res)
	if res.IsError {
		t.Fatalf("fetch failed: %s", got)
	}
	for _, want := range []string{"A page", "First line.", "Second line."} {
		if !strings.Contains(got, want) {
			t.Errorf("page text %q does not carry %q", got, want)
		}
	}
	for _, unwanted := range []string{"<p>", "alert(1)", "color:red"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("page text carries %q, which is markup or code rather than content", unwanted)
		}
	}
}

// TestFetchRefusesWhatThePolicyExcludes is the SSRF half of ADR 0039: the model picks the
// URL, so the model picks the address, and the interesting addresses are local.
func TestFetchRefusesWhatThePolicyExcludes(t *testing.T) {
	p := &webPlugin{cfg: testConfig(), version: "test", searcher: fakeSearcher{}}
	f := fetchToolFor(t, p)
	for _, tc := range []struct{ name, url string }{
		{"loopback", "http://127.0.0.1:9/"},
		{"loopback by name", "http://localhost:9/"},
		{"private", "http://10.1.2.3/"},
		{"link local metadata", "http://169.254.169.254/latest/meta-data/"},
		{"unique local v6", "http://[fd00::1]/"},
		{"a scheme that is not http", "file:///etc/passwd"},
		{"credentials in the url", "http://user:pass@example.com/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := invoke(t, f, `{"url":"`+tc.url+`"}`)
			if !res.IsError {
				t.Fatalf("fetch of %s was allowed: %s", tc.url, text(res))
			}
			if !strings.Contains(text(res), "refused") {
				t.Errorf("refusal of %s reads %q", tc.url, text(res))
			}
		})
	}
}

// TestFetchRechecksEveryRedirect is the same rule one hop later: a public name that answers
// with a redirect to the metadata endpoint is the attack the check exists for.
func TestFetchRechecksEveryRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	p := &webPlugin{cfg: testConfig(), version: "test", searcher: fakeSearcher{}, resolve: loopbackResolver}
	res := invoke(t, fetchToolFor(t, p), `{"url":"`+srv.URL+`"}`)
	if !res.IsError || !strings.Contains(text(res), "refused") {
		t.Fatalf("a redirect to the metadata endpoint was followed: %+v", res)
	}
}

// TestFetchStopsReading bounds what one page can put into the context, whatever the server
// claims about its length.
func TestFetchStopsReading(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Repeat("x", 50000)))
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.FetchMaxBytes = 4096
	p := &webPlugin{cfg: cfg, version: "test", searcher: fakeSearcher{}, resolve: loopbackResolver}
	got := text(invoke(t, fetchToolFor(t, p), `{"url":"`+srv.URL+`"}`))
	if !strings.Contains(got, "[truncated]") {
		t.Error("a page over the byte limit came back without saying it was cut")
	}
	if len(got) > 8192 {
		t.Errorf("fetch returned %d bytes for a 4096 byte limit", len(got))
	}
}

// TestFetchReportsTheServersRefusal keeps rudy a decent user of someone else's service: a
// 429 with a Retry-After is reported with what it asked for, not retried past.
func TestFetchReportsTheServersRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := &webPlugin{cfg: testConfig(), version: "test", searcher: fakeSearcher{}, resolve: loopbackResolver}
	res := invoke(t, fetchToolFor(t, p), `{"url":"`+srv.URL+`"}`)
	if !res.IsError || !strings.Contains(text(res), "17s") {
		t.Fatalf("a 429 with Retry-After reads %q", text(res))
	}
}

// TestBraveIsTheOnlyPlaceThatKnowsBrave checks the adapter end to end against a server
// speaking the documented shape, including the header the key rides in.
func TestBraveIsTheOnlyPlaceThatKnowsBrave(t *testing.T) {
	var gotKey, gotQuery, gotCount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Subscription-Token")
		gotQuery = r.URL.Query().Get("q")
		gotCount = r.URL.Query().Get("count")
		_, _ = w.Write([]byte(`{"web":{"results":[
			{"title":"<strong>Go</strong> 1.26","url":"https://go.dev","description":"The <strong>release</strong>"},
			{"title":"Second","url":"https://example.com","description":"Another"}]}}`))
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.MaxResults = 1
	tools := load(t, &webPlugin{cfg: cfg, version: "test", endpoint: srv.URL,
		resolveSecret: func(string) (string, error) { return "secret-key", nil }})
	got := text(invoke(t, tools["web_search"], `{"query":"go 1.26"}`))
	if gotKey != "secret-key" || gotQuery != "go 1.26" || gotCount != "1" {
		t.Errorf("request carried key %q query %q count %q", gotKey, gotQuery, gotCount)
	}
	if strings.Contains(got, "<strong>") {
		t.Errorf("the vendor's markup reached the model: %q", got)
	}
	if strings.Contains(got, "Second") {
		t.Errorf("max_results was not honoured: %q", got)
	}
}

// TestExaAnswersWhenBraveCannot is the fallback ADR 0039 asks for: one vendor's quota or
// outage costs a turn a retry, not the capability.
func TestExaAnswersWhenBraveCannot(t *testing.T) {
	var braveCalls, exaCalls int
	braveSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		braveCalls++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer braveSrv.Close()
	exaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exaCalls++
		if got := r.Header.Get("x-api-key"); got != "exa-key" {
			t.Errorf("exa key header = %q", got)
		}
		var body exaRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("exa body: %v", err)
		}
		if body.Query != "go 1.26" || body.NumResults != 3 {
			t.Errorf("exa asked for %+v", body)
		}
		_, _ = w.Write([]byte(`{"results":[{"title":"Go 1.26","url":"https://go.dev","text":"The   release\n\nnotes"}]}`))
	}))
	defer exaSrv.Close()

	cfg := testConfig()
	cfg.ExaAPIKey = "env:RUDY_TEST_EXA_KEY"
	tools := load(t, &webPlugin{
		cfg: cfg, version: "test", endpoint: braveSrv.URL, exaEndpoint: exaSrv.URL,
		resolveSecret: func(ref string) (string, error) {
			if strings.Contains(ref, "EXA") {
				return "exa-key", nil
			}
			return "brave-key", nil
		},
	})
	got := text(invoke(t, tools["web_search"], `{"query":"go 1.26"}`))
	if braveCalls != 1 || exaCalls != 1 {
		t.Errorf("brave asked %d times and exa %d, want one each", braveCalls, exaCalls)
	}
	if !strings.Contains(got, "https://go.dev") || !strings.Contains(got, "The release notes") {
		t.Errorf("the fallback's results did not reach the model: %q", got)
	}
}

// TestBraveIsAskedFirst keeps the order the config promises: Exa is the fallback, not a
// second opinion, so a Brave answer ends it.
func TestBraveIsAskedFirst(t *testing.T) {
	exaSrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("exa was asked while brave was answering")
	}))
	defer exaSrv.Close()
	braveSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"web":{"results":[{"title":"Brave","url":"https://brave.example","description":"hit"}]}}`))
	}))
	defer braveSrv.Close()

	cfg := testConfig()
	cfg.ExaAPIKey = "env:RUDY_TEST_EXA_KEY"
	tools := load(t, &webPlugin{
		cfg: cfg, version: "test", endpoint: braveSrv.URL, exaEndpoint: exaSrv.URL,
		resolveSecret: func(string) (string, error) { return "key", nil },
	})
	if got := text(invoke(t, tools["web_search"], `{"query":"anything"}`)); !strings.Contains(got, "https://brave.example") {
		t.Errorf("search returned %q, want Brave's answer", got)
	}
}

// TestEveryBackendFailingIsOneError: the model is told the search failed, with the last
// backend's reason, rather than being handed an empty result set that reads like "nothing
// exists on the web".
func TestEveryBackendFailingIsOneError(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer down.Close()

	cfg := testConfig()
	cfg.ExaAPIKey = "env:RUDY_TEST_EXA_KEY"
	tools := load(t, &webPlugin{
		cfg: cfg, version: "test", endpoint: down.URL, exaEndpoint: down.URL,
		resolveSecret: func(string) (string, error) { return "key", nil },
	})
	res := invoke(t, tools["web_search"], `{"query":"anything"}`)
	if !res.IsError || !strings.Contains(text(res), "exa") {
		t.Errorf("all backends down = %+v, want an error naming the last one tried", res)
	}
}

// TestNoKeySaysHowToEnableIt: an operator who wanted the tools and has not keyed them has no
// other way to learn why they are missing, so the plugin says so once, with the keys and the
// file it needs.
func TestNoKeySaysHowToEnableIt(t *testing.T) {
	var notices []string
	reg := plugin.NewRegistry(nil, func(s string) { notices = append(notices, s) })
	reg.Load(context.Background(), &webPlugin{cfg: testConfig(), version: "test"})
	t.Cleanup(func() { _ = reg.Close(context.Background()) })

	if len(reg.Tools()) != 0 {
		t.Fatalf("registered %d tools with no key", len(reg.Tools()))
	}
	var said string
	for _, n := range notices {
		if strings.Contains(n, "web_search") {
			said = n
		}
	}
	if said == "" {
		t.Fatalf("no notice told the operator how to enable the tools: %v", notices)
	}
	for _, want := range []string{"web.brave_api_key", "web.exa_api_key", "config.toml", "env:", "cache:"} {
		if !strings.Contains(said, want) {
			t.Errorf("the notice does not name %q: %q", want, said)
		}
	}
}
