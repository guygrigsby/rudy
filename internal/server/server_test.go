package server_test

import (
	"context"
	"encoding/json"
	"path/filepath"
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
	mu    sync.Mutex
	calls int
	block chan struct{}
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
	ctx := context.Background()
	dir := t.TempDir()
	store, err := session.OpenStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	fp := &fakePlugin{prov: prov}
	plugins := plugin.NewRegistry(nil, func(string) {})
	plugins.Load(ctx, fp)
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
	ns := drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyEntryAppended {
			return false
		}
		var ea protocol.EntryAppended
		_ = json.Unmarshal(n.Params, &ea)
		return ea.Entry.Kind == session.KindTurnInterrupted
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
