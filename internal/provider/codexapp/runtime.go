// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/guygrigsby/rudy/internal/agentruntime"
	"github.com/guygrigsby/rudy/internal/session"
)

type wireThread struct {
	ID    string     `json:"id"`
	Turns []wireTurn `json:"turns"`
}

type wireTurn struct {
	ID     string            `json:"id"`
	Status string            `json:"status"`
	Items  []json.RawMessage `json:"items"`
}

func (c *Client) StartThread(ctx context.Context, request agentruntime.StartThreadRequest) (agentruntime.ThreadRef, error) {
	if err := c.Start(ctx); err != nil {
		return agentruntime.ThreadRef{}, err
	}
	var response struct {
		Thread wireThread `json:"thread"`
	}
	err := c.callMutation(ctx, methodThreadStart, map[string]any{
		"cwd": request.Workspace.Root, "model": request.Model.Model, "approvalPolicy": approvalPolicy(request.Mode),
	}, &response)
	if err != nil {
		return agentruntime.ThreadRef{}, err
	}
	if response.Thread.ID == "" {
		return agentruntime.ThreadRef{}, errors.New("codex app server thread/start returned no thread id")
	}
	return agentruntime.ThreadRef{Runtime: c.Name(), SessionID: request.SessionID, ThreadID: response.Thread.ID}, nil
}

func (c *Client) ResumeThread(ctx context.Context, ref agentruntime.ThreadRef) error {
	if err := c.Start(ctx); err != nil {
		return err
	}
	var response struct {
		Thread wireThread `json:"thread"`
	}
	if err := c.call(ctx, methodThreadResume, map[string]any{"threadId": ref.ThreadID}, &response); err != nil {
		return err
	}
	if response.Thread.ID != ref.ThreadID {
		return fmt.Errorf("codex app server resumed thread %q, want %q", response.Thread.ID, ref.ThreadID)
	}
	return nil
}

func (c *Client) ForkThread(ctx context.Context, ref agentruntime.ThreadRef) (agentruntime.ThreadRef, error) {
	if err := c.Start(ctx); err != nil {
		return agentruntime.ThreadRef{}, err
	}
	var response struct {
		Thread wireThread `json:"thread"`
	}
	if err := c.callMutation(ctx, methodThreadFork, map[string]any{"threadId": ref.ThreadID}, &response); err != nil {
		return agentruntime.ThreadRef{}, err
	}
	if response.Thread.ID == "" {
		return agentruntime.ThreadRef{}, errors.New("codex app server thread/fork returned no thread id")
	}
	return agentruntime.ThreadRef{Runtime: c.Name(), SessionID: ref.SessionID, ThreadID: response.Thread.ID}, nil
}

func (c *Client) ReadThread(ctx context.Context, ref agentruntime.ThreadRef) (agentruntime.Thread, error) {
	if err := c.Start(ctx); err != nil {
		return agentruntime.Thread{}, err
	}
	var response struct {
		Thread wireThread `json:"thread"`
	}
	if err := c.call(ctx, methodThreadRead, map[string]any{"threadId": ref.ThreadID, "includeTurns": true}, &response); err != nil {
		return agentruntime.Thread{}, err
	}
	if response.Thread.ID != ref.ThreadID {
		return agentruntime.Thread{}, errors.New("codex app server read a different thread")
	}
	return translateThread(response.Thread)
}

func (c *Client) SteerTurn(ctx context.Context, request agentruntime.SteerTurnRequest) (agentruntime.TurnRef, error) {
	if err := c.Start(ctx); err != nil {
		return agentruntime.TurnRef{}, err
	}
	input, err := turnInput(request.Content)
	if err != nil {
		return agentruntime.TurnRef{}, err
	}
	var response struct {
		TurnID string `json:"turnId"`
	}
	err = c.callMutation(ctx, methodTurnSteer, map[string]any{
		"threadId": request.Turn.ThreadID, "expectedTurnId": request.Turn.TurnID, "input": input,
	}, &response)
	if err != nil {
		return agentruntime.TurnRef{}, err
	}
	return agentruntime.TurnRef{ThreadRef: request.Turn.ThreadRef, TurnID: response.TurnID}, nil
}

func (c *Client) InterruptTurn(ctx context.Context, ref agentruntime.TurnRef) error {
	if err := c.Start(ctx); err != nil {
		return err
	}
	return c.call(ctx, methodTurnInterrupt, map[string]string{"threadId": ref.ThreadID, "turnId": ref.TurnID}, nil)
}

func approvalPolicy(mode session.Mode) string {
	switch mode {
	case session.ModePermissive:
		return "unlessTrusted"
	case session.ModeOff:
		return "never"
	default:
		return "onRequest"
	}
}

