// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	switch method {
	case methodItemToolRequestUserInput:
		return map[string]any{"answers": map[string]any{}}, nil
	case methodItemToolCall:
		return map[string]any{"contentItems": []any{}, "success": false}, nil
	}
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
			if protectedPermissionRequest(params.AdditionalPermissions, question.CWD, protectedCodexPaths(c.command)) {
				return deny, nil
			}
			var err error
			question.Permissions, _, err = permissionMembers(params.AdditionalPermissions)
			if err != nil {
				return deny, nil
			}
		}
	case methodItemFileChangeApproval:
		return deny, nil
	case methodItemPermissionsApproval:
		question.Kind = agentruntime.ApprovalPermissions
		question.AllowedScopes = []agentruntime.ApprovalScope{agentruntime.ScopeOnce, agentruntime.ScopeSession}
		if params.CWD == nil || question.CWD == "" {
			return deny, nil
		}
		if protectedPermissionRequest(params.Permissions, question.CWD, protectedCodexPaths(c.command)) {
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

func protectedPermissionRequest(raw json.RawMessage, cwd string, protected []string) bool {
	if len(protected) == 0 {
		return false
	}
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil {
		return true
	}
	filesystem, ok := members["fileSystem"]
	if !ok || string(filesystem) == "null" {
		return false
	}
	var filesystemMembers map[string]json.RawMessage
	if json.Unmarshal(filesystem, &filesystemMembers) != nil {
		return true
	}
	for name := range filesystemMembers {
		if !oneOf(name, "read", "write", "entries", "globScanMaxDepth") {
			return true
		}
	}
	var profile struct {
		FileSystem *struct {
			Read    []string `json:"read"`
			Write   []string `json:"write"`
			Entries []struct {
				Access string `json:"access"`
				Path   struct {
					Type string `json:"type"`
					Path string `json:"path"`
				} `json:"path"`
			} `json:"entries"`
		} `json:"fileSystem"`
	}
	if json.Unmarshal(raw, &profile) != nil || profile.FileSystem == nil {
		return true
	}
	for _, path := range append(profile.FileSystem.Read, profile.FileSystem.Write...) {
		if protectedByAnyPath(path, cwd, protected) {
			return true
		}
	}
	for _, entry := range profile.FileSystem.Entries {
		if entry.Access == "deny" {
			continue
		}
		if entry.Access != "read" && entry.Access != "write" {
			return true
		}
		if entry.Path.Type != "path" || protectedByAnyPath(entry.Path.Path, cwd, protected) {
			return true
		}
	}
	return false
}

func protectedByAnyPath(path, cwd string, protected []string) bool {
	for _, root := range protected {
		if protectedPath(path, cwd, root) {
			return true
		}
	}
	return false
}

func protectedPath(path, cwd, home string) bool {
	if home == "" {
		return false
	}
	if path == "" {
		return true
	}
	if !filepath.IsAbs(path) {
		if cwd == "" || !filepath.IsAbs(cwd) {
			return true
		}
		path = filepath.Join(cwd, path)
	}
	path = resolvedPath(path)
	home = resolvedPath(home)
	if sameFileAncestor(path, home) {
		return true
	}
	relative, err := filepath.Rel(home, path)
	return err != nil || relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func sameFileAncestor(path, home string) bool {
	homeInfo, err := os.Stat(home)
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err == nil && os.SameFile(info, homeInfo) {
			return true
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
	}
}

func resolvedPath(path string) string {
	path = filepath.Clean(path)
	current := path
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := range len(suffix) {
				resolved = filepath.Join(resolved, suffix[len(suffix)-1-i])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
		if !errors.Is(err, os.ErrNotExist) {
			return path
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}
