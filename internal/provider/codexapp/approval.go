// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/guygrigsby/rudy/internal/agentruntime"
)

type approvalParams struct {
	ThreadID               string             `json:"threadId"`
	TurnID                 string             `json:"turnId"`
	ItemID                 string             `json:"itemId"`
	StartedAt              *int64             `json:"startedAtMs"`
	Command                *string            `json:"command"`
	CWD                    *string            `json:"cwd"`
	Reason                 *string            `json:"reason"`
	GrantRoot              *string            `json:"grantRoot"`
	Kind                   string             `json:"kind"`
	AvailableDecisions     *[]json.RawMessage `json:"availableDecisions"`
	NetworkApprovalContext *struct {
		Host     string `json:"host"`
		Protocol string `json:"protocol"`
	} `json:"networkApprovalContext"`
	AdditionalPermissions json.RawMessage `json:"additionalPermissions"`
	Permissions           json.RawMessage `json:"permissions"`
}

type permissionResponse struct {
	Permissions map[string]json.RawMessage `json:"permissions"`
	Scope       string                     `json:"scope,omitempty"`
}

func (c *Client) handleRequest(ctx context.Context, requestID, method string, raw json.RawMessage) (any, error) {
	if !oneOf(method, methodItemCommandExecutionApproval, methodItemFileChangeApproval, methodItemPermissionsApproval) {
		return nil, &wireError{Code: -32601, Message: "unsupported App Server request"}
	}
	deny := denialResponse(method)
	if !c.awaitNotificationBarrier(ctx) {
		return deny, nil
	}
	var params approvalParams
	if err := json.Unmarshal(raw, &params); err != nil || params.ThreadID == "" || params.TurnID == "" || params.ItemID == "" || params.StartedAt == nil || requestID == "" {
		return deny, nil
	}
	question := agentruntime.ApprovalQuestion{
		Runtime: c.Name(), RequestID: requestID, ThreadID: params.ThreadID,
		TurnID: params.TurnID, ItemID: params.ItemID,
	}
	if params.Reason != nil {
		question.Reason = *params.Reason
	}
	if params.CWD != nil {
		question.CWD = *params.CWD
	}
	var requested map[string]json.RawMessage
	switch method {
	case methodItemCommandExecutionApproval:
		question.Kind = agentruntime.ApprovalCommand
		if params.Kind != "" && !oneOf(params.Kind, "command", "writeStdin") {
			return deny, nil
		}
		if params.Command != nil {
			question.Command = *params.Command
		}
		question.AllowedScopes = []agentruntime.ApprovalScope{agentruntime.ScopeOnce}
		if params.AvailableDecisions != nil {
			question.AllowedScopes = nil
			for _, decision := range *params.AvailableDecisions {
				var choice string
				if json.Unmarshal(decision, &choice) == nil {
					switch choice {
					case "accept":
						question.AllowedScopes = append(question.AllowedScopes, agentruntime.ScopeOnce)
					case "acceptForSession":
						question.AllowedScopes = append(question.AllowedScopes, agentruntime.ScopeSession)
					}
				}
			}
		}
		if params.NetworkApprovalContext != nil {
			network := params.NetworkApprovalContext
			if network.Host == "" || !oneOf(network.Protocol, "http", "https", "socks5Tcp", "socks5Udp") {
				return deny, nil
			}
			question.Network = []agentruntime.NetworkPermission{{Host: network.Host, Protocol: network.Protocol}}
		}
		if len(params.AdditionalPermissions) > 0 && string(params.AdditionalPermissions) != "null" {
			var err error
			question.Permissions, _, err = permissionMembers(params.AdditionalPermissions)
			if err != nil {
				return deny, nil
			}
		}
	case methodItemFileChangeApproval:
		question.Kind = agentruntime.ApprovalFileChange
		question.AllowedScopes = []agentruntime.ApprovalScope{agentruntime.ScopeOnce, agentruntime.ScopeSession}
		if params.GrantRoot != nil {
			question.Summary = *params.GrantRoot
		}
	case methodItemPermissionsApproval:
		question.Kind = agentruntime.ApprovalPermissions
		question.AllowedScopes = []agentruntime.ApprovalScope{agentruntime.ScopeOnce, agentruntime.ScopeSession}
		if params.CWD == nil || question.CWD == "" {
			return deny, nil
		}
		var err error
		question.Permissions, requested, err = permissionMembers(params.Permissions)
		if err != nil || len(question.Permissions) == 0 {
			return deny, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return deny, nil
	}
	sink := c.currentSink()
	if sink == nil {
		return deny, nil
	}
	answer, err := sink.RequestApproval(ctx, question)
	if err != nil || ctx.Err() != nil || answer.Decision != agentruntime.DecisionAllow {
		return deny, nil
	}
	if !oneOf(string(answer.Scope), string(agentruntime.ScopeOnce), string(agentruntime.ScopeSession)) || !hasScope(question.AllowedScopes, answer.Scope) {
		return deny, nil
	}
	if method != methodItemPermissionsApproval {
		decision := "accept"
		if answer.Scope == agentruntime.ScopeSession {
			decision = "acceptForSession"
		}
		return struct {
			Decision string `json:"decision"`
		}{Decision: decision}, nil
	}
	if len(answer.Granted) == 0 {
		return deny, nil
	}
	granted := make(map[string]json.RawMessage, len(answer.Granted))
	for _, member := range answer.Granted {
		name, _, ok := strings.Cut(member, ":")
		if !ok || string(requested[name]) != member[len(name)+1:] || granted[name] != nil {
			return deny, nil
		}
		granted[name] = requested[name]
	}
	scope := "turn"
	if answer.Scope == agentruntime.ScopeSession {
		scope = "session"
	}
	return permissionResponse{Permissions: granted, Scope: scope}, nil
}

func (c *Client) awaitNotificationBarrier(ctx context.Context) bool {
	delivered := make(chan struct{})
	select {
	case c.notifications <- wireNotification{barrier: delivered}:
	case <-ctx.Done():
		return false
	case <-c.dispatchStop:
		return false
	}
	select {
	case <-delivered:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	case <-c.dispatchStop:
		return false
	}
}

func denialResponse(method string) any {
	if method == methodItemPermissionsApproval {
		return permissionResponse{Permissions: map[string]json.RawMessage{}}
	}
	return struct {
		Decision string `json:"decision"`
	}{Decision: "decline"}
}

func hasScope(scopes []agentruntime.ApprovalScope, scope agentruntime.ApprovalScope) bool {
	for _, allowed := range scopes {
		if allowed == scope {
			return true
		}
	}
	return false
}

func permissionMembers(raw json.RawMessage) ([]string, map[string]json.RawMessage, error) {
	var profile map[string]json.RawMessage
	if err := json.Unmarshal(raw, &profile); err != nil || profile == nil {
		return nil, nil, errors.New("codex permission profile is malformed")
	}
	for name := range profile {
		if !oneOf(name, "fileSystem", "network") {
			return nil, nil, errors.New("codex permission profile has unknown member")
		}
	}
	members := make([]string, 0, len(profile))
	requested := make(map[string]json.RawMessage, len(profile))
	for _, name := range []string{"fileSystem", "network"} {
		value := profile[name]
		if len(value) == 0 || string(value) == "null" {
			continue
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(value, &body); err != nil || body == nil {
			return nil, nil, errors.New("codex permission member is malformed")
		}
		requested[name] = value
		members = append(members, name+":"+string(value))
	}
	return members, requested, nil
}
