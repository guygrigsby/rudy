package skills

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, root, name, fm string) {
	dir := filepath.Join(root, name)
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\n"+fm+"\n---\n# body\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("x"), 0o644)
}

func TestLoadFirstWinsAndErrors(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	writeSkill(t, a, "deploy", "name: deploy\ndescription: Ship it\nallowed-tools: [bash, read]")
	writeSkill(t, b, "deploy", "name: deploy\ndescription: Other")
	writeSkill(t, b, "review", "name: review\ndescription: Review a diff")
	writeSkill(t, b, "broken", "description: [")
	_ = os.MkdirAll(filepath.Join(b, "noskill"), 0o755)
	got, errs := Load([]string{a, b, filepath.Join(a, "missing")})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "broken") {
		t.Errorf("errs %v", errs)
	}
	if len(got) != 2 || got[0].Name != "deploy" || got[0].Description != "Ship it" || !reflect.DeepEqual(got[0].AllowedTools, []string{"bash", "read"}) || got[1].Name != "review" {
		t.Errorf("skills %+v", got)
	}
	if got[0].Dir != filepath.Join(a, "deploy") {
		t.Errorf("dir %s", got[0].Dir)
	}
}

func TestMigrateNeverOverwrites(t *testing.T) {
	from1, from2, to := t.TempDir(), t.TempDir(), t.TempDir()
	writeSkill(t, from1, "deploy", "name: deploy\ndescription: one")
	writeSkill(t, from2, "deploy", "name: deploy\ndescription: two")
	writeSkill(t, from2, "review", "name: review\ndescription: r")
	writeSkill(t, to, "review", "name: review\ndescription: existing")
	copied, skipped, err := Migrate([]string{from1, from2, "/nonexistent"}, to)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(copied, []string{"deploy"}) || !reflect.DeepEqual(skipped, []string{"deploy", "review"}) {
		t.Errorf("copied %v skipped %v", copied, skipped)
	}
	if b, _ := os.ReadFile(filepath.Join(to, "deploy", "SKILL.md")); !strings.Contains(string(b), "description: one") {
		t.Error("first source wins")
	}
	if _, err := os.Stat(filepath.Join(to, "deploy", "extra.txt")); err != nil {
		t.Error("whole directory copied")
	}
	if b, _ := os.ReadFile(filepath.Join(to, "review", "SKILL.md")); !strings.Contains(string(b), "existing") {
		t.Error("existing skill overwritten")
	}
}
