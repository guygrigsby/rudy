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

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/agentdef"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/fsroot"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

const schema = `{"type":"object","properties":{"agent":{"type":"string","description":"An agent definition name from agents/<name>.md"},"prompt":{"type":"string","description":"The task for the subagent"}},"required":["agent","prompt"],"additionalProperties":false}`

type agentPlugin struct {
	configDir string
	host      plugin.Host
}

// New returns the plugin. configDir is the user's rudy config directory, whose agents/
// subdirectory names the definitions the tool's description advertises; a workspace's own
// .rudy/agents/ is read by the server when the child session opens.
func New(configDir string) plugin.Plugin { return &agentPlugin{configDir: configDir} }

func (p *agentPlugin) Name() string { return "subagents" }

func (p *agentPlugin) Init(_ context.Context, h plugin.Host) error {
	p.host = h
	defs, _ := agentdef.Load([]string{filepath.Join(p.configDir, "agents")})
	desc := "Delegate a task to a subagent running in its own session and return its final answer. Agents defined for this machine: " + describe(defs) + ". A workspace may add more under .rudy/agents/."
	return h.RegisterTool(tool.Tool{Name: "agent", Description: desc, Schema: json.RawMessage(schema), Safety: tool.Safe, Invoke: p.invoke})
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
		Agent  string `json:"agent"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(call.Input, &a); err != nil || a.Agent == "" || strings.TrimSpace(a.Prompt) == "" {
		return fsroot.Fail("agent: input needs agent and prompt"), nil
	}
	client, err := p.host.Connect(ctx)
	if err != nil {
		return fsroot.Fail("agent: %v", err), nil
	}
	defer func() { _ = client.Close() }()

	var info protocol.SessionInfo
	err = client.Call(ctx, protocol.MethodSessionOpen, protocol.SessionOpenParams{
		Cwd: call.Workspace.Root, Agent: a.Agent,
		Parent: &protocol.ParentRef{SessionID: call.SessionID.String(), ToolUseID: call.ID},
	}, &info)
	if err != nil {
		return fsroot.Fail("agent: open: %v", err), nil
	}
	// The child belongs to this one tool call: closing it here releases its log and its
	// place in the server's live set whatever happened to the turn. context.Background
	// deliberately, since the call context may be exactly what ended the turn.
	defer func() {
		_ = client.Call(context.Background(), protocol.MethodSessionClose, protocol.SessionCloseParams{SessionID: info.SessionID}, nil)
	}()

	var sub protocol.SessionSubmitResult
	if err := client.Call(ctx, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Content: []session.Block{session.TextBlock(a.Prompt)}, Source: session.SourceTyped,
	}, &sub); err != nil {
		return fsroot.Fail("agent: submit: %v", err), nil
	}
	text, failure, err := waitTurn(ctx, client, info.SessionID, sub.TurnID)
	switch {
	case err != nil:
		return fsroot.Fail("agent: %v", err), nil
	case failure != "":
		return fsroot.Fail("agent %s failed: %s", a.Agent, failure), nil
	}
	return fsroot.Text(fmt.Sprintf("[agent %s, session %s]\n%s", a.Agent, info.SessionID, text)), nil
}

// waitTurn follows the child session's notifications until its turn comes to rest, and
// returns the text of its last assistant message. It reads the same two notifications a
// client does (entry.appended and turn.state) and ignores entries older than the turn, which
// is how the replay of everything that came before is skipped: a turn id is the ULID of the
// user_message that started it, so every entry of that turn sorts at or above it.
func waitTurn(ctx context.Context, client *protocol.Client, sessionID, turnID string) (text, failure string, err error) {
	turnULID, perr := ulid.Parse(turnID)
	if perr != nil {
		return "", "", fmt.Errorf("turn id %q: %w", turnID, perr)
	}
	for {
		select {
		case <-ctx.Done():
			// The parent turn was interrupted or timed out. Cancel the child rather than
			// leaving it streaming into a session nobody is reading any more.
			cancelCtx := context.WithoutCancel(ctx)
			_ = client.Call(cancelCtx, protocol.MethodSessionInterrupt, protocol.SessionInterruptParams{
				SessionID: sessionID, How: session.InterruptCancel,
			}, nil)
			return "", "", ctx.Err()
		case n, ok := <-client.Notifications():
			if !ok {
				return "", "", errors.New("the server closed the subagent's connection")
			}
			switch n.Method {
			case protocol.NotifyEntryAppended:
				var ea protocol.EntryAppended
				if err := json.Unmarshal(n.Params, &ea); err != nil {
					return "", "", fmt.Errorf("entry.appended: %w", err)
				}
				if ea.SessionID != sessionID || ea.Entry.ID.Compare(turnULID) < 0 {
					continue
				}
				switch pl := ea.Entry.Payload.(type) {
				case session.AssistantMessage:
					if t := textOf(pl.Content); t != "" {
						text = t
					}
				case session.TurnFailed:
					failure = pl.Message
				}
			case protocol.NotifyTurnState:
				var ts protocol.TurnStateChanged
				if err := json.Unmarshal(n.Params, &ts); err != nil {
					return "", "", fmt.Errorf("turn.state: %w", err)
				}
				if ts.SessionID != sessionID || ts.TurnID != turnID {
					continue
				}
				switch ts.State {
				case "completed", "failed", "idle":
					return text, failure, nil
				}
			}
		}
	}
}

func textOf(blocks []session.Block) string {
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Type == session.BlockText {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(bl.Text)
		}
	}
	return b.String()
}
