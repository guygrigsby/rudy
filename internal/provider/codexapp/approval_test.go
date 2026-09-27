// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/agentruntime"
)

type approvalSink struct {
	question agentruntime.ApprovalQuestion
	answer   agentruntime.ApprovalAnswer
	err      error
	calls    int
}

func (*approvalSink) RuntimeEvent(agentruntime.Event)             {}
func (*approvalSink) AccountUpdated(agentruntime.AccountState)    {}
func (*approvalSink) LoginCompleted(agentruntime.LoginCompletion) {}
func (s *approvalSink) RequestApproval(_ context.Context, question agentruntime.ApprovalQuestion) (agentruntime.ApprovalAnswer, error) {
	s.calls++
	s.question = question
	return s.answer, s.err
}

func sendInboundApproval(t *testing.T, sink agentruntime.Sink, payload string) map[string]json.RawMessage {
	t.Helper()
	return sendInboundApprovalWithCommand(t, Command{}, sink, payload)
}

func sendInboundApprovalWithCommand(t *testing.T, command Command, sink agentruntime.Sink, payload string) map[string]json.RawMessage {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	client := NewClient(command)
	client.SetSink(sink)
	t.Cleanup(func() { _ = client.Close() })
	peer := newPeer(clientConn, clientConn, clientConn, client.handleRequest, nil)
	t.Cleanup(func() { _ = peer.Close() })
	t.Cleanup(func() { _ = serverConn.Close() })
	if _, err := serverConn.Write([]byte(payload + "\n")); err != nil {
		t.Fatal(err)
	}
	var response map[string]json.RawMessage
	if err := json.NewDecoder(serverConn).Decode(&response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestCommandApprovalPreservesStringRequestIDAndSessionDecision(t *testing.T) {
	sink := &approvalSink{answer: agentruntime.ApprovalAnswer{Decision: agentruntime.DecisionAllow, Scope: agentruntime.ScopeSession}}
	response := sendInboundApproval(t, sink, `{"id":"42","method":"item/commandExecution/requestApproval","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","startedAtMs":1,"command":"go test ./...","cwd":"/repo","reason":"run tests","networkApprovalContext":{"host":"example.com","protocol":"https"},"availableDecisions":["accept","acceptForSession","decline"]}}`)
	if string(response["id"]) != `"42"` || string(response["result"]) != `{"decision":"acceptForSession"}` {
		t.Fatalf("response = %v", response)
	}
	want := agentruntime.ApprovalQuestion{
		Runtime: "codex", RequestID: `"42"`, ThreadID: "thread-1", TurnID: "turn-1", ItemID: "item-1",
		Kind: agentruntime.ApprovalCommand, Command: "go test ./...", CWD: "/repo", Reason: "run tests",
		Network:       []agentruntime.NetworkPermission{{Host: "example.com", Protocol: "https"}},
		AllowedScopes: []agentruntime.ApprovalScope{agentruntime.ScopeOnce, agentruntime.ScopeSession},
	}
	if sink.calls != 1 || !reflect.DeepEqual(sink.question, want) {
		t.Fatalf("question = %+v, calls = %d; want %+v", sink.question, sink.calls, want)
	}
}

func TestCommandApprovalCarriesScopedAdditionalPermissions(t *testing.T) {
	sink := &approvalSink{answer: agentruntime.ApprovalAnswer{Decision: agentruntime.DecisionAllow, Scope: agentruntime.ScopeOnce}}
	response := sendInboundApproval(t, sink, `{"id":43,"method":"item/commandExecution/requestApproval","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","startedAtMs":1,"command":"touch result","cwd":"/repo","additionalPermissions":{"fileSystem":{"write":["/repo"]},"network":{"enabled":true}},"availableDecisions":["accept","decline"]}}`)
	if string(response["result"]) != `{"decision":"accept"}` {
		t.Fatalf("response = %v", response)
	}
	want := []string{`fileSystem:{"write":["/repo"]}`, `network:{"enabled":true}`}
	if !reflect.DeepEqual(sink.question.Permissions, want) {
		t.Fatalf("permissions = %v, want %v", sink.question.Permissions, want)
	}
}

func TestApprovalRejectsPermissionsInsideCodexHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codex")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	homeJSON, _ := json.Marshal(filepath.Join(home, "config.toml"))
	aliasJSON, _ := json.Marshal(filepath.Join(alias, "rules", "injected.rules"))
	tests := []struct {
		name        string
		method      string
		permissions string
		want        string
	}{
		{"legacy write", methodItemCommandExecutionApproval, `{"fileSystem":{"write":[` + string(homeJSON) + `]}}`, `{"decision":"decline"}`},
		{"explicit entry", methodItemCommandExecutionApproval, `{"fileSystem":{"entries":[{"access":"write","path":{"type":"path","path":` + string(homeJSON) + `}}]}}`, `{"decision":"decline"}`},
		{"symlink", methodItemCommandExecutionApproval, `{"fileSystem":{"read":[` + string(aliasJSON) + `]}}`, `{"decision":"decline"}`},
		{"unknown filesystem member", methodItemCommandExecutionApproval, `{"fileSystem":{"futureGrant":` + string(homeJSON) + `}}`, `{"decision":"decline"}`},
		{"malformed filesystem member", methodItemCommandExecutionApproval, `{"fileSystem":{"entries":"all"}}`, `{"decision":"decline"}`},
		{"permission request", methodItemPermissionsApproval, `{"fileSystem":{"write":[` + string(homeJSON) + `]}}`, `{"permissions":{}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sink := &approvalSink{answer: agentruntime.ApprovalAnswer{Decision: agentruntime.DecisionAllow, Scope: agentruntime.ScopeOnce}}
			field := "additionalPermissions"
			if test.method == methodItemPermissionsApproval {
				field = "permissions"
			}
			payload := `{"id":44,"method":"` + test.method + `","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","startedAtMs":1,"cwd":"/repo","availableDecisions":["accept","decline"],"` + field + `":` + test.permissions + `}}`
			response := sendInboundApprovalWithCommand(t, Command{CodexHome: home}, sink, payload)
			if string(response["result"]) != test.want {
				t.Fatalf("response = %v, want %s", response, test.want)
			}
			if sink.calls != 0 {
				t.Fatalf("protected permission reached asker %d times", sink.calls)
			}
		})
	}
	altHome := filepath.Join(filepath.Dir(home), strings.ToUpper(filepath.Base(home)))
	if altInfo, err := os.Stat(altHome); err == nil {
		homeInfo, statErr := os.Stat(home)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if os.SameFile(altInfo, homeInfo) {
			t.Run("case alias", func(t *testing.T) {
				protectedJSON, _ := json.Marshal(filepath.Join(altHome, "config.toml"))
				sink := &approvalSink{answer: agentruntime.ApprovalAnswer{Decision: agentruntime.DecisionAllow, Scope: agentruntime.ScopeOnce}}
				payload := `{"id":46,"method":"item/commandExecution/requestApproval","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","startedAtMs":1,"cwd":"/repo","availableDecisions":["accept","decline"],"additionalPermissions":{"fileSystem":{"write":[` + string(protectedJSON) + `]}}}}`
				response := sendInboundApprovalWithCommand(t, Command{CodexHome: home}, sink, payload)
				if string(response["result"]) != `{"decision":"decline"}` || sink.calls != 0 {
					t.Fatalf("response = %v, calls = %d", response, sink.calls)
				}
			})
		}
	}
	t.Run("file change root", func(t *testing.T) {
		sink := &approvalSink{answer: agentruntime.ApprovalAnswer{Decision: agentruntime.DecisionAllow, Scope: agentruntime.ScopeOnce}}
		payload := `{"id":45,"method":"item/fileChange/requestApproval","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","startedAtMs":1,"cwd":"/repo","grantRoot":` + string(homeJSON) + `}}`
		response := sendInboundApprovalWithCommand(t, Command{CodexHome: home}, sink, payload)
		if string(response["result"]) != `{"decision":"decline"}` || sink.calls != 0 {
			t.Fatalf("response = %v, calls = %d", response, sink.calls)
		}
	})
}

func TestApprovalAllowsScopedPermissionOutsideCodexHome(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "missing-operator-home"))
	t.Setenv("CODEX_HOME", "")
	home := filepath.Join(root, "codex")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	workspaceJSON, _ := json.Marshal(workspace)
	sink := &approvalSink{answer: agentruntime.ApprovalAnswer{Decision: agentruntime.DecisionAllow, Scope: agentruntime.ScopeOnce}}
	payload := `{"id":47,"method":"item/commandExecution/requestApproval","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","startedAtMs":1,"cwd":` + string(workspaceJSON) + `,"availableDecisions":["accept","decline"],"additionalPermissions":{"fileSystem":{"write":[` + string(workspaceJSON) + `]}}}}`
	response := sendInboundApprovalWithCommand(t, Command{CodexHome: home}, sink, payload)
	if string(response["result"]) != `{"decision":"accept"}` || sink.calls != 1 {
		t.Fatalf("response = %v, calls = %d", response, sink.calls)
	}
}

func TestFileApprovalDeniesOpaqueChanges(t *testing.T) {
	sink := &approvalSink{answer: agentruntime.ApprovalAnswer{Decision: agentruntime.DecisionAllow, Scope: agentruntime.ScopeOnce}}
	response := sendInboundApproval(t, sink, `{"id":42,"method":"item/fileChange/requestApproval","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","startedAtMs":1,"reason":"write file","grantRoot":"/repo"}}`)
	if string(response["id"]) != `42` || string(response["result"]) != `{"decision":"decline"}` {
		t.Fatalf("response = %v", response)
	}
	if sink.calls != 0 {
		t.Fatalf("opaque file approval reached asker %d times", sink.calls)
	}
}

func TestPermissionApprovalGrantsOnlyExactRequestedMembers(t *testing.T) {
	sink := &approvalSink{answer: agentruntime.ApprovalAnswer{
		Decision: agentruntime.DecisionAllow, Scope: agentruntime.ScopeOnce,
		Granted: []string{`network:{"enabled":true}`},
	}}
	response := sendInboundApproval(t, sink, `{"id":7,"method":"item/permissions/requestApproval","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","startedAtMs":1,"cwd":"/repo","permissions":{"fileSystem":{"read":["/repo"]},"network":{"enabled":true}}}}`)
	if string(response["result"]) != `{"permissions":{"network":{"enabled":true}},"scope":"turn"}` {
		t.Fatalf("response = %v", response)
	}
	want := []string{`fileSystem:{"read":["/repo"]}`, `network:{"enabled":true}`}
	if sink.question.RequestID != "7" || sink.question.Kind != agentruntime.ApprovalPermissions || !reflect.DeepEqual(sink.question.Permissions, want) || sink.question.CWD != "/repo" {
		t.Fatalf("question = %+v, want permissions %v", sink.question, want)
	}
}

func TestApprovalFailuresDenyWithoutGranting(t *testing.T) {
	tests := []struct {
		name   string
		method string
		params string
		sink   agentruntime.Sink
		want   string
	}{
		{"no asker", methodItemCommandExecutionApproval, `{"threadId":"t","turnId":"u","itemId":"i","startedAtMs":1}`, nil, `{"decision":"decline"}`},
		{"asker error", methodItemFileChangeApproval, `{"threadId":"t","turnId":"u","itemId":"i","startedAtMs":1}`, &approvalSink{err: errors.New("asker lost")}, `{"decision":"decline"}`},
		{"malformed command", methodItemCommandExecutionApproval, `{"threadId":"","turnId":"u","itemId":"i","startedAtMs":1}`, &approvalSink{answer: agentruntime.ApprovalAnswer{Decision: agentruntime.DecisionAllow, Scope: agentruntime.ScopeOnce}}, `{"decision":"decline"}`},
		{"invalid session scope", methodItemCommandExecutionApproval, `{"threadId":"t","turnId":"u","itemId":"i","startedAtMs":1,"availableDecisions":["accept","decline"]}`, &approvalSink{answer: agentruntime.ApprovalAnswer{Decision: agentruntime.DecisionAllow, Scope: agentruntime.ScopeSession}}, `{"decision":"decline"}`},
		{"no available decision", methodItemCommandExecutionApproval, `{"threadId":"t","turnId":"u","itemId":"i","startedAtMs":1,"availableDecisions":[]}`, &approvalSink{answer: agentruntime.ApprovalAnswer{Decision: agentruntime.DecisionAllow, Scope: agentruntime.ScopeOnce}}, `{"decision":"decline"}`},
		{"unrequested grant", methodItemPermissionsApproval, `{"threadId":"t","turnId":"u","itemId":"i","startedAtMs":1,"cwd":"/repo","permissions":{"network":{"enabled":true}}}`, &approvalSink{answer: agentruntime.ApprovalAnswer{Decision: agentruntime.DecisionAllow, Scope: agentruntime.ScopeOnce, Granted: []string{`fileSystem:{"write":["/"]}`}}}, `{"permissions":{}}`},
		{"permission asker error", methodItemPermissionsApproval, `{"threadId":"t","turnId":"u","itemId":"i","startedAtMs":1,"cwd":"/repo","permissions":{"network":{"enabled":true}}}`, &approvalSink{err: errors.New("asker lost")}, `{"permissions":{}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := sendInboundApproval(t, test.sink, `{"id":1,"method":"`+test.method+`","params":`+test.params+`}`)
			if string(response["result"]) != test.want {
				t.Fatalf("response = %v, want %s", response, test.want)
			}
		})
	}
}

