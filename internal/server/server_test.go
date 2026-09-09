package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/commands"
	"github.com/guygrigsby/rudy/internal/plugins/compactcmd"
	"github.com/guygrigsby/rudy/internal/plugins/subagents"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/server"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
	"github.com/guygrigsby/rudy/internal/turn"
)

// scriptProvider answers odd calls with a tool_use for "danger" and even calls with "done".
// When block is non-nil it streams one delta and then waits for ctx or block.
type scriptProvider struct {
	mu        sync.Mutex
	calls     int
	block     chan struct{}
	textOnly  bool               // answer every call with text, never a tool_use
	toolName  string             // the tool to call; empty means danger
	toolInput string             // that tool's input; empty means {"x":1}
	reqs      []provider.Request // every request, in order
	// A compaction's summary request (the one carrying the summary system prompt) waits on
	// summary when it is non-nil, so a test can hold a compaction open. summaryHit is signalled
	// once when such a request arrives, summaryDone is closed when it returns, and summaryErr
	// is what it returned.
	summary     chan struct{}
	summaryHit  chan struct{}
	summaryDone chan struct{}
	summaryErr  error
}

// summaryOutcome is what the blocked summary request returned, once it has returned.
func (p *scriptProvider) summaryOutcome() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.summaryErr
}

func (p *scriptProvider) lastRequest() provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.reqs) == 0 {
		return provider.Request{}
	}
	return p.reqs[len(p.reqs)-1]
}

func (p *scriptProvider) lastSystem() string { return p.lastRequest().System }

// request is the nth request the provider saw, in order.
func (p *scriptProvider) request(i int) provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= len(p.reqs) {
		return provider.Request{}
	}
	return p.reqs[i]
}

func (p *scriptProvider) Name() string { return "fake" }

func (p *scriptProvider) ListModels(ctx context.Context) ([]provider.Model, error) {
	return []provider.Model{{
		Ref:           session.ModelRef{Provider: "fake", Model: "m1"},
		DisplayName:   "Fake 1",
		ContextWindow: 100000,
		Capabilities:  provider.Capabilities{Tools: true},
	}}, nil
}

func (p *scriptProvider) Complete(ctx context.Context, req provider.Request, emit func(provider.Part) error) error {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.reqs = append(p.reqs, req)
	textOnly := p.textOnly
	toolName, toolInput := p.toolName, p.toolInput
	p.mu.Unlock()
	if toolName == "" {
		toolName = "danger"
	}
	if toolInput == "" {
		toolInput = `{"x":1}`
	}
	if p.summary != nil && strings.Contains(req.System, "summarize") {
		select {
		case p.summaryHit <- struct{}{}:
		default:
		}
		var err error
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-p.summary:
		}
		p.mu.Lock()
		p.summaryErr = err
		p.mu.Unlock()
		close(p.summaryDone)
		if err != nil {
			return err
		}
	}
	if p.block != nil {
		if err := emit(provider.Part{Type: provider.PartTextDelta, Text: "thinking"}); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.block:
		}
	}
	var parts []provider.Part
	if n%2 == 1 && !textOnly {
		parts = []provider.Part{
			{Type: provider.PartTextDelta, Text: "Looking."},
			{Type: provider.PartToolUseStart, ID: "tu" + itoa(n), Name: toolName},
			{Type: provider.PartToolUseDelta, ID: "tu" + itoa(n), Text: toolInput},
			{Type: provider.PartToolUseEnd, ID: "tu" + itoa(n)},
			{Type: provider.PartUsage, Usage: session.Usage{Input: 10, Output: 5}},
			{Type: provider.PartStop, StopReason: session.StopToolUse, StopReasonRaw: "tool_calls"},
		}
	} else {
		parts = []provider.Part{
			{Type: provider.PartTextDelta, Text: "done"},
			{Type: provider.PartUsage, Usage: session.Usage{Input: 20, Output: 1}},
			{Type: provider.PartStop, StopReason: session.StopEndTurn, StopReasonRaw: "stop"},
		}
	}
	for _, part := range parts {
		if err := emit(part); err != nil {
			return err
		}
	}
	return nil
}

func itoa(n int) string { return string(rune('0' + n)) }

type fakePlugin struct {
	prov      *scriptProvider
	toolCalls int32
}

func (f *fakePlugin) Name() string { return "fake" }

func (f *fakePlugin) Init(ctx context.Context, h plugin.Host) error {
	if err := h.RegisterProvider(f.prov); err != nil {
		return err
	}
	err := h.RegisterTool(tool.Tool{
		Name:        "danger",
		Description: "an unsafe fake tool",
		Schema:      json.RawMessage(`{"type":"object"}`),
		Safety:      tool.Unsafe,
		Invoke: func(ctx context.Context, call tool.Call) (tool.Result, error) {
			atomic.AddInt32(&f.toolCalls, 1)
			return tool.Result{Content: []session.Block{session.TextBlock("ran")}}, nil
		},
	})
	if err != nil {
		return err
	}
	return h.RegisterCommand(plugin.Command{
		Name:        "hello",
		Description: "submits hi",
		Run: func(ctx context.Context, call plugin.CommandCall) (plugin.Action, error) {
			return plugin.SubmitPrompt{Text: "hi"}, nil
		},
	})
}

type harness struct {
	srv   *server.Server
	ws    string
	fp    *fakePlugin
	store *session.Store
}

func newHarness(t *testing.T, prov *scriptProvider) *harness {
	t.Helper()
	return newHarnessWith(t, prov)
}

// newHarnessWith is newHarness plus extra plugins, for a test that needs hooks registered.
// The hook runner is always wired: with no handler registered it is what every other test
// exercises, a fire that finds nothing and returns.
func newHarnessWith(t *testing.T, prov *scriptProvider, extra ...plugin.Plugin) *harness {
	t.Helper()
	fp := &fakePlugin{prov: prov}
	srv, store := newServerWith(t, testConfig(), append([]plugin.Plugin{fp}, extra...)...)
	return &harness{srv: srv, ws: t.TempDir(), fp: fp, store: store}
}

// testConfig is what every server test starts from: the fake provider's one model, strict
// permissions, no tool timeout.
func testConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Default.Provider = "fake"
	cfg.Default.Model = "m1"
	cfg.Default.Thinking = "off"
	cfg.Permissions.Mode = "strict"
	cfg.MaxTokens = 1000
	return cfg
}

// newServerWith wires a server in wire.go's order and returns it with its store: the server
// exists before Load so a plugin's Host can reach it, and the provider registry takes its
// providers from what Load committed.
func newServerWith(t *testing.T, cfg *config.Config, plugins ...plugin.Plugin) (*server.Server, *session.Store) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	store, err := session.OpenStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	preg := plugin.NewRegistry(nil, func(string) {})
	reg := provider.NewRegistry(filepath.Join(dir, "registry.json"))
	srv := server.New(server.Deps{
		Version:  "test",
		Config:   cfg,
		Store:    store,
		Registry: reg,
		Plugins:  preg,
		Gate:     gate.New(nil),
		Hooks:    plugin.NewHookRunner(preg, 5*time.Second, func(string) {}),
	})
	services := srv.PluginServices()
	services.ProvidersChanged = func(ps []provider.Provider) { reg.SetProviders(ps...) }
	preg.SetServices(services)
	preg.Load(ctx, plugins...)
	reg.SetProviders(preg.Providers()...)
	if err := reg.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv, store
}

func (h *harness) dial(t *testing.T, asker bool) *protocol.Client {
	t.Helper()
	return dialAs(t, h.srv, asker)
}

// dialAs opens one connection to srv and greets it as a client of the given asker class.
func dialAs(t *testing.T, srv *server.Server, asker bool) *protocol.Client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cc, sc := protocol.Pipe()
	go func() { _ = srv.Serve(ctx, sc) }()
	cl := protocol.NewClient(cc)
	t.Cleanup(func() { _ = cl.Close(); cancel() })
	var hr protocol.ClientHelloResult
	if err := cl.Call(ctx, protocol.MethodClientHello, protocol.ClientHelloParams{Client: "test", Version: "0", Asker: asker}, &hr); err != nil {
		t.Fatalf("hello: %v", err)
	}
	return cl
}

func (h *harness) open(t *testing.T, cl *protocol.Client) protocol.SessionInfo {
	t.Helper()
	var info protocol.SessionInfo
	if err := cl.Call(context.Background(), protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: h.ws}, &info); err != nil {
		t.Fatalf("open: %v", err)
	}
	return info
}

// drain reads notifications until stop returns true or the deadline passes.
func drain(t *testing.T, cl *protocol.Client, stop func(protocol.Notification) bool) []protocol.Notification {
	t.Helper()
	var got []protocol.Notification
	deadline := time.After(5 * time.Second)
	for {
		select {
		case n := <-cl.Notifications():
			got = append(got, n)
			if stop(n) {
				return got
			}
		case <-deadline:
			t.Fatalf("timeout after %d notifications: %v", len(got), methods(got))
		}
	}
}

func methods(ns []protocol.Notification) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.Method)
	}
	return out
}

func entries(t *testing.T, ns []protocol.Notification) []session.Entry {
	t.Helper()
	var out []session.Entry
	for _, n := range ns {
		if n.Method != protocol.NotifyEntryAppended {
			continue
		}
		var ea protocol.EntryAppended
		if err := json.Unmarshal(n.Params, &ea); err != nil {
			t.Fatal(err)
		}
		out = append(out, ea.Entry)
	}
	return out
}

func kinds(es []session.Entry) []session.Kind {
	out := make([]session.Kind, 0, len(es))
	for _, e := range es {
		out = append(out, e.Kind)
	}
	return out
}

func equalKinds(a, b []session.Kind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTurnWithAskerAllows(t *testing.T) {
	h := newHarness(t, &scriptProvider{})
	cl := h.dial(t, true)
	info := h.open(t, cl)
	ctx := context.Background()
	var sub protocol.SessionSubmitResult
	err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID,
		Content:   []session.Block{session.TextBlock("go")},
		Source:    session.SourceTyped,
	}, &sub)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if sub.TurnID == "" {
		t.Fatal("empty turn id")
	}
	ns := drain(t, cl, func(n protocol.Notification) bool {
		switch n.Method {
		case protocol.NotifyPermissionRequested:
			var pr protocol.PermissionRequested
			_ = json.Unmarshal(n.Params, &pr)
			if pr.Tool != "danger" {
				t.Errorf("asked about %q", pr.Tool)
			}
			go func() {
				_ = cl.Call(ctx, protocol.MethodSessionAnswer, protocol.SessionAnswerParams{
					SessionID: info.SessionID, ToolUseID: pr.ToolUseID,
					Decision: session.Allow, Scope: session.ScopeOnce, Reason: "test allows",
				}, &struct{}{})
			}()
		case protocol.NotifyTurnState:
			var ts protocol.TurnStateChanged
			_ = json.Unmarshal(n.Params, &ts)
			return ts.State == "completed"
		}
		return false
	})
	es := entries(t, ns)
	want := []session.Kind{
		session.KindSessionOpened, session.KindUserMessage, session.KindAssistantMessage,
		session.KindPermissionDecision, session.KindToolResult, session.KindAssistantMessage,
	}
	if !equalKinds(kinds(es), want) {
		t.Fatalf("kinds = %v, want %v", kinds(es), want)
	}
	pd := es[3].Payload.(session.PermissionDecision)
	if pd.Decision != session.Allow || pd.DecidedBy != session.ByAsker || pd.Scope != session.ScopeOnce {
		t.Fatalf("decision = %+v", pd)
	}
	tr := es[4].Payload.(session.ToolResult)
	if tr.Outcome != session.OutcomeOK {
		t.Fatalf("outcome = %s", tr.Outcome)
	}
	if atomic.LoadInt32(&h.fp.toolCalls) != 1 {
		t.Fatalf("tool ran %d times", h.fp.toolCalls)
	}
	// Resume from a second connection replays every entry.
	cl2 := h.dial(t, false)
	var info2 protocol.SessionInfo
	if err := cl2.Call(ctx, protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: info.SessionID}, &info2); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if info2.SessionID != info.SessionID {
		t.Fatalf("resumed %s, want %s", info2.SessionID, info.SessionID)
	}
	seen := 0
	drain(t, cl2, func(n protocol.Notification) bool {
		if n.Method == protocol.NotifyEntryAppended {
			seen++
		}
		return seen == len(want)
	})
}

