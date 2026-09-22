// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runConfig(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := NewRoot()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

// userConfigPath is the config.toml tempXDG pointed the config root at.
func userConfigPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "rudy", "config.toml")
}

// TestConfigLintPointsAtTheLine: the whole value of the command is the file and line an
// editor can be opened on, and the fix said out loud. It lints the file rudy reads, with no
// --path, because resolving that file is half of what the command does.
func TestConfigLintPointsAtTheLine(t *testing.T) {
	tempXDG(t)
	writeConfig(t, `[providers]
  [aperture]
  wire = "openai_chat"
  base_url = "https://ai.example.ts.net/v1"
`)
	out, err := runConfig(t, "config", "lint")
	if err != nil {
		t.Fatalf("lint: %v\n%s", err, out)
	}
	if !strings.Contains(out, userConfigPath(t)+":2:") {
		t.Errorf("output does not point at the [aperture] line:\n%s", out)
	}
	if !strings.Contains(out, "[providers.aperture]") {
		t.Errorf("output does not name the nesting it meant:\n%s", out)
	}
}

// TestConfigLintSaysSoWhenThereIsNothingToSay: silence would read as a command that did
// not run.
func TestConfigLintSaysSoWhenThereIsNothingToSay(t *testing.T) {
	tempXDG(t)
	writeConfig(t, "[providers.aperture]\nwire = \"openai_chat\"\n")
	out, err := runConfig(t, "config", "lint")
	if err != nil {
		t.Fatalf("lint: %v\n%s", err, out)
	}
	if !strings.Contains(out, "reads every key") {
		t.Errorf("a clean file says nothing at all:\n%s", out)
	}
}

// TestConfigLintFailsOnAFileThatWillNotParse is the one failure the command has: findings
// are warnings, and a document TOML cannot read is not a finding, it is the end of the road.
// --path is here too, since a file that is not rudy's own is exactly what it is for.
func TestConfigLintFailsOnAFileThatWillNotParse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.toml")
	if err := os.WriteFile(path, []byte("[default\nprovider = \"aperture\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runConfig(t, "config", "lint", "--path", path)
	if err == nil {
		t.Fatalf("an unparsable file exits clean:\n%s", out)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the error does not name the file: %v", err)
	}
}
