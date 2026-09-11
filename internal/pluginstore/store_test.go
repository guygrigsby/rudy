package pluginstore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// helloManifest is a valid plugin.toml for a plugin named hello, kept as a literal here so
// these tests do not depend on the working directory a test binary happens to run from. It
// carries no build command, unlike the real examples/plugins/hello/plugin.toml: newSourceRepo
// commits only plugin.toml, not the example's main.go, so a real go build here would have
// nothing to build.
const helloManifest = "name = \"hello\"\n" +
	"version = \"0.1.0\"\n" +
	"protocol_version = 1\n" +
	"command = \"hello\"\n" +
	"description = \"Example spawned plugin\"\n"

// newTestStore is New over a fresh temp root, the construction every Install/Update test in
// this file wants.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return New(t.TempDir())
}

// PluginDir exposes checkoutDir to tests that need to look inside a checkout after Install or
// Update, without duplicating its join.
func (s *Store) PluginDir(name string) string { return s.checkoutDir(name) }

// Lock is the lock file read straight from disk, for tests asserting on a raw entry's
// presence rather than going through Read's per-entry copy.
func (s *Store) Lock() lockFile {
	locked, err := s.Read()
	if err != nil {
		return lockFile{}
	}
	return lockFile{Plugins: locked}
}

var commitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// requireGit skips the test when git is not on PATH: these tests exercise real clones and
// commits, and cannot fake that out.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}
}

// gitEnv is the hermetic environment every git invocation in these tests runs with: no
// user or system config, no credential prompt, and an author identity so commit never
// blocks on one.
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=rudy",
		"GIT_AUTHOR_EMAIL=rudy@example.com",
		"GIT_COMMITTER_NAME=rudy",
		"GIT_COMMITTER_EMAIL=rudy@example.com",
	)
}

func runGitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// newSourceRepo creates a git repository in t.TempDir() holding manifest as plugin.toml,
// with one commit.
func newSourceRepo(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	runGitT(t, dir, "init", "-q", "-b", "main")
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(dir, "plugin.toml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	} else {
		// A repository with no plugin.toml still needs a commit to be a usable git source.
		if err := os.WriteFile(filepath.Join(dir, "README"), []byte("no manifest here\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGitT(t, dir, "add", "-A")
	runGitT(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

func wantHeadCommit(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	cmd.Env = gitEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// newTaggedSourceRepo is newSourceRepo plus a lightweight tag on its one commit, for tests
// pinning an install to a ref. It returns the repo path and the commit tag names, so a test
// can later advance the repo's default branch and assert the tag still names the same
// commit.
func newTaggedSourceRepo(t *testing.T, tag string) (dir, commit string) {
	t.Helper()
	dir = newSourceRepo(t, helloManifest)
	runGitT(t, dir, "tag", tag)
	return dir, wantHeadCommit(t, dir)
}

// TestInstallRunsTheManifestsBuild covers rudy-kab: a manifest's build command has to
// actually run in the staged checkout, not merely be parsed and carried around.
func TestInstallRunsTheManifestsBuild(t *testing.T) {
	requireGit(t)
	src := newSourceRepo(t, `name = "built"
version = "0.1.0"
protocol_version = 1
command = "./built"
build = "printf '#!/bin/sh\necho hi\n' > built && chmod +x built"
`)
	s := newTestStore(t)
	inst, _, err := s.Install(context.Background(), src, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.PluginDir(inst.Name), "built")); err != nil {
		t.Fatalf("build did not produce the binary: %v", err)
	}
}

// TestAFailingBuildRefusesTheInstall covers the other half: a build that fails must refuse
// the install outright, naming the step and the exit code, and leave neither a checkout nor
// a lock entry behind.
func TestAFailingBuildRefusesTheInstall(t *testing.T) {
	requireGit(t)
	src := newSourceRepo(t, `name = "broken"
version = "0.1.0"
protocol_version = 1
command = "./nothing"
build = "exit 3"
`)
	s := newTestStore(t)
	_, _, err := s.Install(context.Background(), src, time.Now())
	if err == nil {
		t.Fatal("a failing build was accepted")
	}
	if !strings.Contains(err.Error(), "build") || !strings.Contains(err.Error(), "3") {
		t.Fatalf("error names neither the step nor the exit code: %v", err)
	}
	if _, ok := s.Lock().Plugins["broken"]; ok {
		t.Fatal("a plugin whose build failed was written to the lock")
	}
	if _, err := os.Stat(s.PluginDir("broken")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a failed install left its checkout behind")
	}
}

// TestUpdateRunsTheBuildAgain installs a plugin whose build writes a marker, moves the
// source's build command to write a different marker, updates, and asserts the new marker
// replaced the old one. A git reset leaves an untracked file (the built marker) alone, so
// this only passes if Update actually reran the build rather than keeping install's stale
// artefact.
func TestUpdateRunsTheBuildAgain(t *testing.T) {
	requireGit(t)
	const name = "versioned"
	manifest := func(echo string) string {
		return "name = \"" + name + "\"\n" +
			"version = \"0.1.0\"\n" +
			"protocol_version = 1\n" +
			"command = \"./versioned\"\n" +
			"build = \"echo " + echo + " > marker\"\n"
	}
	src := newSourceRepo(t, manifest("v1"))
	s := newTestStore(t)
	if _, _, err := s.Install(context.Background(), src, time.Now()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	marker := filepath.Join(s.PluginDir(name), "marker")
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("marker not written by install's build: %v", err)
	}
	if strings.TrimSpace(string(b)) != "v1" {
		t.Fatalf("marker = %q, want v1", b)
	}

	if err := os.WriteFile(filepath.Join(src, "plugin.toml"), []byte(manifest("v2")), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, src, "add", "-A")
	runGitT(t, src, "commit", "-q", "-m", "bump build")

	if _, err := s.Update(context.Background(), name, time.Now()); err != nil {
		t.Fatalf("Update: %v", err)
	}
	b, err = os.ReadFile(marker)
	if err != nil {
		t.Fatalf("marker missing after update: %v", err)
	}
	if strings.TrimSpace(string(b)) != "v2" {
		t.Fatalf("marker = %q, want v2: update did not rerun the build", b)
	}
}

// TestAFailingBuildDuringUpdateLeavesTheLiveCheckoutUntouched covers fix round 1's Critical:
// Update stages a fetch or copy and its build the same way Install does, so a build that
// fails during Update must leave the live checkout, its build artefact and the lock exactly
// where a successful Update would have found them, not partway to the new commit.
func TestAFailingBuildDuringUpdateLeavesTheLiveCheckoutUntouched(t *testing.T) {
	requireGit(t)
	const name = "stable"
	src := newSourceRepo(t, `name = "stable"
version = "0.1.0"
protocol_version = 1
command = "./stable"
build = "echo v1 > marker"
`)
	s := newTestStore(t)
	inst, _, err := s.Install(context.Background(), "git:"+src, time.Now())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	wantCommit := inst.Commit
	marker := filepath.Join(s.PluginDir(name), "marker")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker not written by install's build: %v", err)
	}

	if err := os.WriteFile(filepath.Join(src, "plugin.toml"), []byte(`name = "stable"
version = "0.2.0"
protocol_version = 1
command = "./stable"
build = "exit 3"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, src, "add", "-A")
	runGitT(t, src, "commit", "-q", "-m", "break the build")

	if _, err := s.Update(context.Background(), name, time.Now()); err == nil {
		t.Fatal("an update whose build fails was accepted")
	} else if !strings.Contains(err.Error(), "build") || !strings.Contains(err.Error(), "3") {
		t.Fatalf("error names neither the step nor the exit code: %v", err)
	}

	// The live checkout must still be exactly what install left: the old manifest, the old
	// commit, the old build artefact, nothing from the broken commit.
	b, err := os.ReadFile(filepath.Join(s.PluginDir(name), "plugin.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "0.1.0") || strings.Contains(string(b), "0.2.0") {
		t.Fatalf("checkout plugin.toml = %s, want the pre-update version still in place", b)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("original build artefact gone after a failed update: %v", err)
	}
	if got := wantHeadCommit(t, s.PluginDir(name)); got != wantCommit {
		t.Fatalf("checkout HEAD = %s, want the pre-update commit %s", got, wantCommit)
	}
	if got := s.Lock().Plugins[name].Commit; got != wantCommit {
		t.Fatalf("lock commit = %s, want the pre-update commit %s", got, wantCommit)
	}

	ents, err := os.ReadDir(filepath.Join(s.Root, "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".update-") {
			t.Fatalf("stage left behind after a failed update: %s", e.Name())
		}
	}
}

func TestInstallClonesGitSourceAndRecordsLock(t *testing.T) {
	requireGit(t)
	src := newSourceRepo(t, helloManifest)
	source := "git:" + src
	s := New(t.TempDir())

	before := time.Now().Add(-time.Second)
	inst, m, err := s.Install(context.Background(), source, time.Now())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if m.Name != "hello" || m.Version != "0.1.0" {
		t.Fatalf("manifest = %+v", m)
	}
	if inst.Source != source {
		t.Fatalf("Source = %q, want %q", inst.Source, source)
	}
	if inst.Kind != KindGit {
		t.Fatalf("Kind = %q, want git", inst.Kind)
	}
	if !commitRe.MatchString(inst.Commit) {
		t.Fatalf("Commit = %q, want a 40 character hex sha", inst.Commit)
	}
	if inst.Commit != wantHeadCommit(t, src) {
		t.Fatalf("Commit = %s, want the source HEAD %s", inst.Commit, wantHeadCommit(t, src))
	}
	if !inst.Enabled {
		t.Fatal("Enabled = false, want true on a fresh install")
	}
	if inst.InstalledAt.Before(before) || inst.InstalledAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("InstalledAt = %v, want close to now", inst.InstalledAt)
	}
	if _, err := os.Stat(filepath.Join(s.Root, "plugins", "hello", "plugin.toml")); err != nil {
		t.Fatalf("checkout not written: %v", err)
	}

	locked, err := s.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	got, ok := locked["hello"]
	if !ok {
		t.Fatal("lock has no hello entry")
	}
	if got.Source != source || got.Kind != KindGit || got.Commit != inst.Commit || !got.Enabled {
		t.Fatalf("lock entry = %+v", got)
	}
}

// TestInstallCopiesAPathThatIsItselfARepository covers the "path" kind's other half: a bare
// local filesystem argument, with no "git:" prefix, is the path kind even when it happens to
// be a git checkout, so it never records a commit even though staging still clones it to get
// a clean copy free of uncommitted or ignored files. TestInstallClonesGitSourceAndRecordsLock
// covers the same checkout reached through "git:", where a commit is recorded.
func TestInstallCopiesAPathThatIsItselfARepository(t *testing.T) {
	requireGit(t)
	src := newSourceRepo(t, helloManifest)
	s := New(t.TempDir())

	inst, _, err := s.Install(context.Background(), src, time.Now())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if inst.Kind != KindPath {
		t.Fatalf("Kind = %q, want path for a bare local argument", inst.Kind)
	}
	if inst.Commit != "" {
		t.Fatalf("Commit = %q, want empty for the path kind", inst.Commit)
	}
	if _, err := os.Stat(filepath.Join(s.Root, "plugins", "hello", "plugin.toml")); err != nil {
		t.Fatalf("checkout not written: %v", err)
	}
}

func TestInstallRefusesADuplicateName(t *testing.T) {
	requireGit(t)
	src := newSourceRepo(t, helloManifest)
	s := New(t.TempDir())
	if _, _, err := s.Install(context.Background(), src, time.Now()); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	_, _, err := s.Install(context.Background(), src, time.Now())
	if err == nil || err.Error() != "hello is already installed" {
		t.Fatalf("second Install err = %v, want %q", err, "hello is already installed")
	}
}

func TestInstallRefusesASourceWithoutAManifest(t *testing.T) {
	requireGit(t)
	src := newSourceRepo(t, "")
	root := t.TempDir()
	s := New(root)

	_, _, err := s.Install(context.Background(), src, time.Now())
	if err == nil {
		t.Fatal("Install with no plugin.toml: want an error")
	}
	ents, rerr := os.ReadDir(filepath.Join(root, "plugins"))
	if rerr == nil && len(ents) != 0 {
		t.Fatalf("plugins dir = %v, want no leftover stage or checkout", ents)
	}
	locked, rerr := s.Read()
	if rerr != nil {
		t.Fatalf("Read: %v", rerr)
	}
	if len(locked) != 0 {
		t.Fatalf("lock = %+v, want no entry recorded", locked)
	}
}

func TestSetEnabledAndDisabled(t *testing.T) {
	requireGit(t)
	src := newSourceRepo(t, helloManifest)
	s := New(t.TempDir())
	if _, _, err := s.Install(context.Background(), src, time.Now()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got := s.Disabled(); len(got) != 0 {
		t.Fatalf("Disabled = %v before disabling anything", got)
	}
	if err := s.SetEnabled("hello", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if got := s.Disabled(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("Disabled = %v, want [hello]", got)
	}
	locked, err := s.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if locked["hello"].Enabled {
		t.Fatal("lock still shows hello enabled after SetEnabled(false)")
	}
	if err := s.SetEnabled("hello", true); err != nil {
		t.Fatalf("SetEnabled(true): %v", err)
	}
	if got := s.Disabled(); len(got) != 0 {
		t.Fatalf("Disabled = %v after re-enabling", got)
	}
}

func TestSetEnabledUnknownName(t *testing.T) {
	s := New(t.TempDir())
	err := s.SetEnabled("nope", false)
	if err == nil || err.Error() != "no plugin named nope" {
		t.Fatalf("err = %v, want %q", err, "no plugin named nope")
	}
}

func TestUpdateMovesCommitForwardForAClone(t *testing.T) {
	requireGit(t)
	src := newSourceRepo(t, helloManifest)
	s := New(t.TempDir())
	first, _, err := s.Install(context.Background(), "git:"+src, time.Now())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	// A second commit in the source repository, after the shallow clone already exists.
	if err := os.WriteFile(filepath.Join(src, "plugin.toml"), []byte(helloManifest+"# v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, src, "add", "-A")
	runGitT(t, src, "commit", "-q", "-m", "second")
	want := wantHeadCommit(t, src)
	if want == first.Commit {
		t.Fatal("test setup: second commit did not change HEAD")
	}

	updated, err := s.Update(context.Background(), "hello", time.Now())
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Commit != want {
		t.Fatalf("Commit after Update = %s, want %s", updated.Commit, want)
	}
	b, err := os.ReadFile(filepath.Join(s.Root, "plugins", "hello", "plugin.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "# v2") {
		t.Fatal("checkout was not updated to the new commit")
	}
	locked, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if locked["hello"].Commit != want {
		t.Fatalf("lock Commit = %s, want %s", locked["hello"].Commit, want)
	}
}

func TestUpdateUnknownName(t *testing.T) {
	s := New(t.TempDir())
	_, err := s.Update(context.Background(), "nope", time.Now())
	if err == nil || err.Error() != "no plugin named nope" {
		t.Fatalf("err = %v, want %q", err, "no plugin named nope")
	}
}

func TestUninstallRemovesDirectoryAndEntry(t *testing.T) {
	requireGit(t)
	src := newSourceRepo(t, helloManifest)
	s := New(t.TempDir())
	if _, _, err := s.Install(context.Background(), src, time.Now()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := s.Uninstall("hello"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, "plugins", "hello")); !os.IsNotExist(err) {
		t.Fatalf("checkout dir stat err = %v, want not exist", err)
	}
	locked, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := locked["hello"]; ok {
		t.Fatal("lock still has hello after Uninstall")
	}
}

func TestUninstallUnknownName(t *testing.T) {
	s := New(t.TempDir())
	err := s.Uninstall("nope")
	if err == nil || err.Error() != "no plugin named nope" {
		t.Fatalf("err = %v, want %q", err, "no plugin named nope")
	}
}

func TestInstallCopiesAPlainDirectorySource(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.toml"), []byte(helloManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(t.TempDir())

	inst, m, err := s.Install(context.Background(), dir, time.Now())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if m.Name != "hello" {
		t.Fatalf("manifest name = %q", m.Name)
	}
	if inst.Commit != "" {
		t.Fatalf("Commit = %q, want empty for a plain directory source", inst.Commit)
	}
	if inst.Source != dir {
		t.Fatalf("Source = %q, want %q", inst.Source, dir)
	}
	if _, err := os.Stat(filepath.Join(s.Root, "plugins", "hello", "plugin.toml")); err != nil {
		t.Fatalf("checkout not written: %v", err)
	}
	// The source directory itself must be untouched: Install copies, it does not move or
	// link.
	if _, err := os.Stat(filepath.Join(dir, "plugin.toml")); err != nil {
		t.Fatalf("source directory disturbed: %v", err)
	}
}

func TestUpdateRecopiesAPathSource(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.toml"), []byte(helloManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(t.TempDir())
	if _, _, err := s.Install(context.Background(), dir, time.Now()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.toml"), []byte(helloManifest+"# v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	updated, err := s.Update(context.Background(), "hello", time.Now())
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Commit != "" {
		t.Fatalf("Commit = %q, want empty after re-copy", updated.Commit)
	}
	b, err := os.ReadFile(filepath.Join(s.Root, "plugins", "hello", "plugin.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "# v2") {
		t.Fatal("checkout was not re-copied from the source")
	}
}

func TestReadMissingLockIsEmpty(t *testing.T) {
	s := New(t.TempDir())
	locked, err := s.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(locked) != 0 {
		t.Fatalf("locked = %+v, want empty", locked)
	}
}

func TestWriteIsWholeFileTempAndRenamePermissionsAndSortedNames(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	m := map[string]Installed{
		"zeta":  {Source: "z", Enabled: true, InstalledAt: time.Now()},
		"alpha": {Source: "a", Enabled: false, InstalledAt: time.Now()},
	}
	if err := s.Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	path := filepath.Join(root, LockFile)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ai := strings.Index(string(b), "[plugins.alpha]")
	zi := strings.Index(string(b), "[plugins.zeta]")
	if ai < 0 || zi < 0 || ai > zi {
		t.Fatalf("names not sorted in %s", b)
	}
	// No leftover temp file beside the lock.
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != LockFile {
		t.Fatalf("root entries = %v, want only %s", ents, LockFile)
	}
}

func TestDisabledFromLockKeepsItsBehavior(t *testing.T) {
	root := t.TempDir()
	// A missing lock disables nothing.
	got, err := DisabledFromLock(filepath.Join(root, LockFile))
	if err != nil || len(got) != 0 {
		t.Fatalf("DisabledFromLock(missing) = %v, %v", got, err)
	}
	s := New(root)
	if err := s.Write(map[string]Installed{
		"hello": {Source: "x", Enabled: false, InstalledAt: time.Now()},
		"other": {Source: "y", Enabled: true, InstalledAt: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	got, err = DisabledFromLock(filepath.Join(root, LockFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "hello" {
		t.Fatalf("DisabledFromLock = %v, want [hello]", got)
	}
	// A lock that will not parse is an error, not silence.
	if err := os.WriteFile(filepath.Join(root, LockFile), []byte("[plugins.hello\nenabled = \"yes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DisabledFromLock(filepath.Join(root, LockFile)); err == nil {
		t.Fatal("DisabledFromLock on a corrupt lock: want an error")
	}
}

// TestInstallRefusesAPathTraversingManifestName covers fix round 1 finding 1: a manifest
// naming itself with "../" must never let Install rename a checkout outside Root/plugins.
// plugin.ReadManifest is what actually refuses the name; this confirms Install surfaces
// that refusal cleanly, with no stage left behind and nothing landed outside the tree the
// traversal pointed at.
func TestInstallRefusesAPathTraversingManifestName(t *testing.T) {
	dir := t.TempDir()
	body := "name = \"../../somewhere/evil\"\nversion = \"0.1.0\"\nprotocol_version = 1\ncommand = \"hello\"\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	s := New(root)

	_, _, err := s.Install(context.Background(), dir, time.Now())
	if err == nil {
		t.Fatal("Install with a path-traversing manifest name: want an error")
	}
	ents, rerr := os.ReadDir(filepath.Join(root, "plugins"))
	if rerr == nil && len(ents) != 0 {
		t.Fatalf("plugins dir = %v, want no leftover stage or checkout", ents)
	}
	// The directory the traversal named, two levels above Root/plugins, must not exist.
	outside := filepath.Join(root, "plugins", "..", "..", "somewhere")
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("stat %s = %v, want not exist", outside, err)
	}
	locked, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(locked) != 0 {
		t.Fatalf("lock = %+v, want no entry recorded", locked)
	}
}

// TestInstallRecordsARelativePathSourceAsAbsolute covers fix round 1 finding 2: Update reads
// Installed.Source back later, possibly from a different working directory, so a relative
// local path source must be resolved to absolute before it is written to the lock.
func TestInstallRecordsARelativePathSourceAsAbsolute(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.toml"), []byte(helloManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(dir)
	base := filepath.Base(dir)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(parent); err != nil {
		t.Fatal(err)
	}

	// The expected value is computed the same way Install resolves it (filepath.Abs of the
	// same relative string, from the same cwd) rather than independently from dir: macOS
	// resolves symlinks (/tmp, /var/folders/...) into os.Getwd() after a Chdir, so a value
	// computed from dir before the Chdir is not always byte-identical to one computed after
	// it, even though both name the same directory.
	wantAbs, err := filepath.Abs("./" + base)
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	s := New(root)
	inst, _, err := s.Install(context.Background(), "./"+base, time.Now())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !filepath.IsAbs(inst.Source) || inst.Source != wantAbs {
		t.Fatalf("Source = %q, want the absolute path %q", inst.Source, wantAbs)
	}
	locked, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if locked["hello"].Source != wantAbs {
		t.Fatalf("lock Source = %q, want %q", locked["hello"].Source, wantAbs)
	}

	// A second commit's worth of change to the source, then Update from a cwd that has
	// nothing to do with the relative path Install saw: it must still find the source,
	// because the lock holds an absolute path now.
	if err := os.WriteFile(filepath.Join(dir, "plugin.toml"), []byte(helloManifest+"# v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	updated, err := s.Update(context.Background(), "hello", time.Now())
	if err != nil {
		t.Fatalf("Update from a different cwd: %v", err)
	}
	if updated.Commit != "" {
		t.Fatalf("Commit = %q, want empty for a path source", updated.Commit)
	}
	b, err := os.ReadFile(filepath.Join(root, "plugins", "hello", "plugin.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "# v2") {
		t.Fatal("Update from a different cwd did not re-copy the source")
	}
}

// TestNewToleratesAMissingPluginsDirectory covers the common case a fresh XDG data root: no
// plugins directory yet, and New must not create one or error.
func TestNewToleratesAMissingPluginsDirectory(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if s.Root != root {
		t.Fatalf("Root = %q, want %q", s.Root, root)
	}
	if _, err := os.Stat(filepath.Join(root, "plugins")); !os.IsNotExist(err) {
		t.Fatalf("plugins dir stat = %v, want still not exist", err)
	}
}

// TestNewLeavesAFreshStageAlone covers fix round 2 finding 1: New must never sweep a stage,
// since it runs on every session boot (through DisabledFromLock) and a session starting
// while another terminal's rudy plugin install is mid-clone must not touch that terminal's
// stage. Sweeping moved to Install and Update, and is age-gated there; see
// TestInstallSweepsAStaleStageButLeavesAFreshOne.
func TestNewLeavesAFreshStageAlone(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, "plugins", ".install-inprogress")
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	New(root)
	if _, err := os.Stat(stage); err != nil {
		t.Fatalf("New touched a live stage: %v", err)
	}
}

// TestDisabledFromLockLeavesAFreshStageAlone covers the same finding for the other path that
// reaches New on every session boot, through wire's discoverPlugins.
func TestDisabledFromLockLeavesAFreshStageAlone(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, "plugins", ".install-inprogress")
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := DisabledFromLock(filepath.Join(root, LockFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); err != nil {
		t.Fatalf("DisabledFromLock touched a live stage: %v", err)
	}
}

// TestInstallSweepsAStaleStageButLeavesAFreshOne covers the age gate itself: Install removes
// a leftover stage only once it is older than staleStageAge (backdated here with
// os.Chtimes, the way a real crash would leave one after an hour passes), and leaves a stage
// that could still be an in-progress install or update alone.
func TestInstallSweepsAStaleStageButLeavesAFreshOne(t *testing.T) {
	requireGit(t)
	src := newSourceRepo(t, helloManifest)
	root := t.TempDir()
	pluginsDir := filepath.Join(root, "plugins")
	stale := filepath.Join(pluginsDir, ".install-stale")
	fresh := filepath.Join(pluginsDir, ".update-fresh")
	for _, d := range []string{stale, fresh} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-staleStageAge - time.Minute)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	s := New(root)
	if _, _, err := s.Install(context.Background(), src, time.Now()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale stage stat = %v, want swept", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh stage was swept: %v", err)
	}
}

// TestIsRemoteSourceHostOnlySCPForm covers fix round 2 finding 2: git's scp-like shorthand
// works with or without a "user@", and isRemoteSource has to recognize both.
func TestIsRemoteSourceHostOnlySCPForm(t *testing.T) {
	if isRemoteSource("./rel") {
		t.Fatal(`"./rel" classified as remote`)
	}
	if !isRemoteSource("build.example.com:team/plugin.git") {
		t.Fatal("host-only scp form not classified as remote")
	}
	if !isRemoteSource("git@build.example.com:team/plugin.git") {
		t.Fatal("user@host scp form not classified as remote")
	}
}

// TestParseSourcePassesHostOnlySCPFormThrough is the same finding at the level Install
// actually calls: a host-only scp source must reach the lock untouched, not mangled through
// filepath.Abs as if it were a relative filesystem path.
func TestParseSourcePassesHostOnlySCPFormThrough(t *testing.T) {
	const source = "build.example.com:team/plugin.git"
	got, err := ParseSource(source)
	if err != nil {
		t.Fatalf("ParseSource: %v", err)
	}
	if got.Kind != KindGit || got.Location != source {
		t.Fatalf("ParseSource(%q) = %+v, want Kind git and Location untouched", source, got)
	}
}

// TestParseSourceStillResolvesALocalRelativePath guards against isRemoteSource's new
// colon-before-slash rule swallowing an ordinary local path.
func TestParseSourceStillResolvesALocalRelativePath(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(filepath.Dir(dir)); err != nil {
		t.Fatal(err)
	}
	rel := "./" + filepath.Base(dir)
	want, err := filepath.Abs(rel)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseSource(rel)
	if err != nil {
		t.Fatalf("ParseSource: %v", err)
	}
	if got.Kind != KindPath || got.Location != want {
		t.Fatalf("ParseSource(%q) = %+v, want Kind path and Location %q", rel, got, want)
	}
}

// TestUninstallRefusesAnUnsafeName covers fix round 2 finding 3: Uninstall, SetEnabled and
// Update all validate name with plugin.ValidateManifestName before touching the lock or the
// checkout directory. One covering test, per the ruling; the other two verbs share the exact
// same validateName call.
func TestUninstallRefusesAnUnsafeName(t *testing.T) {
	s := New(t.TempDir())
	err := s.Uninstall("../x")
	if err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("err = %v, want the manifest name rule's error", err)
	}
}

// TestInstallAtARefStaysThereOnUpdate is the pin test the contracts row promises: an install
// at a tag must stay at that tag's commit across an update even after the source's default
// branch has moved past it. Before stageGitUpdate re-resolved ref through a fetch and reset,
// Update always landed wherever a fresh shallow clone's default branch tip was, so this test
// fails the moment Update stops honouring Ref.
func TestInstallAtARefStaysThereOnUpdate(t *testing.T) {
	requireGit(t)
	src, tagged := newTaggedSourceRepo(t, "v1")
	s := newTestStore(t)
	inst, _, err := s.Install(context.Background(), "git:"+src+"@v1", time.Now())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if inst.Commit != tagged {
		t.Fatalf("Commit = %s, want the tagged commit %s", inst.Commit, tagged)
	}
	if inst.Ref != "v1" {
		t.Fatalf("Ref = %q, want v1", inst.Ref)
	}

	// The source's default branch moves on past the tag.
	if err := os.WriteFile(filepath.Join(src, "plugin.toml"), []byte(helloManifest+"# v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, src, "add", "-A")
	runGitT(t, src, "commit", "-q", "-m", "past the tag")
	tip := wantHeadCommit(t, src)
	if tip == tagged {
		t.Fatal("test setup: the new commit did not move the source's tip")
	}

	updated, err := s.Update(context.Background(), "hello", time.Now())
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Commit != tagged {
		t.Fatalf("Commit after Update = %s, want it to stay at the tag %s rather than drift to the tip %s", updated.Commit, tagged, tip)
	}
	if updated.Ref != "v1" {
		t.Fatalf("Ref after Update = %q, want v1 unchanged", updated.Ref)
	}
	locked, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if locked["hello"].Commit != tagged || locked["hello"].Ref != "v1" {
		t.Fatalf("lock entry after Update = %+v, want commit %s and ref v1", locked["hello"], tagged)
	}
}

// TestInstallAtABareCommitFallsBackToAFullClone covers the other half of stageGitInstall's
// pin: a ref that names a commit rather than a branch or tag is not something git's --branch
// flag understands, so the shallow attempt has to fail over to a full clone plus checkout.
func TestInstallAtABareCommitFallsBackToAFullClone(t *testing.T) {
	requireGit(t)
	src := newSourceRepo(t, helloManifest)
	first := wantHeadCommit(t, src)
	if err := os.WriteFile(filepath.Join(src, "plugin.toml"), []byte(helloManifest+"# v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, src, "add", "-A")
	runGitT(t, src, "commit", "-q", "-m", "second")

	s := newTestStore(t)
	inst, _, err := s.Install(context.Background(), "git:"+src+"@"+first, time.Now())
	if err != nil {
		t.Fatalf("Install at a bare commit: %v", err)
	}
	if inst.Commit != first {
		t.Fatalf("Commit = %s, want the pinned commit %s", inst.Commit, first)
	}
	if inst.Ref != first {
		t.Fatalf("Ref = %q, want %q", inst.Ref, first)
	}
	b, err := os.ReadFile(filepath.Join(s.Root, "plugins", "hello", "plugin.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "# v2") {
		t.Fatal("checkout has the second commit's content, want the first commit's")
	}
}

// TestLockRecordsKindAndRef asserts the lock's own bytes carry kind and ref, not just that
// Read reports them: rudy plugins update and a future rudy plugins list read the file
// directly, and either field silently missing from the TOML would still let this pass if the
// assertion only went through Installed.
func TestLockRecordsKindAndRef(t *testing.T) {
	requireGit(t)
	src, _ := newTaggedSourceRepo(t, "v1")
	s := newTestStore(t)
	if _, _, err := s.Install(context.Background(), "git:"+src+"@v1", time.Now()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(s.Root, LockFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `kind = 'git'`) && !strings.Contains(string(b), `kind = "git"`) {
		t.Fatalf("lock does not record kind = git:\n%s", b)
	}
	if !strings.Contains(string(b), `ref = 'v1'`) && !strings.Contains(string(b), `ref = "v1"`) {
		t.Fatalf("lock does not record ref = v1:\n%s", b)
	}
}

// TestReadDerivesKindForAnOldFormatLock covers the contracts row's compatibility clause: a
// lock written before kind existed has only source, commit, installed_at and enabled, and
// Read must derive kind from commit being set rather than leave it the zero value. The
// fixture is hand-written, not produced by Write, so this fails if the derivation in Read is
// ever removed rather than passing by construction from the current writer.
func TestReadDerivesKindForAnOldFormatLock(t *testing.T) {
	root := t.TempDir()
	body := `[plugins.fromgit]
source = "git:example.com/a/b"
commit = "` + strings.Repeat("a", 40) + `"
installed_at = 2024-01-01T00:00:00Z
enabled = true

[plugins.frompath]
source = "/some/local/plugin"
installed_at = 2024-01-01T00:00:00Z
enabled = true
`
	if err := os.WriteFile(filepath.Join(root, LockFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	locked, err := New(root).Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := locked["fromgit"].Kind; got != KindGit {
		t.Fatalf("fromgit Kind = %q, want git derived from a non-empty commit", got)
	}
	if got := locked["frompath"].Kind; got != KindPath {
		t.Fatalf("frompath Kind = %q, want path derived from an empty commit", got)
	}
}

// TestSwapCheckoutOrphansABackupRatherThanDestroyingOnFailure covers rudy-xpe: the old
// os.RemoveAll(dir) then os.Rename(stage, dir) swap could lose a checkout entirely if the
// rename half failed after the removal had already succeeded. swapCheckout's three-step
// version renames dir aside first, so the same failure leaves the pre-update checkout intact
// at dir+".old" instead. A missing stage forces the second rename to fail deterministically,
// after the first rename has already moved the live checkout aside.
func TestSwapCheckoutOrphansABackupRatherThanDestroyingOnFailure(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "hello")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "marker")
	if err := os.WriteFile(marker, []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}
	missingStage := filepath.Join(root, "does-not-exist")

	if err := swapCheckout(dir, missingStage); err == nil {
		t.Fatal("swapCheckout with a missing stage: want an error")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir stat = %v, want not exist: the failed rename should have moved it aside", err)
	}
	b, err := os.ReadFile(filepath.Join(dir+".old", "marker"))
	if err != nil {
		t.Fatalf("backup missing after a failed swap: %v", err)
	}
	if string(b) != "live" {
		t.Fatalf("backup contents = %q, want %q", b, "live")
	}
}

// TestUpdateLeavesNoBackupDirectoryOnSuccess is swapCheckout's happy path from Update: the
// .old directory it swaps through is a step, not a permanent artefact, so a successful
// update must not leave one behind next to the live checkout.
func TestUpdateLeavesNoBackupDirectoryOnSuccess(t *testing.T) {
	requireGit(t)
	src := newSourceRepo(t, helloManifest)
	s := newTestStore(t)
	if _, _, err := s.Install(context.Background(), "git:"+src, time.Now()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "plugin.toml"), []byte(helloManifest+"# v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, src, "add", "-A")
	runGitT(t, src, "commit", "-q", "-m", "second")
	if _, err := s.Update(context.Background(), "hello", time.Now()); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := os.Stat(s.PluginDir("hello") + ".old"); !os.IsNotExist(err) {
		t.Fatalf("stat .old = %v, want not exist after a successful update", err)
	}
}

// TestSwapCheckoutRecoversWhenDirIsMissingButOldHoldsABackup covers fix round 1's Important 1,
// first way in: a prior swapCheckout call failed between its two renames, leaving dir gone
// and dir+".old" holding the only copy. The old swapCheckout unconditionally removed old
// before checking whether dir existed, so retrying here would delete that surviving backup
// and then fail the rename with ENOENT, leaving neither. The fixed version must reach straight
// for the rename-in when dir is already absent, leaving old untouched until the new checkout
// is confirmed in place.
func TestSwapCheckoutRecoversWhenDirIsMissingButOldHoldsABackup(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "hello")
	old := dir + ".old"
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "marker"), []byte("backup"), 0o644); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, "stage")
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "marker"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := swapCheckout(dir, stage); err != nil {
		t.Fatalf("swapCheckout: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "marker"))
	if err != nil {
		t.Fatalf("dir missing the new checkout: %v", err)
	}
	if string(b) != "new" {
		t.Fatalf("dir contents = %q, want the new checkout", b)
	}
}

// TestSwapCheckoutRepairsAMissingDirWithNoBackup covers fix round 1's Important 1, second way
// in: no prior fault at all, an operator ran rm -rf on the live checkout directly (the repair
// the old remove-then-rename code supported) and there is no dir+".old" either. swapCheckout
// must still land the new checkout rather than fail on a rename whose source it wrongly
// assumed would exist.
func TestSwapCheckoutRepairsAMissingDirWithNoBackup(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "hello") // never created; nothing at dir+".old" either
	stage := filepath.Join(root, "stage")
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "marker"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := swapCheckout(dir, stage); err != nil {
		t.Fatalf("swapCheckout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "marker")); err != nil {
		t.Fatalf("dir missing after repair: %v", err)
	}
}

// TestUpdateAtABareCommitPinStaysThere covers fix round 1's minor: Update had no coverage at
// all for a bare-commit pin, only Install did. Installs pinned at a commit that is not the
// tip of any branch or tag, advances the source further, and asserts Update's fetch-and-reset
// re-resolves to the same pinned commit rather than the new tip.
func TestUpdateAtABareCommitPinStaysThere(t *testing.T) {
	requireGit(t)
	src := newSourceRepo(t, helloManifest)
	first := wantHeadCommit(t, src)
	if err := os.WriteFile(filepath.Join(src, "plugin.toml"), []byte(helloManifest+"# v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, src, "add", "-A")
	runGitT(t, src, "commit", "-q", "-m", "second")

	s := newTestStore(t)
	inst, _, err := s.Install(context.Background(), "git:"+src+"@"+first, time.Now())
	if err != nil {
		t.Fatalf("Install at a bare commit: %v", err)
	}
	if inst.Commit != first {
		t.Fatalf("Commit = %s, want %s", inst.Commit, first)
	}

	// The source moves on again after the install.
	if err := os.WriteFile(filepath.Join(src, "plugin.toml"), []byte(helloManifest+"# v3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, src, "add", "-A")
	runGitT(t, src, "commit", "-q", "-m", "third")

	updated, err := s.Update(context.Background(), "hello", time.Now())
	if err != nil {
		t.Fatalf("Update at a bare commit: %v", err)
	}
	if updated.Commit != first {
		t.Fatalf("Commit after Update = %s, want it to stay at the pinned commit %s", updated.Commit, first)
	}
	b, err := os.ReadFile(filepath.Join(s.Root, "plugins", "hello", "plugin.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "# v2") || strings.Contains(string(b), "# v3") {
		t.Fatal("checkout moved past the pinned commit")
	}
}

// TestUninstallRemovesAnOrphanedOldBackup covers fix round 1's Important 2: a crash between
// swapCheckout's two renames can leave name+".old" beside the checkout, which plugin.Discover
// then walks as a plugin directory whose manifest name does not match, reporting an error
// notice on every boot. Uninstall must remove it too, not just the live checkout.
func TestUninstallRemovesAnOrphanedOldBackup(t *testing.T) {
	requireGit(t)
	src := newSourceRepo(t, helloManifest)
	s := newTestStore(t)
	if _, _, err := s.Install(context.Background(), "git:"+src, time.Now()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	old := s.PluginDir("hello") + ".old"
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Uninstall("hello"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("orphaned .old backup stat = %v, want removed by Uninstall", err)
	}
}

// TestInstallSweepsAStaleOldBackup covers the other half of fix round 1's Important 2: the
// sweep that clears abandoned .install-*/.update-* stages must clear a stale *.old backup the
// same way, age-gated identically, so one left behind by a crash does not sit forever for
// plugin.Discover to trip over.
func TestInstallSweepsAStaleOldBackup(t *testing.T) {
	requireGit(t)
	src := newSourceRepo(t, helloManifest)
	root := t.TempDir()
	pluginsDir := filepath.Join(root, "plugins")
	staleOld := filepath.Join(pluginsDir, "orphan.old")
	freshOld := filepath.Join(pluginsDir, "fresh.old")
	for _, d := range []string{staleOld, freshOld} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-staleStageAge - time.Minute)
	if err := os.Chtimes(staleOld, old, old); err != nil {
		t.Fatal(err)
	}

	s := New(root)
	if _, _, err := s.Install(context.Background(), src, time.Now()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(staleOld); !os.IsNotExist(err) {
		t.Fatalf("stale .old stat = %v, want swept", err)
	}
	if _, err := os.Stat(freshOld); err != nil {
		t.Fatalf("fresh .old was swept: %v", err)
	}
}
