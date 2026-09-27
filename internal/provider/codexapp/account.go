// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/agentruntime"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

const maxEarlyLoginCompletions = 64

type wireNotification struct {
	method string
	params json.RawMessage
}

type wireLoginCompletion struct {
	LoginID *string `json:"loginId"`
	Success bool    `json:"success"`
	Error   *string `json:"error"`
}

func (c *Client) Name() string { return "codex" }

func (c *Client) SetSink(sink agentruntime.Sink) {
	c.sinkMu.Lock()
	c.sink = sink
	c.sinkMu.Unlock()
}

func (c *Client) Account(ctx context.Context) (agentruntime.AccountState, error) {
	if err := c.Start(ctx); err != nil {
		return agentruntime.AccountState{}, err
	}
	var response struct {
		Account *struct {
			Type     string `json:"type"`
			PlanType string `json:"planType"`
		} `json:"account"`
	}
	if err := c.call(ctx, methodAccountRead, map[string]bool{"refreshToken": false}, &response); err != nil {
		return agentruntime.AccountState{}, err
	}
	state := agentruntime.AccountState{Runtime: c.Name()}
	if response.Account == nil {
		return state, nil
	}
	state.Authenticated = true
	state.AuthMode = accountAuthMode(response.Account.Type)
	state.PlanType = response.Account.PlanType
	return state, nil
}

func accountAuthMode(accountType string) string {
	switch accountType {
	case "apiKey":
		return "apikey"
	case "chatgpt":
		return "chatgpt"
	case "amazonBedrock":
		return "bedrock"
	default:
		return accountType
	}
}

func (c *Client) StartLogin(ctx context.Context, mode agentruntime.LoginMode) (agentruntime.AuthChallenge, error) {
	select {
	case <-c.loginStart:
		defer func() { c.loginStart <- struct{}{} }()
	case <-ctx.Done():
		return agentruntime.AuthChallenge{}, ctx.Err()
	}
	loginType := ""
	switch mode {
	case agentruntime.LoginBrowser:
		loginType = "chatgpt"
	case agentruntime.LoginDevice:
		loginType = "chatgptDeviceCode"
	default:
		return agentruntime.AuthChallenge{}, fmt.Errorf("codex: invalid login mode %q", mode)
	}
	if err := c.Start(ctx); err != nil {
		return agentruntime.AuthChallenge{}, err
	}
	var response struct {
		Type            string `json:"type"`
		LoginID         string `json:"loginId"`
		AuthURL         string `json:"authUrl"`
		VerificationURL string `json:"verificationUrl"`
		UserCode        string `json:"userCode"`
	}
	if err := c.callMutation(ctx, methodAccountLoginStart, map[string]string{"type": loginType}, &response); err != nil {
		if errors.Is(err, agentruntime.ErrAmbiguous) {
			c.abortProcess()
		}
		return agentruntime.AuthChallenge{}, err
	}
	if response.Type != loginType {
		c.abortProcess()
		return agentruntime.AuthChallenge{}, fmt.Errorf("codex: login response type %q, want %q", response.Type, loginType)
	}
	if response.LoginID == "" {
		c.abortProcess()
		return agentruntime.AuthChallenge{}, errors.New("codex: login response has no login id")
	}
	c.loginMu.Lock()
	_, active := c.activeLogins[response.LoginID]
	_, finished := c.finishedLogins[response.LoginID]
	c.loginMu.Unlock()
	if active || finished {
		c.abortProcess()
		return agentruntime.AuthChallenge{}, errors.New("codex: app server reused a login id")
	}
	loginID := ulid.Make().String()
	var (
		challenge agentruntime.AuthChallenge
		err       error
	)
	if mode == agentruntime.LoginBrowser {
		challenge, err = agentruntime.NewBrowserChallenge(c.Name(), loginID, response.AuthURL)
	} else {
		challenge, err = agentruntime.NewDeviceChallenge(c.Name(), loginID, response.VerificationURL, response.UserCode)
	}
	if err != nil {
		c.abortProcess()
		return agentruntime.AuthChallenge{}, err
	}

	var completion *wireLoginCompletion
	c.loginMu.Lock()
	c.activeLogins[response.LoginID] = challenge.LoginID
	c.loginIDs[challenge.LoginID] = response.LoginID
	if early, ok := c.earlyLogins[response.LoginID]; ok {
		completion = &early
		delete(c.earlyLogins, response.LoginID)
		delete(c.activeLogins, response.LoginID)
		c.rememberFinishedLoginLocked(response.LoginID)
		if early.Success {
			delete(c.loginIDs, challenge.LoginID)
		}
	}
	c.loginMu.Unlock()
	if completion != nil {
		c.emitLoginCompletion(*completion, challenge.LoginID)
	}
	return challenge, nil
}

