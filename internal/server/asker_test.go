package server_test

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/server"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// resumeOn attaches cl to sid and returns the info the server replied with.
func resumeOn(t *testing.T, cl *protocol.Client, sid string) protocol.SessionInfo {
	t.Helper()
	var info protocol.SessionInfo
	if err := cl.Call(context.Background(), protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: sid}, &info); err != nil {
		t.Fatalf("resume: %v", err)
	}
	return info
}

// question drains cl until a permission question arrives, returning it with every
// notification up to and including it.
func question(t *testing.T, cl *protocol.Client) (protocol.PermissionRequested, []protocol.Notification) {
	t.Helper()
	var pr protocol.PermissionRequested
	ns := drain(t, cl, func(n protocol.Notification) bool {
		if n.Method != protocol.NotifyPermissionRequested {
			return false
		}
		if err := json.Unmarshal(n.Params, &pr); err != nil {
			t.Fatalf("permission.requested params: %v", err)
		}
		return true
	})
	return pr, ns
}

// answerWith sends one session.answer and returns whatever the server made of it.
func answerWith(cl *protocol.Client, sid, toolUseID string, d session.Decision) error {
	return cl.Call(context.Background(), protocol.MethodSessionAnswer, protocol.SessionAnswerParams{
		SessionID: sid, ToolUseID: toolUseID, Decision: d, Scope: session.ScopeOnce, Reason: "test",
	}, &struct{}{})
}

// slowPlugin registers a second unsafe tool whose invocation blocks until release is closed,
// so a test can hold a turn open past the answer that allowed it: the answered set lives only
// until the turn rests, and a second answer arriving after that would be not_found rather than
// the conflict the contract promises.
type slowPlugin struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func newSlowPlugin() *slowPlugin {
	return &slowPlugin{started: make(chan struct{}), release: make(chan struct{})}
}

func (p *slowPlugin) Name() string { return "slow" }

func (p *slowPlugin) Init(_ context.Context, h plugin.Host) error {
	return h.RegisterTool(tool.Tool{
		Name:        "slow",
		Description: "an unsafe fake tool that blocks",
		Schema:      json.RawMessage(`{"type":"object"}`),
		Safety:      tool.Unsafe,
		Invoke: func(ctx context.Context, _ tool.Call) (tool.Result, error) {
			p.once.Do(func() { close(p.started) })
			select {
			case <-p.release:
			case <-ctx.Done():
				return tool.Result{}, ctx.Err()
			}
			return tool.Result{Content: []session.Block{session.TextBlock("ran")}}, nil
		},
	})
}

// rawClient is a connection whose messages the test reads itself, in the exact order the
// server sent them. protocol.Client routes responses and notifications through separate
// queues on purpose, so it cannot answer "did the state arrive before the response", which is
// the whole of what the two attach tests below assert.
type rawClient struct {
	t    *testing.T
	conn protocol.Conn
	next int64
}

type rawMsg struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *protocol.Error `json:"error"`
}

func rawDialAs(t *testing.T, srv *server.Server, asker bool) *rawClient {
	t.Helper()
	cc := dialConn(t, srv)
	t.Cleanup(func() { _ = cc.Close() })
	r := &rawClient{t: t, conn: cc}
	if _, before := r.call(protocol.MethodClientHello, protocol.ClientHelloParams{Client: "raw", Version: "0", Asker: asker}); len(sessionNotes(before)) > 0 {
		t.Fatalf("hello brought session notifications: %v", rawMethods(before))
	}
	return r
}

// call sends one request and reads until its response, returning that response's result and
// every notification that arrived before it, in order.
func (r *rawClient) call(method string, params any) (json.RawMessage, []rawMsg) {
	r.t.Helper()
	r.next++
	req, err := protocol.NewRequest(r.next, method, params)
	if err != nil {
		r.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.conn.Send(ctx, req); err != nil {
		r.t.Fatalf("send %s: %v", method, err)
	}
	var before []rawMsg
	for {
		raw, err := r.conn.Recv(ctx)
		if err != nil {
			r.t.Fatalf("recv after %s: %v (%v)", method, err, rawMethods(before))
		}
		var m rawMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			r.t.Fatalf("decode: %v", err)
		}
		if m.Method != "" && len(m.ID) == 0 {
			before = append(before, m)
			continue
		}
		if m.Error != nil {
			r.t.Fatalf("%s: %v", method, m.Error)
		}
		return m.Result, before
	}
}