func TestStrictWithoutAskerDenies(t *testing.T) {
	h := newHarness(t, &scriptProvider{})
	cl := h.dial(t, false)
	info := h.open(t, cl)
	ctx := context.Background()
	var sub protocol.SessionSubmitResult
	if err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Content: []session.Block{session.TextBlock("go")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatal(err)
	}
	ns := drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyTurnState {
			return false
		}
		var ts protocol.TurnStateChanged
		_ = json.Unmarshal(n.Params, &ts)
		return ts.State == "completed"
	})
	es := entries(t, ns)
	var pd *session.PermissionDecision
	var tr *session.ToolResult
	for _, e := range es {
		switch p := e.Payload.(type) {
		case session.PermissionDecision:
			pd = &p
		case session.ToolResult:
			tr = &p
		}
	}
	if pd == nil || pd.Decision != session.Deny || pd.DecidedBy != session.ByNoAsker {
		t.Fatalf("decision = %+v", pd)
	}
	if tr == nil || tr.Outcome != session.OutcomeError {
		t.Fatalf("result = %+v", tr)
	}
	if atomic.LoadInt32(&h.fp.toolCalls) != 0 {
		t.Fatal("tool ran without permission")
	}
	for _, n := range ns {
		if n.Method == protocol.NotifyPermissionRequested {
			t.Fatal("asked a non-asker")
		}
	}
}

func TestInterruptCancelMidStream(t *testing.T) {
	prov := &scriptProvider{block: make(chan struct{})}
	h := newHarness(t, prov)
	cl := h.dial(t, true)
	info := h.open(t, cl)
	ctx := context.Background()
	var sub protocol.SessionSubmitResult
	if err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Content: []session.Block{session.TextBlock("go")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatal(err)
	}
	drain(t, cl, func(n protocol.Notification) bool { return n.Method == protocol.NotifyStreamDelta })
	var ir server.InterruptResult
	if err := cl.Call(ctx, protocol.MethodSessionInterrupt, protocol.SessionInterruptParams{
		SessionID: info.SessionID, How: session.InterruptCancel,
	}, &ir); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	// Wait for turn.state idle, not just for the turn_interrupted entry: the entry is
	// broadcast while the runner is still finishing (it syncs the log before it rests), and
	// turn.state is the notification that says the session is free for the next turn.
	ns := drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyTurnState {
			return false
		}
		var ts protocol.TurnStateChanged
		_ = json.Unmarshal(n.Params, &ts)
		return ts.State == "idle"
	})
	last := entries(t, ns)
	ti := last[len(last)-1].Payload.(session.TurnInterrupted)
	if ti.How != session.InterruptCancel {
		t.Fatalf("how = %s", ti.How)
	}
	// A second typed submit now starts a fresh turn instead of a conflict.
	close(prov.block)
	prov.block = nil
	if err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Content: []session.Block{session.TextBlock("again")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatalf("second submit: %v", err)
	}
}

func TestCommandRunSubmitsPrompt(t *testing.T) {
	h := newHarness(t, &scriptProvider{})
	cl := h.dial(t, true)
	info := h.open(t, cl)
	ctx := context.Background()
	var cr protocol.CommandRunResult
	if err := cl.Call(ctx, protocol.MethodCommandRun, protocol.CommandRunParams{SessionID: info.SessionID, Name: "hello"}, &cr); err != nil {
		t.Fatalf("command.run: %v", err)
	}
	if cr.TurnID == "" {
		t.Fatal("command did not start a turn")
	}
	ns := drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyEntryAppended {
			return false
		}
		var ea protocol.EntryAppended
		_ = json.Unmarshal(n.Params, &ea)
		return ea.Entry.Kind == session.KindUserMessage
	})
	es := entries(t, ns)
	um := es[len(es)-1].Payload.(session.UserMessage)
	if um.Content[0].Text != "hi" || um.Source != session.SourceTyped {
		t.Fatalf("user message = %+v", um)
	}
	var missing protocol.CommandRunResult
	err := cl.Call(ctx, protocol.MethodCommandRun, protocol.CommandRunParams{SessionID: info.SessionID, Name: "nope"}, &missing)
	var pe *protocol.Error
	if !errorsAs(err, &pe) || pe.Code != protocol.CodeNotFound {
		t.Fatalf("unknown command err = %v", err)
	}
}

// namedModelPlugin registers a provider with one model, for a test that needs a second model to
// switch to; the harness's own fakePlugin always registers "fake:m1".
type namedModelPlugin struct{ provider, model string }

func (p namedModelPlugin) Name() string { return p.provider }

func (p namedModelPlugin) Init(_ context.Context, h plugin.Host) error {
	return h.RegisterProvider(namedModelProvider(p))
}

type namedModelProvider struct{ provider, model string }

func (p namedModelProvider) Name() string { return p.provider }

func (p namedModelProvider) ListModels(context.Context) ([]provider.Model, error) {
	return []provider.Model{{
		Ref:          session.ModelRef{Provider: p.provider, Model: p.model},
		DisplayName:  p.model,
		Capabilities: provider.Capabilities{Tools: true},
	}}, nil
}

func (p namedModelProvider) Complete(context.Context, provider.Request, func(provider.Part) error) error {
	return errors.New("namedModelProvider: no turn ever runs on it in a test")
}

// TestCommandRunSetsModelAndRefusesDuringActiveTurn: /model routes through plugin.SetModel and
// s.setModel the same way session.set_model does, so it appends a model_change and reports the
// resolved ref in its notice; and it is refused with conflict while a turn is active, the same
// as the raw RPC.
func TestCommandRunSetsModelAndRefusesDuringActiveTurn(t *testing.T) {
	prov := &scriptProvider{block: make(chan struct{})}
	h := newHarnessWith(t, prov, commands.New(), namedModelPlugin{"aperture", "other"})
	cl := h.dial(t, false)
	info := h.open(t, cl)
	ctx := context.Background()

	var sub protocol.SessionSubmitResult
	if err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Content: []session.Block{session.TextBlock("go")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatal(err)
	}
	drain(t, cl, func(n protocol.Notification) bool { return n.Method == protocol.NotifyStreamDelta })

	var conflict protocol.CommandRunResult
	err := cl.Call(ctx, protocol.MethodCommandRun, protocol.CommandRunParams{
		SessionID: info.SessionID, Name: "model", Args: "aperture:other",
	}, &conflict)
	if got := code(t, err); got != protocol.CodeConflict {
		t.Fatalf("/model during active turn code %d (%v)", got, err)
	}

	close(prov.block)
	drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyTurnState {
			return false
		}
		var ts protocol.TurnStateChanged
		_ = json.Unmarshal(n.Params, &ts)
		return ts.State == "completed"
	})

	var res protocol.CommandRunResult
	if err := cl.Call(ctx, protocol.MethodCommandRun, protocol.CommandRunParams{
		SessionID: info.SessionID, Name: "model", Args: "aperture:other",
	}, &res); err != nil {
		t.Fatalf("/model: %v", err)
	}
	if res.TurnID != "" || res.Notice != "model set to aperture:other" || res.SessionID != "" {
		t.Fatalf("result %+v", res)
	}
	ns := drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyEntryAppended {
			return false
		}
		var ea protocol.EntryAppended
		_ = json.Unmarshal(n.Params, &ea)
		return ea.Entry.Kind == session.KindModelChange
	})
	es := entries(t, ns)
	mc := es[len(es)-1].Payload.(session.ModelChange)
	if mc.Model.String() != "aperture:other" {
		t.Fatalf("model_change = %+v", mc)
	}
}

// TestCommandRunForksAtTheNewestEntry: a bare /fork forks at the session's newest entry of any
// kind and the caller connection is attached to the child, so it receives the child's own
// replay ending with the fork_point (Session.Fork's doc: "a new session whose first entry is a
// fork_point at `at`" - "first" of the child's own log, last of the merged view this test reads
// off the wire).
func TestCommandRunForksAtTheNewestEntry(t *testing.T) {
	prov := &scriptProvider{textOnly: true}
	h := newHarnessWith(t, prov, commands.New())
	cl := h.dial(t, false)
	info := h.open(t, cl)
	ctx := context.Background()

	runTurn(t, cl, info.SessionID, "go")

	// Appended right before the bare /fork, so it is the newest entry when /fork runs, not
	// whatever was newest before it. forkAt must resolve "newest" itself under parent.mu,
	// not from a value a caller read earlier: reading it outside that lock (as command.run
	// once did, before calling forkAt) leaves a window where an append like this one lands
	// after the read and before the lock, and the fork would land one entry stale, missing
	// this title_change entirely.
	var setRes server.EntryIDResult
	if err := cl.Call(ctx, protocol.MethodSessionSetTitle, protocol.SessionSetTitleParams{
		SessionID: info.SessionID, Title: "renamed",
	}, &setRes); err != nil {
		t.Fatalf("set_title: %v", err)
	}
	if setRes.EntryID == "" {
		t.Fatal("empty entry id")
	}

	var res protocol.CommandRunResult
	if err := cl.Call(ctx, protocol.MethodCommandRun, protocol.CommandRunParams{
		SessionID: info.SessionID, Name: "fork",
	}, &res); err != nil {
		t.Fatalf("/fork: %v", err)
	}
	if res.SessionID == "" || res.SessionID == info.SessionID {
		t.Fatalf("result %+v", res)
	}
	if res.Notice != "forked to "+res.SessionID {
		t.Fatalf("notice %q", res.Notice)
	}

	childNs := drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyEntryAppended {
			return false
		}
		var ea protocol.EntryAppended
		_ = json.Unmarshal(n.Params, &ea)
		return ea.SessionID == res.SessionID && ea.Entry.Kind == session.KindForkPoint
	})
	childEntries := entries(t, childNs)
	last := childEntries[len(childEntries)-1]
	fp, ok := last.Payload.(session.ForkPoint)
	if !ok {
		t.Fatalf("last replayed child entry kind = %v, want fork_point", last.Kind)
	}
	if fp.ParentSessionID.String() != info.SessionID || fp.ParentEntryID.String() != setRes.EntryID {
		t.Fatalf("fork_point = %+v, want parent %s at %s (the set_title entry, the newest)", fp, info.SessionID, setRes.EntryID)
	}
}

