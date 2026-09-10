package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/config"
)

// write puts body in a temp file and answers its path.
func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestSyncKeepsEveryValueAndComment is the promise: sync adds, and that is all it does.
func TestSyncKeepsEveryValueAndComment(t *testing.T) {
	const before = `# my own note, which nobody may take away
[default]
provider = "aperture"   # trailing comments too
model = "cline-pass/kimi-k3"

[ui]
render = "inline"
`
	path := write(t, before)
	res, err := config.Sync(path, false)
	if err != nil {
		t.Fatal(err)
	}
	after := read(t, path)
	for _, keep := range []string{
		"# my own note, which nobody may take away",
		`provider = "aperture"   # trailing comments too`,
		`model = "cline-pass/kimi-k3"`,
		`render = "inline"`,
	} {
		if !strings.Contains(after, keep) {
			t.Errorf("sync must keep %q:\n%s", keep, after)
		}
	}
	if strings.Count(after, `render = `) != 1 {
		t.Errorf("a key that is already set is never written twice:\n%s", after)
	}
	if len(res.Added) == 0 {
		t.Error("a file with three keys is missing the rest")
	}
	for _, k := range res.Added {
		if k == "ui.render" || k == "default.provider" || k == "default.model" {
			t.Errorf("sync reported adding %q, which was already there", k)
		}
	}
}

// TestSyncPutsAKeyUnderItsOwnTable: a key appended at the end of the file would land in
// whatever table happened to be last, which is a different setting with the same name.
func TestSyncPutsAKeyUnderItsOwnTable(t *testing.T) {
	path := write(t, "[ui]\nrender = \"inline\"\n\n[permissions]\nmode = \"off\"\n")
	if _, err := config.Sync(path, false); err != nil {
		t.Fatal(err)
	}
	after := read(t, path)
	cfg := loadFileAt(t, path)
	if cfg.UI.Render != "inline" || cfg.Permissions.Mode != "off" {
		t.Fatalf("the file still means what it meant: render %q, mode %q", cfg.UI.Render, cfg.Permissions.Mode)
	}
	// ui.vim belongs to [ui], which is above [permissions]: it has to land in the first.
	uiAt := strings.Index(after, "[ui]")
	permAt := strings.Index(after, "[permissions]")
	vimAt := strings.Index(after, "vim = ")
	if uiAt >= vimAt || vimAt >= permAt {
		t.Errorf("vim landed outside [ui]:\n%s", after)
	}
}

// TestSyncAppendsATableTheFileNeverHad, header and comment and all.
func TestSyncAppendsATableTheFileNeverHad(t *testing.T) {
	path := write(t, "[default]\nprovider = \"p\"\nmodel = \"m\"\n")
	if _, err := config.Sync(path, false); err != nil {
		t.Fatal(err)
	}
	after := read(t, path)
	if !strings.Contains(after, "[ui.header]") || !strings.Contains(after, "greeting = true") {
		t.Errorf("a table the file never had arrives whole:\n%s", after)
	}
	if strings.Count(after, "[default]") != 1 {
		t.Errorf("and no table is written twice:\n%s", after)
	}
}

// TestSyncIsIdempotent: running it twice adds nothing the second time, which is what makes
// it safe to hang off make install.
func TestSyncIsIdempotent(t *testing.T) {
	path := write(t, "[default]\nprovider = \"p\"\n")
	if _, err := config.Sync(path, false); err != nil {
		t.Fatal(err)
	}
	first := read(t, path)
	res, err := config.Sync(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 0 {
		t.Errorf("a second sync adds nothing, added %v", res.Added)
	}
	if read(t, path) != first {
		t.Error("and changes not one byte")
	}
}

// TestSyncWritesTheWholeExampleWhenThereIsNoFile, directory and all.
func TestSyncWritesTheWholeExampleWhenThereIsNoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.toml")
	res, err := config.Sync(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created {
		t.Error("a file that was not there is created")
	}
	if read(t, path) != config.Example() {
		t.Error("and what it is created with is the example")
	}
}

// TestSyncDryRunWritesNothing.
func TestSyncDryRunWritesNothing(t *testing.T) {
	const before = "[default]\nprovider = \"p\"\n"
	path := write(t, before)
	res, err := config.Sync(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) == 0 {
		t.Error("a dry run still says what it would add")
	}
	if read(t, path) != before {
		t.Error("and writes nothing")
	}
	missing := filepath.Join(t.TempDir(), "config.toml")
	if _, err := config.Sync(missing, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Error("a dry run creates no file either")
	}
}

// TestSyncReadsAValueSpreadOverSeveralLines: an array written across lines must not be read
// as keys, and must not be written a second time.
func TestSyncReadsAValueSpreadOverSeveralLines(t *testing.T) {
	path := write(t, `[permissions]
mode = "off"
dangerous = [
  "rm -rf",
  "sudo",
]
`)
	if _, err := config.Sync(path, false); err != nil {
		t.Fatal(err)
	}
	after := read(t, path)
	if strings.Count(after, "dangerous = ") != 1 {
		t.Errorf("a multi line array is one key:\n%s", after)
	}
	cfg := loadFileAt(t, path)
	if len(cfg.Permissions.Dangerous) != 2 {
		t.Errorf("and keeps its own value: %v", cfg.Permissions.Dangerous)
	}
}

// TestASyncedFileLoads is the point of all of it: whatever sync wrote, rudy reads.
func TestASyncedFileLoads(t *testing.T) {
	for _, before := range []string{
		"",
		"[default]\nprovider = \"p\"\nmodel = \"m\"\n",
		"# comments only\n",
		"[ui]\nrender = \"inline\"\n[ui.header]\nshow = false\n",
	} {
		dir := t.TempDir()
		paths := config.Paths{Config: dir, Data: dir, Runtime: dir, Cache: dir, Home: dir}
		if before != "" {
			if err := os.WriteFile(paths.ConfigFile(), []byte(before), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := config.Sync(paths.ConfigFile(), false); err != nil {
			t.Fatalf("sync %q: %v", before, err)
		}
		if _, err := config.Load(paths, map[string]any{"default.provider": "p", "default.model": "m"}); err != nil {
			t.Fatalf("a synced file must load, started from %q: %v\n%s", before, err, read(t, paths.ConfigFile()))
		}
	}
}

// loadFileAt loads a config from the directory holding path.
func loadFileAt(t *testing.T, path string) *config.Config {
	t.Helper()
	dir := filepath.Dir(path)
	paths := config.Paths{Config: dir, Data: dir, Runtime: dir, Cache: dir, Home: dir}
	cfg, err := config.Load(paths, map[string]any{"default.provider": "p", "default.model": "m"})
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return cfg
}