// sessionNotes keeps only the notifications this session sent. Every connection also gets
// status.updated and plugin.state from the plugin registry, on the registry's own schedule,
// and those say nothing about the order an attach is owed.
func sessionNotes(ms []rawMsg) []rawMsg {
	var out []rawMsg
	for _, m := range ms {
		switch m.Method {
		case protocol.NotifyEntryAppended, protocol.NotifyTurnState,
			protocol.NotifyPermissionRequested, protocol.NotifyStreamDelta, protocol.NotifyToolState:
			out = append(out, m)
		}
	}
	return out
}

func rawMethods(ms []rawMsg) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Method)
	}
	return out
}

func rawParams[T any](t *testing.T, m rawMsg) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(m.Params, &v); err != nil {
		t.Fatalf("%s params: %v", m.Method, err)
	}
	return v
}

// TestEveryAskerHearsTheQuestion: a question is put to every attached asker, not just the
// first one to subscribe.
func TestEveryAskerHearsTheQuestion(t *testing.T) {
	h := newHarness(t, &scriptProvider{})
	cl := h.dial(t, true)
	info := h.open(t, cl)
	second := h.dial(t, true)
	resumeOn(t, second, info.SessionID)

	submit(t, cl, info.SessionID, "go")
	first, _ := question(t, cl)
	also, _ := question(t, second)
	if first.ToolUseID != also.ToolUseID || first.Tool != also.Tool {
		t.Fatalf("second asker got %+v, want the same question as %+v", also, first)
	}
	if first.Tool != "danger" || first.TurnID == "" {
		t.Fatalf("question = %+v", first)
	}

	if err := answerWith(cl, info.SessionID, first.ToolUseID, session.Allow); err != nil {
		t.Fatalf("answer: %v", err)
	}
	drain(t, cl, completedOn(info.SessionID))
}

// TestTheFirstAnswerDecides: the second answer for the same tool_use is a conflict and the log
// carries exactly one permission_decision. The tool blocks so the turn is still running when
// the second answer lands: the answered set is what makes this a conflict rather than the
// not_found a question nobody ever asked would get.
func TestTheFirstAnswerDecides(t *testing.T) {
	sp := newSlowPlugin()
	h := newHarnessWith(t, &scriptProvider{toolName: "slow"}, sp)
	cl := h.dial(t, true)
	info := h.open(t, cl)
	second := h.dial(t, true)
	resumeOn(t, second, info.SessionID)

	submit(t, cl, info.SessionID, "go")
	pr, _ := question(t, cl)
	_, ns := question(t, second)
	if err := answerWith(cl, info.SessionID, pr.ToolUseID, session.Allow); err != nil {
		t.Fatalf("first answer: %v", err)
	}
	select {
	case <-sp.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the allowed tool never ran")
	}
	err := answerWith(second, info.SessionID, pr.ToolUseID, session.Deny)
	if err == nil {
		t.Fatal("the second answer was accepted")
	}
	if got := code(t, err); got != protocol.CodeConflict {
		t.Fatalf("second answer code = %d, want %d (conflict)", got, protocol.CodeConflict)
	}

	close(sp.release)
	ns = append(ns, drain(t, second, completedOn(info.SessionID))...)
	var decisions []session.PermissionDecision
	for _, e := range entries(t, ns) {
		if pd, ok := e.Payload.(session.PermissionDecision); ok {
			decisions = append(decisions, pd)
		}
	}
	if len(decisions) != 1 {
		t.Fatalf("permission decisions = %d, want 1: %+v", len(decisions), decisions)
	}
	if decisions[0].Decision != session.Allow || decisions[0].DecidedBy != session.ByAsker {
		t.Fatalf("decision = %+v, want the first answer's allow", decisions[0])
	}

	// The answered set is the turn's, not the session's: once the turn has rested, the same
	// tool_use id is a question nobody is asking rather than one already decided.
	err = answerWith(second, info.SessionID, pr.ToolUseID, session.Deny)
	if got := code(t, err); got != protocol.CodeNotFound {
		t.Fatalf("answer after the turn rested = %d, want %d (not_found)", got, protocol.CodeNotFound)
	}
}