// TestSetTitleRefusedDuringActiveTurn exercises session.set_title while a turn is running:
// the runner's goroutine is concurrently touching the session (parked in Provider.Complete)
// on its own goroutine while this call runs on the connection's, so -race must see no race and
// the call itself must be refused with CodeConflict rather than corrupt anything.
func TestSetTitleRefusedDuringActiveTurn(t *testing.T) {
	prov := &scriptProvider{block: make(chan struct{})}
	h := newHarness(t, prov)
	cl := h.dial(t, false)
	info := h.open(t, cl)
	ctx := context.Background()
	var sub protocol.SessionSubmitResult
	if err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Content: []session.Block{session.TextBlock("go")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatal(err)
	}
	drain(t, cl, func(n protocol.Notification) bool { return n.Method == protocol.NotifyStreamDelta })

	var res server.EntryIDResult
	err := cl.Call(ctx, protocol.MethodSessionSetTitle, protocol.SessionSetTitleParams{
		SessionID: info.SessionID, Title: "new title",
	}, &res)
	var pe *protocol.Error
	if !errorsAs(err, &pe) || pe.Code != protocol.CodeConflict {
		t.Fatalf("set_title during active turn err = %v, want CodeConflict", err)
	}

	close(prov.block)
	drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyTurnState {
			return false
		}
		var ts protocol.TurnStateChanged
		_ = json.Unmarshal(n.Params, &ts)
		return ts.State == "completed"
	})

	// Once the turn is idle again, the same call succeeds.
	if err := cl.Call(ctx, protocol.MethodSessionSetTitle, protocol.SessionSetTitleParams{
		SessionID: info.SessionID, Title: "new title",
	}, &res); err != nil {
		t.Fatalf("set_title after turn completed: %v", err)
	}
	if res.EntryID == "" {
		t.Fatal("empty entry id")
	}
}

func TestHelloMustComeFirst(t *testing.T) {
	h := newHarness(t, &scriptProvider{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cc, sc := protocol.Pipe()
	go func() { _ = h.srv.Serve(ctx, sc) }()
	cl := protocol.NewClient(cc)
	defer func() { _ = cl.Close() }()
	var out protocol.SessionListResult
	err := cl.Call(ctx, protocol.MethodSessionList, struct{}{}, &out)
	var pe *protocol.Error
	if !errorsAs(err, &pe) || pe.Code != protocol.CodeUnauthorized {
		t.Fatalf("err = %v", err)
	}
}

func errorsAs(err error, target **protocol.Error) bool {
	for err != nil {
		if pe, ok := err.(*protocol.Error); ok {
			*target = pe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// longTurnProvider answers with a safe tool call for its first toolCalls completions, then
// ends the turn. It never blocks: the point of TestLongTurnConcurrentMutationNoDeadlock is to
// hammer the session with concurrent mutation RPCs while a real turn is genuinely in flight and
// its Observer callbacks are genuinely firing, not to synchronize on a channel.
type longTurnProvider struct {
	mu        sync.Mutex
	calls     int
	toolCalls int
}

func (p *longTurnProvider) Name() string { return "long" }

func (p *longTurnProvider) ListModels(ctx context.Context) ([]provider.Model, error) {
	return []provider.Model{{
		Ref:           session.ModelRef{Provider: "long", Model: "m1"},
		DisplayName:   "Long 1",
		ContextWindow: 100000,
		Capabilities:  provider.Capabilities{Tools: true},
	}}, nil
}

func (p *longTurnProvider) Complete(ctx context.Context, req provider.Request, emit func(provider.Part) error) error {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	var parts []provider.Part
	if n <= p.toolCalls {
		id := fmt.Sprintf("tu%d", n)
		parts = []provider.Part{
			{Type: provider.PartTextDelta, Text: "step"},
			{Type: provider.PartToolUseStart, ID: id, Name: "safe"},
			{Type: provider.PartToolUseDelta, ID: id, Text: `{}`},
			{Type: provider.PartToolUseEnd, ID: id},
			{Type: provider.PartUsage, Usage: session.Usage{Input: 1, Output: 1}},
			{Type: provider.PartStop, StopReason: session.StopToolUse, StopReasonRaw: "tool_calls"},
		}
	} else {
		parts = []provider.Part{
			{Type: provider.PartTextDelta, Text: "done"},
			{Type: provider.PartUsage, Usage: session.Usage{Input: 1, Output: 1}},
			{Type: provider.PartStop, StopReason: session.StopEndTurn, StopReasonRaw: "stop"},
		}
	}
	for _, part := range parts {
		if err := emit(part); err != nil {
			return err
		}
	}
	return nil
}

type longTurnPlugin struct {
	prov      *longTurnProvider
	toolCalls int32
}

func (f *longTurnPlugin) Name() string { return "long" }

func (f *longTurnPlugin) Init(ctx context.Context, h plugin.Host) error {
	if err := h.RegisterProvider(f.prov); err != nil {
		return err
	}
	return h.RegisterTool(tool.Tool{
		Name:        "safe",
		Description: "a safe fake tool that always runs without asking",
		Schema:      json.RawMessage(`{"type":"object"}`),
		Safety:      tool.Safe,
		Invoke: func(ctx context.Context, call tool.Call) (tool.Result, error) {
			atomic.AddInt32(&f.toolCalls, 1)
			return tool.Result{Content: []session.Block{session.TextBlock("ran")}}, nil
		},
	})
}

func newLongTurnHarness(t *testing.T, toolCalls int) (*server.Server, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	store, err := session.OpenStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	prov := &longTurnProvider{toolCalls: toolCalls}
	fp := &longTurnPlugin{prov: prov}
	plugins := plugin.NewRegistry(nil, func(string) {})
	plugins.Load(ctx, fp)
	reg := provider.NewRegistry(filepath.Join(dir, "registry.json"), plugins.Providers()...)
	if err := reg.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Default.Provider = "long"
	cfg.Default.Model = "m1"
	cfg.Default.Thinking = "off"
	cfg.Permissions.Mode = "strict"
	cfg.MaxTokens = 1000
	srv := server.New(server.Deps{
		Version:  "test",
		Config:   cfg,
		Store:    store,
		Registry: reg,
		Plugins:  plugins,
		Gate:     gate.New(nil),
	})
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv, t.TempDir()
}

func dialRaw(t *testing.T, srv *server.Server) *protocol.Client {
	t.Helper()
	return dialAs(t, srv, false)
}

// TestLongTurnConcurrentMutationNoDeadlock is the regression test for the AB-BA deadlock
// between a liveSession's mutex and turn.Runner's: a real turn runs for twenty consecutive
// tool calls (so its Observer fires many times, genuinely concurrently with everything below)
// while a second connection hammers session.set_title and session.set_mode in a tight loop for
// the whole turn. Run with -race -count=10 -timeout 120s: a reintroduced deadlock shows up as
// the run timing out rather than as a race report.
func TestLongTurnConcurrentMutationNoDeadlock(t *testing.T) {
	const toolCalls = 20
	srv, ws := newLongTurnHarness(t, toolCalls)
	cl := dialRaw(t, srv)
	var info protocol.SessionInfo
	if err := cl.Call(context.Background(), protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: ws}, &info); err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	var sub protocol.SessionSubmitResult
	if err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Content: []session.Block{session.TextBlock("go")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatalf("submit: %v", err)
	}

	cl2 := dialRaw(t, srv)
	stop := make(chan struct{})
	var hammer sync.WaitGroup
	hammer.Add(2)
	go func() {
		defer hammer.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			var res server.EntryIDResult
			_ = cl2.Call(ctx, protocol.MethodSessionSetTitle, protocol.SessionSetTitleParams{
				SessionID: info.SessionID, Title: fmt.Sprintf("title %d", i),
			}, &res)
		}
	}()
	go func() {
		defer hammer.Done()
		modes := []session.Mode{session.ModeStrict, session.ModePermissive, session.ModeOff}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			var res server.EntryIDResult
			_ = cl2.Call(ctx, protocol.MethodSessionSetMode, protocol.SessionSetModeParams{
				SessionID: info.SessionID, Mode: modes[i%len(modes)],
			}, &res)
		}
	}()

	drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyTurnState {
			return false
		}
		var ts protocol.TurnStateChanged
		_ = json.Unmarshal(n.Params, &ts)
		return ts.State == "completed"
	})
	close(stop)
	hammer.Wait()
}

// TestConcurrentColdResume is the regression test for single-flighting cold session.Load
// calls: two connections resume the same session, currently held by nobody, at once.
// session.Store's flock is exclusive per file descriptor within one process, so without
// single-flighting the loser would fail with CodeUnavailable instead of sharing the winner's
// result.
func TestConcurrentColdResume(t *testing.T) {
	h := newHarness(t, &scriptProvider{})
	cl := h.dial(t, false)
	info := h.open(t, cl)
	ctx := context.Background()
	var sub protocol.SessionSubmitResult
	if err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Content: []session.Block{session.TextBlock("go")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatal(err)
	}
	drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyTurnState {
			return false
		}
		var ts protocol.TurnStateChanged
		_ = json.Unmarshal(n.Params, &ts)
		return ts.State == "completed"
	})
	// session.close detaches synchronously within the RPC handler; since this is the only
	// subscriber and no turn is active, the session is now cold (removed from Server.live and
	// closed on disk).
	if err := cl.Call(ctx, protocol.MethodSessionClose, protocol.SessionCloseParams{SessionID: info.SessionID}, &struct{}{}); err != nil {
		t.Fatal(err)
	}

	const n = 2
	conns := make([]*protocol.Client, n)
	for i := range n {
		conns[i] = h.dial(t, false)
	}
	results := make([]protocol.SessionInfo, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			errs[i] = conns[i].Call(ctx, protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: info.SessionID}, &results[i])
		}(i)
	}
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("resume %d: %v", i, errs[i])
		}
		if results[i].SessionID != info.SessionID {
			t.Fatalf("resume %d: got session %s, want %s", i, results[i].SessionID, info.SessionID)
		}
	}
	for i := range n {
		seen := 0
		drain(t, conns[i], func(n protocol.Notification) bool {
			if n.Method == protocol.NotifyEntryAppended {
				seen++
			}
			return seen >= 3
		})
	}
}

// TestDetachRacesResume is the regression test for the zombie-subscription race: one
// connection's session.close (which detaches it, and since it is the only subscriber with no
// active turn, closes the session) runs concurrently with a second connection's
// session.resume for the same id. However that race resolves, the resuming connection must end
// up attached to a live, working session, never one that has already been (or is concurrently
// being) closed out from under it. Run with -count=20: each run picks a different interleaving.
func TestDetachRacesResume(t *testing.T) {
	h := newHarness(t, &scriptProvider{})
	clA := h.dial(t, false)
	info := h.open(t, clA)
	clB := h.dial(t, false)

	ctx := context.Background()
	var wg sync.WaitGroup
	var resumeErr error
	var resumeInfo protocol.SessionInfo
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = clA.Call(ctx, protocol.MethodSessionClose, protocol.SessionCloseParams{SessionID: info.SessionID}, &struct{}{})
	}()
	go func() {
		defer wg.Done()
		resumeErr = clB.Call(ctx, protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: info.SessionID}, &resumeInfo)
	}()
	wg.Wait()
	if resumeErr != nil {
		t.Fatalf("resume: %v", resumeErr)
	}
	if resumeInfo.SessionID != info.SessionID {
		t.Fatalf("resumed %s, want %s", resumeInfo.SessionID, info.SessionID)
	}

	// However the race resolved, clB must be attached to a live, working session: submit a
	// turn and confirm the resulting notifications actually reach clB. Under the bug this
	// fixes, clB could end up subscribed to a liveSession already removed from Server.live and
	// closed, and submitting through it would panic inside the runner's goroutine (session's
	// log is nil after Close).
	var sub protocol.SessionSubmitResult
	if err := clB.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: resumeInfo.SessionID, Content: []session.Block{session.TextBlock("go")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatalf("submit: %v", err)
	}
	drain(t, clB, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyTurnState {
			return false
		}
		var ts protocol.TurnStateChanged
		_ = json.Unmarshal(n.Params, &ts)
		return ts.State == "completed"
	})
}