func (c *Client) CancelLogin(ctx context.Context, loginID string) error {
	if loginID == "" {
		return errors.New("codex: login id is required")
	}
	if err := c.Start(ctx); err != nil {
		return err
	}
	var response struct {
		Status string `json:"status"`
	}
	c.loginMu.Lock()
	rawLoginID, ok := c.loginIDs[loginID]
	c.loginMu.Unlock()
	if !ok {
		return errors.New("codex: unknown login id")
	}
	if err := c.call(ctx, methodAccountLoginCancel, map[string]string{"loginId": rawLoginID}, &response); err != nil {
		return err
	}
	c.loginMu.Lock()
	delete(c.activeLogins, rawLoginID)
	delete(c.loginIDs, loginID)
	delete(c.earlyLogins, rawLoginID)
	c.rememberFinishedLoginLocked(rawLoginID)
	c.loginMu.Unlock()
	return nil
}

func (c *Client) ListModels(ctx context.Context) ([]provider.Model, error) {
	if err := c.Start(ctx); err != nil {
		return nil, err
	}
	type wireModel struct {
		ID                        string   `json:"id"`
		DisplayName               string   `json:"displayName"`
		InputModalities           []string `json:"inputModalities"`
		SupportedReasoningEfforts []struct {
			ReasoningEffort string `json:"reasoningEffort"`
		} `json:"supportedReasoningEfforts"`
	}
	var (
		models []provider.Model
		cursor *string
		seen   = map[string]bool{}
	)
	for {
		var page struct {
			Data       []wireModel `json:"data"`
			NextCursor *string     `json:"nextCursor"`
		}
		if err := c.call(ctx, methodModelList, struct {
			Cursor        *string `json:"cursor,omitempty"`
			IncludeHidden bool    `json:"includeHidden"`
		}{Cursor: cursor}, &page); err != nil {
			return nil, err
		}
		for _, model := range page.Data {
			efforts := make([]string, 0, len(model.SupportedReasoningEfforts))
			for _, option := range model.SupportedReasoningEfforts {
				efforts = append(efforts, option.ReasoningEffort)
			}
			models = append(models, provider.Model{
				Ref: session.ModelRef{Provider: c.Name(), Model: model.ID}, OwnerKind: provider.OwnerRuntime,
				DisplayName: model.DisplayName, ReasoningEfforts: efforts,
				Capabilities: provider.Capabilities{
					Tools: true, Vision: contains(model.InputModalities, "image"), Reasoning: len(efforts) > 0,
				},
			})
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		if seen[*page.NextCursor] {
			return nil, fmt.Errorf("codex model/list repeated cursor %q", *page.NextCursor)
		}
		seen[*page.NextCursor] = true
		cursor = page.NextCursor
	}
	c.modelsMu.Lock()
	c.models = append([]provider.Model(nil), models...)
	c.modelsMu.Unlock()
	return models, nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (c *Client) enqueueNotification(method string, params json.RawMessage) {
	notification := wireNotification{method: method, params: append(json.RawMessage(nil), params...)}
	select {
	case c.notifications <- notification:
	case <-c.dispatchStop:
	}
}

func (c *Client) dispatchNotifications() {
	defer close(c.dispatchDone)
	for {
		select {
		case notification := <-c.notifications:
			c.handleNotification(notification)
		case <-c.dispatchStop:
			return
		}
	}
}

func (c *Client) handleNotification(notification wireNotification) {
	switch notification.method {
	case methodAccountLoginCompleted:
		var completion wireLoginCompletion
		if json.Unmarshal(notification.params, &completion) == nil {
			c.matchLoginCompletion(completion)
		}
	case methodAccountUpdated:
		var update struct {
			AuthMode *string `json:"authMode"`
			PlanType *string `json:"planType"`
		}
		if json.Unmarshal(notification.params, &update) != nil {
			return
		}
		state := agentruntime.AccountState{Runtime: c.Name(), Authenticated: update.AuthMode != nil}
		if update.AuthMode != nil {
			state.AuthMode = *update.AuthMode
		}
		if update.PlanType != nil {
			state.PlanType = *update.PlanType
		}
		if sink := c.currentSink(); sink != nil {
			sink.AccountUpdated(state)
		}
	default:
		event, err := translateRuntimeNotification(notification)
		if err != nil || event == nil {
			return
		}
		event.Sequence = c.eventSequence.Add(1)
		if sink := c.currentSink(); sink != nil {
			sink.RuntimeEvent(*event)
		}
	}
}

func (c *Client) matchLoginCompletion(completion wireLoginCompletion) {
	if completion.LoginID == nil || *completion.LoginID == "" {
		return
	}
	rawLoginID := *completion.LoginID
	c.loginMu.Lock()
	if _, finished := c.finishedLogins[rawLoginID]; finished {
		c.loginMu.Unlock()
		return
	}
	loginID, active := c.activeLogins[rawLoginID]
	if active {
		delete(c.activeLogins, rawLoginID)
		c.rememberFinishedLoginLocked(rawLoginID)
		if completion.Success {
			delete(c.loginIDs, loginID)
		}
	} else if len(c.earlyLogins) < maxEarlyLoginCompletions {
		c.earlyLogins[rawLoginID] = completion
	}
	c.loginMu.Unlock()
	if active {
		c.emitLoginCompletion(completion, loginID)
	}
}

func (c *Client) rememberFinishedLoginLocked(loginID string) {
	c.finishedLogins[loginID] = struct{}{}
}

func (c *Client) emitLoginCompletion(completion wireLoginCompletion, loginID string) {
	if completion.LoginID == nil || loginID == "" {
		return
	}
	result := agentruntime.LoginCompletion{Runtime: c.Name(), LoginID: loginID, Success: completion.Success}
	if completion.Error != nil {
		result.Error = Redact(*completion.Error)
	}
	if sink := c.currentSink(); sink != nil {
		sink.LoginCompleted(result)
	}
}

func (c *Client) reasoningEffort(ref session.ModelRef, thinking session.ThinkingLevel) string {
	target := map[session.ThinkingLevel]string{
		session.ThinkingOff: "minimal", session.ThinkingLow: "low", session.ThinkingMedium: "medium", session.ThinkingHigh: "high",
	}[thinking]
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	for _, model := range c.models {
		if model.Ref == ref {
			return nearestEffort(target, model.ReasoningEfforts)
		}
	}
	return target
}

func nearestEffort(target string, available []string) string {
	order := map[string]int{"minimal": 0, "low": 1, "medium": 2, "high": 3}
	targetRank, ok := order[target]
	if !ok {
		return ""
	}
	best, bestRank := "", -1
	lowest, lowestRank := "", len(order)
	for _, effort := range available {
		rank, known := order[effort]
		if !known {
			continue
		}
		if rank < lowestRank {
			lowest, lowestRank = effort, rank
		}
		if rank <= targetRank && rank > bestRank {
			best, bestRank = effort, rank
		}
	}
	if best != "" {
		return best
	}
	return lowest
}

func (c *Client) currentSink() agentruntime.Sink {
	c.sinkMu.RLock()
	defer c.sinkMu.RUnlock()
	return c.sink
}
