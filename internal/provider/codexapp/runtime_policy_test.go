// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/agentruntime"
	"github.com/guygrigsby/rudy/internal/session"
)

func TestRuntimePolicyMapsRudyModes(t *testing.T) {
	tests := []struct {
		mode        session.Mode
		approval    string
		permissions string
	}{
		{mode: session.ModeStrict, approval: "on-request", permissions: "rudy_strict"},
		{mode: "", approval: "on-request", permissions: "rudy_strict"},
	}
	for _, test := range tests {
		if got := approvalPolicy(test.mode); got != test.approval {
			t.Errorf("approvalPolicy(%q) = %q, want %q", test.mode, got, test.approval)
		}
		if got := threadPermissions(test.mode); got != test.permissions {
			t.Errorf("threadPermissions(%q) = %q, want %q", test.mode, got, test.permissions)
		}
	}
}

func TestRuntimeRejectsNonStrictPermissionModesBeforeStartingProcess(t *testing.T) {
	client := NewClient(Command{Path: "/does/not/exist"})
	t.Cleanup(func() { _ = client.Close() })
	for _, mode := range []session.Mode{session.ModePermissive, session.ModeOff} {
		if _, err := client.StartThread(context.Background(), agentruntime.StartThreadRequest{Mode: mode}); err == nil || !strings.Contains(err.Error(), "strict permission mode") {
			t.Errorf("StartThread(%q) error = %v", mode, err)
		}
		if _, err := client.StartTurn(context.Background(), agentruntime.StartTurnRequest{Mode: mode}); err == nil || !strings.Contains(err.Error(), "strict permission mode") {
			t.Errorf("StartTurn(%q) error = %v", mode, err)
		}
	}
}

func TestStrictRuntimePolicyDisablesInheritedReviewersAndRemoteTools(t *testing.T) {
	if got := approvalsReviewer(session.ModeStrict); got != "user" {
		t.Fatalf("approvalsReviewer(strict) = %q, want user", got)
	}
	want := map[string]any{
		"features.apps":                      false,
		"features.exec_permission_approvals": true,
		"features.plugins":                   false,
		"features.remote_plugin":             false,
		"features.request_permissions_tool":  true,
	}
	if got := strictThreadConfig(session.ModeStrict); !reflect.DeepEqual(got, want) {
		t.Fatalf("strictThreadConfig(strict) = %#v, want %#v", got, want)
	}
}