// blockingProvider blocks inside Complete, before emitting anything, until block is closed.
type blockingProvider struct {
	block chan struct{}
}

func (p *blockingProvider) Name() string { return "blocking" }

func (p *blockingProvider) ListModels(ctx context.Context) ([]provider.Model, error) {
	return []provider.Model{{
		Ref:           session.ModelRef{Provider: "blocking", Model: "m1"},
		DisplayName:   "Blocking 1",
		ContextWindow: 100000,
	}}, nil
}

func (p *blockingProvider) Complete(ctx context.Context, req provider.Request, emit func(provider.Part) error) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.block:
	}
	parts := []provider.Part{
		{Type: provider.PartTextDelta, Text: "done"},
		{Type: provider.PartUsage, Usage: session.Usage{Input: 1, Output: 1}},
		{Type: provider.PartStop, StopReason: session.StopEndTurn, StopReasonRaw: "stop"},
	}
	for _, part := range parts {
		if err := emit(part); err != nil {
			return err
		}
	}
	return nil
}

type blockingPlugin struct{ prov *blockingProvider }

func (p *blockingPlugin) Name() string { return "blocking" }

func (p *blockingPlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterProvider(p.prov)
}

func newBlockingHarness(t *testing.T, prov *blockingProvider) (*server.Server, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	store, err := session.OpenStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	fp := &blockingPlugin{prov: prov}
	plugins := plugin.NewRegistry(nil, func(string) {})
	plugins.Load(ctx, fp)
	reg := provider.NewRegistry(filepath.Join(dir, "registry.json"), plugins.Providers()...)
	if err := reg.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Default.Provider = "blocking"
	cfg.Default.Model = "m1"
	cfg.Default.Thinking = "off"
	cfg.Permissions.Mode = "strict"
	cfg.MaxTokens = 1000
	srv := server.New(server.Deps{
		Version:  "test",
		Config:   cfg,
		Store:    store,
		Registry: reg,
		Plugins:  plugins,
		Gate:     gate.New(nil),
	})
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv, t.TempDir()
}

// TestSetTitleRefusedImmediatelyAfterSubmit is the protocol-level regression test for the fix
// round 2 residual: session.submit's response only arrives once the runner has appended the
// user_message (see firstAppendSignal), which happens before the provider is ever called - the
// provider here blocks on a channel before emitting anything at all, so no stream.delta or
// turn.state notification can have reached the client yet either. A session.set_title issued
// immediately after submit returns, with no wait and no drain, must still be refused with
// CodeConflict: without fix round 2's markStarting, the mirrored state at that point could
// still be whatever it was before this turn started, and set_title would pass the active-turn
// check and append to the session concurrently with the turn.
func TestSetTitleRefusedImmediatelyAfterSubmit(t *testing.T) {
	prov := &blockingProvider{block: make(chan struct{})}
	srv, ws := newBlockingHarness(t, prov)
	cl := dialRaw(t, srv)
	var info protocol.SessionInfo
	if err := cl.Call(context.Background(), protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: ws}, &info); err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	var sub protocol.SessionSubmitResult
	if err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Content: []session.Block{session.TextBlock("go")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatalf("submit: %v", err)
	}

	var res server.EntryIDResult
	err := cl.Call(ctx, protocol.MethodSessionSetTitle, protocol.SessionSetTitleParams{
		SessionID: info.SessionID, Title: "new title",
	}, &res)
	var pe *protocol.Error
	if !errorsAs(err, &pe) || pe.Code != protocol.CodeConflict {
		t.Fatalf("set_title immediately after submit err = %v, want CodeConflict", err)
	}

	close(prov.block)
	drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyTurnState {
			return false
		}
		var ts protocol.TurnStateChanged
		_ = json.Unmarshal(n.Params, &ts)
		return ts.State == "completed"
	})
}

// TestShutdownWaitsForServe pins the shutdown order: Shutdown closes every live session,
// and a Serve loop still running can still be dispatching requests against one, so
// Shutdown must not close anything until every Serve loop has returned. It is bounded by
// the context it is given, since a connection's own lifetime is the caller's to end.
func TestShutdownWaitsForServe(t *testing.T) {
	h := newHarness(t, &scriptProvider{})
	serveCtx, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()
	cc, sc := protocol.Pipe()
	served := make(chan struct{})
	go func() { defer close(served); _ = h.srv.Serve(serveCtx, sc) }()
	cl := protocol.NewClient(cc)
	defer func() { _ = cl.Close() }()
	var hr protocol.ClientHelloResult
	if err := cl.Call(context.Background(), protocol.MethodClientHello, protocol.ClientHelloParams{Client: "test", Version: "0"}, &hr); err != nil {
		t.Fatalf("hello: %v", err)
	}

	done := make(chan struct{})
	go func() { defer close(done); _ = h.srv.Shutdown(context.Background()) }()
	select {
	case <-done:
		t.Fatal("Shutdown returned while a Serve loop was still running")
	case <-time.After(200 * time.Millisecond):
	}

	cancelServe()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after its context ended")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after Serve did")
	}
}

// TestShutdownIsBoundedByItsContext is the other half: a Serve loop the caller never ends
// must not hold Shutdown past the deadline it was given.
func TestShutdownIsBoundedByItsContext(t *testing.T) {
	h := newHarness(t, &scriptProvider{})
	serveCtx, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()
	_, sc := protocol.Pipe()
	go func() { _ = h.srv.Serve(serveCtx, sc) }()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = h.srv.Shutdown(ctx) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown outlived the context it was given")
	}
}

// TestSubmitRejectsAnInvalidMessage covers the submit that used to hang the caller: a
// user_message the session log refuses (here a text block with no text) makes the runner's
// very first append fail, and the runner cannot record a turn_failed for a turn whose id
// was never assigned, so nothing ever answered the submit. Validating the message here is
// the answer the caller deserves.
func TestSubmitRejectsAnInvalidMessage(t *testing.T) {
	h := newHarness(t, &scriptProvider{})
	cl := h.dial(t, true)
	info := h.open(t, cl)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var sub protocol.SessionSubmitResult
	err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID,
		Content:   []session.Block{{Type: session.BlockText}},
		Source:    session.SourceTyped,
	}, &sub)
	var pe *protocol.Error
	if !errorsAs(err, &pe) {
		t.Fatalf("submit err = %v, want a protocol error", err)
	}
	if pe.Code != protocol.CodeInvalidArgument {
		t.Fatalf("submit code = %d, want CodeInvalidArgument", pe.Code)
	}
}

// hookRecorder registers the two session hooks and counts what reaches them. Its
// session_opened context is what the turn's system prompt must carry.
type hookRecorder struct {
	mu     sync.Mutex
	opened []plugin.SessionOpenedPayload
	closed []string
}

func (h *hookRecorder) Name() string { return "hookrec" }

func (h *hookRecorder) Init(ctx context.Context, host plugin.Host) error {
	err := host.RegisterHook(plugin.HookHandler{Point: plugin.HookSessionOpened, Handle: func(ctx context.Context, c plugin.HookCall) (any, error) {
		p, ok := c.Payload.(*plugin.SessionOpenedPayload)
		if !ok {
			return nil, fmt.Errorf("session_opened payload %T", c.Payload)
		}
		h.mu.Lock()
		h.opened = append(h.opened, *p)
		h.mu.Unlock()
		return &plugin.SessionOpenedResult{Context: "HOOK CONTEXT"}, nil
	}})
	if err != nil {
		return err
	}
	return host.RegisterHook(plugin.HookHandler{Point: plugin.HookSessionClosed, Handle: func(ctx context.Context, c plugin.HookCall) (any, error) {
		h.mu.Lock()
		h.closed = append(h.closed, c.SessionID)
		h.mu.Unlock()
		return nil, nil
	}})
}

func (h *hookRecorder) counts() ([]plugin.SessionOpenedPayload, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]plugin.SessionOpenedPayload(nil), h.opened...), append([]string(nil), h.closed...)
}

// TestSessionHooksFireOnceAndReachTheSystemPrompt covers the two hooks the server itself
// fires: session_opened once when the session goes live, with its context reaching the
// turn's system prompt, and session_closed once when the last client detaches.
func TestSessionHooksFireOnceAndReachTheSystemPrompt(t *testing.T) {
	hr := &hookRecorder{}
	prov := &scriptProvider{}
	h := newHarnessWith(t, prov, hr)
	cl := h.dial(t, false) // no asker: the tool is denied and the turn still runs two requests
	info := h.open(t, cl)
	ctx := context.Background()

	opened, closed := hr.counts()
	if len(opened) != 1 || opened[0].SessionID != info.SessionID || opened[0].Resumed || opened[0].Workspace.Root != h.ws {
		t.Fatalf("session_opened %+v", opened)
	}
	if len(closed) != 0 {
		t.Fatalf("session_closed before any close: %v", closed)
	}

	var sub protocol.SessionSubmitResult
	if err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Content: []session.Block{session.TextBlock("go")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatal(err)
	}
	drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyTurnState {
			return false
		}
		var ts protocol.TurnStateChanged
		_ = json.Unmarshal(n.Params, &ts)
		return ts.State == "completed"
	})
	if got := prov.lastSystem(); !strings.HasSuffix(got, "HOOK CONTEXT") {
		t.Errorf("system prompt %q does not end with the session_opened context", got)
	}

	if err := cl.Call(ctx, protocol.MethodSessionClose, protocol.SessionCloseParams{SessionID: info.SessionID}, &struct{}{}); err != nil {
		t.Fatal(err)
	}
	opened, closed = hr.counts()
	if len(opened) != 1 {
		t.Errorf("session_opened fired %d times", len(opened))
	}
	if len(closed) != 1 || closed[0] != info.SessionID {
		t.Errorf("session_closed %v", closed)
	}

	// Shutdown must not fire again for a session detach already closed: both paths claim the
	// same flag, and only the winner fires. Close the client first so Shutdown's wait for
	// this connection's Serve loop ends at once rather than at its deadline.
	_ = cl.Close()
	shutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := h.srv.Shutdown(shutCtx); err != nil {
		t.Fatal(err)
	}
	if _, closed = hr.counts(); len(closed) != 1 {
		t.Errorf("session_closed after shutdown %v", closed)
	}
}

// hostPlugin captures the Host its Init receives so a test can drive Connect, Note and
// SetStatus from outside, the way a long-lived plugin does after Load has returned.
type hostPlugin struct{ host plugin.Host }

func (p *hostPlugin) Name() string { return "p" }

func (p *hostPlugin) Init(_ context.Context, h plugin.Host) error {
	p.host = h
	return nil
}

