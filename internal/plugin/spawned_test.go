package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// fakeChild is a plugin process without a process: it speaks the protocol over an in-memory
// Pipe from its own goroutine, exactly as a spawned child speaks it over stdio.
type fakeChild struct {
	t *testing.T

	peer *protocol.Peer

	version      int  // what to answer plugin.init with
	register     bool // send registrations before answering plugin.init
	provider     bool // register a wire: custom provider too
	slowTool     bool // never answer tool.invoke, so the caller's context is what ends it
	slowCommand  bool // never answer command.invoke
	failComplete bool // answer provider.complete with an error

	mu      sync.Mutex
	init    protocol.PluginInitParams
	invoked []protocol.ToolInvokeParams
	hooks   []protocol.HookFireParams
	cancels []string
	regErrs []error

	cancelled chan string
}

func newFakeChild(t *testing.T, conn protocol.Conn) *fakeChild {
	return &fakeChild{
		t:         t,
		peer:      protocol.NewPeer(conn),
		version:   protocol.ProtocolVersion,
		register:  true,
		cancelled: make(chan string, 4),
	}
}

func (c *fakeChild) run(ctx context.Context) {
	inc := c.peer.Incoming()
	for {
		raw, err := inc.Recv(ctx)
		if err != nil {
			return
		}
		var req protocol.Request
		if err := json.Unmarshal(raw, &req); err != nil || req.IsNotification() {
			continue
		}
		result, rerr, silent := c.handle(ctx, req)
		if silent {
			continue
		}
		if rerr != nil {
			_ = inc.Send(ctx, protocol.NewErrorResponse(req.ID, rerr))
			continue
		}
		resp, err := protocol.NewResponse(req.ID, result)
		if err != nil {
			c.t.Errorf("child: response for %s: %v", req.Method, err)
			continue
		}
		_ = inc.Send(ctx, resp)
	}
}

func (c *fakeChild) handle(ctx context.Context, req protocol.Request) (any, *protocol.Error, bool) {
	switch req.Method {
	case protocol.MethodPluginInit:
		var p protocol.PluginInitParams
		_ = json.Unmarshal(req.Params, &p)
		c.mu.Lock()
		c.init = p
		c.mu.Unlock()
		if c.register {
			c.registerAll(ctx)
		}
		return protocol.PluginInitResult{Name: "hello", Version: "0.1.0", ProtocolVersion: c.version}, nil, false
	case protocol.MethodToolInvoke:
		var p protocol.ToolInvokeParams
		_ = json.Unmarshal(req.Params, &p)
		c.mu.Lock()
		c.invoked = append(c.invoked, p)
		slow := c.slowTool
		c.mu.Unlock()
		if slow {
			return nil, nil, true
		}
		var in struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(p.Input, &in)
		return protocol.ToolInvokeResult{Content: []session.Block{session.TextBlock(strings.ToUpper(in.Text))}}, nil, false
	case protocol.MethodToolCancel:
		var p protocol.ToolCancelParams
		_ = json.Unmarshal(req.Params, &p)
		c.mu.Lock()
		c.cancels = append(c.cancels, p.ToolUseID)
		c.mu.Unlock()
		select {
		case c.cancelled <- p.ToolUseID:
		default:
		}
		return struct{}{}, nil, false
	case protocol.MethodHookFire:
		var p protocol.HookFireParams
		_ = json.Unmarshal(req.Params, &p)
		c.mu.Lock()
		c.hooks = append(c.hooks, p)
		c.mu.Unlock()
		res, err := json.Marshal(BeforeTurnResult{SystemPromptAdditions: []string{"hello plugin was here"}})
		if err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, err.Error(), nil), false
		}
		return protocol.HookFireResult{Result: res}, nil, false
	case protocol.MethodCommandInvoke:
		var p protocol.CommandInvokeParams
		_ = json.Unmarshal(req.Params, &p)
		if c.slowCommand {
			return nil, nil, true
		}
		if p.Args == "prompt" {
			return protocol.CommandInvokeResult{Prompt: "say hi"}, nil, false
		}
		return protocol.CommandInvokeResult{Notice: "hello from the plugin"}, nil, false
	case protocol.MethodProviderComplete:
		var p protocol.ProviderCompleteParams
		_ = json.Unmarshal(req.Params, &p)
		if c.failComplete {
			return nil, protocol.NewError(protocol.CodePluginError, "the model said no", nil), false
		}
		for _, text := range []string{"one", "two"} {
			note, err := protocol.NewNotification(protocol.NotifyProviderDelta, protocol.ProviderDelta{
				RequestID: p.RequestID,
				Part:      provider.Part{Type: provider.PartTextDelta, Text: text},
			})
			if err != nil {
				return nil, protocol.NewError(protocol.CodeInternal, err.Error(), nil), false
			}
			if err := c.peer.Incoming().Send(ctx, note); err != nil {
				return nil, protocol.NewError(protocol.CodeInternal, err.Error(), nil), false
			}
		}
		return protocol.ProviderCompleteResult{
			StopReason:    session.StopEndTurn,
			StopReasonRaw: "stop",
			Usage:         session.Usage{Input: 3, Output: 4},
		}, nil, false
	case protocol.MethodProviderListModels:
		return protocol.ProviderListModelsResult{Models: []provider.Model{{
			Ref: session.ModelRef{Provider: "hello", Model: "m1"}, DisplayName: "Hello 1",
		}}}, nil, false
	}
	return nil, protocol.NewError(protocol.CodeMethodNotFound, "no method "+req.Method, nil), false
}

