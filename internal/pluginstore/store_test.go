package pluginstore

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// helloManifest is a valid plugin.toml matching examples/plugins/hello, kept as a literal
// here so these tests do not depend on the working directory a test binary happens to run
// from.
const helloManifest = "name = \"hello\"\n" +
	"version = \"0.1.0\"\n" +
	"protocol_version = 1\n" +
	"command = \"hello\"\n" +
	"description = \"Example spawned plugin\"\n"

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

func TestInstallClonesGitSourceAndRecordsLock(t *testing.T) {
	requireGit(t)
	src := newSourceRepo(t, helloManifest)
	s := New(t.TempDir())

	before := time.Now().Add(-time.Second)
	inst, m, err := s.Install(context.Background(), src, time.Now())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if m.Name != "hello" || m.Version != "0.1.0" {
		t.Fatalf("manifest = %+v", m)
	}
	if inst.Source != src {
		t.Fatalf("Source = %q, want %q", inst.Source, src)
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
	if got.Source != src || got.Commit != inst.Commit || !got.Enabled {
		t.Fatalf("lock entry = %+v", got)
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
	first, _, err := s.Install(context.Background(), src, time.Now())
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

// TestResolveSourcePassesHostOnlySCPFormThrough is the same finding at the level Install
// actually calls: a host-only scp source must reach the lock untouched, not mangled through
// filepath.Abs as if it were a relative filesystem path.
func TestResolveSourcePassesHostOnlySCPFormThrough(t *testing.T) {
	const source = "build.example.com:team/plugin.git"
	got, err := resolveSource(source)
	if err != nil {
		t.Fatalf("resolveSource: %v", err)
	}
	if got != source {
		t.Fatalf("resolveSource(%q) = %q, want it untouched", source, got)
	}
}

// TestResolveSourceStillResolvesALocalRelativePath guards against isRemoteSource's new
// colon-before-slash rule swallowing an ordinary local path.
func TestResolveSourceStillResolvesALocalRelativePath(t *testing.T) {
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
	got, err := resolveSource(rel)
	if err != nil {
		t.Fatalf("resolveSource: %v", err)
	}
	if got != want {
		t.Fatalf("resolveSource(%q) = %q, want %q", rel, got, want)
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