// TestAnAskerAttachingMidQuestionHearsIt pins the order the contract gives a newcomer: every
// entry, then the current turn.state, then a tool.state for the call in flight, then the
// standing question, then the resume response.
func TestAnAskerAttachingMidQuestionHearsIt(t *testing.T) {
	h := newHarness(t, &scriptProvider{})
	cl := h.dial(t, true)
	info := h.open(t, cl)
	sub := submit(t, cl, info.SessionID, "go")
	pr, _ := question(t, cl)

	raw := rawDialAs(t, h.srv, true)
	res, raws := raw.call(protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: info.SessionID})
	before := sessionNotes(raws)
	want := []string{
		protocol.NotifyEntryAppended, protocol.NotifyEntryAppended, protocol.NotifyEntryAppended,
		protocol.NotifyTurnState, protocol.NotifyToolState, protocol.NotifyPermissionRequested,
	}
	if got := rawMethods(before); !slices.Equal(got, want) {
		t.Fatalf("attach order = %v, want %v", got, want)
	}
	ts := rawParams[protocol.TurnStateChanged](t, before[3])
	if ts.State != "awaiting_permission" || ts.TurnID != sub.TurnID || ts.SessionID != info.SessionID {
		t.Fatalf("turn.state = %+v, want awaiting_permission on turn %s", ts, sub.TurnID)
	}
	tsc := rawParams[protocol.ToolStateChanged](t, before[4])
	if tsc.ToolUseID != pr.ToolUseID || tsc.State != protocol.ToolStateAwaitingPermission || tsc.SessionID != info.SessionID {
		t.Fatalf("tool.state = %+v, want %s awaiting_permission", tsc, pr.ToolUseID)
	}
	standing := rawParams[protocol.PermissionRequested](t, before[5])
	if standing.ToolUseID != pr.ToolUseID || standing.Tool != pr.Tool || standing.TurnID != pr.TurnID {
		t.Fatalf("standing question = %+v, want %+v", standing, pr)
	}
	var attached protocol.SessionInfo
	if err := json.Unmarshal(res, &attached); err != nil {
		t.Fatal(err)
	}
	if attached.SessionID != info.SessionID {
		t.Fatalf("resumed %s, want %s", attached.SessionID, info.SessionID)
	}

	// A headless client attaching to the same standing question is told the state and the
	// call in flight, but not the question itself: that goes only to a connection that can
	// answer it.
	headless := rawDialAs(t, h.srv, false)
	_, raws = headless.call(protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: info.SessionID})
	quiet := []string{
		protocol.NotifyEntryAppended, protocol.NotifyEntryAppended, protocol.NotifyEntryAppended,
		protocol.NotifyTurnState, protocol.NotifyToolState,
	}
	if got := rawMethods(sessionNotes(raws)); !slices.Equal(got, quiet) {
		t.Fatalf("headless attach order = %v, want %v", got, quiet)
	}

	if err := answerWith(cl, info.SessionID, pr.ToolUseID, session.Deny); err != nil {
		t.Fatalf("answer: %v", err)
	}
	drain(t, cl, completedOn(info.SessionID))
}

// TestTheLastAskerLeavingDeniesTheQuestion: the only asker's client goes away while a question
// stands, so the question is denied by no_asker and the turn runs on to completion in front of
// the headless client that stayed.
func TestTheLastAskerLeavingDeniesTheQuestion(t *testing.T) {
	h := newHarness(t, &scriptProvider{})
	cl := h.dial(t, true)
	info := h.open(t, cl)
	watcher := h.dial(t, false)
	resumeOn(t, watcher, info.SessionID)

	submit(t, cl, info.SessionID, "go")
	question(t, cl)
	// Closing the client ends its serve loop, which detaches it: the only asker is gone
	// while the question stands.
	if err := cl.Close(); err != nil {
		t.Fatalf("close the asker: %v", err)
	}

	ns := drain(t, watcher, completedOn(info.SessionID))
	es := entries(t, ns)
	want := []session.Kind{
		session.KindSessionOpened, session.KindUserMessage, session.KindAssistantMessage,
		session.KindPermissionDecision, session.KindToolResult, session.KindAssistantMessage,
	}
	if !equalKinds(kinds(es), want) {
		t.Fatalf("kinds = %v, want %v", kinds(es), want)
	}
	pd := es[3].Payload.(session.PermissionDecision)
	if pd.Decision != session.Deny || pd.DecidedBy != session.ByNoAsker {
		t.Fatalf("decision = %+v, want a no_asker deny", pd)
	}
	if pd.Reason != "no asker attached" {
		t.Fatalf("reason = %q, want the fixed no_asker string", pd.Reason)
	}
	if tr := es[4].Payload.(session.ToolResult); tr.Outcome != session.OutcomeError {
		t.Fatalf("tool result = %+v, want an error", tr)
	}
	if got := atomic.LoadInt32(&h.fp.toolCalls); got != 0 {
		t.Fatalf("tool ran %d times without permission", got)
	}
}

