// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"reflect"
	"testing"

	"github.com/guygrigsby/rudy/internal/session"
)

func TestRuntimePolicyMapsRudyModes(t *testing.T) {
	tests := []struct {
		mode       session.Mode
		approval   string
		threadMode string
		turnPolicy *sandboxPolicy
	}{
		{mode: session.ModeStrict, approval: "untrusted", threadMode: "read-only", turnPolicy: &sandboxPolicy{Type: "readOnly", NetworkAccess: false}},
		{mode: session.ModePermissive, approval: "on-request"},
		{mode: session.ModeOff, approval: "never"},
	}
	for _, test := range tests {
		if got := approvalPolicy(test.mode); got != test.approval {
			t.Errorf("approvalPolicy(%q) = %q, want %q", test.mode, got, test.approval)
		}
		if got := threadSandbox(test.mode); got != test.threadMode {
			t.Errorf("threadSandbox(%q) = %q, want %q", test.mode, got, test.threadMode)
		}
		if got := turnSandboxPolicy(test.mode); !reflect.DeepEqual(got, test.turnPolicy) {
			t.Errorf("turnSandboxPolicy(%q) = %+v, want %+v", test.mode, got, test.turnPolicy)
		}
	}
}
