package config_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/config"
)

// The guards in this file keep one fact in one place. A config key lives in the defaults,
// in the catalogue that documents it, in the example this repository ships and in the
// contracts table; each guard below fails, naming the file to edit, the moment those four
// disagree. ADR 0021.

// repoFile reads a file from the repository root, which is two directories up from this
// package.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(body)
}

// TestEveryDefaultIsDocumented is the first guard: a new key in Defaults with no entry in
// Sections would reach an operator's config with no comment above it, and would never be
// written by sync at all.
func TestEveryDefaultIsDocumented(t *testing.T) {
	documented := config.DocumentedKeys()
	for key := range config.Defaults() {
		if !slices.Contains(documented, key) {
			t.Errorf("config key %q has no entry in internal/config/docs.go; add one to the Section for its table", key)
		}
	}
	for _, key := range documented {
		if _, ok := config.Defaults()[key]; !ok {
			t.Errorf("internal/config/docs.go documents %q, which Defaults() does not carry; add the default or drop the doc", key)
		}
	}
}

// TestTheShippedExampleIsGenerated: examples/config.toml is written by `rudy config
// example`, so a default that changes changes the file. Regenerate with
// `make config-example`.
func TestTheShippedExampleIsGenerated(t *testing.T) {
	if got, want := repoFile(t, "examples/config.toml"), config.Example(); got != want {
		t.Errorf("examples/config.toml is not what `rudy config example` prints; run `make config-example`")
	}
}

// TestTheShippedExampleLoads is the example-executes guard: the file a reader copies has to
// be a config rudy accepts, defaults and all.
func TestTheShippedExampleLoads(t *testing.T) {
	dir := t.TempDir()
	paths := config.Paths{Config: dir, Data: dir, Runtime: dir, Cache: dir, Home: dir}
	if err := os.WriteFile(paths.ConfigFile(), []byte(config.Example()), 0o600); err != nil {
		t.Fatal(err)
	}
	// The example ships with no provider, which is the one thing a real config must add.
	cfg, err := config.Load(paths, map[string]any{"default.provider": "p", "default.model": "m"})
	if err != nil {
		t.Fatalf("the example this repository ships must load: %v", err)
	}
	// And what it loads is the defaults, since every value in it is one.
	if cfg.UI.Render != config.Defaults()["ui.render"] || cfg.UI.Notices.Max != config.Defaults()["ui.notices.max"] {
		t.Errorf("the example carries values that are not the defaults: render %q, notices %d", cfg.UI.Render, cfg.UI.Notices.Max)
	}
}

// contractKey matches the first cell of a contracts table row, which is a backticked key.
var contractKey = regexp.MustCompile(`^\|\s*` + "`" + `([^` + "`" + `]+)` + "`")

// TestEveryDefaultIsInTheContracts is the registry-to-doc guard: the contracts table is
// normative over the code, so a key the code carries and the table does not is a contract
// nobody wrote.
func TestEveryDefaultIsInTheContracts(t *testing.T) {
	rows := map[string]bool{}
	for _, line := range strings.Split(repoFile(t, "docs/specs/rudy-contracts.md"), "\n") {
		if m := contractKey.FindStringSubmatch(line); m != nil {
			rows[m[1]] = true
		}
	}
	for key := range config.Defaults() {
		if !rows[key] {
			t.Errorf("config key %q is in no row of docs/specs/rudy-contracts.md; add it to the config.toml table", key)
		}
	}
}

// unshipped marks a config.toml row as documented but not built yet. A row that carries it
// must not be in Defaults(), and one that does not carry it must be: that is what keeps a
// table nobody has implemented from reading like a promise the code keeps.
const unshipped = "UNSHIPPED"

// TestEveryContractedKeyExists is the other direction, over the whole config.toml table:
// a documented key the code does not carry is a promise nothing keeps. It was ui.* only,
// which is how log.level and log.file sat in the table for a wave with no logger behind
// them and nothing to write the rudy.log the file table also promises (rudy-3fb).
//
// Rows naming a pattern rather than a key (ui.theme.<role>) are the table's own shorthand
// and are skipped; rows marked UNSHIPPED are checked the other way round.
func TestEveryContractedKeyExists(t *testing.T) {
	defaults := config.Defaults()
	var seen int
	for _, line := range configSection(t) {
		m := contractKey.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key := m[1]
		if strings.Contains(key, "<") {
			continue
		}
		seen++
		_, ok := defaults[key]
		switch {
		case strings.Contains(line, unshipped) && ok:
			t.Errorf("docs/specs/rudy-contracts.md marks %q %s, but Defaults() carries it; drop the marker", key, unshipped)
		case !strings.Contains(line, unshipped) && !ok:
			t.Errorf("docs/specs/rudy-contracts.md documents %q, which Defaults() does not carry; add the default, mark the row %s with the issue that will, or drop the row", key, unshipped)
		}
	}
	if seen < len(defaults) {
		t.Errorf("read %d config rows for %d defaults; the table shape moved and this guard stopped reading it", seen, len(defaults))
	}
}

// configSection is the lines of the contracts' config.toml table and nothing else: every
// other table in that file is keyed by the same backtick shape, so a key is only a config
// key by where it sits. A heading that has moved is a failure rather than an empty read.
func configSection(t *testing.T) []string {
	t.Helper()
	const heading = "### config.toml"
	var out []string
	var in bool
	for _, line := range strings.Split(repoFile(t, "docs/specs/rudy-contracts.md"), "\n") {
		switch {
		case strings.HasPrefix(line, heading):
			in = true
		case in && strings.HasPrefix(line, "#"):
			return out
		case in:
			out = append(out, line)
		}
	}
	if !in {
		t.Fatalf("docs/specs/rudy-contracts.md has no %q heading; this guard reads the table under it", heading)
	}
	return out
}

// TestTheReadmeNamesTheConfigCommands is the doc-commands guard: the README tells a reader
// to run these, so they have to exist.
func TestTheReadmeNamesTheConfigCommands(t *testing.T) {
	readme := repoFile(t, "README.md")
	for _, cmd := range []string{"rudy config sync", "rudy config example"} {
		if !strings.Contains(readme, cmd) {
			t.Errorf("README.md does not mention %q; a command nobody is told about is a command nobody runs", cmd)
		}
	}
}

// TestTheMouseIsTheTerminalsUntilAsked: with reporting on, a drag stops selecting text in
// most terminals, and copying an error out of the transcript is the more daily thing. A
// client takes the mouse only when the config says to.
func TestTheMouseIsTheTerminalsUntilAsked(t *testing.T) {
	if got := config.Defaults()["ui.mouse"]; got != "off" {
		t.Errorf("ui.mouse defaults to off, got %v", got)
	}
}
