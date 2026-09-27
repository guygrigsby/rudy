// SPDX-License-Identifier: AGPL-3.0-or-later

// Package codexapp adapts the Codex App Server protocol to Rudy's agent-runtime port.
package codexapp

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/guygrigsby/rudy/internal/agentruntime"
	"github.com/guygrigsby/rudy/internal/session"
)

type Client struct {
	command Command

	mu      sync.Mutex
	process *appProcess
	closed  bool
}

func NewClient(command Command) *Client { return &Client{command: command} }

func (c *Client) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("codex app server client is closed")
	}
	if c.process != nil && c.process.peer.alive() {
		return nil
	}
	process, err := startProcess(ctx, c.command, nil, nil)
	if err != nil {
		return err
	}
	var initialized struct {
		CodexHome      string `json:"codexHome"`
		PlatformFamily string `json:"platformFamily"`
		PlatformOS     string `json:"platformOs"`
		UserAgent      string `json:"userAgent"`
	}
	err = process.peer.Call(ctx, methodInitialize, map[string]any{
		"clientInfo": map[string]string{
			"name":    "rudy",
			"title":   "Rudy",
			"version": "0",
		},
		"capabilities": map[string]bool{"experimentalApi": false},
	}, &initialized)
	if err == nil {
		err = process.peer.Notify(methodInitialized, map[string]any{})
	}
	if err != nil {
		_ = process.close()
		return fmt.Errorf("initialize codex app server: %w", err)
	}
	c.process = process
	return nil
}

func (c *Client) StartTurn(ctx context.Context, request agentruntime.StartTurnRequest) (agentruntime.TurnRef, error) {
	if err := c.Start(ctx); err != nil {
		return agentruntime.TurnRef{}, err
	}
	input, err := turnInput(request.Content)
	if err != nil {
		return agentruntime.TurnRef{}, err
	}
	params := struct {
		ThreadID string      `json:"threadId"`
		Input    []userInput `json:"input"`
	}{ThreadID: request.Thread.ThreadID, Input: input}
	var response struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	process := c.currentProcess()
	if process == nil {
		return agentruntime.TurnRef{}, errors.New("codex app server process unavailable")
	}
	if err := process.peer.Call(ctx, methodTurnStart, params, &response); err != nil {
		var rejected *wireError
		if errors.As(err, &rejected) {
			return agentruntime.TurnRef{}, err
		}
		return agentruntime.TurnRef{}, errors.Join(agentruntime.ErrAmbiguous, err)
	}
	if response.Turn.ID == "" {
		return agentruntime.TurnRef{}, errors.New("codex app server turn/start returned no turn id")
	}
	return agentruntime.TurnRef{ThreadRef: request.Thread, TurnID: response.Turn.ID}, nil
}

func (c *Client) currentProcess() *appProcess {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.process
}

func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	process := c.process
	c.process = nil
	c.mu.Unlock()
	if process == nil {
		return nil
	}
	return process.close()
}

type userInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func turnInput(blocks []session.Block) ([]userInput, error) {
	input := make([]userInput, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != session.BlockText {
			return nil, fmt.Errorf("codex app server turn input does not support %q blocks", block.Type)
		}
		input = append(input, userInput{Type: "text", Text: block.Text})
	}
	return input, nil
}