func code(t *testing.T, err error) int {
	t.Helper()
	var pe *protocol.Error
	if !errors.As(err, &pe) {
		t.Fatalf("not a protocol error: %v", err)
	}
	return pe.Code
}

func TestPluginHostReachesTheServerOverTheProtocol(t *testing.T) {
	hp := &hostPlugin{}
	h := newHarnessWith(t, &scriptProvider{}, hp)
	if hp.host == nil {
		t.Fatal("Init never ran")
	}
	ctx := context.Background()
	cl := h.dial(t, false)
	info := h.open(t, cl)

	// The host's own connection is caller class plugin, and answers a hello like any other.
	pc, err := hp.host.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = pc.Close() }()
	var hr protocol.ClientHelloResult
	if err := pc.Call(ctx, protocol.MethodClientHello, protocol.ClientHelloParams{Client: "p", Version: "0"}, &hr); err != nil {
		t.Fatalf("plugin hello: %v", err)
	}
	// The registry is one of the few things a plugin may query.
	var models protocol.RegistryListResult
	if err := pc.Call(ctx, protocol.MethodRegistryList, nil, &models); err != nil {
		t.Fatalf("plugin registry.list: %v", err)
	}
	if len(models.Models) == 0 {
		t.Fatal("registry.list came back empty")
	}

	// A plugin may name a parent, so it reaches the branch that looks the parent up.
	parent := &protocol.ParentRef{SessionID: session.NewID().String(), ToolUseID: "tu1"}
	err = pc.Call(ctx, protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: h.ws, Parent: parent}, &protocol.SessionInfo{})
	if got := code(t, err); got != protocol.CodeNotFound {
		t.Fatalf("plugin open with parent: code %d (%v)", got, err)
	}
	// A client may not: parent is not part of its class.
	err = cl.Call(ctx, protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: h.ws, Parent: parent}, &protocol.SessionInfo{})
	if got := code(t, err); got != protocol.CodeInvalidArgument {
		t.Fatalf("client open with parent: code %d (%v)", got, err)
	}

	// Note reaches the session log and every subscriber.
	sid := ulid.MustParse(info.SessionID)
	if err := hp.host.Note(sid, "hello", session.NoteInfo); err != nil {
		t.Fatalf("note: %v", err)
	}
	ns := drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyEntryAppended {
			return false
		}
		var ea protocol.EntryAppended
		if err := json.Unmarshal(n.Params, &ea); err != nil {
			t.Fatal(err)
		}
		return ea.Entry.Kind == session.KindNote
	})
	es := entries(t, ns)
	note, ok := es[len(es)-1].Payload.(session.Note)
	if !ok {
		t.Fatalf("last entry %+v", es[len(es)-1])
	}
	if note.Plugin != "p" || note.Text != "hello" || note.Role != session.NoteInfo {
		t.Fatalf("note = %+v", note)
	}

	// A status item set after Init reaches every connection that is already attached.
	hp.host.SetStatus("k", []plugin.Span{{Text: "one", Role: "muted"}})
	ns = drain(t, cl, func(n protocol.Notification) bool { return n.Method == protocol.NotifyStatusUpdated })
	var su protocol.StatusUpdated
	if err := json.Unmarshal(ns[len(ns)-1].Params, &su); err != nil {
		t.Fatal(err)
	}
	if len(su.Items) != 1 || su.Items[0].Owner != "p" || su.Items[0].Key != "k" || su.Items[0].Content[0].Text != "one" {
		t.Fatalf("status items %+v", su.Items)
	}

	// A widget set after Init reaches them the same way.
	if err := hp.host.SetWidget("w", plugin.SlotHeader, []plugin.Span{{Text: "hi", Role: "text"}}); err != nil {
		t.Fatalf("set widget: %v", err)
	}
	ns = drain(t, cl, func(n protocol.Notification) bool { return n.Method == protocol.NotifyWidgetUpdated })
	assertWidget(t, ns[len(ns)-1])

	// A connection that arrives afterwards is told the same state right after its hello.
	cl2 := h.dial(t, false)
	states := map[string]string{}
	sawStatus, sawWidget := false, false
	drain(t, cl2, func(n protocol.Notification) bool {
		switch n.Method {
		case protocol.NotifyStatusUpdated:
			var s2 protocol.StatusUpdated
			if err := json.Unmarshal(n.Params, &s2); err != nil {
				t.Fatal(err)
			}
			if len(s2.Items) != 1 || s2.Items[0].Owner != "p" {
				t.Errorf("status on connect %+v", s2.Items)
			}
			sawStatus = true
		case protocol.NotifyWidgetUpdated:
			assertWidget(t, n)
			sawWidget = true
		case protocol.NotifyPluginState:
			var ps protocol.PluginState
			if err := json.Unmarshal(n.Params, &ps); err != nil {
				t.Fatal(err)
			}
			states[ps.Name] = ps.State
		}
		return sawStatus && sawWidget && len(states) == 2
	})
	if states["fake"] != string(plugin.StateReady) || states["p"] != string(plugin.StateReady) {
		t.Fatalf("plugin.state = %+v", states)
	}
}

func assertWidget(t *testing.T, n protocol.Notification) {
	t.Helper()
	var w protocol.WidgetUpdated
	if err := json.Unmarshal(n.Params, &w); err != nil {
		t.Fatal(err)
	}
	if w.Owner != "p" || w.Key != "w" || w.Slot != plugin.SlotHeader || len(w.Content) != 1 || w.Content[0].Text != "hi" {
		t.Errorf("widget = %+v", w)
	}
}

// dialPlugin is a plugin-class connection: the pipe the host got from Connect, wrapped in a
// client that has said hello, exactly as a plugin would drive it.
func dialPlugin(t *testing.T, hp *hostPlugin) *protocol.Client {
	t.Helper()
	ctx := context.Background()
	pc, err := hp.host.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	if err := pc.Call(ctx, protocol.MethodClientHello, protocol.ClientHelloParams{Client: "p", Version: "0", Asker: true}, &protocol.ClientHelloResult{}); err != nil {
		t.Fatalf("plugin hello: %v", err)
	}
	return pc
}

func TestPluginConnectionCannotAnswerOrSubmitElsewhere(t *testing.T) {
	hp := &hostPlugin{}
	h := newHarnessWith(t, &scriptProvider{}, hp)
	ctx := context.Background()
	cl := h.dial(t, true)
	info := h.open(t, cl)
	pc := dialPlugin(t, hp)

	// Answering a permission question is the asker's job, never a plugin's, even one whose
	// hello declared asker.
	err := pc.Call(ctx, protocol.MethodSessionAnswer, protocol.SessionAnswerParams{
		SessionID: info.SessionID, ToolUseID: "tu1",
		Decision: session.Allow, Scope: session.ScopeOnce, Reason: "plugin says so",
	}, &struct{}{})
	if got := code(t, err); got != protocol.CodeUnauthorized {
		t.Fatalf("plugin answer: code %d (%v)", got, err)
	}

	// Nor may it drive a session somebody else opened.
	err = pc.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Content: []session.Block{session.TextBlock("go")}, Source: session.SourceTyped,
	}, &protocol.SessionSubmitResult{})
	if got := code(t, err); got != protocol.CodeUnauthorized {
		t.Fatalf("plugin submit elsewhere: code %d (%v)", got, err)
	}
}

// TestDetachWhileSteeringCancelsTheTurn is the other end of a steer: the turn is parked
// waiting for a steer message, and the only connection that could send one has gone. Nothing
// else would ever resolve that turn, so the session would stay live for the life of the
// process; detaching cancels it instead, and the log says so.
func TestDetachWhileSteeringCancelsTheTurn(t *testing.T) {
	prov := &scriptProvider{block: make(chan struct{})}
	h := newHarness(t, prov)
	cl := h.dial(t, true)
	info := h.open(t, cl)
	ctx := context.Background()
	var sub protocol.SessionSubmitResult
	if err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Content: []session.Block{session.TextBlock("go")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatal(err)
	}
	drain(t, cl, func(n protocol.Notification) bool { return n.Method == protocol.NotifyStreamDelta })
	var ir server.InterruptResult
	if err := cl.Call(ctx, protocol.MethodSessionInterrupt, protocol.SessionInterruptParams{
		SessionID: info.SessionID, How: session.InterruptSteer,
	}, &ir); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyTurnState {
			return false
		}
		var ts protocol.TurnStateChanged
		_ = json.Unmarshal(n.Params, &ts)
		return ts.State == string(turn.Steering)
	})

	// The last subscriber leaves while the turn is still steering.
	if err := cl.Call(ctx, protocol.MethodSessionClose, protocol.SessionCloseParams{SessionID: info.SessionID}, &struct{}{}); err != nil {
		t.Fatalf("close: %v", err)
	}
	cl2 := dialAs(t, h.srv, false)
	if err := cl2.Call(ctx, protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: info.SessionID}, &protocol.SessionInfo{}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	var ti session.TurnInterrupted
	found := false
	for _, e := range entries(t, drain(t, cl2, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyEntryAppended {
			return false
		}
		var ea protocol.EntryAppended
		_ = json.Unmarshal(n.Params, &ea)
		return ea.Entry.Kind == session.KindTurnInterrupted
	})) {
		if got, ok := e.Payload.(session.TurnInterrupted); ok {
			ti, found = got, true
		}
	}
	if !found || ti.How != session.InterruptCancel {
		t.Fatalf("turn_interrupted = %+v (found %v)", ti, found)
	}
	close(prov.block)
}

// TestPluginClassIsGatedPerTheCallerTable is the plugin caller class in one place: what a
// plugin may never ask for at all, and what it may ask for only about a session it opened.
func TestPluginClassIsGatedPerTheCallerTable(t *testing.T) {
	hp := &hostPlugin{}
	h := newHarnessWith(t, &scriptProvider{}, hp)
	ctx := context.Background()
	cl := h.dial(t, true)
	info := h.open(t, cl) // the client's session, not the plugin's
	pc := dialPlugin(t, hp)

	never := []struct {
		method string
		params any
	}{
		{protocol.MethodSessionList, nil},
		{protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: info.SessionID}},
		{protocol.MethodSessionFork, protocol.SessionForkParams{SessionID: info.SessionID}},
		{protocol.MethodRegistryRefresh, nil},
	}
	for _, c := range never {
		err := pc.Call(ctx, c.method, c.params, &struct{}{})
		if got := code(t, err); got != protocol.CodeUnauthorized {
			t.Errorf("plugin %s: code %d (%v)", c.method, got, err)
		}
	}

	notMine := []struct {
		method string
		params any
	}{
		{protocol.MethodSessionInterrupt, protocol.SessionInterruptParams{SessionID: info.SessionID, How: session.InterruptCancel}},
		{protocol.MethodSessionClose, protocol.SessionCloseParams{SessionID: info.SessionID}},
		{protocol.MethodSessionSetTitle, protocol.SessionSetTitleParams{SessionID: info.SessionID, Title: "mine now"}},
		{protocol.MethodSessionCompact, protocol.SessionCompactParams{SessionID: info.SessionID}},
		{protocol.MethodCommandRun, protocol.CommandRunParams{SessionID: info.SessionID, Name: "hello"}},
	}
	for _, c := range notMine {
		err := pc.Call(ctx, c.method, c.params, &struct{}{})
		if got := code(t, err); got != protocol.CodeUnauthorized {
			t.Errorf("plugin %s on somebody else's session: code %d (%v)", c.method, got, err)
		}
	}

	// Its own session, though, it may drive.
	var own protocol.SessionInfo
	if err := pc.Call(ctx, protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: h.ws}, &own); err != nil {
		t.Fatalf("plugin open: %v", err)
	}
	err := pc.Call(ctx, protocol.MethodSessionSetTitle, protocol.SessionSetTitleParams{
		SessionID: own.SessionID, Title: "mine",
	}, &server.EntryIDResult{})
	if err != nil {
		t.Fatalf("plugin set_title on its own session: %v", err)
	}
	if err := pc.Call(ctx, protocol.MethodSessionClose, protocol.SessionCloseParams{SessionID: own.SessionID}, &struct{}{}); err != nil {
		t.Fatalf("plugin close of its own session: %v", err)
	}
}