// registerAll sends every registration and waits for each answer before the next, so they
// are all committed by the time the init response goes out. That ordering is the contract a
// spawned plugin has to keep.
func (c *fakeChild) registerAll(ctx context.Context) {
	cl := c.peer.Client()
	calls := []struct {
		method string
		params any
	}{
		{protocol.MethodPluginRegisterTool, protocol.PluginRegisterToolParams{
			Name: "hello_upper", Description: "upper cases text",
			InputSchema: json.RawMessage(helloSchema), Safety: tool.Safe,
		}},
		{protocol.MethodPluginRegisterCommand, protocol.PluginRegisterCommandParams{Name: "hello", Description: "says hello"}},
		{protocol.MethodPluginRegisterHook, protocol.PluginRegisterHookParams{Point: string(HookBeforeTurn), Priority: 50}},
		{protocol.MethodPluginSetStatus, protocol.PluginSetStatusParams{Key: "hello", Content: []Span{{Text: "hello ready", Role: "muted"}}}},
	}
	if c.provider {
		calls = append(calls, struct {
			method string
			params any
		}{protocol.MethodPluginRegisterProvider, protocol.PluginRegisterProviderParams{Name: "hello", Wire: protocol.WireCustom}})
	}
	for _, call := range calls {
		if err := cl.Call(ctx, call.method, call.params, nil); err != nil {
			c.mu.Lock()
			c.regErrs = append(c.regErrs, err)
			c.mu.Unlock()
		}
	}
}

// helloSchema is kept as a literal so a test can assert the bytes reached the registry
// unchanged: a tool's schema is never re-marshaled.
const helloSchema = `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`

func (c *fakeChild) initParams() protocol.PluginInitParams {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.init
}

func (c *fakeChild) firedHooks() []protocol.HookFireParams {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]protocol.HookFireParams(nil), c.hooks...)
}