func translateThread(raw wireThread) (agentruntime.Thread, error) {
	if raw.ID == "" {
		return agentruntime.Thread{}, errors.New("codex app server thread has no id")
	}
	thread := agentruntime.Thread{Runtime: "codex", ThreadID: raw.ID, Turns: make([]agentruntime.Turn, 0, len(raw.Turns))}
	seenTurns := make(map[string]bool, len(raw.Turns))
	for _, rawTurn := range raw.Turns {
		if rawTurn.ID == "" || seenTurns[rawTurn.ID] {
			return agentruntime.Thread{}, errors.New("codex thread has empty or duplicate turn id")
		}
		seenTurns[rawTurn.ID] = true
		status, err := translateTurnStatus(rawTurn.Status)
		if err != nil {
			return agentruntime.Thread{}, err
		}
		turn := agentruntime.Turn{TurnID: rawTurn.ID, Status: status}
		seenItems := make(map[string]bool, len(rawTurn.Items))
		for _, rawItem := range rawTurn.Items {
			item, err := translateItem(rawItem)
			if err != nil {
				return agentruntime.Thread{}, err
			}
			if seenItems[item.ItemID] {
				return agentruntime.Thread{}, errors.New("codex turn has duplicate item id")
			}
			seenItems[item.ItemID] = true
			turn.Items = append(turn.Items, item)
		}
		thread.Turns = append(thread.Turns, turn)
	}
	return thread, nil
}

func translateTurnStatus(status string) (agentruntime.TurnStatus, error) {
	switch status {
	case "completed":
		return agentruntime.TurnCompleted, nil
	case "interrupted":
		return agentruntime.TurnInterrupted, nil
	case "failed":
		return agentruntime.TurnFailed, nil
	case "inProgress":
		return agentruntime.TurnRunning, nil
	}
	return "", errors.New("codex turn has unknown status")
}

func translateItem(raw json.RawMessage) (agentruntime.Item, error) {
	var item struct {
		ID               string          `json:"id"`
		Type             string          `json:"type"`
		Status           string          `json:"status"`
		Text             string          `json:"text"`
		Content          json.RawMessage `json:"content"`
		Summary          []string        `json:"summary"`
		Command          string          `json:"command"`
		CWD              string          `json:"cwd"`
		AggregatedOutput *string         `json:"aggregatedOutput"`
		Changes          []struct {
			Path string `json:"path"`
			Kind struct {
				Type string `json:"type"`
			} `json:"kind"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return agentruntime.Item{}, fmt.Errorf("codex thread item: %w", err)
	}
	if item.ID == "" {
		return agentruntime.Item{}, errors.New("codex thread item has no id")
	}
	out := agentruntime.Item{ItemID: item.ID, Status: item.Status, Command: item.Command, CWD: item.CWD}
	for _, change := range item.Changes {
		if change.Path == "" || !oneOf(change.Kind.Type, "add", "delete", "update") {
			return agentruntime.Item{}, errors.New("codex file change has invalid path or kind")
		}
		out.Changes = append(out.Changes, agentruntime.FileChange{Path: change.Path, Kind: change.Kind.Type})
	}
	if item.AggregatedOutput != nil {
		out.Output = *item.AggregatedOutput
	}
	switch item.Type {
	case "userMessage":
		out.Type = agentruntime.ItemUserMessage
		var contents []json.RawMessage
		if err := json.Unmarshal(item.Content, &contents); err != nil {
			return agentruntime.Item{}, errors.New("codex user message has invalid content")
		}
		for _, content := range contents {
			var input struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(content, &input) == nil && input.Type == "text" {
				out.Content = append(out.Content, session.TextBlock(input.Text))
			}
		}
	case "agentMessage":
		out.Type = agentruntime.ItemAgentMessage
		out.Content = []session.Block{session.TextBlock(item.Text)}
	case "reasoning":
		out.Type = agentruntime.ItemReasoning
		var reasoningContent []string
		if len(item.Content) > 0 {
			if err := json.Unmarshal(item.Content, &reasoningContent); err != nil {
				return agentruntime.Item{}, errors.New("codex reasoning item has invalid content")
			}
		}
		out.Content = []session.Block{{Type: session.BlockThinking, Text: strings.Join(append(item.Summary, reasoningContent...), "\n")}}
	case "commandExecution":
		out.Type = agentruntime.ItemCommand
		if !oneOf(item.Status, "inProgress", "completed", "failed", "declined") {
			return agentruntime.Item{}, errors.New("codex command item has unknown status")
		}
	case "fileChange":
		out.Type = agentruntime.ItemFileChange
		if !oneOf(item.Status, "inProgress", "completed", "failed", "declined") {
			return agentruntime.Item{}, errors.New("codex file change item has unknown status")
		}
	case "plan":
		out.Type = agentruntime.ItemPlan
		out.Content = []session.Block{session.TextBlock(item.Text)}
	case "mcpToolCall", "dynamicToolCall":
		out.Type = agentruntime.ItemTool
		if !oneOf(item.Status, "inProgress", "completed", "failed") {
			return agentruntime.Item{}, errors.New("codex tool item has unknown status")
		}
	case "collabAgentToolCall":
		out.Type = agentruntime.ItemTool
		if !oneOf(item.Status, "inProgress", "completed", "failed", "interrupted") {
			return agentruntime.Item{}, errors.New("codex collaboration item has unknown status")
		}
	case "imageGeneration":
		out.Type = agentruntime.ItemTool
		if item.Status == "" {
			return agentruntime.Item{}, errors.New("codex image generation item has no status")
		}
	case "hookPrompt", "functionCallOutput", "subAgentActivity", "webSearch", "imageView", "sleep", "enteredReviewMode", "exitedReviewMode", "contextCompaction":
		out.Type = agentruntime.ItemTool
	default:
		return agentruntime.Item{}, errors.New("codex thread item has unknown type")
	}
	return out, nil
}

func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}