// TestBroadcastsSkipPluginConnections pins the fan-out rule: a plugin's own connection gets
// the render state once, when it says hello, and never again. Nothing obliges a plugin to
// drain its notification queue, so a broadcast on every status change would grow that queue
// for the life of the process. The ordering is deterministic: the outbox is FIFO, so a
// broadcast enqueued before session.open would have to arrive before that call's replay.
func TestBroadcastsSkipPluginConnections(t *testing.T) {
	hp := &hostPlugin{}
	h := newHarnessWith(t, &scriptProvider{}, hp)
	ctx := context.Background()
	pc := dialPlugin(t, hp)
	drain(t, pc, func(n protocol.Notification) bool {
		var ps protocol.PluginState
		return n.Method == protocol.NotifyPluginState && json.Unmarshal(n.Params, &ps) == nil && ps.Name == "p"
	})

	hp.host.SetStatus("k", []plugin.Span{{Text: "one", Role: "muted"}})
	if err := hp.host.SetWidget("w", plugin.SlotHeader, []plugin.Span{{Text: "hi", Role: "text"}}); err != nil {
		t.Fatalf("set widget: %v", err)
	}
	if err := pc.Call(ctx, protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: h.ws}, &protocol.SessionInfo{}); err != nil {
		t.Fatalf("plugin open: %v", err)
	}
	ns := drain(t, pc, func(n protocol.Notification) bool { return n.Method == protocol.NotifyEntryAppended })
	for _, n := range ns {
		if n.Method == protocol.NotifyStatusUpdated || n.Method == protocol.NotifyWidgetUpdated {
			t.Fatalf("plugin connection got broadcasts: %v", methods(ns))
		}
	}
}

func TestPluginConnectionSubmitsToItsOwnSession(t *testing.T) {
	hp := &hostPlugin{}
	h := newHarnessWith(t, &scriptProvider{}, hp)
	ctx := context.Background()
	pc := dialPlugin(t, hp)

	var own protocol.SessionInfo
	if err := pc.Call(ctx, protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: h.ws}, &own); err != nil {
		t.Fatalf("plugin open: %v", err)
	}
	var sub protocol.SessionSubmitResult
	if err := pc.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: own.SessionID, Content: []session.Block{session.TextBlock("go")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatalf("plugin submit: %v", err)
	}
	if sub.TurnID == "" {
		t.Fatal("empty turn id")
	}
}

func TestAppendNoteRefusedFromAClient(t *testing.T) {
	h := newHarness(t, &scriptProvider{})
	cl := h.dial(t, false)
	info := h.open(t, cl)
	err := cl.Call(context.Background(), protocol.MethodPluginAppendNote, protocol.PluginAppendNoteParams{
		SessionID: info.SessionID, Text: "no", Role: session.NoteInfo,
	}, &server.EntryIDResult{})
	if got := code(t, err); got != protocol.CodeUnauthorized {
		t.Fatalf("code %d (%v)", got, err)
	}
}

// runTurn submits one message and waits for the turn to complete.
func runTurn(t *testing.T, cl *protocol.Client, sid, text string) {
	t.Helper()
	var sub protocol.SessionSubmitResult
	if err := cl.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: sid, Content: []session.Block{session.TextBlock(text)}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatalf("submit: %v", err)
	}
	drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyTurnState {
			return false
		}
		var ts protocol.TurnStateChanged
		_ = json.Unmarshal(n.Params, &ts)
		return ts.State == "completed"
	})
}

// lastCompaction is the newest compaction entry the connection was told about.
func lastCompaction(t *testing.T, cl *protocol.Client) session.Entry {
	t.Helper()
	ns := drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyEntryAppended {
			return false
		}
		var ea protocol.EntryAppended
		_ = json.Unmarshal(n.Params, &ea)
		return ea.Entry.Kind == session.KindCompaction
	})
	es := entries(t, ns)
	return es[len(es)-1]
}

func TestSessionCompactOnAnIdleSession(t *testing.T) {
	prov := &scriptProvider{textOnly: true}
	h := newHarness(t, prov)
	cl := h.dial(t, false)
	info := h.open(t, cl)
	ctx := context.Background()
	for range 3 {
		runTurn(t, cl, info.SessionID, "go")
	}
	var res server.EntryIDResult
	if err := cl.Call(ctx, protocol.MethodSessionCompact, protocol.SessionCompactParams{SessionID: info.SessionID}, &res); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if res.EntryID == "" {
		t.Fatal("empty entry id")
	}
	e := lastCompaction(t, cl)
	if e.ID.String() != res.EntryID {
		t.Fatalf("broadcast entry %s, answered %s", e.ID, res.EntryID)
	}
	c := e.Payload.(session.Compaction)
	if c.Summary == "" || c.Usage.Output == 0 || c.Model != info.Model {
		t.Fatalf("compaction %+v", c)
	}
	if !strings.Contains(prov.lastSystem(), "summarize") {
		t.Fatalf("the summary request must carry the summary system prompt, got %q", prov.lastSystem())
	}
	if req := prov.lastRequest(); len(req.Tools) != 0 {
		t.Fatalf("the summary request must offer no tools: %+v", req.Tools)
	}
	// Nothing has been said since, so a second compaction has fewer than two entries to cover.
	var again server.EntryIDResult
	if err := cl.Call(ctx, protocol.MethodSessionCompact, protocol.SessionCompactParams{SessionID: info.SessionID}, &again); err != nil {
		t.Fatalf("second compact: %v", err)
	}
	if again.EntryID != "" {
		t.Fatalf("second compact entry id = %q, want empty", again.EntryID)
	}
}

func TestSessionCompactRefusedDuringActiveTurn(t *testing.T) {
	prov := &scriptProvider{textOnly: true, block: make(chan struct{})}
	h := newHarness(t, prov)
	cl := h.dial(t, false)
	info := h.open(t, cl)
	ctx := context.Background()
	var sub protocol.SessionSubmitResult
	if err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Content: []session.Block{session.TextBlock("go")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatal(err)
	}
	drain(t, cl, func(n protocol.Notification) bool { return n.Method == protocol.NotifyStreamDelta })
	var res server.EntryIDResult
	err := cl.Call(ctx, protocol.MethodSessionCompact, protocol.SessionCompactParams{SessionID: info.SessionID}, &res)
	var pe *protocol.Error
	if !errorsAs(err, &pe) || pe.Code != protocol.CodeConflict {
		t.Fatalf("compact during active turn err = %v, want CodeConflict", err)
	}
	close(prov.block)
	drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyTurnState {
			return false
		}
		var ts protocol.TurnStateChanged
		_ = json.Unmarshal(n.Params, &ts)
		return ts.State == "completed"
	})
}

func TestCompactCommandThroughCommandRun(t *testing.T) {
	prov := &scriptProvider{textOnly: true}
	h := newHarnessWith(t, prov, compactcmd.New())
	cl := h.dial(t, false)
	info := h.open(t, cl)
	ctx := context.Background()
	for range 3 {
		runTurn(t, cl, info.SessionID, "go")
	}
	var res protocol.CommandRunResult
	if err := cl.Call(ctx, protocol.MethodCommandRun, protocol.CommandRunParams{
		SessionID: info.SessionID, Name: "compact", Args: "focus on decisions",
	}, &res); err != nil {
		t.Fatalf("/compact: %v", err)
	}
	if res.Notice != "compacted 6 entries" || res.TurnID != "" {
		t.Fatalf("result %+v", res)
	}
	msgs := prov.lastRequest().Messages
	if len(msgs) == 0 || !strings.Contains(msgs[len(msgs)-1].Content[0].Text, "focus on decisions") {
		t.Fatalf("instructions must reach the summary request: %+v", msgs)
	}
	e := lastCompaction(t, cl)
	if e.Payload.(session.Compaction).Summary == "" {
		t.Fatal("empty summary")
	}
	// Nothing left to cover, so the command says so instead of asking the model again.
	var again protocol.CommandRunResult
	if err := cl.Call(ctx, protocol.MethodCommandRun, protocol.CommandRunParams{
		SessionID: info.SessionID, Name: "compact",
	}, &again); err != nil {
		t.Fatal(err)
	}
	if again.Notice != "nothing to compact" {
		t.Fatalf("second /compact notice %q", again.Notice)
	}
}

// TestCompactTwiceWithATurnBetween is the log a second compaction has to cope with: the first
// compaction is the oldest entry of the request context but the youngest entry of the range it
// belongs to, so the second one must take its span, not its entry id.
func TestCompactTwiceWithATurnBetween(t *testing.T) {
	prov := &scriptProvider{textOnly: true}
	h := newHarness(t, prov)
	cl := h.dial(t, false)
	info := h.open(t, cl)
	ctx := context.Background()
	for range 2 {
		runTurn(t, cl, info.SessionID, "go")
	}
	var first server.EntryIDResult
	if err := cl.Call(ctx, protocol.MethodSessionCompact, protocol.SessionCompactParams{SessionID: info.SessionID}, &first); err != nil {
		t.Fatalf("first compact: %v", err)
	}
	c1 := lastCompaction(t, cl).Payload.(session.Compaction)

	runTurn(t, cl, info.SessionID, "again")
	var second server.EntryIDResult
	if err := cl.Call(ctx, protocol.MethodSessionCompact, protocol.SessionCompactParams{SessionID: info.SessionID}, &second); err != nil {
		t.Fatalf("second compact: %v", err)
	}
	if second.EntryID == "" || second.EntryID == first.EntryID {
		t.Fatalf("second compact entry id %q, first %q", second.EntryID, first.EntryID)
	}
	c2 := lastCompaction(t, cl).Payload.(session.Compaction)
	if c2.FirstEntryID != c1.FirstEntryID {
		t.Errorf("second compaction starts at %s, want the first one's start %s", c2.FirstEntryID, c1.FirstEntryID)
	}
	if c2.LastEntryID.Compare(c1.LastEntryID) <= 0 {
		t.Errorf("second compaction ends at %s, not after %s", c2.LastEntryID, c1.LastEntryID)
	}
}