// serveRegistrar is the test's stand-in for the server's plugin serve loop: it applies the
// plugin's registrations through the same plugin.Register the server's dispatch uses, and
// hands notifications to Deliver.
func serveRegistrar(ctx context.Context, conn protocol.Conn, reg Registrar) error {
	for {
		raw, err := conn.Recv(ctx)
		if err != nil {
			return err
		}
		var req protocol.Request
		if err := json.Unmarshal(raw, &req); err != nil {
			continue
		}
		if req.IsNotification() {
			reg.Deliver(req.Method, req.Params)
			continue
		}
		result, rerr := Register(reg, req.Method, req.Params)
		if rerr != nil {
			_ = conn.Send(ctx, protocol.NewErrorResponse(req.ID, protocol.ErrorFrom(rerr)))
			continue
		}
		resp, err := protocol.NewResponse(req.ID, result)
		if err != nil {
			return err
		}
		if err := conn.Send(ctx, resp); err != nil {
			return err
		}
	}
}

// spawnHarness is one Spawned loaded into a real Registry against a fake child.
type spawnHarness struct {
	reg   *Registry
	sp    *Spawned
	child *fakeChild
	tail  *tail
	exit  chan error // the child's exit status; send to end it
	notes func() []string
}

func loadSpawned(t *testing.T, cfg map[string]any, tune func(*fakeChild)) *spawnHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var mu sync.Mutex
	var notices []string
	reg := NewRegistry(map[string]map[string]any{"hello": cfg}, func(s string) {
		mu.Lock()
		notices = append(notices, s)
		mu.Unlock()
	})
	childEnd, serverEnd := protocol.Pipe()
	child := newFakeChild(t, childEnd)
	if tune != nil {
		tune(child)
	}
	go child.run(ctx)
	t.Cleanup(func() { _ = child.peer.Close() })

	tl := newTail(4096)
	exit := make(chan error, 1)
	h := &spawnHarness{reg: reg, child: child, tail: tl, exit: exit}
	h.notes = func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), notices...)
	}
	start := func(context.Context) (protocol.Conn, *tail, func() error, error) {
		return serverEnd, tl, func() error { return <-exit }, nil
	}
	h.sp = newSpawnedWith(
		Manifest{Name: "hello", Version: "0.1.0", ProtocolVersion: 1, Command: "hello"},
		SpawnServices{
			ServePlugin: func(ctx context.Context, conn protocol.Conn, r Registrar) error {
				return serveRegistrar(ctx, conn, r)
			},
			Version:        "test-version",
			Workspaces:     []string{"/ws"},
			Fail:           reg.Fail,
			CommandTimeout: 300 * time.Millisecond,
		},
		start,
	)
	reg.Load(ctx, h.sp)
	return h
}

func TestSpawnedInitHandshakeAndRegistrations(t *testing.T) {
	h := loadSpawned(t, map[string]any{"greeting": "hi"}, nil)
	if got := h.reg.Statuses(); len(got) != 1 || got[0].State != StateReady {
		t.Fatalf("statuses = %+v (notices %v)", got, h.notes())
	}
	if got := h.reg.Statuses()[0].Origin; got != OriginSpawned {
		t.Fatalf("origin = %q, want %q", got, OriginSpawned)
	}
	h.child.mu.Lock()
	regErrs := append([]error(nil), h.child.regErrs...)
	h.child.mu.Unlock()
	if len(regErrs) != 0 {
		t.Fatalf("the child's registrations came back with %v", regErrs)
	}
	init := h.child.initParams()
	if init.Name != "hello" || init.Version != "test-version" || init.ProtocolVersion != protocol.ProtocolVersion {
		t.Fatalf("init params = %+v", init)
	}
	if init.Config["greeting"] != "hi" {
		t.Fatalf("config = %+v", init.Config)
	}
	if !reflect.DeepEqual(init.WorkspaceRoots, []string{"/ws"}) {
		t.Fatalf("workspace roots = %v", init.WorkspaceRoots)
	}
	tl, ok := h.reg.Tool("hello_upper")
	if !ok {
		t.Fatalf("no hello_upper tool: %v", h.notes())
	}
	if string(tl.Schema) != helloSchema {
		t.Fatalf("schema = %s", tl.Schema)
	}
	if tl.Safety != tool.Safe {
		t.Fatalf("safety = %s", tl.Safety)
	}
	if _, ok := h.reg.Command("hello"); !ok {
		t.Fatal("no hello command")
	}
	if got := h.reg.Hooks(HookBeforeTurn); len(got) != 1 || got[0].Priority != 50 || got[0].Owner != "hello" {
		t.Fatalf("hooks = %+v", got)
	}
	items := h.reg.StatusItems()
	if len(items) != 1 || items[0].Owner != "hello" || items[0].Content[0].Text != "hello ready" {
		t.Fatalf("status = %+v", items)
	}
}

