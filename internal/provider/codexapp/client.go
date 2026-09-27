// SPDX-License-Identifier: AGPL-3.0-or-later

// Package codexapp adapts the Codex App Server protocol to Rudy's agent-runtime port.
package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/guygrigsby/rudy/internal/agentruntime"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

type Client struct {
	command Command

	mu            sync.Mutex
	process       *appProcess
	closed        bool
	turnMu        sync.Mutex
	activeTurns   map[string]activeTurn
	finishedTurns map[string]string

	sinkMu sync.RWMutex
	sink   agentruntime.Sink

	loginMu        sync.Mutex
	loginStart     chan struct{}
	activeLogins   map[string]string
	loginIDs       map[string]loginBinding
	loginProcesses map[string]*appProcess
	earlyLogins    map[string]wireLoginCompletion
	finishedLogins map[string]struct{}
	modelsMu       sync.Mutex
	models         []provider.Model
	eventSequence  atomic.Uint64

	notifications chan wireNotification
	dispatchStop  chan struct{}
	dispatchDone  chan struct{}
	stopOnce      sync.Once
}

func NewClient(command Command) *Client {
	c := &Client{
		command: command, activeLogins: map[string]string{}, loginIDs: map[string]loginBinding{},
		loginProcesses: map[string]*appProcess{},
		activeTurns:    map[string]activeTurn{},
		finishedTurns:  map[string]string{},
		earlyLogins:    map[string]wireLoginCompletion{}, finishedLogins: map[string]struct{}{},
		loginStart: make(chan struct{}, 1), notifications: make(chan wireNotification, 256),
		dispatchStop: make(chan struct{}), dispatchDone: make(chan struct{}),
	}
	c.loginStart <- struct{}{}
	go c.dispatchNotifications()
	return c
}

func (c *Client) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var failedLogins []failedLogin
	c.mu.Lock()
	defer func() {
		c.mu.Unlock()
		c.emitFailedLogins(failedLogins)
	}()
	if c.closed {
		return errors.New("codex app server client is closed")
	}
	if c.process != nil && c.process.peer.alive() {
		return nil
	}
	if c.process != nil {
		failedLogins = c.collectFailedProcessLogins(c.process)
	}
	var process *appProcess
	process, err := startProcess(ctx, c.command, c.handleRequest, func(method string, params json.RawMessage) {
		c.enqueueNotificationFrom(process, method, params)
	})
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
	c.loginMu.Lock()
	clear(c.activeLogins)
	clear(c.loginProcesses)
	clear(c.earlyLogins)
	clear(c.finishedLogins)
	c.loginMu.Unlock()
	c.process = process
	go c.watchProcess(process)
	return nil
}

func (c *Client) StartTurn(ctx context.Context, request agentruntime.StartTurnRequest) (agentruntime.TurnRef, error) {
	if err := c.Start(ctx); err != nil {
		return agentruntime.TurnRef{}, err
	}
	process := c.currentProcess()
	input, err := turnInput(request.Content)
	if err != nil {
		return agentruntime.TurnRef{}, err
	}
	params := struct {
		ThreadID       string      `json:"threadId"`
		Input          []userInput `json:"input"`
		Model          string      `json:"model,omitempty"`
		Effort         string      `json:"effort,omitempty"`
		ApprovalPolicy string      `json:"approvalPolicy,omitempty"`
	}{
		ThreadID: request.Thread.ThreadID, Input: input, Model: request.Model.Model,
		Effort: c.reasoningEffort(request.Model, request.Thinking), ApprovalPolicy: approvalPolicy(request.Mode),
	}
	var response struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := c.callMutationOn(ctx, process, methodTurnStart, params, &response); err != nil {
		return agentruntime.TurnRef{}, err
	}
	if response.Turn.ID == "" {
		return agentruntime.TurnRef{}, errors.Join(agentruntime.ErrAmbiguous, errors.New("codex app server turn/start returned no turn id"))
	}
	ref := agentruntime.TurnRef{ThreadRef: request.Thread, TurnID: response.Turn.ID}
	c.trackTurn(ref, process)
	return ref, nil
}

func (c *Client) currentProcess() *appProcess {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.process
}

func (c *Client) abortProcess() {
	c.mu.Lock()
	process := c.process
	c.process = nil
	c.mu.Unlock()
	if process != nil {
		_ = process.close()
	}
}

func (c *Client) call(ctx context.Context, method string, params, result any) error {
	process := c.currentProcess()
	if process == nil {
		return errors.New("codex app server process unavailable")
	}
	return process.peer.Call(ctx, method, params, result)
}

func (c *Client) callMutation(ctx context.Context, method string, params, result any) error {
	return c.callMutationOn(ctx, c.currentProcess(), method, params, result)
}

func (c *Client) callMutationOn(ctx context.Context, process *appProcess, method string, params, result any) error {
	var err error
	if process == nil {
		err = &callNotSentError{err: errors.New("codex app server process unavailable")}
	} else {
		err = process.peer.Call(ctx, method, params, result)
	}
	if err == nil {
		return nil
	}
	var rejected *wireError
	if errors.As(err, &rejected) {
		return err
	}
	var notSent *callNotSentError
	if errors.As(err, &notSent) {
		return err
	}
	return errors.Join(agentruntime.ErrAmbiguous, err)
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
		c.stopDispatcher()
		return nil
	}
	err := process.close()
	c.stopDispatcher()
	return err
}

func (c *Client) stopDispatcher() {
	c.stopOnce.Do(func() { close(c.dispatchStop) })
	<-c.dispatchDone
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
