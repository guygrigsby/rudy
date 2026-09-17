// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

func TestErrorFrom(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"passthrough", NewError(CodeConflict, "taken", nil), CodeConflict},
		{"wrapped passthrough", fmt.Errorf("outer: %w", NewError(CodeNoAsker, "nobody", nil)), CodeNoAsker},
		{"invariant", fmt.Errorf("fork_point after first: %w", session.ErrInvariant), CodeRefusedByInvariant},
		{"unknown model", fmt.Errorf("%w: x", provider.ErrUnknownModel), CodeNotFound},
		{"provider", &provider.Error{Class: session.ErrProvider, Status: 500, Message: "boom", Body: []byte("x")}, CodeProviderError},
		{"locked", session.ErrLocked, CodeUnavailable},
		{"canceled", context.Canceled, CodeInterrupted},
		{"invalid argument", fmt.Errorf("cwd: %w", ErrInvalidArgument), CodeInvalidArgument},
		{"other", errors.New("disk on fire"), CodeInternal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ErrorFrom(c.err)
			if got.Code != c.code {
				t.Fatalf("code %d, want %d (%v)", got.Code, c.code, got)
			}
		})
	}
	got := ErrorFrom(&provider.Error{Status: 429, Message: "slow", Body: []byte("b")})
	data, ok := got.Data.(map[string]any)
	if !ok || data["status"] != 429 || data["body"] != "b" {
		t.Fatalf("provider data %#v", got.Data)
	}
}

func TestEnvelopes(t *testing.T) {
	req, err := NewRequest(1, MethodClientHello, ClientHelloParams{Client: "test", Version: "0", Asker: false})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(req)
	want := `{"jsonrpc":"2.0","id":1,"method":"client.hello","params":{"client":"test","version":"0","asker":false}}`
	if string(b) != want {
		t.Fatalf("request %s", b)
	}
	n, _ := NewNotification(NotifyNotice, nil)
	if !n.IsNotification() {
		t.Fatal("notification must have no id")
	}
	b, _ = json.Marshal(n)
	if string(b) != `{"jsonrpc":"2.0","method":"notice"}` {
		t.Fatalf("notification %s", b)
	}
	resp, _ := NewResponse(json.RawMessage("1"), nil)
	b, _ = json.Marshal(resp)
	if string(b) != `{"jsonrpc":"2.0","id":1,"result":{}}` {
		t.Fatalf("response %s", b)
	}
	e := NewErrorResponse(nil, NewError(CodeInvalidArgument, "bad", nil))
	b, _ = json.Marshal(e)
	if string(b) != `{"jsonrpc":"2.0","id":null,"error":{"code":-32602,"message":"bad"}}` {
		t.Fatalf("error response %s", b)
	}
}

// TestErrorData round-trips an *Error through JSON the way a Response really arrives on the
// client: Data goes in as the typed value NewError was built with and comes back out as
// map[string]any, so ErrorData has to decode that map into v rather than type-assert it.
func TestErrorData(t *testing.T) {
	built := NewError(CodeUnavailable, "locked", map[string]string{"socket": "/tmp/rudy.sock"})
	raw, err := json.Marshal(NewErrorResponse(json.RawMessage("1"), built))
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}

	var data struct {
		Socket string `json:"socket"`
	}
	if !ErrorData(resp.Error, &data) {
		t.Fatal("ErrorData found nothing on a round-tripped error that carries data")
	}
	if data.Socket != "/tmp/rudy.sock" {
		t.Fatalf("socket = %q, want /tmp/rudy.sock", data.Socket)
	}

	if ErrorData(errors.New("not a protocol.Error"), &data) {
		t.Fatal("ErrorData found data behind an error with no *Error in its chain")
	}
	if ErrorData(NewError(CodeInternal, "no data", nil), &data) {
		t.Fatal("ErrorData found data on an *Error that carries none")
	}
}