func TestSpawnedInitRefusesProtocolMismatch(t *testing.T) {
	h := loadSpawned(t, nil, func(c *fakeChild) { c.version = 2 })
	st := h.reg.Statuses()
	if len(st) != 1 || st[0].State != StateFailed {
		t.Fatalf("statuses = %+v", st)
	}
	if !strings.Contains(st[0].Reason, "protocol version 2, want 1") {
		t.Fatalf("reason = %q", st[0].Reason)
	}
	if _, ok := h.reg.Tool("hello_upper"); ok {
		t.Fatal("a plugin that failed init left its tool behind")
	}
}

func TestSpawnedToolInvokeMapsResult(t *testing.T) {
	h := loadSpawned(t, nil, nil)
	tl, ok := h.reg.Tool("hello_upper")
	if !ok {
		t.Fatal("no hello_upper tool")
	}
	res, err := tl.Invoke(context.Background(), tool.Call{
		ID: "tu1", Name: "hello_upper", Input: json.RawMessage(`{"text":"hello"}`),
		Workspace: session.Workspace{Root: "/ws"},
	})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "HELLO" || res.IsError {
		t.Fatalf("result = %+v", res)
	}
	h.child.mu.Lock()
	defer h.child.mu.Unlock()
	if len(h.child.invoked) != 1 || h.child.invoked[0].ToolUseID != "tu1" || h.child.invoked[0].Name != "hello_upper" {
		t.Fatalf("invoked = %+v", h.child.invoked)
	}
	if string(h.child.invoked[0].Input) != `{"text":"hello"}` {
		t.Fatalf("input = %s", h.child.invoked[0].Input)
	}
}