// TestAttachMidTurnHearsTheState: a client attaching while a turn streams is told the state
// after the replay and before its resume response, whether or not it is an asker.
func TestAttachMidTurnHearsTheState(t *testing.T) {
	prov := &scriptProvider{block: make(chan struct{}), textOnly: true}
	h := newHarness(t, prov)
	cl := h.dial(t, true)
	info := h.open(t, cl)
	sub := submit(t, cl, info.SessionID, "go")
	drain(t, cl, func(n protocol.Notification) bool { return n.Method == protocol.NotifyStreamDelta })

	raw := rawDialAs(t, h.srv, false)
	res, raws := raw.call(protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: info.SessionID})
	before := sessionNotes(raws)
	want := []string{protocol.NotifyEntryAppended, protocol.NotifyEntryAppended, protocol.NotifyTurnState}
	if got := rawMethods(before); !slices.Equal(got, want) {
		t.Fatalf("attach order = %v, want %v", got, want)
	}
	ts := rawParams[protocol.TurnStateChanged](t, before[2])
	if ts.State != "streaming" || ts.TurnID != sub.TurnID {
		t.Fatalf("turn.state = %+v, want streaming on turn %s", ts, sub.TurnID)
	}
	var attached protocol.SessionInfo
	if err := json.Unmarshal(res, &attached); err != nil {
		t.Fatal(err)
	}
	if attached.SessionID != info.SessionID {
		t.Fatalf("resumed %s, want %s", attached.SessionID, info.SessionID)
	}

	close(prov.block)
	drain(t, cl, completedOn(info.SessionID))
}

// inFlightTool is a safe tool whose Invoke blocks until release closes.
// TestAClientAttachingMidTurnSeesCallsInFlight uses two calls of it (tu_a, tu_b) to hold the
// turn's RunningTool state open while a third, unsafe call is awaiting permission, so a client
// attaching mid-turn has more than one call to tell apart.
type inFlightTool struct{ release chan struct{} }

func (b *inFlightTool) invoke(ctx context.Context, _ tool.Call) (tool.Result, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
		return tool.Result{}, ctx.Err()
	}
	return tool.Result{Content: []session.Block{session.TextBlock("ran")}}, nil
}

// inFlightProvider answers its first call with one assistant message naming four tool calls at
// once (an instant safe one, two slow safe ones and one unsafe one) and every call after with
// plain text, ending the turn. TestAClientAttachingMidTurnSeesCallsInFlight is the only user.
type inFlightProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *inFlightProvider) Name() string { return "fake" }

func (p *inFlightProvider) ListModels(context.Context) ([]provider.Model, error) {
	return []provider.Model{{
		Ref: session.ModelRef{Provider: "fake", Model: "m1"}, DisplayName: "Fake 1",
		ContextWindow: 100000, Capabilities: provider.Capabilities{Tools: true},
	}}, nil
}

