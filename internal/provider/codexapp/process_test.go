// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareCodexHomeInstallsStrictProfile(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codex")
	ambientHome := filepath.Join(t.TempDir(), "operator")
	t.Setenv("HOME", ambientHome)
	t.Setenv("CODEX_HOME", "")
	if err := prepareCodexHome(home, nil); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`[permissions.rudy_strict]`,
		`[permissions.rudy_strict.filesystem]`,
		`':minimal' = 'read'`,
		`':project_roots' = 'read'`,
		`'` + filepath.Clean(home) + `' = 'deny'`,
		`'` + filepath.Join(ambientHome, ".codex") + `' = 'deny'`,
		`[permissions.rudy_strict.network]`,
		`enabled = false`,
	}
	for _, fragment := range want {
		if !strings.Contains(string(config), fragment) {
			t.Errorf("config.toml missing %q:\n%s", fragment, config)
		}
	}
	if info, err := os.Stat(filepath.Join(home, "config.toml")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config.toml: info=%v err=%v", info, err)
	}
}

func TestPrepareCodexHomeRejectsConfigSymlink(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(target, []byte("injected"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, "config.toml")); err != nil {
		t.Fatal(err)
	}
	if err := prepareCodexHome(home, nil); err == nil || !strings.Contains(err.Error(), "authority file") {
		t.Fatalf("prepareCodexHome error = %v", err)
	}
}

func TestCodexEnvironmentAllowlistMatchesNormativeDocs(t *testing.T) {
	quoted := make([]string, len(codexEnvironmentKeys))
	for i, name := range codexEnvironmentKeys {
		quoted[i] = "`" + name + "`"
	}
	list := strings.Join(quoted[:len(quoted)-1], ", ") + " and " + quoted[len(quoted)-1]
	want := "The operational environment allowlist is " + list + "."
	for _, path := range []string{
		"../../../docs/specs/codex-app-server-runtime.md",
		"../../../docs/adr/0047-isolate-codex-process-environment.md",
	} {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		normalized := strings.Join(strings.Fields(string(contents)), " ")
		if !strings.Contains(normalized, want) {
			t.Errorf("%s must record the codexEnvironmentKeys allowlist as %q", path, want)
		}
	}
}
