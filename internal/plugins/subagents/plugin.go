// Package subagents is the agent tool: it opens a child session under a named agent
// definition, submits one prompt to it and returns the subagent's final answer to the parent
// model as the tool result. It reaches the server the same way any plugin does, over the
// protocol through Host.Connect, and imports nothing from internal/server.
package subagents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/agentdef"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/fsroot"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

const schema = `{"type":"object","properties":{"agent":{"type":"string","description":"An agent definition name from agents/<name>.md"},"prompt":{"type":"string","description":"The task for the subagent"},"tools":{"type":"array","items":{"type":"string"},"description":"Narrow the subagent to these tools. Only removes: naming a tool the definition or this session does not hold does not grant it."}},"required":["agent","prompt"],"additionalProperties":false}`

type agentPlugin struct {
	configDir string
	host      plugin.Host
}

// New returns the plugin. configDir is the user's rudy config directory, whose agents/
// subdirectory is the first root the roster hook and the server both read; a workspace's own
// .rudy/agents/ is the second, read fresh per session so an edit between sessions takes effect
// without a restart.
func New(configDir string) plugin.Plugin { return &agentPlugin{configDir: configDir} }

func (p *agentPlugin) Name() string { return "subagents" }

func (p *agentPlugin) Init(_ context.Context, h plugin.Host) error {
	p.host = h
	desc := "Delegate a task to a subagent running in its own session and return its final answer. " +
		"The agents available to this session, and what each is for, are listed in the system prompt. " +
		"Pass tools to narrow what the subagent may use; it only ever removes."
	if err := h.RegisterTool(tool.Tool{
		Name: "agent", Description: desc, Schema: json.RawMessage(schema),
		Safety: tool.Safe, Invoke: p.invoke,
	}); err != nil {
		return err
	}
	// The roster is a session fact, not a process fact: it depends on the workspace and on
	// plugins that may register after this one. A description is written once and cannot be
	// revised, so it says where to look rather than trying to hold the answer (ADR 0028).
	return h.RegisterHook(plugin.HookHandler{
		Point:  plugin.HookSessionOpened,
		Handle: p.roster,
	})
}

// roster lists the definitions visible to one session: the operator's config directory, then
// the workspace's own .rudy/agents, then whatever plugins registered, first name winning. That
// is the same precedence resolveAgent uses to pick a definition, so what the roster advertises
// is what a call actually gets.
func (p *agentPlugin) roster(_ context.Context, call plugin.HookCall) (any, error) {
	payload, ok := call.Payload.(*plugin.SessionOpenedPayload)
	if !ok || payload == nil {
		return nil, nil
	}
	if payload.ParentSessionID != "" {
		// A child holds no agent tool: session.open strips it for a subagent, so telling this
		// session who it could delegate to invites a turn that ends in an unknown-tool result.
		// The shipped example lists bash, edit and write, so it is a live invitation, not a
		// theoretical one (ADR 0028).
		return nil, nil
	}
	defs, errs := agentdef.Load([]string{
		filepath.Join(p.configDir, "agents"),
		filepath.Join(payload.Workspace.Root, ".rudy", "agents"),
	})
	for _, e := range errs {
		p.host.Notice(e.Error())
	}
	for n, d := range p.host.AgentDefs() {
		if _, seen := defs[n]; !seen {
			defs[n] = d
		}
	}
	if len(defs) == 0 {
		return nil, nil
	}
	return &plugin.SessionOpenedResult{
		Context: "Agents you can delegate to with the agent tool: " + describe(defs) + ".",
	}, nil
}

// describe lists the definitions by name and description, in name order so the tool's
// description is stable between runs.
func describe(defs map[string]agentdef.Definition) string {
	if len(defs) == 0 {
		return "none"
	}
	names := make([]string, 0, len(defs))
	for n := range defs {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, n+" ("+defs[n].Description+")")
	}
	return strings.Join(parts, ", ")
}

