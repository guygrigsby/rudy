package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestManifest writes a plugin.toml with the given name into a fresh temp directory
// (whose own basename is unrelated to name, the way rudy plugin Install's random stage
// directory is) and returns the directory.
func writeTestManifest(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	body := "name = \"" + name + "\"\nversion = \"0.1.0\"\nprotocol_version = 1\ncommand = \"hello\"\n"
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReadManifestRefusesAPathTraversingName(t *testing.T) {
	dir := writeTestManifest(t, "../../somewhere/evil")
	_, err := ReadManifest(dir)
	if err == nil {
		t.Fatal("ReadManifest with a path-traversing name: want an error")
	}
	if !strings.Contains(err.Error(), "must match") {
		t.Fatalf("err = %v, want it to name the rule", err)
	}
}

func TestReadManifestRefusesASlashInTheName(t *testing.T) {
	dir := writeTestManifest(t, "sub/dir")
	if _, err := ReadManifest(dir); err == nil {
		t.Fatal("ReadManifest with a slash in the name: want an error")
	}
}

func TestReadManifestAcceptsAnOrdinaryName(t *testing.T) {
	dir := writeTestManifest(t, "hello-world_2")
	m, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.Name != "hello-world_2" {
		t.Fatalf("Name = %q", m.Name)
	}
}