func (p *inFlightProvider) Complete(ctx context.Context, req provider.Request, emit func(provider.Part) error) error {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	var parts []provider.Part
	if n == 1 {
		parts = append(parts, provider.Part{Type: provider.PartTextDelta, Text: "go"})
		parts = append(parts, inFlightToolCall("tu_quick", "quick")...)
		parts = append(parts, inFlightToolCall("tu_a", "slow")...)
		parts = append(parts, inFlightToolCall("tu_b", "slow")...)
		parts = append(parts, inFlightToolCall("tu_danger", "danger")...)
		parts = append(parts,
			provider.Part{Type: provider.PartUsage, Usage: session.Usage{Input: 5, Output: 5}},
			provider.Part{Type: provider.PartStop, StopReason: session.StopToolUse, StopReasonRaw: "tool_calls"},
		)
	} else {
		parts = []provider.Part{
			{Type: provider.PartTextDelta, Text: "done"},
			{Type: provider.PartUsage, Usage: session.Usage{Input: 5, Output: 5}},
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

func inFlightToolCall(id, name string) []provider.Part {
	return []provider.Part{
		{Type: provider.PartToolUseStart, ID: id, Name: name},
		{Type: provider.PartToolUseDelta, ID: id, Text: `{}`},
		{Type: provider.PartToolUseEnd, ID: id},
	}
}

// inFlightPlugin registers inFlightProvider and its three tools: quick returns immediately,
// slow blocks on blk.release, and danger is unsafe so strict mode (testConfig's default) asks
// for it.
type inFlightPlugin struct {
	prov *inFlightProvider
	blk  *inFlightTool
}

func (p *inFlightPlugin) Name() string { return "inflight" }

func (p *inFlightPlugin) Init(_ context.Context, h plugin.Host) error {
	if err := h.RegisterProvider(p.prov); err != nil {
		return err
	}
	if err := h.RegisterTool(tool.Tool{
		Name: "quick", Description: "returns immediately", Schema: json.RawMessage(`{"type":"object"}`), Safety: tool.Safe,
		Invoke: func(context.Context, tool.Call) (tool.Result, error) {
			return tool.Result{Content: []session.Block{session.TextBlock("quick done")}}, nil
		},
	}); err != nil {
		return err
	}
	if err := h.RegisterTool(tool.Tool{
		Name: "slow", Description: "blocks until released", Schema: json.RawMessage(`{"type":"object"}`), Safety: tool.Safe,
		Invoke: p.blk.invoke,
	}); err != nil {
		return err
	}
	return h.RegisterTool(tool.Tool{
		Name: "danger", Description: "an unsafe tool that asks", Schema: json.RawMessage(`{"type":"object"}`), Safety: tool.Unsafe,
		Invoke: func(context.Context, tool.Call) (tool.Result, error) {
			return tool.Result{Content: []session.Block{session.TextBlock("danger done")}}, nil
		},
	})
}

// TestAClientAttachingMidTurnSeesCallsInFlight is the tool.state contracts row's "every call in
// flight is sent to a client attaching mid-turn": two calls running and one awaiting permission,
// and a call that already finished must not be among what a newcomer is told, since that one is
// its own tool_result entry in the replay already delivered.
func TestAClientAttachingMidTurnSeesCallsInFlight(t *testing.T) {
	blk := &inFlightTool{release: make(chan struct{})}
	prov := &inFlightProvider{}
	// newHarnessWith is not used here: it wraps a scriptProvider into its own fakePlugin,
	// which registers a "danger" tool of its own tied to that provider. This test needs its
	// own plugin and provider instead, so it builds the server directly.
	srv, _ := newServerWith(t, testConfig(), &inFlightPlugin{prov: prov, blk: blk})
	cl := dialAs(t, srv, true)
	var info protocol.SessionInfo
	if err := cl.Call(context.Background(), protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: t.TempDir()}, &info); err != nil {
		t.Fatalf("open: %v", err)
	}
	submit(t, cl, info.SessionID, "go")

	// Wait for every precondition the attach assertion below depends on: both slow calls
	// reported running, the quick call reported done, and the danger call's question stood.
	// tool.state(awaiting_permission) always precedes permission.requested for the same call
	// (runTool emits the former, then asks - see runner.go), so seeing the question is also
	// proof the state map already has tu_danger recorded.
	seen := map[string]bool{}
	var pr protocol.PermissionRequested
	drain(t, cl, func(n protocol.Notification) bool {
		switch n.Method {
		case protocol.NotifyToolState:
			var ts protocol.ToolStateChanged
			_ = json.Unmarshal(n.Params, &ts)
			switch {
			case ts.ToolUseID == "tu_a" && ts.State == protocol.ToolStateRunning:
				seen["a"] = true
			case ts.ToolUseID == "tu_b" && ts.State == protocol.ToolStateRunning:
				seen["b"] = true
			case ts.ToolUseID == "tu_quick" && ts.State == protocol.ToolStateDone:
				seen["quick"] = true
			}
		case protocol.NotifyPermissionRequested:
			_ = json.Unmarshal(n.Params, &pr)
			seen["asked"] = true
		}
		return seen["a"] && seen["b"] && seen["quick"] && seen["asked"]
	})

	raw := rawDialAs(t, srv, false)
	_, raws := raw.call(protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: info.SessionID})
	states := map[string]string{}
	for _, m := range sessionNotes(raws) {
		if m.Method != protocol.NotifyToolState {
			continue
		}
		ts := rawParams[protocol.ToolStateChanged](t, m)
		states[ts.ToolUseID] = ts.State
	}
	want := map[string]string{
		"tu_a":      protocol.ToolStateRunning,
		"tu_b":      protocol.ToolStateRunning,
		"tu_danger": protocol.ToolStateAwaitingPermission,
	}
	for id, state := range want {
		if states[id] != state {
			t.Fatalf("tool.state[%s] = %q, want %q (all attach states: %v)", id, states[id], state, states)
		}
	}
	if _, ok := states["tu_quick"]; ok {
		t.Fatalf("a finished call was replayed as tool.state on attach: %v", states)
	}

	if err := answerWith(cl, info.SessionID, pr.ToolUseID, session.Allow); err != nil {
		t.Fatalf("answer: %v", err)
	}
	close(blk.release)
	drain(t, cl, completedOn(info.SessionID))
}