// TestShutdownYieldsACompactionInFlight: session.compact holds ls.mu across the summary
// request, and Shutdown closes every live session, which needs that lock. The compaction has
// to see the server's own cancellation, not only the caller's, or Shutdown blocks on ls.mu
// forever, long past the budget its caller gave it.
func TestShutdownYieldsACompactionInFlight(t *testing.T) {
	prov := &scriptProvider{
		textOnly:    true,
		summary:     make(chan struct{}),
		summaryHit:  make(chan struct{}, 1),
		summaryDone: make(chan struct{}),
	}
	defer close(prov.summary) // releases the summary request if the shutdown never does
	h := newHarness(t, prov)
	cl := h.dial(t, false)
	info := h.open(t, cl)
	for range 2 {
		runTurn(t, cl, info.SessionID, "go")
	}
	go func() {
		var res server.EntryIDResult
		_ = cl.Call(context.Background(), protocol.MethodSessionCompact, protocol.SessionCompactParams{SessionID: info.SessionID}, &res)
	}()
	select {
	case <-prov.summaryHit:
	case <-time.After(5 * time.Second):
		t.Fatal("the summary request never started")
	}

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		done <- h.srv.Shutdown(ctx)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown blocked on the session lock a compaction was holding")
	}
	select {
	case <-prov.summaryDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the summary request was never cancelled")
	}
	if err := prov.summaryOutcome(); err == nil {
		t.Error("the summary request must end in cancellation, not in a summary")
	}
}

// agentProvider scripts two sessions at once, by session id: the first session it sees (the
// root) calls the agent tool and then answers with text; every other session (the child the
// agent tool opened) calls the unsafe echo tool and then answers "child done".
type agentProvider struct {
	mu    sync.Mutex
	root  ulid.ULID
	calls map[ulid.ULID]int
	reqs  map[ulid.ULID][]provider.Request
}

func (p *agentProvider) Name() string { return "fake" }

func (p *agentProvider) ListModels(context.Context) ([]provider.Model, error) {
	return []provider.Model{{
		Ref:           session.ModelRef{Provider: "fake", Model: "m1"},
		DisplayName:   "Fake 1",
		ContextWindow: 100000,
		Capabilities:  provider.Capabilities{Tools: true},
	}}, nil
}

func (p *agentProvider) requestsFor(sid string) []provider.Request {
	id, err := ulid.Parse(sid)
	if err != nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.Request(nil), p.reqs[id]...)
}

func (p *agentProvider) Complete(ctx context.Context, req provider.Request, emit func(provider.Part) error) error {
	p.mu.Lock()
	if p.calls == nil {
		p.calls, p.reqs = map[ulid.ULID]int{}, map[ulid.ULID][]provider.Request{}
	}
	if p.root.IsZero() {
		p.root = req.SessionID
	}
	p.calls[req.SessionID]++
	n := p.calls[req.SessionID]
	p.reqs[req.SessionID] = append(p.reqs[req.SessionID], req)
	root := req.SessionID == p.root
	p.mu.Unlock()

	call := func(id, name, input string) []provider.Part {
		return []provider.Part{
			{Type: provider.PartToolUseStart, ID: id, Name: name},
			{Type: provider.PartToolUseDelta, ID: id, Text: input},
			{Type: provider.PartToolUseEnd, ID: id},
			{Type: provider.PartUsage, Usage: session.Usage{Input: 10, Output: 5}},
			{Type: provider.PartStop, StopReason: session.StopToolUse, StopReasonRaw: "tool_calls"},
		}
	}
	answer := func(text string) []provider.Part {
		return []provider.Part{
			{Type: provider.PartTextDelta, Text: text},
			{Type: provider.PartUsage, Usage: session.Usage{Input: 20, Output: 1}},
			{Type: provider.PartStop, StopReason: session.StopEndTurn, StopReasonRaw: "stop"},
		}
	}
	var parts []provider.Part
	switch {
	case root && n == 1:
		parts = call("tu_agent", "agent", `{"agent":"explorer","prompt":"look"}`)
	case root && n == 2:
		parts = call("tu_agent2", "agent", `{"agent":"helper","prompt":"help"}`)
	case root && n == 3:
		parts = call("tu_probe", "probe", `{}`)
	case root:
		parts = answer("root done")
	case n == 1:
		parts = call("tu_echo", "echo", `{}`)
	default:
		parts = answer("child done")
	}
	for _, part := range parts {
		if err := emit(part); err != nil {
			return err
		}
	}
	return nil
}

// echoPlugin is the agentProvider plus one unsafe tool the explorer definition allows, one it
// leaves out (so the child's filtered tool set is observable) and a safe "probe" that opens
// child sessions from inside its own tool call, which is the only moment a plugin holds a
// pending tool_use of its own.
type echoPlugin struct {
	prov *agentProvider
	ran  int32

	host      plugin.Host // this plugin's own host, for the probe's connections
	otherHost plugin.Host // another plugin's, to try the same parent from the wrong caller
	otherWS   string      // a directory that is not the parent's workspace

	mu                                     sync.Mutex
	errCwd, errFirst, errSecond, errStolen error
}

func (e *echoPlugin) Name() string { return "echo" }

func (e *echoPlugin) Init(_ context.Context, h plugin.Host) error {
	e.host = h
	if err := h.RegisterProvider(e.prov); err != nil {
		return err
	}
	err := h.RegisterTool(tool.Tool{
		Name: "echo", Description: "an unsafe echo", Schema: json.RawMessage(`{"type":"object"}`), Safety: tool.Unsafe,
		Invoke: func(context.Context, tool.Call) (tool.Result, error) {
			atomic.AddInt32(&e.ran, 1)
			return tool.Result{Content: []session.Block{session.TextBlock("echoed")}}, nil
		},
	})
	if err != nil {
		return err
	}
	err = h.RegisterTool(tool.Tool{
		Name: "bash", Description: "not in the explorer definition", Schema: json.RawMessage(`{"type":"object"}`), Safety: tool.Unsafe,
		Invoke: func(context.Context, tool.Call) (tool.Result, error) {
			return tool.Result{Content: []session.Block{session.TextBlock("bashed")}}, nil
		},
	})
	if err != nil {
		return err
	}
	return h.RegisterTool(tool.Tool{
		Name: "probe", Description: "opens child sessions for its own tool_use", Schema: json.RawMessage(`{"type":"object"}`), Safety: tool.Safe,
		Invoke: e.probe,
	})
}

// probe runs while its own tool_use is pending in the session that called it, which is what
// every parent check is about. It records what the server answered; the test asserts on it
// once the turn is over.
func (e *echoPlugin) probe(ctx context.Context, call tool.Call) (tool.Result, error) {
	open := func(h plugin.Host, cwd string) error {
		client, err := h.Connect(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		var info protocol.SessionInfo
		return client.Call(ctx, protocol.MethodSessionOpen, protocol.SessionOpenParams{
			Cwd:    cwd,
			Parent: &protocol.ParentRef{SessionID: call.SessionID.String(), ToolUseID: call.ID},
		}, &info)
	}
	cwdErr := open(e.host, e.otherWS)
	stolenErr := open(e.otherHost, call.Workspace.Root)
	firstErr := open(e.host, call.Workspace.Root)
	secondErr := open(e.host, call.Workspace.Root)
	e.mu.Lock()
	e.errCwd, e.errStolen, e.errFirst, e.errSecond = cwdErr, stolenErr, firstErr, secondErr
	e.mu.Unlock()
	return tool.Result{Content: []session.Block{session.TextBlock("probed")}}, nil
}

// capturingPlugin keeps its Host so a test can Connect as the plugin caller class itself.
type capturingPlugin struct{ host plugin.Host }

func (c *capturingPlugin) Name() string { return "capture" }

func (c *capturingPlugin) Init(_ context.Context, h plugin.Host) error {
	c.host = h
	return nil
}

// openChild is one session.open naming a parent, made over a plugin's own connection, which
// is the only caller class allowed to name one.
func openChild(t *testing.T, h plugin.Host, parentSID, toolUseID, cwd string) error {
	t.Helper()
	ctx := context.Background()
	client, err := h.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	var info protocol.SessionInfo
	return client.Call(ctx, protocol.MethodSessionOpen, protocol.SessionOpenParams{
		Cwd: cwd, Parent: &protocol.ParentRef{SessionID: parentSID, ToolUseID: toolUseID},
	}, &info)
}

// hasCode reports whether err is a protocol error with this code.
func hasCode(err error, code int) bool {
	var pe *protocol.Error
	return errorsAs(err, &pe) && pe.Code == code
}

// completedOn stops a drain when the turn on sid reports itself completed.
func completedOn(sid string) func(protocol.Notification) bool {
	return func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyTurnState {
			return false
		}
		var ts protocol.TurnStateChanged
		_ = json.Unmarshal(n.Params, &ts)
		return ts.SessionID == sid && ts.State == "completed"
	}
}

func toolNames(defs []provider.ToolDef) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

