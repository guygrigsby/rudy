package subagents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/plugin/plugintest"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

func TestRegistersTheAgentToolNamingTheDefinitions(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	def := "---\ndescription: reads only\n---\nYou explore.\n"
	if err := os.WriteFile(filepath.Join(dir, "agents", "explorer.md"), []byte(def), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &plugintest.Host{Name: "subagents"}
	if err := New(dir).Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if len(h.RegisteredTools) != 1 {
		t.Fatalf("registered %+v", h.RegisteredTools)
	}
	got := h.RegisteredTools[0]
	if got.Name != "agent" || got.Safety != tool.Safe {
		t.Errorf("tool = %s %s", got.Name, got.Safety)
	}
	if !strings.Contains(got.Description, "explorer (reads only)") {
		t.Errorf("description does not name the definitions: %q", got.Description)
	}
	if !json.Valid(got.Schema) {
		t.Errorf("schema is not valid JSON: %s", got.Schema)
	}
}

func TestInvokeRefusesBadInputAndReportsNoServer(t *testing.T) {
	h := &plugintest.Host{Name: "subagents"}
	if err := New(t.TempDir()).Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	invoke := h.RegisteredTools[0].Invoke
	for _, input := range []string{`{}`, `{"agent":"explorer"}`, `{"agent":"","prompt":"go"}`, `{"agent":"explorer","prompt":"  "}`, `not json`} {
		res, err := invoke(context.Background(), tool.Call{ID: "tu1", Input: json.RawMessage(input)})
		if err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		if !res.IsError || !strings.Contains(text(res), "needs agent and prompt") {
			t.Errorf("%s: result %+v", input, res)
		}
	}
	// A host with no server behind it fails the call rather than the turn.
	res, err := invoke(context.Background(), tool.Call{ID: "tu1", Input: json.RawMessage(`{"agent":"explorer","prompt":"look"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(text(res), "no server") {
		t.Errorf("result %+v", res)
	}
}

func text(res tool.Result) string {
	var b strings.Builder
	for _, bl := range res.Content {
		if bl.Type == session.BlockText {
			b.WriteString(bl.Text)
		}
	}
	return b.String()
}

// childServer is the smallest server the agent tool can talk to: it answers the four calls
// the tool makes and lets a test drive the child session's notifications by hand.
type childServer struct {
	t           *testing.T
	conn        protocol.Conn
	childID     ulid.ULID
	turnID      ulid.ULID
	onSubmit    func(cs *childServer)
	onInterrupt func(cs *childServer)
	interrupted chan struct{}

	// openParams is the session.open request the tool sent to open this child, captured for
	// tests that check what the tool asked for rather than what it got back.
	openParams protocol.SessionOpenParams
}

func newChildServer(t *testing.T) (*childServer, protocol.Conn) {
	t.Helper()
	clientEnd, serverEnd := protocol.Pipe()
	cs := &childServer{
		t: t, conn: serverEnd, childID: session.NewID(), turnID: session.NewID(),
		interrupted: make(chan struct{}, 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go cs.serve(ctx)
	return cs, clientEnd
}

func (cs *childServer) serve(ctx context.Context) {
	for {
		raw, err := cs.conn.Recv(ctx)
		if err != nil {
			return
		}
		var req protocol.Request
		if err := json.Unmarshal(raw, &req); err != nil {
			return
		}
		var result any = struct{}{}
		switch req.Method {
		case protocol.MethodSessionOpen:
			_ = json.Unmarshal(req.Params, &cs.openParams)
			result = protocol.SessionInfo{SessionID: cs.childID.String()}
		case protocol.MethodSessionSubmit:
			result = protocol.SessionSubmitResult{TurnID: cs.turnID.String()}
		}
		resp, err := protocol.NewResponse(req.ID, result)
		if err != nil {
			return
		}
		if err := cs.conn.Send(ctx, resp); err != nil {
			return
		}
		switch req.Method {
		case protocol.MethodSessionSubmit:
			if cs.onSubmit != nil {
				cs.onSubmit(cs)
			}
		case protocol.MethodSessionInterrupt:
			select {
			case cs.interrupted <- struct{}{}:
			default:
			}
			if cs.onInterrupt != nil {
				cs.onInterrupt(cs)
			}
		}
	}
}

// notify sends one notification to the tool.
func (cs *childServer) notify(method string, params any) {
	cs.t.Helper()
	n, err := protocol.NewNotification(method, params)
	if err != nil {
		cs.t.Error(err)
		return
	}
	if err := cs.conn.Send(context.Background(), n); err != nil {
		cs.t.Error(err)
	}
}

// say sends an assistant message of this turn, and state sends a turn.state for it.
func (cs *childServer) say(text string) {
	e := session.Entry{
		ID: session.NewID(), At: time.Now(), Kind: session.KindAssistantMessage,
		Payload: session.AssistantMessage{
			Model: session.ModelRef{Provider: "fake", Model: "m1"}, Thinking: session.ThinkingOff,
			Content: []session.Block{session.TextBlock(text)}, StopReason: session.StopEndTurn,
		},
	}
	cs.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: cs.childID.String(), Entry: e})
}

func (cs *childServer) state(st string) {
	cs.notify(protocol.NotifyTurnState, protocol.TurnStateChanged{
		SessionID: cs.childID.String(), TurnID: cs.turnID.String(), State: st,
	})
}

// pipeHost is a plugintest.Host whose Connect reaches a childServer.
type pipeHost struct {
	*plugintest.Host
	conn protocol.Conn
}

func (h *pipeHost) Connect(context.Context) (*protocol.Client, error) {
	return protocol.NewClient(h.conn), nil
}

func agentTool(t *testing.T, conn protocol.Conn) func(context.Context, tool.Call) (tool.Result, error) {
	t.Helper()
	h := &pipeHost{Host: &plugintest.Host{Name: "subagents"}, conn: conn}
	if err := New(t.TempDir()).Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	return h.RegisteredTools[0].Invoke
}

// TestAgentToolPassesItsNarrowing is ADR 0028 decision 5's caller term reaching session.open:
// the orchestrating model names tools on the agent tool's own input, and the tool carries it
// through to the child's session.open request unchanged, rather than deciding anything about it
// itself. The server, not the tool, is what turns that into a narrowing.
func TestAgentToolPassesItsNarrowing(t *testing.T) {
	cs, clientEnd := newChildServer(t)
	cs.onSubmit = func(cs *childServer) {
		cs.say("done")
		cs.state("completed")
	}
	res, err := agentTool(t, clientEnd)(context.Background(), tool.Call{
		ID: "tu1", Input: json.RawMessage(`{"agent":"explorer","prompt":"look","tools":["read","grep"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("result %+v %q", res, text(res))
	}
	if !slices.Equal(cs.openParams.Tools, []string{"read", "grep"}) {
		t.Errorf("session.open did not carry the tools narrowing: %+v", cs.openParams.Tools)
	}
}

func TestInterruptedChildIsNotAnAnswer(t *testing.T) {
	cs, clientEnd := newChildServer(t)
	cs.onSubmit = func(cs *childServer) {
		cs.say("half an answer")
		cs.state("idle")
	}
	res, err := agentTool(t, clientEnd)(context.Background(), tool.Call{
		ID: "tu1", Input: json.RawMessage(`{"agent":"explorer","prompt":"look"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || text(res) != "agent explorer interrupted\nhalf an answer" {
		t.Errorf("result %+v %q", res, text(res))
	}
}

func TestInterruptedChildWithNothingSaid(t *testing.T) {
	cs, clientEnd := newChildServer(t)
	cs.onSubmit = func(cs *childServer) { cs.state("idle") }
	res, err := agentTool(t, clientEnd)(context.Background(), tool.Call{
		ID: "tu1", Input: json.RawMessage(`{"agent":"explorer","prompt":"look"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || text(res) != "agent explorer interrupted" {
		t.Errorf("result %+v %q", res, text(res))
	}
}

// TestCancelInterruptsTheChildAndWaitsForIt is the orphan case: a cancelled tool call must
// interrupt the child and then wait for its turn to actually rest, since the interrupt is
// asynchronous and a child still streaming would otherwise be left running for a tool call
// nobody is waiting on.
func TestCancelInterruptsTheChildAndWaitsForIt(t *testing.T) {
	cs, clientEnd := newChildServer(t)
	submitted := make(chan struct{})
	release := make(chan struct{})
	// The child streams on, saying nothing. The non-terminal state is what makes the cancel
	// below deterministic: a pipe Send returns only once the client's reader has taken the
	// message, and that reader handled the submit response, in order, before this one.
	cs.onSubmit = func(cs *childServer) {
		cs.state("streaming")
		close(submitted)
	}
	// Off the serve loop, so the server can still answer the calls the tool makes while it
	// waits: a handler blocked here would look exactly like a tool that is still waiting.
	cs.onInterrupt = func(cs *childServer) {
		go func() {
			<-release
			cs.state("idle")
		}()
	}
	invoke := agentTool(t, clientEnd)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan tool.Result, 1)
	go func() {
		res, _ := invoke(ctx, tool.Call{ID: "tu1", Input: json.RawMessage(`{"agent":"explorer","prompt":"look"}`)})
		done <- res
	}()
	<-submitted
	cancel()
	select {
	case <-cs.interrupted:
	case res := <-done:
		t.Fatalf("returned instead of interrupting: %+v %q", res, text(res))
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled tool call never interrupted the child")
	}
	// The child has not come to rest yet, so the tool call must still be waiting on it.
	time.Sleep(50 * time.Millisecond)
	select {
	case res := <-done:
		t.Fatalf("returned before the child rested: %+v", res)
	default:
	}
	close(release)
	select {
	case res := <-done:
		if !res.IsError || !strings.Contains(text(res), "context canceled") {
			t.Errorf("result %+v %q", res, text(res))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the tool call never returned after the child rested")
	}
}
