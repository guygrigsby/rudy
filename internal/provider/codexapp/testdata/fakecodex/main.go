// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const approvalRequestID = 9001

type envelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

type fakeState struct {
	Authenticated bool                  `json:"authenticated"`
	Threads       map[string]fakeThread `json:"threads"`
}

type fakeThread struct {
	ID    string     `json:"id"`
	Turns []fakeTurn `json:"turns"`
}

type fakeTurn struct {
	ID     string            `json:"id"`
	Status string            `json:"status"`
	Items  []json.RawMessage `json:"items"`
}

type fakeServer struct {
	writer *json.Encoder
	log    *os.File
	path   string

	writeMu sync.Mutex
	stateMu sync.Mutex
	state   fakeState

	approvalMu sync.Mutex
	approval   chan json.RawMessage
}

func main() {
	if forbidden := os.Getenv("FAKE_CODEX_FORBID_ENV"); forbidden != "" && os.Getenv(forbidden) != "" {
		fmt.Fprintf(os.Stderr, "forbidden environment variable %s\n", forbidden)
		os.Exit(97)
	}
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		version := os.Getenv("FAKE_CODEX_VERSION")
		if version == "" {
			version = "0.155.1"
		}
		fmt.Println("codex-cli " + version)
		return
	}
	if len(os.Args) != 3 || os.Args[1] != "app-server" || os.Args[2] != "--stdio" {
		fmt.Fprintln(os.Stderr, "unexpected argv")
		os.Exit(2)
	}
	if envPath := os.Getenv("FAKE_CODEX_ENV_LOG"); envPath != "" {
		payload, err := json.Marshal(os.Environ())
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(envPath, payload, 0o600); err != nil {
			panic(err)
		}
	}
	server, err := newFakeServer()
	if err != nil {
		panic(err)
	}
	defer server.log.Close()

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var message envelope
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			panic(err)
		}
		if message.Method == "" {
			server.answerInbound(message)
			continue
		}
		if _, err := fmt.Fprintln(server.log, message.Method); err != nil {
			panic(err)
		}
		if message.Method == os.Getenv("FAKE_CODEX_DROP") {
			return
		}
		server.handle(message)
	}
	if err := scanner.Err(); err != nil {
		panic(err)
	}
}

func newFakeServer() (*fakeServer, error) {
	logPath := fakeDataPath("FAKE_CODEX_LOG", "fake-codex.methods")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	server := &fakeServer{writer: json.NewEncoder(os.Stdout), log: log, path: fakeDataPath("FAKE_CODEX_STATE", "fake-codex-state.json")}
	server.state.Threads = map[string]fakeThread{}
	if server.path != "" {
		body, err := os.ReadFile(server.path)
		if err == nil {
			if err := json.Unmarshal(body, &server.state); err != nil {
				_ = log.Close()
				return nil, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			_ = log.Close()
			return nil, err
		}
	}
	if server.state.Threads == nil {
		server.state.Threads = map[string]fakeThread{}
	}
	return server, nil
}

func fakeDataPath(environmentName, fileName string) string {
	if path := os.Getenv(environmentName); path != "" {
		return path
	}
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Join(home, fileName)
	}
	return filepath.Join(os.TempDir(), "rudy-"+fileName)
}

