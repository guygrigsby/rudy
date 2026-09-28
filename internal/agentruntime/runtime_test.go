// SPDX-License-Identifier: AGPL-3.0-or-later

package agentruntime_test

import (
	"testing"

	"github.com/guygrigsby/rudy/internal/agentruntime"
)

func TestAuthChallengeConstructorsEnforceVariantFields(t *testing.T) {
	if _, err := agentruntime.NewBrowserChallenge("codex", "", "https://auth.openai.com/"); err == nil {
		t.Fatal("browser challenge accepted an empty login id")
	}
	if _, err := agentruntime.NewDeviceChallenge("codex", "login-1", "https://auth.openai.com/device", ""); err == nil {
		t.Fatal("device challenge accepted an empty user code")
	}

	browser, err := agentruntime.NewBrowserChallenge("codex", "login-1", "https://auth.openai.com/")
	if err != nil {
		t.Fatal(err)
	}
	if browser.Type != agentruntime.ChallengeBrowser || browser.VerificationURL != "" || browser.UserCode != "" {
		t.Fatalf("browser challenge = %+v", browser)
	}

	device, err := agentruntime.NewDeviceChallenge("codex", "login-2", "https://auth.openai.com/device", "ABCD-EFGH")
	if err != nil {
		t.Fatal(err)
	}
	if device.Type != agentruntime.ChallengeDevice || device.URL != "" {
		t.Fatalf("device challenge = %+v", device)
	}
}
