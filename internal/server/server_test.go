package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/server"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// scriptProvider answers odd calls with a tool_use for "danger" and even calls with "done".
// When block is non-nil it streams one delta and then waits for ctx or block.
type scriptProvider struct {
	mu      sync.Mutex
	calls   int
	block   chan struct{}
	systems []string // the system prompt of every request, in order
}

func (p *scriptProvider) lastSystem() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.systems) == 0 {
		return ""
	}
	return p.systems[len(p.systems)-1]
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
	p.systems = append(p.systems, req.System)
	p.mu.Unlock()
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
	if n%2 == 1 {
		parts = []provider.Part{
			{Type: provider.PartTextDelta, Text: "Looking."},
			{Type: provider.PartToolUseStart, ID: "tu" + itoa(n), Name: "danger"},
			{Type: provider.PartToolUseDelta, ID: "tu" + itoa(n), Text: `{"x":1}`},
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
	srv *server.Server
	ws  string
	fp  *fakePlugin
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
	ctx := context.Background()
	dir := t.TempDir()
	store, err := session.OpenStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	fp := &fakePlugin{prov: prov}
	plugins := plugin.NewRegistry(nil, func(string) {})
	plugins.Load(ctx, append([]plugin.Plugin{fp}, extra...)...)
	reg := provider.NewRegistry(filepath.Join(dir, "registry.json"), plugins.Providers()...)
	if err := reg.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Default.Provider = "fake"
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
		Hooks:    plugin.NewHookRunner(plugins, 5*time.Second, func(string) {}),
	})
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return &harness{srv: srv, ws: t.TempDir(), fp: fp}
}

func (h *harness) dial(t *testing.T, asker bool) *protocol.Client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cc, sc := protocol.Pipe()
	go func() { _ = h.srv.Serve(ctx, sc) }()
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
	ctx, cancel := context.WithCancel(context.Background())
	cc, sc := protocol.Pipe()
	go func() { _ = srv.Serve(ctx, sc) }()
	cl := protocol.NewClient(cc)
	t.Cleanup(func() { _ = cl.Close(); cancel() })
	var hr protocol.ClientHelloResult
	if err := cl.Call(ctx, protocol.MethodClientHello, protocol.ClientHelloParams{Client: "test", Version: "0"}, &hr); err != nil {
		t.Fatalf("hello: %v", err)
	}
	return cl
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
}
