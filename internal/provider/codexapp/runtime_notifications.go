// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/guygrigsby/rudy/internal/agentruntime"
	"github.com/guygrigsby/rudy/internal/session"
)

// translateRuntimeNotification keeps App Server payloads inside the adapter.
// Invalid or unsupported notifications never reach the runtime sink.
func translateRuntimeNotification(notification wireNotification) (*agentruntime.Event, error) {
	var wire struct {
		ThreadID  string          `json:"threadId"`
		TurnID    string          `json:"turnId"`
		ItemID    string          `json:"itemId"`
		RequestID json.RawMessage `json:"requestId"`
		Thread    struct {
			ID string `json:"id"`
		} `json:"thread"`
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
		Item   json.RawMessage `json:"item"`
		Status struct {
			Type string `json:"type"`
		} `json:"status"`
		Delta   string `json:"delta"`
		Message string `json:"message"`
		Diff    string `json:"diff"`
		Plan    []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		} `json:"plan"`
		Changes []struct {
			Diff string `json:"diff"`
		} `json:"changes"`
		SummaryIndex int `json:"summaryIndex"`
		Error        struct {
			Message string `json:"message"`
		} `json:"error"`
		TokenUsage *struct {
			Last struct {
				Input      int64 `json:"inputTokens"`
				Output     int64 `json:"outputTokens"`
				CacheRead  int64 `json:"cachedInputTokens"`
				CacheWrite int64 `json:"cacheWriteInputTokens"`
			} `json:"last"`
		} `json:"tokenUsage"`
	}
	if err := json.Unmarshal(notification.params, &wire); err != nil {
		return nil, err
	}
	event := &agentruntime.Event{ThreadID: wire.ThreadID, TurnID: wire.TurnID, ItemID: wire.ItemID}
	switch notification.method {
	case methodThreadStarted:
		event.Type = agentruntime.EventThreadStarted
		event.ThreadID = wire.Thread.ID
	case methodThreadStatusChanged:
		event.Type = agentruntime.EventThreadStatus
		if !oneOf(wire.Status.Type, "notLoaded", "idle", "systemError", "active") {
			return nil, errors.New("codex thread status notification has unknown status")
		}
		event.Status = wire.Status.Type
	case methodTurnStarted, methodTurnCompleted:
		status, err := translateTurnStatus(wire.Turn.Status)
		if err != nil {
			return nil, err
		}
		event.TurnID = wire.Turn.ID
		event.Status = string(status)
		if notification.method == methodTurnStarted {
			event.Type = agentruntime.EventTurnStarted
			if status != agentruntime.TurnRunning {
				return nil, errors.New("codex turn/started has terminal status")
			}
		} else {
			event.Type = agentruntime.EventTurnCompleted
			if status == agentruntime.TurnRunning {
				return nil, errors.New("codex turn/completed has running status")
			}
		}
	case methodItemStarted, methodItemCompleted:
		item, err := translateItem(wire.Item)
		if err != nil {
			return nil, err
		}
		event.ItemID = item.ItemID
		event.Item = item
		if notification.method == methodItemStarted {
			event.Type = agentruntime.EventItemStarted
		} else {
			event.Type = agentruntime.EventItemCompleted
		}
	case methodItemAgentMessageDelta, methodItemCommandExecutionOutputDelta,
		methodItemFileChangeOutputDelta, methodItemPlanDelta,
		methodItemReasoningSummaryTextDelta, methodItemReasoningTextDelta:
		event.Type = agentruntime.EventItemDelta
		event.Text = wire.Delta
	case methodItemReasoningSummaryPartAdded:
		event.Type = agentruntime.EventItemDelta
		if wire.SummaryIndex > 0 {
			event.Text = "\n"
		}
	case methodItemMCPToolCallProgress:
		event.Type = agentruntime.EventItemDelta
		event.Text = wire.Message
		event.Status = "replace"
	case methodItemFileChangePatchUpdated:
		event.Type = agentruntime.EventItemDelta
		parts := make([]string, 0, len(wire.Changes))
		for _, change := range wire.Changes {
			parts = append(parts, change.Diff)
		}
		event.Text = strings.Join(parts, "\n")
		event.Status = "replace"
	case methodTurnDiffUpdated:
		event.Type = agentruntime.EventDiffUpdated
		event.Text = wire.Diff
	case methodTurnPlanUpdated:
		event.Type = agentruntime.EventPlanUpdated
		parts := make([]string, 0, len(wire.Plan))
		for _, step := range wire.Plan {
			if !oneOf(step.Status, "pending", "inProgress", "completed") {
				return nil, errors.New("codex turn plan has unknown step status")
			}
			parts = append(parts, "["+step.Status+"] "+step.Step)
		}
		event.Text = strings.Join(parts, "\n")
	case methodThreadTokenUsageUpdated:
		event.Type = agentruntime.EventUsageUpdated
		if wire.TokenUsage == nil {
			return nil, errors.New("codex usage notification has no usage")
		}
		last := wire.TokenUsage.Last
		event.Usage = session.Usage{Input: last.Input, Output: last.Output, CacheRead: last.CacheRead, CacheWrite: last.CacheWrite}
	case methodError:
		event.Type = agentruntime.EventError
		event.Text = Redact(wire.Error.Message)
	case methodServerRequestResolved:
		event.Type = agentruntime.EventRequestResolved
		var err error
		event.RequestID, err = canonicalRequestID(wire.RequestID)
		if err != nil {
			return nil, err
		}
	default:
		return nil, nil
	}
	if event.ThreadID == "" {
		return nil, errors.New("codex runtime notification has no thread id")
	}
	switch event.Type {
	case agentruntime.EventTurnStarted, agentruntime.EventTurnCompleted,
		agentruntime.EventItemStarted, agentruntime.EventItemDelta, agentruntime.EventItemCompleted,
		agentruntime.EventDiffUpdated, agentruntime.EventPlanUpdated,
		agentruntime.EventUsageUpdated, agentruntime.EventError:
		if event.TurnID == "" {
			return nil, errors.New("codex runtime notification has no turn id")
		}
	}
	switch event.Type {
	case agentruntime.EventItemStarted, agentruntime.EventItemDelta, agentruntime.EventItemCompleted:
		if event.ItemID == "" {
			return nil, errors.New("codex runtime notification has no item id")
		}
	case agentruntime.EventRequestResolved:
		if event.RequestID == "" {
			return nil, errors.New("codex resolved request has no id")
		}
	}
	return event, nil
}