func (s *fakeServer) handle(message envelope) {
	switch message.Method {
	case "initialize":
		if err := validateInitialize(message.Params); err != nil {
			s.writeError(message.ID, err)
			return
		}
		s.write(message.ID, map[string]any{
			"codexHome": "/tmp/codex", "platformFamily": "unix",
			"platformOs": "macos", "userAgent": "fake-codex",
		})
	case "initialized":
	case "account/read":
		s.stateMu.Lock()
		authenticated := s.state.Authenticated
		s.stateMu.Unlock()
		if authenticated {
			s.write(message.ID, map[string]any{"account": map[string]any{"type": "chatgpt", "planType": "plus"}, "requiresOpenaiAuth": true})
		} else {
			s.write(message.ID, map[string]any{"account": nil, "requiresOpenaiAuth": true})
		}
	case "account/login/start":
		s.startLogin(message)
	case "account/login/cancel":
		s.write(message.ID, map[string]any{"status": "canceled"})
	case "model/list":
		s.writeModels(message)
	case "thread/start":
		if err := validateStrictThreadStart(message.Params); err != nil {
			s.writeError(message.ID, err)
			return
		}
		thread := fakeThread{ID: "thread-1"}
		s.storeThread(thread)
		if invalidMutation(message.Method) {
			thread.ID = ""
		}
		s.write(message.ID, map[string]any{"thread": thread})
	case "thread/resume":
		if err := validateStrictThreadRestore(message.Params); err != nil {
			s.writeError(message.ID, err)
			return
		}
		thread := s.threadFrom(message.Params)
		s.write(message.ID, map[string]any{"thread": thread})
	case "thread/read":
		thread := s.threadFrom(message.Params)
		if os.Getenv("FAKE_CODEX_READ_WRONG") != "" {
			thread.ID = "other-thread"
		}
		s.write(message.ID, map[string]any{"thread": thread})
	case "turn/start":
		if err := validateStrictTurnStart(message.Params); err != nil {
			s.writeError(message.ID, err)
			return
		}
		if invalidMutation(message.Method) {
			s.write(message.ID, map[string]any{"turn": map[string]any{}})
			return
		}
		s.startTurn(message)
	case "turn/interrupt":
		s.write(message.ID, map[string]any{})
	case "turn/steer":
		turnID := "turn-1"
		if invalidMutation(message.Method) {
			turnID = "other-turn"
		}
		s.write(message.ID, map[string]any{"turnId": turnID})
	case "thread/fork":
		if err := validateStrictThreadRestore(message.Params); err != nil {
			s.writeError(message.ID, err)
			return
		}
		thread := fakeThread{ID: "thread-fork"}
		s.storeThread(thread)
		if invalidMutation(message.Method) {
			thread.ID = ""
		}
		s.write(message.ID, map[string]any{"thread": thread})
	}
}

func invalidMutation(method string) bool {
	return os.Getenv("FAKE_CODEX_INVALID_MUTATION") == method
}

func (s *fakeServer) startLogin(message envelope) {
	if os.Getenv("FAKE_CODEX_LOGIN_BLOCK") != "" {
		time.Sleep(24 * time.Hour)
	}
	var params struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(message.Params, &params); err != nil {
		panic(err)
	}
	loginID := os.Getenv("FAKE_CODEX_LOGIN_EARLY")
	if loginID == "" {
		loginID = "login-browser"
		if params.Type == "chatgptDeviceCode" {
			loginID = "login-device"
		}
	}
	early := os.Getenv("FAKE_CODEX_LOGIN_EARLY") != ""
	if early {
		s.stateMu.Lock()
		s.state.Authenticated = true
		s.persistLocked()
		s.stateMu.Unlock()
	}
	if early {
		s.notify("account/login/completed", map[string]any{"loginId": loginID, "success": true})
	}
	if params.Type == "chatgptDeviceCode" {
		s.write(message.ID, map[string]any{
			"type": "chatgptDeviceCode", "loginId": loginID,
			"verificationUrl": "https://auth.openai.com/device", "userCode": "OPENAI-CODE",
		})
		if !early && os.Getenv("FAKE_CODEX_HOLD_LOGIN_COMPLETION") == "" {
			s.stateMu.Lock()
			s.state.Authenticated = true
			s.persistLocked()
			s.stateMu.Unlock()
			s.notify("account/login/completed", map[string]any{"loginId": loginID, "success": true})
		}
		return
	}
	s.write(message.ID, map[string]any{
		"type": "chatgpt", "loginId": loginID,
		"authUrl": "https://auth.openai.com/oauth?code=fake",
	})
}

