package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func writeCLISkill(t *testing.T, root, name, fm string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\n"+fm+"\n---\n# body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// tomlStringList renders items as a TOML array of strings; none of the test paths need
// escaping.
func tomlStringList(items []string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = `"` + it + `"`
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// writeSkillsConfig writes a config.toml under cfgHome/rudy that sets skills.dirs and
// skills.migrate_from, so a test can exercise the CLI defaults without touching the real home.
func writeSkillsConfig(t *testing.T, cfgHome string, dirs, migrateFrom []string) {
	t.Helper()
	dir := filepath.Join(cfgHome, "rudy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[skills]\ndirs = " + tomlStringList(dirs) + "\nmigrate_from = " + tomlStringList(migrateFrom) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// tempXDG points every XDG dir, and HOME, at a fresh temp tree, so config.Load's "~/"
// expansion cannot reach the real home either.
func tempXDG(t *testing.T) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("HOME", base)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(base, "run"))
}

// skillsTestRoot is a root command over a fresh temp tree.
func skillsTestRoot(t *testing.T) *cobra.Command {
	t.Helper()
	tempXDG(t)
	return NewRoot()
}

func TestSkillsListCommand(t *testing.T) {
	root := skillsTestRoot(t)
	skillsDir := filepath.Join(t.TempDir(), "skilldir")
	writeCLISkill(t, skillsDir, "deploy", "name: deploy\ndescription: Ship it")
	writeSkillsConfig(t, os.Getenv("XDG_CONFIG_HOME"), []string{skillsDir}, nil)

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"skills", "list"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute: %v\n%s", err, out.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "NAME") {
		t.Fatalf("output %q", out.String())
	}
	if !strings.Contains(lines[1], "deploy") || !strings.Contains(lines[1], "Ship it") || !strings.Contains(lines[1], filepath.Join(skillsDir, "deploy")) {
		t.Fatalf("row %q", lines[1])
	}
}

func TestSkillsMigrateCommandWithExplicitFlags(t *testing.T) {
	root := skillsTestRoot(t)
	from := t.TempDir()
	to := t.TempDir()
	writeCLISkill(t, from, "deploy", "name: deploy\ndescription: Ship it")
	writeCLISkill(t, from, "review", "name: review\ndescription: r")
	writeCLISkill(t, to, "review", "name: review\ndescription: existing")

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"skills", "migrate", "--from", from, "--to", to})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute: %v\n%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "copied: deploy\n") || !strings.Contains(got, "skipped: review (exists)\n") {
		t.Fatalf("output %q", got)
	}
	if _, err := os.Stat(filepath.Join(to, "deploy", "SKILL.md")); err != nil {
		t.Fatal("skill not copied")
	}
}

func TestSkillsMigrateCommandDefaultsFromConfig(t *testing.T) {
	root := skillsTestRoot(t)
	from := t.TempDir()
	to := t.TempDir()
	writeCLISkill(t, from, "deploy", "name: deploy\ndescription: Ship it")
	// dirs holds one absolute entry (to) so migrate picks it as the destination with no
	// --to; migrate_from holds the source with no --from.
	writeSkillsConfig(t, os.Getenv("XDG_CONFIG_HOME"), []string{to}, []string{from})

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"skills", "migrate"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "copied: deploy\n") {
		t.Fatalf("output %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(to, "deploy", "SKILL.md")); err != nil {
		t.Fatal("skill not copied to the configured skills.dirs entry")
	}
}
