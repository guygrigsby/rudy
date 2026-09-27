// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
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
	clientConn, serverConn := net.Pipe()
	client := NewClient(Command{})
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

func TestFileApprovalPreservesNumericRequestIDAndMapsDecisions(t *testing.T) {
	tests := []struct {
		name     string
		answer   agentruntime.ApprovalAnswer
		decision string
	}{
		{"once", agentruntime.ApprovalAnswer{Decision: agentruntime.DecisionAllow, Scope: agentruntime.ScopeOnce}, "accept"},
		{"session", agentruntime.ApprovalAnswer{Decision: agentruntime.DecisionAllow, Scope: agentruntime.ScopeSession}, "acceptForSession"},
		{"deny", agentruntime.ApprovalAnswer{Decision: agentruntime.DecisionDeny, Scope: agentruntime.ScopeOnce}, "decline"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sink := &approvalSink{answer: test.answer}
			response := sendInboundApproval(t, sink, `{"id":42,"method":"item/fileChange/requestApproval","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","startedAtMs":1,"reason":"write file","grantRoot":"/repo"}}`)
			if string(response["id"]) != `42` || string(response["result"]) != `{"decision":"`+test.decision+`"}` {
				t.Fatalf("response = %v", response)
			}
			if sink.question.RequestID != "42" || sink.question.Kind != agentruntime.ApprovalFileChange || sink.question.Reason != "write file" || !reflect.DeepEqual(sink.question.AllowedScopes, []agentruntime.ApprovalScope{agentruntime.ScopeOnce, agentruntime.ScopeSession}) {
				t.Fatalf("question = %+v", sink.question)
			}
		})
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
