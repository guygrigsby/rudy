package skills

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/frontmatter"
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

// TestLoadSkipsDotDirectories: a skills root is a directory on disk like any other, and .git
// or an editor's backup directory sitting in it is not a skill however complete its contents
// look.
func TestLoadSkipsDotDirectories(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "deploy", "name: deploy\ndescription: Ship it")
	writeSkill(t, root, ".git", "name: git\ndescription: not a skill")
	writeSkill(t, root, ".deploy.bak", "name: deploy\ndescription: an old copy")
	got, errs := Load([]string{root})
	if len(errs) != 0 {
		t.Fatalf("errs %v", errs)
	}
	if len(got) != 1 || got[0].Name != "deploy" || got[0].Description != "Ship it" {
		t.Errorf("skills %+v, want the one real skill", got)
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

// TestLoadDistinguishesMissingFromUnterminatedFence covers a regression from moving the
// frontmatter reader to the shared internal/frontmatter package: a SKILL.md with no opening
// fence and one whose fence never closes must still be told apart.
func TestLoadDistinguishesMissingFromUnterminatedFence(t *testing.T) {
	dir := t.TempDir()
	write := func(name, dirName, body string) {
		t.Helper()
		full := filepath.Join(dir, dirName)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(full, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("SKILL.md", "noopen", "description: x\nbody")
	write("SKILL.md", "noclose", "---\ndescription: x\nbody")
	_, errs := Load([]string{dir})
	if len(errs) != 2 {
		t.Fatalf("errs %v", errs)
	}
	var noOpen, noClose int
	for _, err := range errs {
		switch {
		case errors.Is(err, frontmatter.ErrNoOpeningFence):
			noOpen++
		case errors.Is(err, frontmatter.ErrNoClosingFence):
			noClose++
		}
	}
	if noOpen != 1 || noClose != 1 {
		t.Errorf("want one of each fence error, got %v", errs)
	}
}

// TestMigrateStagesAndCleansUpOnFailure covers the wedge a naive os.CopyFS(dst, ...) leaves:
// a copy that fails partway through must never leave a half-copied <to>/<name> behind, since
// the next run's os.Stat would treat it as already migrated and skip it forever.
func TestMigrateStagesAndCleansUpOnFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read anything; a permission-denied copy failure cannot be induced")
	}
	from, to := t.TempDir(), t.TempDir()
	writeSkill(t, from, "deploy", "name: deploy\ndescription: one")
	blocked := filepath.Join(from, "deploy", "extra.txt")
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o644) })

	copied, _, err := Migrate([]string{from}, to)
	if err == nil {
		t.Fatal("want an error from the blocked source file")
	}
	if !strings.Contains(err.Error(), "deploy") {
		t.Errorf("error should name the skill: %v", err)
	}
	if len(copied) != 0 {
		t.Errorf("copied %v, want none", copied)
	}
	if _, statErr := os.Stat(filepath.Join(to, "deploy")); !os.IsNotExist(statErr) {
		t.Errorf("half-copied destination left behind: stat err %v", statErr)
	}
	ents, rerr := os.ReadDir(to)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(ents) != 0 {
		t.Errorf("staging directory not cleaned up: %v", ents)
	}

	if err := os.Chmod(blocked, 0o644); err != nil {
		t.Fatal(err)
	}
	copied, _, err = Migrate([]string{from}, to)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(copied, []string{"deploy"}) {
		t.Errorf("copied %v once the cause was removed", copied)
	}
}
