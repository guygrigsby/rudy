package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/pluginstore"
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

// TestResolvedAtPrintsEachKindsRecord covers what install and update print after "at". A go: or
// https install records no commit at all, so printing the source for anything without one threw
// away the digest that is the entire reproducibility record of those two kinds: "at
// go:x@v0.2.0" tells an operator only what they already typed.
func TestResolvedAtPrintsEachKindsRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		inst pluginstore.Installed
		want string
	}{
		{"git shortens the commit", pluginstore.Installed{Kind: pluginstore.KindGit, Commit: "0123456789abcdef0123", Source: "git:example.com/o/r"}, "01234567"},
		{"go prints the whole digest", pluginstore.Installed{Kind: pluginstore.KindGo, Digest: "v0.2.0 h1:Zm9vYmFy=", Source: "go:example.com/m@v0.2.0"}, "v0.2.0 h1:Zm9vYmFy="},
		{"https prints the whole digest", pluginstore.Installed{Kind: pluginstore.KindHTTPS, Digest: "sha256:d0be", Source: "https://example.com/p.tar.gz"}, "sha256:d0be"},
		{"a path has neither", pluginstore.Installed{Kind: pluginstore.KindPath, Source: "/src/hello"}, "/src/hello"},
		{"a git entry with no commit falls back", pluginstore.Installed{Kind: pluginstore.KindGit, Source: "git:example.com/o/r"}, "git:example.com/o/r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvedAt(tc.inst); got != tc.want {
				t.Fatalf("resolvedAt = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPluginListPrintsTheDigestColumn covers the same gap in the listing: without a DIGEST
// column there is nowhere for a go: or https entry's resolved sum to appear at all.
// renderPlugins is called directly, with a lock holding one entry per kind, since installing a
// real go module or tarball needs a module proxy or a TLS server this command has no way to be
// handed. The store is an empty root, so every version reads "-": the manifest lookup is not
// what is under test here.
func TestPluginListPrintsTheDigestColumn(t *testing.T) {
	locked := map[string]pluginstore.Installed{
		"agit":   {Name: "agit", Kind: pluginstore.KindGit, Ref: "v1", Commit: "0123456789abcdef", Source: "git:example.com/o/r@v1", Enabled: true},
		"bgo":    {Name: "bgo", Kind: pluginstore.KindGo, Ref: "v0.2.0", Digest: "v0.2.0 h1:Zm9vYmFy=", Source: "go:example.com/m@v0.2.0", Enabled: true},
		"chttps": {Name: "chttps", Kind: pluginstore.KindHTTPS, Digest: "sha256:d0be", Source: "https://example.com/p.tar.gz", Enabled: true},
	}
	var out bytes.Buffer
	if err := renderPlugins(&out, pluginstore.New(t.TempDir()), locked); err != nil {
		t.Fatalf("renderPlugins: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("output = %q", out.String())
	}
	if !strings.Contains(lines[0], "DIGEST") {
		t.Fatalf("header = %q, want a DIGEST column", lines[0])
	}
	for i, want := range []string{"0123456789abcdef", "v0.2.0 h1:Zm9vYmFy=", "sha256:d0be"} {
		if !strings.Contains(lines[i+1], want) {
			t.Fatalf("row = %q, want it to carry %q", lines[i+1], want)
		}
	}
}

// TestPluginUpdateSaysUnchangedWhenNothingMoved covers the update line itself, through the real
// command: "updated" printed for a re-resolve that landed on the commit already installed
// claims something happened that did not, and the operator has no other signal that it didn't.
func TestPluginUpdateSaysUnchangedWhenNothingMoved(t *testing.T) {
	requireGitCLI(t)
	tempXDG(t)
	src := newPluginSourceRepo(t)
	if _, err := runPlugin(t, "install", "git:"+src); err != nil {
		t.Fatalf("install: %v", err)
	}

	out, err := runPlugin(t, "plugin", "update", "hello")
	if err != nil {
		t.Fatalf("update: %v\n%s", err, out)
	}
	if !strings.HasPrefix(out, "unchanged hello at ") {
		t.Fatalf("update with nothing new upstream printed %q, want it to say unchanged", out)
	}

	// The source moves on, and the same command says so.
	if err := os.WriteFile(filepath.Join(src, "plugin.toml"), []byte(cliHelloManifest+"# v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCLI(t, src, "add", "-A")
	runGitCLI(t, src, "commit", "-q", "-m", "second")

	out, err = runPlugin(t, "plugin", "update", "hello")
	if err != nil {
		t.Fatalf("update: %v\n%s", err, out)
	}
	if !strings.HasPrefix(out, "updated hello at ") {
		t.Fatalf("update across a new commit printed %q, want it to say updated", out)
	}
}
