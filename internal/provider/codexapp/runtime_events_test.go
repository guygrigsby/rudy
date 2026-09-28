// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/agentruntime"
	"github.com/guygrigsby/rudy/internal/session"
)

type collectingRuntimeSink struct{ events []agentruntime.Event }

func (s *collectingRuntimeSink) RuntimeEvent(event agentruntime.Event) {
	s.events = append(s.events, event)
}
func (*collectingRuntimeSink) AccountUpdated(agentruntime.AccountState)    {}
func (*collectingRuntimeSink) LoginCompleted(agentruntime.LoginCompletion) {}
func (*collectingRuntimeSink) RequestApproval(context.Context, agentruntime.ApprovalQuestion) (agentruntime.ApprovalAnswer, error) {
	return agentruntime.ApprovalAnswer{}, nil
}

func TestRuntimeNotificationsTranslateEveryProjectionVariant(t *testing.T) {
	client := NewClient(Command{})
	t.Cleanup(func() { _ = client.Close() })
	sink := &collectingRuntimeSink{}
	client.SetSink(sink)
	tests := []struct {
		method string
		params string
		want   agentruntime.Event
	}{
		{methodThreadStarted, `{"thread":{"id":"thread-1"}}`, agentruntime.Event{Type: agentruntime.EventThreadStarted, ThreadID: "thread-1"}},
		{methodThreadStatusChanged, `{"threadId":"thread-1","status":{"type":"active","activeFlags":[]}}`, agentruntime.Event{Type: agentruntime.EventThreadStatus, ThreadID: "thread-1", Status: "active"}},
		{methodTurnStarted, `{"threadId":"thread-1","turn":{"id":"turn-1","status":"inProgress","items":[]}}`, agentruntime.Event{Type: agentruntime.EventTurnStarted, ThreadID: "thread-1", TurnID: "turn-1", Status: "running"}},
		{methodItemStarted, `{"threadId":"thread-1","turnId":"turn-1","item":{"id":"item-1","type":"agentMessage","text":"Hi"}}`, agentruntime.Event{Type: agentruntime.EventItemStarted, ThreadID: "thread-1", TurnID: "turn-1", ItemID: "item-1", Item: agentruntime.Item{ItemID: "item-1", Type: agentruntime.ItemAgentMessage, Content: []session.Block{session.TextBlock("Hi")}}}},
		{methodItemAgentMessageDelta, `{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":" there"}`, agentruntime.Event{Type: agentruntime.EventItemDelta, ThreadID: "thread-1", TurnID: "turn-1", ItemID: "item-1", Text: " there"}},
		{methodItemCommandExecutionOutputDelta, `{"threadId":"thread-1","turnId":"turn-1","itemId":"item-2","delta":"stdout"}`, agentruntime.Event{Type: agentruntime.EventItemDelta, ThreadID: "thread-1", TurnID: "turn-1", ItemID: "item-2", Text: "stdout"}},
		{methodItemFileChangeOutputDelta, `{"threadId":"thread-1","turnId":"turn-1","itemId":"item-3","delta":"patch output"}`, agentruntime.Event{Type: agentruntime.EventItemDelta, ThreadID: "thread-1", TurnID: "turn-1", ItemID: "item-3", Text: "patch output"}},
		{methodItemMCPToolCallProgress, `{"threadId":"thread-1","turnId":"turn-1","itemId":"item-4","message":"fetching"}`, agentruntime.Event{Type: agentruntime.EventItemDelta, ThreadID: "thread-1", TurnID: "turn-1", ItemID: "item-4", Text: "fetching", Status: "replace"}},
		{methodItemPlanDelta, `{"threadId":"thread-1","turnId":"turn-1","itemId":"item-5","delta":"step"}`, agentruntime.Event{Type: agentruntime.EventItemDelta, ThreadID: "thread-1", TurnID: "turn-1", ItemID: "item-5", Text: "step"}},
		{methodItemReasoningSummaryPartAdded, `{"threadId":"thread-1","turnId":"turn-1","itemId":"item-6","summaryIndex":1}`, agentruntime.Event{Type: agentruntime.EventItemDelta, ThreadID: "thread-1", TurnID: "turn-1", ItemID: "item-6", Text: "\n"}},
		{methodItemReasoningSummaryTextDelta, `{"threadId":"thread-1","turnId":"turn-1","itemId":"item-6","summaryIndex":1,"delta":"summary"}`, agentruntime.Event{Type: agentruntime.EventItemDelta, ThreadID: "thread-1", TurnID: "turn-1", ItemID: "item-6", Text: "summary"}},
		{methodItemReasoningTextDelta, `{"threadId":"thread-1","turnId":"turn-1","itemId":"item-6","contentIndex":0,"delta":"thought"}`, agentruntime.Event{Type: agentruntime.EventItemDelta, ThreadID: "thread-1", TurnID: "turn-1", ItemID: "item-6", Text: "thought"}},
		{methodItemFileChangePatchUpdated, `{"threadId":"thread-1","turnId":"turn-1","itemId":"item-3","changes":[{"path":"a.go","kind":{"type":"update"},"diff":"@@ -1 +1 @@"}]}`, agentruntime.Event{Type: agentruntime.EventItemDelta, ThreadID: "thread-1", TurnID: "turn-1", ItemID: "item-3", Text: "@@ -1 +1 @@", Status: "replace"}},
		{methodItemCompleted, `{"threadId":"thread-1","turnId":"turn-1","item":{"id":"item-1","type":"agentMessage","text":"Hi there"}}`, agentruntime.Event{Type: agentruntime.EventItemCompleted, ThreadID: "thread-1", TurnID: "turn-1", ItemID: "item-1", Item: agentruntime.Item{ItemID: "item-1", Type: agentruntime.ItemAgentMessage, Content: []session.Block{session.TextBlock("Hi there")}}}},
		{methodTurnDiffUpdated, `{"threadId":"thread-1","turnId":"turn-1","diff":"@@ diff"}`, agentruntime.Event{Type: agentruntime.EventDiffUpdated, ThreadID: "thread-1", TurnID: "turn-1", Text: "@@ diff"}},
		{methodTurnPlanUpdated, `{"threadId":"thread-1","turnId":"turn-1","plan":[{"step":"Build","status":"completed"},{"step":"Test","status":"inProgress"}],"explanation":null}`, agentruntime.Event{Type: agentruntime.EventPlanUpdated, ThreadID: "thread-1", TurnID: "turn-1", Text: "[completed] Build\n[inProgress] Test"}},
		{"thread/tokenUsage/updated", `{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"last":{"inputTokens":12,"cachedInputTokens":3,"outputTokens":5,"reasoningOutputTokens":2,"totalTokens":17,"cacheWriteInputTokens":1},"total":{"inputTokens":99,"cachedInputTokens":9,"outputTokens":80,"reasoningOutputTokens":20,"totalTokens":179}}}`, agentruntime.Event{Type: agentruntime.EventUsageUpdated, ThreadID: "thread-1", TurnID: "turn-1", Usage: session.Usage{Input: 12, Output: 5, CacheRead: 3, CacheWrite: 1}}},
		{methodTurnCompleted, `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[]}}`, agentruntime.Event{Type: agentruntime.EventTurnCompleted, ThreadID: "thread-1", TurnID: "turn-1", Status: "completed"}},
		{methodError, `{"threadId":"thread-1","turnId":"turn-1","error":{"message":"retry unavailable"},"willRetry":false}`, agentruntime.Event{Type: agentruntime.EventError, ThreadID: "thread-1", TurnID: "turn-1", Text: "retry unavailable"}},
		{methodServerRequestResolved, `{"threadId":"thread-1","requestId":"request-1"}`, agentruntime.Event{Type: agentruntime.EventRequestResolved, ThreadID: "thread-1", RequestID: `"request-1"`}},
	}
	for index, test := range tests {
		client.handleNotification(wireNotification{method: test.method, params: json.RawMessage(test.params)})
		if len(sink.events) != index+1 {
			t.Fatalf("%s emitted %d events, want %d", test.method, len(sink.events), index+1)
		}
		got := sink.events[index]
		if got.Sequence != uint64(index+1) {
			t.Fatalf("%s sequence = %d, want %d", test.method, got.Sequence, index+1)
		}
		got.Sequence = 0
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%s event = %+v, want %+v", test.method, got, test.want)
		}
	}
}

