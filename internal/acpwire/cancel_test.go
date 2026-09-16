package acpwire

import (
	"strings"
	"testing"
)

func TestCancellationIngressAndRetiredBinding(t *testing.T) {
	var events []Dispatch
	raw := `{"jsonrpc":"2.0","id":"a","method":"session/prompt","params":{"sessionId":"s","prompt":[]}}` + "\n" +
		`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"\u0073","ignored":"SECRET"}}` + "\n" +
		`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":"\u0061","ignored":"SECRET"}}` + "\n"
	w, r, _ := newTestWire(t, raw, Options{BeforeDispatch: func(d Dispatch) { events = append(events, d) }})
	readLine(t, r)
	for range 2 {
		if s := readLine(t, r); strings.Contains(s, "SECRET") || strings.Contains(s, `\u`) {
			t.Fatal("cancel routing not normalized")
		}
	}
	if len(events) != 3 || events[1].Intent != SemanticCancel || events[2].Intent != RequestCancel || events[2].Request != events[0].Request {
		t.Fatal("lost ingress order or exact request binding")
	}
	old := events[0].Request
	if !old.Reserve(CancelResponse) || old.Reserve(ResultResponse) {
		t.Fatal("cancel response reservation lost")
	}
	a, err := w.Send([]byte(`{"jsonrpc":"2.0","id":"a","error":{"code":-32800,"message":"Request cancelled"}}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	waitAck(t, a)
	if old.Reserve(ResultResponse) {
		t.Fatal("retired request rebound")
	}
	if w.Request(old.ID()) != nil {
		t.Fatal("retired request retained")
	}
}
func TestReservedResponseIgnoresLateCancellation(t *testing.T) {
	var events []Dispatch
	w, r, _ := newTestWire(t, string([]byte(`{"jsonrpc":"2.0","id":1,"method":"unknown"}`))+"\n"+`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":1}}`+"\n"+`{"jsonrpc":"2.0","id":2,"method":"unknown"}`+"\n", Options{BeforeDispatch: func(d Dispatch) { events = append(events, d) }})
	readLine(t, r)
	req := events[0].Request
	if !req.Reserve(ResultResponse) {
		t.Fatal("cannot reserve result")
	}
	readLine(t, r)
	if len(events) != 2 || events[1].Request.ID().Value != "2" {
		t.Fatal("late cancel reached hook")
	}
	if w.Usage().Requests != 2 {
		t.Fatal("cancel retired live response")
	}
}

func TestRequestGatePrecedesPromptParams(t *testing.T) {
	raw := `{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":false}}` + "\n" + `{"jsonrpc":"2.0","id":2,"method":"unknown"}` + "\n"
	w, r, out := newTestWire(t, raw, Options{RequestGate: func(method string) Admission {
		if method == "session/prompt" {
			return NotReady
		}
		return DispatchRequest
	}})
	readLine(t, r)
	w.mu.Lock()
	req := w.inbound[numericID(1)]
	w.mu.Unlock()
	if req != nil {
		waitAck(t, req.Written())
	}
	if !strings.Contains(out.String(), `-32014`) {
		t.Fatal("parameter rejection preceded readiness gate")
	}
}