func TestChildSessionThroughThePluginClass(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".rudy", "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	def := "---\ndescription: Read-only exploration of the workspace\ntools: [echo]\n---\nYou explore the repository and report.\n"
	if err := os.WriteFile(filepath.Join(ws, ".rudy", "agents", "explorer.md"), []byte(def), 0o600); err != nil {
		t.Fatal(err)
	}
	// The helper names no tools, so its child gets every registered tool except the agent
	// tool itself: that exception is what keeps subagent depth at one.
	helper := "---\ndescription: A general helper\n---\nYou help.\n"
	if err := os.WriteFile(filepath.Join(ws, ".rudy", "agents", "helper.md"), []byte(helper), 0o600); err != nil {
		t.Fatal(err)
	}
	prov := &agentProvider{}
	ep := &echoPlugin{prov: prov, otherWS: t.TempDir()}
	cp := &capturingPlugin{}
	cfg := testConfig()
	cfg.ConfigDir = t.TempDir()
	srv, store := newServerWith(t, cfg, ep, cp, subagents.New(cfg.ConfigDir))
	ep.otherHost = cp.host

	ctx := context.Background()
	cl := dialAs(t, srv, true)
	var info protocol.SessionInfo
	if err := cl.Call(ctx, protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: ws}, &info); err != nil {
		t.Fatalf("open: %v", err)
	}
	var sub protocol.SessionSubmitResult
	if err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Content: []session.Block{session.TextBlock("go")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// The child's permission question reaches this connection, the root's asker, carrying the
	// child's own session id.
	var childSIDs []string
	var childToolUse string
	var depthErr error
	ns := drain(t, cl, func(n protocol.Notification) bool {
		switch n.Method {
		case protocol.NotifyPermissionRequested:
			var pr protocol.PermissionRequested
			_ = json.Unmarshal(n.Params, &pr)
			if pr.Tool != "echo" {
				t.Errorf("asked about %q, want echo", pr.Tool)
			}
			childSIDs = append(childSIDs, pr.SessionID)
			childToolUse = pr.ToolUseID
			// The echo tool is pending in a child right now, and echo is this plugin's
			// own tool, so only depth stands between it and a grandchild.
			if len(childSIDs) == 1 {
				depthErr = openChild(t, ep.host, pr.SessionID, pr.ToolUseID, ws)
			}
			go func() {
				_ = cl.Call(ctx, protocol.MethodSessionAnswer, protocol.SessionAnswerParams{
					SessionID: pr.SessionID, ToolUseID: pr.ToolUseID,
					Decision: session.Allow, Scope: session.ScopeOnce, Reason: "test allows",
				}, &struct{}{})
			}()
		case protocol.NotifyTurnState:
			var ts protocol.TurnStateChanged
			_ = json.Unmarshal(n.Params, &ts)
			return ts.SessionID == info.SessionID && ts.State == "completed"
		}
		return false
	})
	if len(childSIDs) != 2 {
		t.Fatalf("child sessions asked for permission: %v", childSIDs)
	}
	childSID, helperSID := childSIDs[0], childSIDs[1]
	if childSID == info.SessionID || helperSID == childSID {
		t.Fatalf("child session ids %v alongside the root %s", childSIDs, info.SessionID)
	}
	if childToolUse != "tu_echo" {
		t.Errorf("permission asked about tool_use %q", childToolUse)
	}
	if got := atomic.LoadInt32(&ep.ran); got != 2 {
		t.Errorf("echo ran %d times", got)
	}

	// The child's log names its parent and its agent.
	childID, err := ulid.Parse(childSID)
	if err != nil {
		t.Fatal(err)
	}
	childLog, err := session.ReadLog(store.Dir(childID))
	if err != nil {
		t.Fatal(err)
	}
	opened, ok := childLog[0].Payload.(session.SessionOpened)
	if !ok {
		t.Fatalf("child first entry is %s", childLog[0].Kind)
	}
	if opened.ParentSessionID != info.SessionID || opened.ParentToolUseID != "tu_agent" {
		t.Errorf("child parent = %q/%q, want %q/tu_agent", opened.ParentSessionID, opened.ParentToolUseID, info.SessionID)
	}
	if opened.Agent != "explorer" {
		t.Errorf("child agent = %q", opened.Agent)
	}

	// The child was offered the definition's tools only, under the definition's prompt.
	creqs := prov.requestsFor(childSID)
	if len(creqs) != 2 {
		t.Fatalf("child made %d requests", len(creqs))
	}
	if names := toolNames(creqs[0].Tools); !reflect.DeepEqual(names, []string{"echo"}) {
		t.Errorf("child tools = %v", names)
	}
	if !strings.HasPrefix(creqs[0].System, "You explore the repository and report.") {
		t.Errorf("child system = %q", creqs[0].System)
	}

	// What the server answered the four parent checks. The happy path above is the fifth:
	// the agent tool's own child, opened by the plugin that owns the agent tool.
	ep.mu.Lock()
	errCwd, errStolen, errFirst, errSecond := ep.errCwd, ep.errStolen, ep.errFirst, ep.errSecond
	ep.mu.Unlock()
	if !hasCode(errStolen, protocol.CodeUnauthorized) {
		t.Errorf("another plugin opened a child on a tool_use it does not own: %v", errStolen)
	}
	if !hasCode(depthErr, protocol.CodeRefusedByInvariant) {
		t.Errorf("a child session opened a grandchild: %v", depthErr)
	}
	if !hasCode(errCwd, protocol.CodeInvalidArgument) {
		t.Errorf("a child opened outside the parent's workspace: %v", errCwd)
	}
	if errFirst != nil {
		t.Errorf("a plugin could not open a child for its own pending tool_use: %v", errFirst)
	}
	if !hasCode(errSecond, protocol.CodeConflict) {
		t.Errorf("one tool_use opened two children: %v", errSecond)
	}

	// The helper child named no tools, so it was offered every registered tool and never
	// the agent tool: a child cannot open a grandchild.
	hreqs := prov.requestsFor(helperSID)
	if len(hreqs) != 2 {
		t.Fatalf("helper made %d requests", len(hreqs))
	}
	if names := toolNames(hreqs[0].Tools); !reflect.DeepEqual(names, []string{"echo", "bash", "probe"}) {
		t.Errorf("helper tools = %v", names)
	}

	// The root's tool_result for the agent call carries the child's answer.
	var result *session.ToolResult
	for _, e := range entries(t, ns) {
		if tr, ok := e.Payload.(session.ToolResult); ok && tr.ToolUseID == "tu_agent" {
			result = &tr
		}
	}
	if result == nil {
		t.Fatal("no tool_result for the agent call")
	}
	if result.Outcome != session.OutcomeOK || len(result.Content) != 1 || !strings.Contains(result.Content[0].Text, "child done") {
		t.Errorf("agent result = %+v", result)
	}

	// A resumed child keeps its agent definition, re-resolved from the workspace on cold
	// load since the definition is a file rather than part of the log, and it is still a
	// child: no agent tool, whatever became of the session that spawned it.
	var back protocol.SessionInfo
	if err := cl.Call(ctx, protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: helperSID}, &back); err != nil {
		t.Fatalf("resume child: %v", err)
	}
	if err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: helperSID, Content: []session.Block{session.TextBlock("again")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatalf("submit to the resumed child: %v", err)
	}
	drain(t, cl, completedOn(helperSID))
	hreqs = prov.requestsFor(helperSID)
	if len(hreqs) != 3 {
		t.Fatalf("helper made %d requests after the resume", len(hreqs))
	}
	if names := toolNames(hreqs[2].Tools); !reflect.DeepEqual(names, []string{"echo", "bash", "probe"}) {
		t.Errorf("resumed child tools = %v", names)
	}
	if !strings.HasPrefix(hreqs[2].System, "You help.") {
		t.Errorf("resumed child system = %q", hreqs[2].System)
	}

	// A resumed root session is no child: it runs the default agent and the whole tool set.
	if err := cl.Call(ctx, protocol.MethodSessionClose, protocol.SessionCloseParams{SessionID: info.SessionID}, &struct{}{}); err != nil {
		t.Fatalf("close root: %v", err)
	}
	if err := cl.Call(ctx, protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: info.SessionID}, &back); err != nil {
		t.Fatalf("resume root: %v", err)
	}
	if err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Content: []session.Block{session.TextBlock("more")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatalf("submit to the resumed root: %v", err)
	}
	drain(t, cl, completedOn(info.SessionID))
	rreqs := prov.requestsFor(info.SessionID)
	if names := toolNames(rreqs[len(rreqs)-1].Tools); !slices.Contains(names, "agent") {
		t.Errorf("resumed root tools = %v, want the agent tool among them", names)
	}

	// A parent naming a tool_use that is not pending is not_found, and a parent from a
	// client connection is invalid_argument.
	pcl, err := cp.host.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pcl.Close() }()
	var pe *protocol.Error
	err = pcl.Call(ctx, protocol.MethodSessionOpen, protocol.SessionOpenParams{
		Cwd: ws, Parent: &protocol.ParentRef{SessionID: info.SessionID, ToolUseID: "tu_gone"},
	}, &protocol.SessionInfo{})
	if !errorsAs(err, &pe) || pe.Code != protocol.CodeNotFound {
		t.Errorf("open with a stale parent tool_use = %v, want not_found", err)
	}
	err = cl.Call(ctx, protocol.MethodSessionOpen, protocol.SessionOpenParams{
		Cwd: ws, Parent: &protocol.ParentRef{SessionID: info.SessionID, ToolUseID: "tu_agent"},
	}, &protocol.SessionInfo{})
	if !errorsAs(err, &pe) || pe.Code != protocol.CodeInvalidArgument {
		t.Errorf("open with a parent from a client = %v, want invalid_argument", err)
	}
}

// namedToolPlugin registers one safe tool under its own name, for a test that needs the
// registry to hold more than the fake plugin's one tool.
type namedToolPlugin struct{ name string }

func (p namedToolPlugin) Name() string { return p.name }

func (p namedToolPlugin) Init(_ context.Context, h plugin.Host) error {
	return h.RegisterTool(tool.Tool{
		Name: p.name, Description: p.name, Schema: json.RawMessage(`{"type":"object"}`), Safety: tool.Safe,
		Invoke: func(context.Context, tool.Call) (tool.Result, error) {
			return tool.Result{Content: []session.Block{session.TextBlock("ok")}}, nil
		},
	})
}

// TestForkKeepsTheAgentDefinition: a fork inherits its parent's entries, so it inherits the
// agent those entries name. Without re-applying the definition a fork of a restricted session
// would come back with every tool, the default prompt and no step limit.
func TestForkKeepsTheAgentDefinition(t *testing.T) {
	prov := &scriptProvider{textOnly: true}
	// A second registered tool the definition leaves out, so "the fork kept the definition"
	// and "the fork got the whole registry" are different answers.
	h := newHarnessWith(t, prov, namedToolPlugin{"extra"})
	if err := os.MkdirAll(filepath.Join(h.ws, ".rudy", "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	def := "---\ndescription: Read-only exploration\ntools: [danger]\n---\nYou explore and report.\n"
	if err := os.WriteFile(filepath.Join(h.ws, ".rudy", "agents", "explorer.md"), []byte(def), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cl := h.dial(t, false)
	var info protocol.SessionInfo
	if err := cl.Call(ctx, protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: h.ws, Agent: "explorer"}, &info); err != nil {
		t.Fatalf("open: %v", err)
	}
	opened := entries(t, drain(t, cl, func(n protocol.Notification) bool {
		return n.Method == protocol.NotifyEntryAppended
	}))
	if len(opened) != 1 || opened[0].Kind != session.KindSessionOpened {
		t.Fatalf("replayed %v", kinds(opened))
	}
	var forkInfo protocol.SessionInfo
	if err := cl.Call(ctx, protocol.MethodSessionFork, protocol.SessionForkParams{
		SessionID: info.SessionID, AtEntryID: opened[0].ID.String(),
	}, &forkInfo); err != nil {
		t.Fatalf("fork: %v", err)
	}
	var sub protocol.SessionSubmitResult
	if err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: forkInfo.SessionID, Content: []session.Block{session.TextBlock("go")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatalf("submit to the fork: %v", err)
	}
	drain(t, cl, completedOn(forkInfo.SessionID))
	req := prov.lastRequest()
	if names := toolNames(req.Tools); !reflect.DeepEqual(names, []string{"danger"}) {
		t.Errorf("fork tools = %v, want the definition's", names)
	}
	if !strings.HasPrefix(req.System, "You explore and report.") {
		t.Errorf("fork system = %q", req.System)
	}
}

// TestOrphanedSessionClosesWhenItsTurnEnds: the last connection can leave while a turn is
// running, and detach cannot close the session then because the turn still owns it. Nothing
// else comes back for it, so the turn closes it on its way out. The agent tool's interrupted
// child is exactly this, and so is a client that disconnects mid-turn.
func TestOrphanedSessionClosesWhenItsTurnEnds(t *testing.T) {
	prov := &scriptProvider{block: make(chan struct{}), textOnly: true}
	h := newHarness(t, prov)
	ctx := context.Background()
	cl := h.dial(t, false)
	info := h.open(t, cl)
	sid := mustULID(t, info.SessionID)
	var sub protocol.SessionSubmitResult
	if err := cl.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Content: []session.Block{session.TextBlock("go")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// The provider is inside Complete once its first delta has arrived, so the turn is
	// genuinely running when the only connection lets the session go.
	drain(t, cl, func(n protocol.Notification) bool { return n.Method == protocol.NotifyStreamDelta })
	if err := cl.Call(ctx, protocol.MethodSessionClose, protocol.SessionCloseParams{SessionID: info.SessionID}, &struct{}{}); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Still held: the turn owns the session, so detach left it open and locked.
	if s, err := session.Load(h.store, sid); !errors.Is(err, session.ErrLocked) {
		if err == nil {
			_ = s.Close()
		}
		t.Fatalf("session should still be held while its turn runs, got %v", err)
	}
	close(prov.block)
	deadline := time.Now().Add(5 * time.Second)
	for {
		s, err := session.Load(h.store, sid)
		if err == nil {
			_ = s.Close()
			return
		}
		if !errors.Is(err, session.ErrLocked) {
			t.Fatalf("load: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the session was never closed after its turn ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// mustULID parses an id a server response just produced.
func mustULID(t *testing.T, s string) ulid.ULID {
	t.Helper()
	id, err := ulid.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return id
}
