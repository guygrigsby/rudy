// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lintFile writes body to a temp file and lints it.
func lintFile(t *testing.T, body string) []Finding {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := Lint(path)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	return fs
}

// only is the one finding a file was supposed to produce.
func only(t *testing.T, fs []Finding) Finding {
	t.Helper()
	if len(fs) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(fs), fs)
	}
	return fs[0]
}

// TestLintCatchesTheMisnestedProviderTable is the file that lost an evening: TOML pays no
// attention to indentation, so [aperture] under [providers] is a table of its own, the
// providers map is empty, and the only word anybody hears about it is a session.submit that
// fails with "provider not loaded" long after boot.
func TestLintCatchesTheMisnestedProviderTable(t *testing.T) {
	fs := lintFile(t, `[default]
provider = "aperture"

[providers]
  [aperture]
  wire = "openai_chat"
  base_url = "https://ai.example.ts.net/v1"
  dialect = "clinepass"
`)
	f := only(t, fs)
	if f.Line != 5 {
		t.Errorf("line = %d, want 5 (the [aperture] header)", f.Line)
	}
	if !strings.Contains(f.Text, "[providers.aperture]") {
		t.Errorf("finding does not name the nesting it meant: %q", f.Text)
	}
}

// TestLintKeepsQuietAboutTheShippedExample is the no-false-positives guard, and the reason
// it reads the generated example rather than a fixture: every key rudy documents is in it,
// so a linter that flags anything here is wrong about the config it ships.
func TestLintKeepsQuietAboutTheShippedExample(t *testing.T) {
	if fs := lintFile(t, Example()); len(fs) != 0 {
		t.Errorf("the shipped example lints dirty: %v", fs)
	}
}

// TestLintKeepsQuietAboutTheRepositoryExample is the same guard one file over: the
// examples/config.toml a reader copies out of the repository.
func TestLintKeepsQuietAboutTheRepositoryExample(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "examples", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if fs := lintFile(t, string(body)); len(fs) != 0 {
		t.Errorf("examples/config.toml lints dirty: %v", fs)
	}
}

// TestLintNamesTheKeyItMeant: a near miss is a typo, and the fix is worth saying out loud.
func TestLintNamesTheKeyItMeant(t *testing.T) {
	f := only(t, lintFile(t, "max_token = 8192\n"))
	if f.Line != 1 {
		t.Errorf("line = %d, want 1", f.Line)
	}
	if !strings.Contains(f.Text, "max_tokens") {
		t.Errorf("no suggestion of max_tokens: %q", f.Text)
	}
}

// TestLintCatchesAnUnknownProviderKey: a provider table's own keys are a closed set, which
// is what makes the open [providers.<name>] level worth modelling rather than waving through.
func TestLintCatchesAnUnknownProviderKey(t *testing.T) {
	f := only(t, lintFile(t, `[providers.aperture]
wire = "openai_chat"
base_ur1 = "https://ai.example.ts.net/v1"
`))
	if f.Line != 3 {
		t.Errorf("line = %d, want 3", f.Line)
	}
	if !strings.Contains(f.Text, "base_url") {
		t.Errorf("no suggestion of base_url: %q", f.Text)
	}
}

// TestLintLeavesOperatorNamedTablesAlone: the tables whose children an operator names are
// open at exactly one level, and each one below that is a closed schema again.
func TestLintLeavesOperatorNamedTablesAlone(t *testing.T) {
	fs := lintFile(t, `[providers.aperture]
wire = "openai_chat"
base_url = "https://ai.example.ts.net/v1"
auth = "env:APERTURE_API_KEY"

[providers.aperture.headers]
X-Title = "rudy"

[plugins.memory]
anything = "a plugin's settings are its own"

[plugins.memory.nested]
deeper = true

[keys]
"app.model.select" = "ctrl+l"
"app.tools.expand" = ["ctrl+o", "ctrl+e"]

[ui.theme]
name = "tokyonight"
accent = "#7aa2f7"

[ui.icons]
set = "nerd"
cat = ""

[memory.fold]
session = 3
`)
	if len(fs) != 0 {
		t.Errorf("operator-named tables flagged: %v", fs)
	}
}

// TestLintReadsKeysTheWayViperDoes: viper lowercases every key it reads, so Max_Tokens does
// set max_tokens. A linter that models the decoder differently from the decoder invents
// findings.
func TestLintReadsKeysTheWayViperDoes(t *testing.T) {
	if fs := lintFile(t, "Max_Tokens = 8192\n"); len(fs) != 0 {
		t.Errorf("case difference flagged, but viper reads it: %v", fs)
	}
}

// TestLintReportsOneUnknownTableOnce: the table header is the mistake, and repeating it per
// key under it buries the line worth reading.
func TestLintReportsOneUnknownTableOnce(t *testing.T) {
	f := only(t, lintFile(t, `[nonsense]
a = 1
b = 2
c = 3
`))
	if f.Line != 1 {
		t.Errorf("line = %d, want 1", f.Line)
	}
}

// TestLintStopsAtAFileThatIsNotTOML: an unparsable file has no keys to reason about, so it
// is the one error Lint answers with rather than a warning about something rudy ignores.
func TestLintStopsAtAFileThatIsNotTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[default\nprovider = \"aperture\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := Lint(path)
	if err == nil {
		t.Fatalf("a file that is not TOML lints clean: %v", fs)
	}
	if !strings.Contains(err.Error(), ":1:") {
		t.Errorf("the error does not carry the line: %v", err)
	}
	if !strings.Contains(err.Error(), "will not parse") {
		t.Errorf("the error does not say the file will not parse: %v", err)
	}
}

// TestLintOnAMissingFile: no file is no findings and no error, the same nothing Load makes
// of it, so a first run warns about nothing.
func TestLintOnAMissingFile(t *testing.T) {
	fs, err := Lint(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if len(fs) != 0 {
		t.Errorf("findings for a file that is not there: %v", fs)
	}
}

// TestEveryDefaultIsAKeyTheLinterKnows is the drift guard between the linter's idea of what
// rudy reads and the keys rudy documents: a default the linter would flag is a false
// positive waiting for the operator who sets it.
func TestEveryDefaultIsAKeyTheLinterKnows(t *testing.T) {
	var b strings.Builder
	for key := range Defaults() {
		// A dotted key under its own header is the same key; the flat form is enough to
		// ask the schema about.
		b.WriteString(key)
		b.WriteString(" = 0\n")
	}
	if fs := lintFile(t, b.String()); len(fs) != 0 {
		t.Errorf("the linter does not know keys rudy defaults: %v", fs)
	}
}
