package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const cliHelloManifest = "name = \"hello\"\n" +
	"version = \"0.1.0\"\n" +
	"protocol_version = 1\n" +
	"command = \"hello\"\n" +
	"description = \"Example spawned plugin\"\n"

// runPlugin executes one rudy invocation and returns everything it printed. Each call gets
// a fresh root, the same reason runMCP does: a cobra command remembers the flags it parsed.
func runPlugin(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := NewRoot()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func requireGitCLI(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}
}

func runGitCLI(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=rudy",
		"GIT_AUTHOR_EMAIL=rudy@example.com",
		"GIT_COMMITTER_NAME=rudy",
		"GIT_COMMITTER_EMAIL=rudy@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// newPluginSourceRepo is a git repository in t.TempDir() holding the hello manifest, with
// one commit.
func newPluginSourceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGitCLI(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "plugin.toml"), []byte(cliHelloManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCLI(t, dir, "add", "-A")
	runGitCLI(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

func TestPluginInstallPrintsNameVersionAndCommit(t *testing.T) {
	requireGitCLI(t)
	tempXDG(t)
	src := newPluginSourceRepo(t)

	// The "git:" prefix, rather than the bare path: a bare local argument is the path kind
	// (ADR 0025), which never records a commit even when it happens to be a checkout, so
	// this printed line would otherwise show the full source path instead of a short sha.
	out, err := runPlugin(t, "plugin", "install", "git:"+src)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if !strings.Contains(out, "build command") {
		t.Fatalf("output missing the pre-install build warning: %q", out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "installed hello 0.1.0 at ") {
		t.Fatalf("output = %q", out)
	}
	commitPart := strings.TrimPrefix(last, "installed hello 0.1.0 at ")
	if len(commitPart) != 8 {
		t.Fatalf("commit part = %q, want 8 characters", commitPart)
	}
}

func TestPluginListPrintsHeaderAndRow(t *testing.T) {
	requireGitCLI(t)
	tempXDG(t)
	src := newPluginSourceRepo(t)
	if _, err := runPlugin(t, "plugin", "install", src); err != nil {
		t.Fatalf("install: %v", err)
	}

	out, err := runPlugin(t, "plugin", "list")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("output = %q", out)
	}
	if !strings.HasPrefix(lines[0], "NAME") || !strings.Contains(lines[0], "VERSION") ||
		!strings.Contains(lines[0], "ENABLED") || !strings.Contains(lines[0], "KIND") ||
		!strings.Contains(lines[0], "REF") || !strings.Contains(lines[0], "COMMIT") ||
		!strings.Contains(lines[0], "SOURCE") {
		t.Fatalf("header = %q", lines[0])
	}
	// A bare local argument installs as the path kind (ADR 0025): no ref, a pin would be
	// invisible in this listing if kind and ref were not their own columns.
	if !strings.Contains(lines[1], "hello") || !strings.Contains(lines[1], "0.1.0") ||
		!strings.Contains(lines[1], "true") || !strings.Contains(lines[1], "path") ||
		!strings.Contains(lines[1], src) {
		t.Fatalf("row = %q", lines[1])
	}
}

func TestPluginDisableEnableUpdateUninstall(t *testing.T) {
	requireGitCLI(t)
	tempXDG(t)
	src := newPluginSourceRepo(t)
	if _, err := runPlugin(t, "plugin", "install", src); err != nil {
		t.Fatalf("install: %v", err)
	}

	if out, err := runPlugin(t, "plugin", "disable", "hello"); err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("disable: %v %q", err, out)
	}
	if out, err := runPlugin(t, "plugin", "enable", "hello"); err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("enable: %v %q", err, out)
	}

	// A second commit upstream, so update has somewhere to move to.
	if err := os.WriteFile(filepath.Join(src, "plugin.toml"), []byte(cliHelloManifest+"# v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCLI(t, src, "add", "-A")
	runGitCLI(t, src, "commit", "-q", "-m", "second")

	if out, err := runPlugin(t, "plugin", "update", "hello"); err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("update: %v %q", err, out)
	}
	if out, err := runPlugin(t, "plugin", "uninstall", "hello"); err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("uninstall: %v %q", err, out)
	}
}

// TestInstallIsTheSameCommandUnderBothSpellings holds root install and the plugins noun's
// install to the same behavior: ADR 0025 decision 2 says the top-level spelling does the
// same thing as rudy plugins install, not an approximation of it.
func TestInstallIsTheSameCommandUnderBothSpellings(t *testing.T) {
	requireGitCLI(t)
	src := newPluginSourceRepo(t)
	for _, argv := range [][]string{{"install", src}, {"plugins", "install", src}} {
		tempXDG(t)
		out, err := runPlugin(t, argv...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", argv, err, out)
		}
		if !strings.Contains(out, "installed hello 0.1.0 at ") {
			t.Fatalf("%v: output = %q", argv, out)
		}
	}
}

func TestPluginUninstallUnknownNameExitsWithMessage(t *testing.T) {
	tempXDG(t)
	_, err := runPlugin(t, "plugin", "uninstall", "nope")
	if err == nil || err.Error() != "no plugin named nope" {
		t.Fatalf("err = %v, want %q", err, "no plugin named nope")
	}
}