func TestUnsupportedInboundRequestReturnsMethodError(t *testing.T) {
	response := sendInboundApproval(t, &approvalSink{}, `{"id":1,"method":"future/request","params":{}}`)
	var rpcErr wireError
	if err := json.Unmarshal(response["error"], &rpcErr); err != nil || rpcErr.Code != -32601 {
		t.Fatalf("response = %v, err = %v", response, err)
	}
}

// Codex 0.155.1's generated ToolRequestUserInputResponse and
// DynamicToolCallResponse schemas require these fields even on cancellation.
func TestUnsupportedToolRequestsReturnSchemaValidCancellation(t *testing.T) {
	tests := []struct {
		name   string
		method string
		params string
		want   string
	}{
		{"user input", methodItemToolRequestUserInput, `{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","isBlocking":true,"questions":[{"header":"Choose","id":"q1","question":"Continue?"}]}`, `{"answers":{}}`},
		{"dynamic tool", methodItemToolCall, `{"threadId":"thread-1","turnId":"turn-1","callId":"call-1","tool":"outside","arguments":{}}`, `{"contentItems":[],"success":false}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sink := &approvalSink{answer: agentruntime.ApprovalAnswer{Decision: agentruntime.DecisionAllow, Scope: agentruntime.ScopeOnce}}
			response := sendInboundApproval(t, sink, `{"id":1,"method":"`+test.method+`","params":`+test.params+`}`)
			if got := string(response["result"]); got != test.want {
				t.Fatalf("cancellation response = %s, want %s; full response = %v", got, test.want, response)
			}
			if sink.calls != 0 {
				t.Fatalf("unsupported tool request reached approval sink %d times", sink.calls)
			}
		})
	}
}

type orderedApprovalSink struct {
	itemStarted chan struct{}
	releaseItem chan struct{}
	called      chan bool
	registered  atomic.Bool
}

func (s *orderedApprovalSink) RuntimeEvent(event agentruntime.Event) {
	if event.Type == agentruntime.EventItemStarted {
		close(s.itemStarted)
		<-s.releaseItem
		s.registered.Store(true)
	}
}
func (*orderedApprovalSink) AccountUpdated(agentruntime.AccountState)    {}
func (*orderedApprovalSink) LoginCompleted(agentruntime.LoginCompletion) {}
func (s *orderedApprovalSink) RequestApproval(_ context.Context, _ agentruntime.ApprovalQuestion) (agentruntime.ApprovalAnswer, error) {
	s.called <- s.registered.Load()
	return agentruntime.ApprovalAnswer{Decision: agentruntime.DecisionAllow, Scope: agentruntime.ScopeOnce}, nil
}

func TestApprovalWaitsForEarlierItemStartedNotification(t *testing.T) {
	client := NewClient(Command{})
	sink := &orderedApprovalSink{itemStarted: make(chan struct{}), releaseItem: make(chan struct{}), called: make(chan bool, 1)}
	client.SetSink(sink)
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(sink.releaseItem) }); _ = client.Close() })
	client.enqueueNotification(methodItemStarted, json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","item":{"id":"item-1","type":"commandExecution","command":"pwd","cwd":"/repo","status":"inProgress"}}`))
	<-sink.itemStarted
	result := make(chan any, 1)
	go func() {
		response, _ := client.handleRequest(context.Background(), "1", methodItemCommandExecutionApproval, json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","startedAtMs":1}`))
		result <- response
	}()
	select {
	case known := <-sink.called:
		if !known {
			t.Fatal("approval reached sink before item registration")
		}
	case <-time.After(20 * time.Millisecond):
	}
	release.Do(func() { close(sink.releaseItem) })
	select {
	case response := <-result:
		body, err := json.Marshal(response)
		if err != nil || string(body) != `{"decision":"accept"}` {
			t.Fatalf("response = %s, err = %v", body, err)
		}
	case <-time.After(time.Second):
		t.Fatal("approval did not resume after item registration")
	}
}