func TestRuntimeNotificationsRejectMalformedBindingsAndUnknownTypes(t *testing.T) {
	client := NewClient(Command{})
	t.Cleanup(func() { _ = client.Close() })
	sink := &collectingRuntimeSink{}
	client.SetSink(sink)
	for _, params := range []string{
		`{"threadId":"","turnId":"turn-1","itemId":"item-1","delta":"text"}`,
		`{"threadId":"thread-1","turnId":"","itemId":"item-1","delta":"text"}`,
		`{"threadId":"thread-1","turnId":"turn-1","itemId":"","delta":"text"}`,
		`{"threadId":"thread-1","turnId":"turn-1","item":{"id":"item-1","type":"futureItem"}}`,
		`{"threadId":"thread-1","turnId":"turn-1","item":{"id":"","type":"agentMessage","text":"text"}}`,
	} {
		client.handleNotification(wireNotification{method: methodItemCompleted, params: json.RawMessage(params)})
		client.handleNotification(wireNotification{method: methodItemAgentMessageDelta, params: json.RawMessage(params)})
	}
	if len(sink.events) != 0 {
		t.Fatalf("malformed notifications emitted events: %+v", sink.events)
	}
}

func TestColdThreadRejectsMissingDuplicateAndUnknownIdentity(t *testing.T) {
	base := wireThread{ID: "thread-1", Turns: []wireTurn{{ID: "turn-1", Status: "completed", Items: []json.RawMessage{json.RawMessage(`{"id":"item-1","type":"agentMessage","text":"hi"}`)}}}}
	tests := []struct {
		name string
		edit func(*wireThread)
	}{
		{"empty turn", func(thread *wireThread) { thread.Turns[0].ID = "" }},
		{"duplicate turn", func(thread *wireThread) { thread.Turns = append(thread.Turns, thread.Turns[0]) }},
		{"unknown turn status", func(thread *wireThread) { thread.Turns[0].Status = "unknown" }},
		{"empty item", func(thread *wireThread) {
			thread.Turns[0].Items[0] = json.RawMessage(`{"id":"","type":"agentMessage","text":"hi"}`)
		}},
		{"duplicate item", func(thread *wireThread) {
			thread.Turns[0].Items = append(thread.Turns[0].Items, thread.Turns[0].Items[0])
		}},
		{"unknown item", func(thread *wireThread) {
			thread.Turns[0].Items[0] = json.RawMessage(`{"id":"item-1","type":"futureItem"}`)
		}},
		{"unknown item status", func(thread *wireThread) {
			thread.Turns[0].Items[0] = json.RawMessage(`{"id":"item-1","type":"commandExecution","command":"pwd","cwd":"/tmp","status":"future"}`)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			thread := base
			thread.Turns = append([]wireTurn(nil), base.Turns...)
			thread.Turns[0].Items = append([]json.RawMessage(nil), base.Turns[0].Items...)
			test.edit(&thread)
			if _, err := translateThread(thread); err == nil {
				t.Fatal("translateThread accepted invalid history")
			}
		})
	}
}