func TestSpawnedToolCancelOnContextDone(t *testing.T) {
	h := loadSpawned(t, nil, func(c *fakeChild) { c.slowTool = true })
	tl, ok := h.reg.Tool("hello_upper")
	if !ok {
		t.Fatal("no hello_upper tool")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := tl.Invoke(ctx, tool.Call{ID: "tu-slow", Name: "hello_upper", Input: json.RawMessage(`{"text":"slow"}`)})
		done <- err
	}()
	// Wait until the child has the call before cancelling, so the cancel is a real one.
	waitFor(t, func() bool {
		h.child.mu.Lock()
		defer h.child.mu.Unlock()
		return len(h.child.invoked) == 1
	})
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("invoke returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("invoke never returned")
	}
	select {
	case id := <-h.child.cancelled:
		if id != "tu-slow" {
			t.Fatalf("cancelled %q", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the child never saw tool.cancel")
	}
}

func TestSpawnedHookFireRoundTrip(t *testing.T) {
	h := loadSpawned(t, nil, nil)
	hooks := h.reg.Hooks(HookBeforeTurn)
	if len(hooks) != 1 {
		t.Fatalf("hooks = %+v", hooks)
	}
	msg := session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("go")}}
	payload := &BeforeTurnPayload{
		SessionID: "s1",
		TurnID:    "t1",
		Message:   session.Entry{ID: session.NewID(), At: time.Now(), Kind: msg.Kind(), Payload: msg},
	}
	res, err := hooks[0].Handle(context.Background(), HookCall{
		Point: HookBeforeTurn, SessionID: "s1", TurnID: "t1", Payload: payload,
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	bt, ok := res.(*BeforeTurnResult)
	if !ok {
		t.Fatalf("result %T, want *BeforeTurnResult", res)
	}
	if !reflect.DeepEqual(bt.SystemPromptAdditions, []string{"hello plugin was here"}) {
		t.Fatalf("additions = %v", bt.SystemPromptAdditions)
	}
	fired := h.child.firedHooks()
	if len(fired) != 1 || fired[0].Point != string(HookBeforeTurn) || fired[0].SessionID != "s1" || fired[0].TurnID != "t1" {
		t.Fatalf("fired = %+v", fired)
	}
	var back BeforeTurnPayload
	if err := json.Unmarshal(fired[0].Payload, &back); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if back.SessionID != "s1" {
		t.Fatalf("payload = %+v", back)
	}
}

func TestSpawnedCommandInvokeMapsActions(t *testing.T) {
	h := loadSpawned(t, nil, nil)
	cmd, ok := h.reg.Command("hello")
	if !ok {
		t.Fatal("no hello command")
	}
	act, err := cmd.Run(context.Background(), CommandCall{Args: "prompt"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	sp, ok := act.(SubmitPrompt)
	if !ok || sp.Text != "say hi" {
		t.Fatalf("action = %#v", act)
	}
	act, err = cmd.Run(context.Background(), CommandCall{Args: ""})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	n, ok := act.(Notice)
	if !ok || n.Text != "hello from the plugin" {
		t.Fatalf("action = %#v", act)
	}
}

func TestSpawnedCustomProviderStreamsThenStops(t *testing.T) {
	h := loadSpawned(t, nil, func(c *fakeChild) { c.provider = true })
	ps := h.reg.Providers()
	if len(ps) != 1 || ps[0].Name() != "hello" {
		t.Fatalf("providers = %+v (notices %v)", ps, h.notes())
	}
	var got []provider.Part
	err := ps[0].Complete(context.Background(), provider.Request{
		Model:    session.ModelRef{Provider: "hello", Model: "m1"},
		Messages: []provider.Message{{Role: provider.RoleUser, Content: []session.Block{session.TextBlock("hi")}}},
	}, func(p provider.Part) error {
		got = append(got, p)
		return nil
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("parts = %+v", got)
	}
	if got[0].Type != provider.PartTextDelta || got[0].Text != "one" {
		t.Fatalf("part 0 = %+v", got[0])
	}
	if got[1].Type != provider.PartTextDelta || got[1].Text != "two" {
		t.Fatalf("part 1 = %+v", got[1])
	}
	if got[2].Type != provider.PartUsage || got[2].Usage.Output != 4 {
		t.Fatalf("part 2 = %+v", got[2])
	}
	if got[3].Type != provider.PartStop || got[3].StopReason != session.StopEndTurn {
		t.Fatalf("part 3 = %+v", got[3])
	}
	models, err := ps[0].ListModels(context.Background())
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	if len(models) != 1 || models[0].Ref.Model != "m1" {
		t.Fatalf("models = %+v", models)
	}
}

func TestSpawnedChildExitWithdrawsRegistrations(t *testing.T) {
	h := loadSpawned(t, nil, nil)
	if _, ok := h.reg.Tool("hello_upper"); !ok {
		t.Fatal("no hello_upper tool before the exit")
	}
	_, _ = h.tail.Write([]byte("panic: boom\n"))
	h.exit <- errors.New("exit status 3")
	waitFor(t, func() bool {
		st := h.reg.Statuses()
		return len(st) == 1 && st[0].State == StateFailed
	})
	st := h.reg.Statuses()[0]
	if !strings.Contains(st.Reason, "exited: exit status 3") || !strings.Contains(st.Reason, "panic: boom") {
		t.Fatalf("reason = %q", st.Reason)
	}
	if _, ok := h.reg.Tool("hello_upper"); ok {
		t.Fatal("the dead plugin's tool is still registered")
	}
	if _, ok := h.reg.Command("hello"); ok {
		t.Fatal("the dead plugin's command is still registered")
	}
	if len(h.reg.Hooks(HookBeforeTurn)) != 0 {
		t.Fatal("the dead plugin's hook is still registered")
	}
	notes := h.notes()
	if len(notes) == 0 || !strings.Contains(strings.Join(notes, "\n"), "hello") {
		t.Fatalf("notices = %v", notes)
	}
}

func TestSpawnedRefusesRegistrationAfterInit(t *testing.T) {
	h := loadSpawned(t, nil, nil)
	err := h.sp.RegisterTool("late", "too late", json.RawMessage(`{}`), tool.Safe)
	if err == nil {
		t.Fatal("a registration after init was accepted")
	}
	if !errors.Is(err, session.ErrInvariant) {
		t.Fatalf("err = %v, want a refused_by_invariant error", err)
	}
	// Status and widgets stay live by contract.
	h.sp.SetStatus("hello", []Span{{Text: "still here", Role: "muted"}})
	items := h.reg.StatusItems()
	if len(items) != 1 || items[0].Content[0].Text != "still here" {
		t.Fatalf("status = %+v", items)
	}
	if err := h.sp.SetWidget("w", SlotHeader, []Span{{Text: "w", Role: "muted"}}); err != nil {
		t.Fatalf("set widget: %v", err)
	}
	if got := h.reg.Widgets(); len(got) != 1 {
		t.Fatalf("widgets = %+v", got)
	}
}

// waitFor polls cond until it holds or the test times out. The states it waits on are set
// from the adapter's own goroutines, so there is nothing to synchronize on directly.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition never held")
}

func TestSpawnedCommandTimesOutRatherThanParkingTheCaller(t *testing.T) {
	h := loadSpawned(t, nil, func(c *fakeChild) { c.slowCommand = true })
	cmd, ok := h.reg.Command("hello")
	if !ok {
		t.Fatal("no hello command")
	}
	start := time.Now()
	_, err := cmd.Run(context.Background(), CommandCall{})
	if err == nil {
		t.Fatal("a command the child never answered came back with no error")
	}
	if !strings.Contains(err.Error(), "no answer within") {
		t.Fatalf("err = %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the caller waited %s", took)
	}
	notes := strings.Join(h.notes(), "\n")
	if !strings.Contains(notes, "hello") || !strings.Contains(notes, "no answer within") {
		t.Fatalf("notices = %v", h.notes())
	}
}

func TestSpawnedConnectionLossFailsThePlugin(t *testing.T) {
	h := loadSpawned(t, nil, nil)
	if _, ok := h.reg.Tool("hello_upper"); !ok {
		t.Fatal("no hello_upper tool before the connection went")
	}
	_, _ = h.tail.Write([]byte("still running\n"))
	// The child stops talking without exiting: its end of the connection closes and its
	// process, as far as this side knows, is still there.
	_ = h.child.peer.Close()
	waitFor(t, func() bool {
		st := h.reg.Statuses()
		return len(st) == 1 && st[0].State == StateFailed
	})
	st := h.reg.Statuses()[0]
	if !strings.Contains(st.Reason, "connection lost") || !strings.Contains(st.Reason, "still running") {
		t.Fatalf("reason = %q", st.Reason)
	}
	if _, ok := h.reg.Tool("hello_upper"); ok {
		t.Fatal("the unreachable plugin's tool is still registered")
	}
}

func TestSpawnedProviderFailureCarriesTheProviderClass(t *testing.T) {
	h := loadSpawned(t, nil, func(c *fakeChild) { c.provider = true; c.failComplete = true })
	ps := h.reg.Providers()
	if len(ps) != 1 {
		t.Fatalf("providers = %+v", ps)
	}
	err := ps[0].Complete(context.Background(), provider.Request{
		Model: session.ModelRef{Provider: "hello", Model: "m1"},
	}, func(provider.Part) error { return nil })
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want a *provider.Error", err, err)
	}
	if pe.Class != session.ErrProvider || pe.Attempts != 1 || !strings.Contains(pe.Message, "the model said no") {
		t.Fatalf("provider error = %+v", pe)
	}
}
