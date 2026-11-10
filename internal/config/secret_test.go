// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"strings"
	"testing"
)

// TestResolveSecretErrorNeverEchoesTheValue: an operator who pastes the key itself into
// auth gets an error that provider plugins print and log, so it must not carry the key.
func TestResolveSecretErrorNeverEchoesTheValue(t *testing.T) {
	_, err := ResolveSecret("sk-ant-SECRETVALUE", func(string) string { return "" }, "")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "SECRETVALUE") {
		t.Fatalf("error echoes the value: %v", err)
	}
}