func TestColdThreadMapsKnownItemsAndUsage(t *testing.T) {
	thread, err := translateThread(wireThread{ID: "thread-1", Turns: []wireTurn{{ID: "turn-1", Status: "inProgress", Items: []json.RawMessage{
		json.RawMessage(`{"id":"user-1","type":"userMessage","content":[{"type":"text","text":"hello"}]}`),
		json.RawMessage(`{"id":"tool-1","type":"mcpToolCall","server":"files","tool":"read","arguments":{},"status":"completed"}`),
		json.RawMessage(`{"id":"command-1","type":"commandExecution","command":"pwd","cwd":"/tmp","aggregatedOutput":"/tmp","status":"completed"}`),
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(thread.Turns) != 1 || thread.Turns[0].Status != agentruntime.TurnRunning || len(thread.Turns[0].Items) != 3 {
		t.Fatalf("thread = %+v", thread)
	}
	items := thread.Turns[0].Items
	if items[0].Type != agentruntime.ItemUserMessage || session.TextOf(items[0].Content) != "hello" || items[1].Type != agentruntime.ItemTool || items[2].Type != agentruntime.ItemCommand || items[2].Output != "/tmp" {
		t.Fatalf("items = %+v", items)
	}
}

func TestColdThreadErrorNamesInvalidFieldWithoutRawPayload(t *testing.T) {
	_, err := translateThread(wireThread{ID: "thread-1", Turns: []wireTurn{{ID: "turn-1", Status: "completed", Items: []json.RawMessage{json.RawMessage(`{"id":"item-1","type":"unknown-secret-value"}`)}}}})
	if err == nil || !strings.Contains(err.Error(), "type") || strings.Contains(err.Error(), "unknown-secret-value") {
		t.Fatalf("translateThread error = %v", err)
	}
}
