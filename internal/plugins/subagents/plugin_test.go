// SPDX-License-Identifier: AGPL-3.0-or-later

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

	"github.com/guygrigsby/rudy/internal/agentdef"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugin/plugintest"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// writeDef writes a minimal agents/<name>.md under dir, creating it if needed.
func writeDef(t *testing.T, dir, name, description string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "---\ndescription: " + description + "\n---\nYou do the thing.\n"
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestTheRosterComesFromTheSessionNotTheProcess is ADR 0028 decision 8: the description built
// once in Init from configDir/agents alone can never see a workspace's own .rudy/agents nor a
// plugin's RegisterAgent call, since plugins after subagents in wire.go commit after this
// plugin's Init already ran. The roster has to come from a session_opened hook instead, fired
// once the workspace is known and every plugin has registered.
func TestTheRosterComesFromTheSessionNotTheProcess(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, filepath.Join(dir, "agents"), "explorer", "reads the codebase")
	ws := t.TempDir()
	writeDef(t, filepath.Join(ws, ".rudy", "agents"), "reviewer", "reviews a diff")

	h := &plugintest.Host{Name: "subagents", Agents: []agentdef.Definition{
		{Name: "migrator", Description: "writes migrations"},
	}}
	if err := New(dir).Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if len(h.Hooks) != 1 || h.Hooks[0].Point != plugin.HookSessionOpened {
		t.Fatalf("hooks %+v", h.Hooks)
	}

	res, err := h.Hooks[0].Handle(context.Background(), plugin.HookCall{
		Point:   plugin.HookSessionOpened,
		Payload: &plugin.SessionOpenedPayload{Workspace: session.Workspace{Root: ws}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sor, ok := res.(*plugin.SessionOpenedResult)
	if !ok {
		t.Fatalf("result type %T", res)
	}
	for _, want := range []string{"explorer", "reviewer", "migrator"} {
		if !strings.Contains(sor.Context, want) {
			t.Fatalf("the roster omits %q: %q", want, sor.Context)
		}
	}

	if strings.Contains(h.RegisteredTools[0].Description, "explorer") {
		t.Fatal("the description still enumerates agents, so it is stale the moment a workspace differs")
	}
}

// TestARootDefinitionBeatsAWorkspaceOneAndAWorkspaceOneBeatsAPlugin checks the precedence
// resolveAgent uses (server.go's resolveAgent): the user's config directory first, then the
// workspace's .rudy/agents, then whatever plugins registered, first name winning.
func TestARootDefinitionBeatsAWorkspaceOneAndAWorkspaceOneBeatsAPlugin(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, filepath.Join(dir, "agents"), "shared", "the operator's own")
	ws := t.TempDir()
	writeDef(t, filepath.Join(ws, ".rudy", "agents"), "shared", "the workspace's own")

	h := &plugintest.Host{Name: "subagents", Agents: []agentdef.Definition{
		{Name: "shared", Description: "the plugin's own"},
	}}
	if err := New(dir).Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	res, err := h.Hooks[0].Handle(context.Background(), plugin.HookCall{
		Point:   plugin.HookSessionOpened,
		Payload: &plugin.SessionOpenedPayload{Workspace: session.Workspace{Root: ws}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sor := res.(*plugin.SessionOpenedResult)
	if !strings.Contains(sor.Context, "the operator's own") {
		t.Fatalf("the operator's config definition did not win: %q", sor.Context)
	}
	if strings.Contains(sor.Context, "the workspace's own") || strings.Contains(sor.Context, "the plugin's own") {
		t.Fatalf("a lower-precedence definition of the same name leaked through: %q", sor.Context)
	}
}

// TestRosterNoticesAFileThatFailsToParse: a definition that does not parse must surface as a
// notice, not silence. A shipped example that failed to parse went unnoticed earlier in this
// wave because nothing reported agentdef.Load's errs (rudy-review round 1 on task 5).
func TestRosterNoticesAFileThatFailsToParse(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// No opening fence at all: frontmatter.Split refuses it, so Load reports it in errs and
	// skips the file rather than resolving a definition from it.
	if err := os.WriteFile(filepath.Join(agentsDir, "broken.md"), []byte("description: x\nbody"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &plugintest.Host{Name: "subagents"}
	if err := New(dir).Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Hooks[0].Handle(context.Background(), plugin.HookCall{
		Point:   plugin.HookSessionOpened,
		Payload: &plugin.SessionOpenedPayload{Workspace: session.Workspace{Root: t.TempDir()}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(h.Notices) != 1 {
		t.Fatalf("notices %v", h.Notices)
	}
}

// TestRosterEmptyReturnsNoResult mirrors the skills plugin: no definitions at all means the
// hook has nothing to add, so the runner should drop it rather than injecting an empty line.
func TestRosterEmptyReturnsNoResult(t *testing.T) {
	h := &plugintest.Host{Name: "subagents"}
	if err := New(t.TempDir()).Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	res, err := h.Hooks[0].Handle(context.Background(), plugin.HookCall{
		Point:   plugin.HookSessionOpened,
		Payload: &plugin.SessionOpenedPayload{Workspace: session.Workspace{Root: t.TempDir()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res != nil {
		t.Fatalf("result = %+v, want nil", res)
	}
}

// TestTheRosterIsNotOfferedToASubagent: a child session never holds the agent tool, since
// session.open strips it (ADR 0028, the depth cap), so listing the agents it could delegate to
// would invite a turn that ends in an unknown-tool result. The same hook fires for every child.
func TestTheRosterIsNotOfferedToASubagent(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, filepath.Join(dir, "agents"), "explorer", "reads the codebase")
	h := &plugintest.Host{Name: "subagents"}
	if err := New(dir).Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	ws := session.Workspace{Root: t.TempDir()}

	root, err := h.Hooks[0].Handle(context.Background(), plugin.HookCall{
		Point:   plugin.HookSessionOpened,
		Payload: &plugin.SessionOpenedPayload{Workspace: ws},
	})
	if err != nil {
		t.Fatal(err)
	}
	if root == nil {
		t.Fatal("setup: a root session with a definition on disk was given no roster")
	}

	child, err := h.Hooks[0].Handle(context.Background(), plugin.HookCall{
		Point: plugin.HookSessionOpened,
		Payload: &plugin.SessionOpenedPayload{
			Workspace: ws, ParentSessionID: session.NewID().String(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if child != nil {
		t.Fatalf("a subagent was told it can delegate: %+v", child)
	}
}

// TestRegistersTheAgentToolWithAStaticDescription is ADR 0028 decision 8: the description
// cannot enumerate agents, since a registration cannot be revised once a workspace or a later
// plugin adds one, so it must not name any and must instead point at the system prompt, where
// the session_opened hook puts the roster. It also has to describe tools truthfully: the field
// narrows, never grants.
func TestRegistersTheAgentToolWithAStaticDescription(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, filepath.Join(dir, "agents"), "explorer", "reads only")
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
	if strings.Contains(got.Description, "explorer") {
		t.Errorf("description enumerates a definition, so it is stale the moment a workspace or plugin differs: %q", got.Description)
	}
	if !strings.Contains(got.Description, "system prompt") {
		t.Errorf("description does not say where the roster is: %q", got.Description)
	}
	if !strings.Contains(got.Description, "removes") {
		t.Errorf("description does not say tools can only narrow: %q", got.Description)
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

// TestInvokeReportsTheActualProblemWithTools is Minor 3 from round 1 review: a malformed tools
// value used to fail the whole json.Unmarshal and report "needs agent and prompt", naming
// fields that were fine and hiding what was actually wrong. tools is decoded on its own now, so
// this reports on tools specifically.
func TestInvokeReportsTheActualProblemWithTools(t *testing.T) {
	h := &plugintest.Host{Name: "subagents"}
	if err := New(t.TempDir()).Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	invoke := h.RegisteredTools[0].Invoke
	res, err := invoke(context.Background(), tool.Call{
		ID: "tu1", Input: json.RawMessage(`{"agent":"explorer","prompt":"look","tools":"read"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || strings.Contains(text(res), "needs agent and prompt") || !strings.Contains(text(res), "tools") {
		t.Errorf("result %+v %q", res, text(res))
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

// TestAgentToolPassesAnEmptyToolsNarrowing is IMPORTANT 1 from round 1 review: an explicit
// empty tools list means give this subagent nothing, distinct from omitting the field, which
// means no narrowing at all. SessionOpenParams.Tools carried "omitempty", which drops a
// zero-length slice exactly like a nil one on the way to the wire, so this real json.Marshal
// (the pipe transport encodes for real) lost the distinction before session.open ever saw it.
func TestAgentToolPassesAnEmptyToolsNarrowing(t *testing.T) {
	cs, clientEnd := newChildServer(t)
	cs.onSubmit = func(cs *childServer) {
		cs.say("done")
		cs.state("completed")
	}
	res, err := agentTool(t, clientEnd)(context.Background(), tool.Call{
		ID: "tu1", Input: json.RawMessage(`{"agent":"explorer","prompt":"look","tools":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("result %+v %q", res, text(res))
	}
	if cs.openParams.Tools == nil {
		t.Error("an explicit empty tools list arrived at session.open as nil: narrowing to nothing was lost on the wire")
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