func (s *fakeServer) startTurn(message envelope) {
	var params struct {
		ThreadID string `json:"threadId"`
		Input    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"input"`
	}
	if err := json.Unmarshal(message.Params, &params); err != nil {
		panic(err)
	}
	turn := fakeTurn{ID: "turn-1", Status: "inProgress"}
	s.notify("turn/started", map[string]any{
		"threadId": params.ThreadID,
		"turn":     map[string]any{"id": turn.ID, "status": "inProgress", "items": []any{}},
	})
	s.write(message.ID, map[string]any{"turn": turn})

	approval := make(chan json.RawMessage, 1)
	s.approvalMu.Lock()
	s.approval = approval
	s.approvalMu.Unlock()
	go func() {
		command := map[string]any{
			"id": "command-1", "type": "commandExecution", "command": "printf fake",
			"cwd": "/repo", "status": "inProgress",
		}
		s.notify("item/started", map[string]any{"threadId": params.ThreadID, "turnId": turn.ID, "item": command})
		s.request(approvalRequestID, "item/commandExecution/requestApproval", map[string]any{
			"threadId": params.ThreadID, "turnId": turn.ID, "itemId": "command-1",
			"startedAtMs": 1, "command": "printf fake", "cwd": "/repo",
			"reason": "Run fake command", "availableDecisions": []string{"accept", "decline"},
		})
		<-approval
		s.notify("serverRequest/resolved", map[string]any{"threadId": params.ThreadID, "requestId": approvalRequestID})
		s.notify("item/commandExecution/outputDelta", map[string]any{
			"threadId": params.ThreadID, "turnId": turn.ID, "itemId": "command-1", "delta": "fake",
		})
		command["status"] = "completed"
		command["aggregatedOutput"] = "fake"
		s.notify("item/completed", map[string]any{"threadId": params.ThreadID, "turnId": turn.ID, "item": command})

		agent := map[string]any{"id": "agent-1", "type": "agentMessage", "text": ""}
		s.notify("item/started", map[string]any{"threadId": params.ThreadID, "turnId": turn.ID, "item": agent})
		s.notify("item/agentMessage/delta", map[string]any{
			"threadId": params.ThreadID, "turnId": turn.ID, "itemId": "agent-1", "delta": "completed by ",
		})
		s.notify("item/agentMessage/delta", map[string]any{
			"threadId": params.ThreadID, "turnId": turn.ID, "itemId": "agent-1", "delta": "fake Codex",
		})
		agent["text"] = "completed by fake Codex"
		s.notify("item/completed", map[string]any{"threadId": params.ThreadID, "turnId": turn.ID, "item": agent})
		s.notify("thread/tokenUsage/updated", map[string]any{
			"threadId": params.ThreadID, "turnId": turn.ID,
			"tokenUsage": map[string]any{"last": map[string]any{
				"inputTokens": 4, "cachedInputTokens": 0, "outputTokens": 4,
				"reasoningOutputTokens": 0, "totalTokens": 8, "cacheWriteInputTokens": 0,
			}},
		})

		var userText string
		if len(params.Input) > 0 {
			userText = params.Input[0].Text
		}
		user, _ := json.Marshal(map[string]any{
			"id": "user-1", "type": "userMessage",
			"content": []any{map[string]any{"type": "text", "text": userText}},
		})
		commandRaw, _ := json.Marshal(command)
		agentRaw, _ := json.Marshal(agent)
		turn.Status = "completed"
		turn.Items = []json.RawMessage{user, commandRaw, agentRaw}
		s.stateMu.Lock()
		thread := s.state.Threads[params.ThreadID]
		thread.ID = params.ThreadID
		thread.Turns = []fakeTurn{turn}
		s.state.Threads[params.ThreadID] = thread
		s.persistLocked()
		s.stateMu.Unlock()
		s.notify("turn/completed", map[string]any{
			"threadId": params.ThreadID,
			"turn":     map[string]any{"id": turn.ID, "status": "completed", "items": []any{}},
		})
	}()
}

func (s *fakeServer) answerInbound(message envelope) {
	if string(message.ID) != fmt.Sprint(approvalRequestID) {
		return
	}
	s.approvalMu.Lock()
	approval := s.approval
	s.approval = nil
	s.approvalMu.Unlock()
	if approval != nil {
		approval <- message.Result
	}
}

func (s *fakeServer) threadFrom(raw json.RawMessage) fakeThread {
	var params struct {
		ThreadID string `json:"threadId"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		panic(err)
	}
	if params.ThreadID == "" {
		params.ThreadID = "thread-1"
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	thread, ok := s.state.Threads[params.ThreadID]
	if !ok {
		thread = fakeThread{ID: params.ThreadID}
	}
	return thread
}

func (s *fakeServer) storeThread(thread fakeThread) {
	s.stateMu.Lock()
	s.state.Threads[thread.ID] = thread
	s.persistLocked()
	s.stateMu.Unlock()
}

func (s *fakeServer) persistLocked() {
	if s.path == "" {
		return
	}
	body, err := json.Marshal(s.state)
	if err != nil {
		panic(err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		panic(err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		panic(err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		panic(err)
	}
}

func (s *fakeServer) notify(method string, params any) {
	s.writeEnvelope(map[string]any{"method": method, "params": params})
}

func (s *fakeServer) request(id int, method string, params any) {
	s.writeEnvelope(map[string]any{"id": id, "method": method, "params": params})
}

func (s *fakeServer) write(id json.RawMessage, result any) {
	s.writeEnvelope(map[string]any{"id": id, "result": result})
}

func (s *fakeServer) writeError(id json.RawMessage, err error) {
	s.writeEnvelope(map[string]any{"id": id, "error": map[string]any{"code": -32602, "message": err.Error()}})
}

func validateInitialize(raw json.RawMessage) error {
	var params struct {
		Capabilities struct {
			ExperimentalAPI bool `json:"experimentalApi"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return err
	}
	if !params.Capabilities.ExperimentalAPI {
		return errors.New("experimental API capability missing")
	}
	return nil
}

func validateStrictThreadStart(raw json.RawMessage) error {
	var params struct {
		ApprovalPolicy    string         `json:"approvalPolicy"`
		ApprovalsReviewer string         `json:"approvalsReviewer"`
		Sandbox           string         `json:"sandbox"`
		Permissions       string         `json:"permissions"`
		Config            map[string]any `json:"config"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return err
	}
	if params.ApprovalPolicy != "on-request" || params.ApprovalsReviewer != "user" || params.Sandbox != "" || params.Permissions != "rudy_strict" ||
		params.Config["features.apps"] != false || params.Config["features.exec_permission_approvals"] != true ||
		params.Config["features.plugins"] != false || params.Config["features.remote_plugin"] != false ||
		params.Config["features.request_permissions_tool"] != true {
		return fmt.Errorf("strict thread policy missing: %+v", params)
	}
	return nil
}

func validateStrictThreadRestore(raw json.RawMessage) error {
	var params struct {
		ApprovalPolicy    string         `json:"approvalPolicy"`
		ApprovalsReviewer string         `json:"approvalsReviewer"`
		Permissions       string         `json:"permissions"`
		Sandbox           string         `json:"sandbox"`
		Config            map[string]any `json:"config"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return err
	}
	if params.ApprovalPolicy != "on-request" || params.ApprovalsReviewer != "user" || params.Permissions != "rudy_strict" || params.Sandbox != "" ||
		params.Config["features.apps"] != false || params.Config["features.exec_permission_approvals"] != true ||
		params.Config["features.plugins"] != false || params.Config["features.remote_plugin"] != false ||
		params.Config["features.request_permissions_tool"] != true {
		return fmt.Errorf("strict restored thread policy missing: %+v", params)
	}
	return nil
}

func validateStrictTurnStart(raw json.RawMessage) error {
	var params struct {
		ApprovalPolicy    string          `json:"approvalPolicy"`
		ApprovalsReviewer string          `json:"approvalsReviewer"`
		Permissions       string          `json:"permissions"`
		SandboxPolicy     json.RawMessage `json:"sandboxPolicy"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return err
	}
	if params.ApprovalPolicy != "on-request" || params.ApprovalsReviewer != "user" || params.Permissions != "rudy_strict" || len(params.SandboxPolicy) != 0 {
		return fmt.Errorf("strict turn policy missing: %+v", params)
	}
	return nil
}

func (s *fakeServer) writeEnvelope(value any) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.writer.Encode(value); err != nil {
		panic(err)
	}
}

func (s *fakeServer) writeModels(message envelope) {
	if os.Getenv("FAKE_CODEX_MODELS") != "pages" {
		s.write(message.ID, map[string]any{"data": []any{model("gpt-test", "GPT Test", []string{"text"}, []string{"low", "medium", "high"})}})
		return
	}
	var params struct {
		Cursor *string `json:"cursor"`
	}
	if err := json.Unmarshal(message.Params, &params); err != nil {
		panic(err)
	}
	if params.Cursor == nil {
		s.write(message.ID, map[string]any{
			"data":       []any{model("gpt-a", "GPT A", []string{"text", "image"}, []string{"low", "medium", "high"})},
			"nextCursor": "page-2",
		})
		return
	}
	s.write(message.ID, map[string]any{
		"data": []any{model("gpt-b", "GPT B", []string{"text"}, []string{"minimal"})},
	})
}

func model(id, name string, modalities, efforts []string) map[string]any {
	options := make([]map[string]string, 0, len(efforts))
	for _, effort := range efforts {
		options = append(options, map[string]string{"reasoningEffort": effort, "description": effort})
	}
	return map[string]any{
		"id": id, "model": id, "displayName": name, "description": name,
		"hidden": false, "isDefault": false, "defaultReasoningEffort": efforts[0],
		"inputModalities": modalities, "supportedReasoningEfforts": options,
	}
}
