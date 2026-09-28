// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"context"
	"testing"
)

func TestBrowserOpenerAllowsOnlyApprovedHTTPSHosts(t *testing.T) {
	var got []string
	run := func(_ context.Context, name string, args ...string) error {
		got = append([]string{name}, args...)
		return nil
	}
	for _, raw := range []string{
		"http://auth.openai.com/x", "https://openai.com.evil/x", "javascript:alert(1)",
		"https://evil.test@openai.com.evil/x", "https://chatgpt.com.evil/x",
	} {
		if err := openAuthURL(context.Background(), raw, run); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	if len(got) != 0 {
		t.Fatalf("rejected URL reached opener: %v", got)
	}
	if err := openAuthURL(context.Background(), "https://auth.openai.com/x?code=a", run); err != nil {
		t.Fatal(err)
	}
	if len(got) < 2 || got[len(got)-1] != "https://auth.openai.com/x?code=a" {
		t.Fatalf("argv = %v", got)
	}
	if got[0] != browserCommand() {
		t.Fatalf("opener = %v", got)
	}
}
