// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/guygrigsby/rudy/internal/protocol"
)

func TestProviderCompleteParamsKeepSessionAndHeaders(t *testing.T) {
	want := protocol.ProviderCompleteParams{
		RequestID: "req-1", SessionID: "01K4M0A7Q8ZJ3N6R9T2V5X8B1D",
		Headers: map[string]string{"X-Rudy-Session": "01K4M0A7Q8ZJ3N6R9T2V5X8B1D"},
	}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got protocol.ProviderCompleteParams
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestRuntimeApprovalQuestionHasNoSessionID(t *testing.T) {
	typ := reflect.TypeFor[protocol.RuntimeApprovalRequestParams]()
	for field := range typ.Fields() {
		if field.Name == "SessionID" || field.Tag.Get("json") == "session_id" {
			t.Fatalf("runtime approval question trusts a session id: %s", field.Name)
		}
	}
}
