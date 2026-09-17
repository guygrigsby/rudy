// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/guygrigsby/rudy/internal/session"
)

// TestTheScopeIsRememberedInItsOwnFile: the models ctrl+p cycles through are chosen once
// and meant to stay chosen. config.toml is the operator's, so the choice goes in a file of
// its own the way trust and the plugin lock do (ADR 0027).
func TestTheScopeIsRememberedInItsOwnFile(t *testing.T) {
	dir := t.TempDir()
	got, err := readScope(dir)
	if err != nil || len(got) != 0 {
		t.Fatalf("nothing chosen yet is not an error: %v %v", got, err)
	}
	refs := []session.ModelRef{
		{Provider: "aperture", Model: "moonshotai/kimi-k3"},
		{Provider: "mlx", Model: "qwen3-coder"},
	}
	if err := writeScope(dir, refs); err != nil {
		t.Fatal(err)
	}
	got, err = readScope(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, refs) {
		t.Errorf("read back what was chosen, in order: %v", got)
	}
	// Clearing it is a choice too: back to the whole registry, and it stays cleared.
	if err := writeScope(dir, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := readScope(dir); err != nil || len(got) != 0 {
		t.Errorf("an empty scope is the whole registry: %v %v", got, err)
	}
	// A file somebody edited into nonsense is worth saying so about rather than silently
	// forgetting what they chose.
	if err := os.WriteFile(filepath.Join(dir, scopeFile), []byte("models = ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readScope(dir); err == nil {
		t.Error("a scope file that will not parse says so")
	}
}