func (p *agentPlugin) invoke(ctx context.Context, call tool.Call) (tool.Result, error) {
	var a struct {
		Agent  string          `json:"agent"`
		Prompt string          `json:"prompt"`
		Tools  json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(call.Input, &a); err != nil || a.Agent == "" || strings.TrimSpace(a.Prompt) == "" {
		return fsroot.Fail("agent: input needs agent and prompt"), nil
	}
	// Tools is decoded separately, on its own raw bytes, so a value of the wrong shape (a bare
	// string, say) fails with its own message rather than folding into the generic one above,
	// which named agent and prompt as the problem when neither was one (rudy-review round 1 on
	// task 5). Absent or explicit null both mean no narrowing.
	var tools []string
	if len(a.Tools) > 0 && string(a.Tools) != "null" {
		if err := json.Unmarshal(a.Tools, &tools); err != nil {
			return fsroot.Fail("agent: tools must be an array of strings"), nil
		}
	}
	client, err := p.host.Connect(ctx)
	if err != nil {
		return fsroot.Fail("agent: %v", err), nil
	}
	defer func() { _ = client.Close() }()

	var info protocol.SessionInfo
	err = client.Call(ctx, protocol.MethodSessionOpen, protocol.SessionOpenParams{
		Cwd: call.Workspace.Root, Agent: a.Agent, Tools: tools,
		Parent: &protocol.ParentRef{SessionID: call.SessionID.String(), ToolUseID: call.ID},
	}, &info)
	if err != nil {
		return fsroot.Fail("agent: open: %v", err), nil
	}
	// The child belongs to this one tool call, so this connection gives it up here whatever
	// happened to the turn. context.Background deliberately, since the call context may be
	// exactly what ended the turn. This is a detach, not a guarantee of closure: a child
	// still streaming when it lands stays live until its turn ends, and the server closes it
	// then (see the orphan close in runTurn), which is why the cancel path below waits for
	// the interrupt to take rather than walking away.
	defer func() {
		_ = client.Call(context.Background(), protocol.MethodSessionClose, protocol.SessionCloseParams{SessionID: info.SessionID}, nil)
	}()

	var sub protocol.SessionSubmitResult
	if err := client.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Content: []session.Block{session.TextBlock(a.Prompt)}, Source: session.SourceTyped,
	}, &sub); err != nil {
		return fsroot.Fail("agent: submit: %v", err), nil
	}
	out, err := waitTurn(ctx, client, info.SessionID, sub.TurnID)
	switch {
	case err != nil:
		return fsroot.Fail("agent: %v", err), nil
	case out.state == stateFailed || out.failure != "":
		return fsroot.Fail("agent %s failed: %s", a.Agent, out.failure), nil
	case out.state == stateIdle:
		// Idle is an interrupted subagent, not an answer: report what it managed to say as
		// a failure so the parent model does not read a half turn as a finished one.
		msg := "agent " + a.Agent + " interrupted"
		if out.text != "" {
			msg += "\n" + out.text
		}
		return fsroot.Fail("%s", msg), nil
	}
	return fsroot.Text(fmt.Sprintf("[agent %s, session %s]\n%s", a.Agent, info.SessionID, out.text)), nil
}

// The turn states a child session comes to rest in, as they arrive on turn.state.
const (
	stateCompleted = "completed"
	stateFailed    = "failed"
	stateIdle      = "idle"
)

// interruptGrace bounds how long a cancelled tool call waits for the child it interrupted to
// come to rest. Long enough for a provider stream to unwind, short enough that a parent turn
// being torn down is not held up by a subagent that will not stop.
const interruptGrace = 5 * time.Second

// turnOutcome is what one child turn came to: the state it rested in, the text of its last
// assistant message and, when it failed, the failure the runner recorded.
type turnOutcome struct {
	state   string
	text    string
	failure string
}

// waitTurn follows the child session's notifications until its turn comes to rest. It reads
// the same two notifications a client does (entry.appended and turn.state) and ignores entries
// older than the turn, which is how the replay of everything that came before is skipped: a
// turn id is the ULID of the user_message that started it, so every entry of that turn sorts
// at or above it.
//
// A cancelled ctx interrupts the child and then keeps reading, on a fresh deadline, until its
// turn actually rests: the interrupt is asynchronous, and walking away from a child that is
// still streaming leaves a session running for a tool call nobody is waiting on any more.
func waitTurn(ctx context.Context, client *protocol.Client, sessionID, turnID string) (turnOutcome, error) {
	turnULID, perr := ulid.Parse(turnID)
	if perr != nil {
		return turnOutcome{}, fmt.Errorf("turn id %q: %w", turnID, perr)
	}
	var out turnOutcome
	var cause error // the parent's cancellation, once it has happened
	for {
		select {
		case <-ctx.Done():
			if cause != nil {
				// The grace ran out: the child did not come to rest in time, and the
				// server closes it when its turn ends (see the orphan close in runTurn).
				return out, cause
			}
			cause = ctx.Err()
			grace, done := context.WithTimeout(context.WithoutCancel(ctx), interruptGrace)
			defer done()
			ctx = grace
			_ = client.Call(ctx, protocol.MethodSessionInterrupt, protocol.SessionInterruptParams{
				SessionID: sessionID, How: session.InterruptCancel,
			}, nil)
		case n, ok := <-client.Notifications():
			if !ok {
				return out, errors.New("the server closed the subagent's connection")
			}
			rest, err := out.observe(n, sessionID, turnID, turnULID)
			switch {
			case err != nil:
				return out, err
			case rest && cause != nil:
				return out, cause
			case rest:
				return out, nil
			}
		}
	}
}

// observe folds one notification into the outcome and reports whether the turn has come to
// rest. Notifications for another session or another turn are ignored.
func (out *turnOutcome) observe(n protocol.Notification, sessionID, turnID string, turnULID ulid.ULID) (rest bool, err error) {
	switch n.Method {
	case protocol.NotifyEntryAppended:
		var ea protocol.EntryAppended
		if err := json.Unmarshal(n.Params, &ea); err != nil {
			return false, fmt.Errorf("entry.appended: %w", err)
		}
		if ea.SessionID != sessionID || ea.Entry.ID.Compare(turnULID) < 0 {
			return false, nil
		}
		switch pl := ea.Entry.Payload.(type) {
		case session.AssistantMessage:
			if t := session.TextOf(pl.Content); t != "" {
				out.text = t
			}
		case session.TurnFailed:
			out.failure = pl.Message
		}
	case protocol.NotifyTurnState:
		var ts protocol.TurnStateChanged
		if err := json.Unmarshal(n.Params, &ts); err != nil {
			return false, fmt.Errorf("turn.state: %w", err)
		}
		if ts.SessionID != sessionID || ts.TurnID != turnID {
			return false, nil
		}
		switch ts.State {
		case stateCompleted, stateFailed, stateIdle:
			out.state = ts.State
			return true, nil
		}
	}
	return false, nil
}
