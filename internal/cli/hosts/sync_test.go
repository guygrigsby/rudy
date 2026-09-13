package hosts

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecide(t *testing.T) {
	cases := []struct {
		name   string
		local  LocalState
		remote State
		want   Plan
	}{
		{"absent git", LocalState{IsGit: true}, State{Exists: false}, PlanPush},
		{"clean behind", LocalState{IsGit: true, RemoteIsAncestor: true}, State{Exists: true, IsGit: true}, PlanPush},
		{"clean same", LocalState{IsGit: true, Head: "a", RemoteIsAncestor: true, LocalIsAncestor: true}, State{Exists: true, IsGit: true, Head: "a"}, PlanPush},
		{"dirty box", LocalState{IsGit: true, RemoteIsAncestor: true}, State{Exists: true, IsGit: true, Dirty: true}, PlanOpenAsIs},
		{"box ahead", LocalState{IsGit: true, LocalIsAncestor: true}, State{Exists: true, IsGit: true}, PlanOpenAsIs},
		{"diverged", LocalState{IsGit: true}, State{Exists: true, IsGit: true}, PlanRefuse},
		{"not git there", LocalState{IsGit: true}, State{Exists: true, IsGit: false}, PlanRefuse},
		{"absent copy", LocalState{IsGit: false}, State{Exists: false}, PlanCopy},
		{"present copy", LocalState{IsGit: false}, State{Exists: true}, PlanOpenAsIs},
	}
	for _, c := range cases {
		got, _ := Decide(c.local, c.remote)
		if got != c.want {
			t.Errorf("%s: Decide = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestInspectReadsThePlacement pins the one parser in this package: three lines of shell
// output are the whole of what the client knows about the host's tree, and every plan below
// is chosen from them.
func TestInspectReadsThePlacement(t *testing.T) {
	requireGit(t)
	box := t.TempDir()
	r := localRunner{home: box}
	ctx := context.Background()

	absent := filepath.Join(box, "nothing")
	if got, err := Inspect(ctx, r, absent); err != nil || got.Exists {
		t.Fatalf("Inspect(absent) = %+v, %v", got, err)
	}

	plain := filepath.Join(box, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Inspect(ctx, r, plain)
	if err != nil || !got.Exists || got.IsGit {
		t.Fatalf("Inspect(plain dir) = %+v, %v", got, err)
	}

	repo := gitRepo(t, "one")
	got, err = Inspect(ctx, r, repo)
	if err != nil || !got.Exists || !got.IsGit || got.Dirty || got.Head == "" {
		t.Fatalf("Inspect(clean checkout) = %+v, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(repo, "one.txt"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := Inspect(ctx, r, repo); err != nil || !got.Dirty {
		t.Fatalf("Inspect(dirty checkout) = %+v, %v", got, err)
	}
}

func TestPushCreatesTheCheckoutAndCarriesUncommittedChanges(t *testing.T) {
	requireGit(t)
	box := t.TempDir()
	r := localRunner{home: box}
	local := gitRepo(t, "one")
	// An uncommitted edit and a new file ride along on top of the pushed branch.
	_ = os.WriteFile(filepath.Join(local, "one.txt"), []byte("edited"), 0o644)
	_ = os.WriteFile(filepath.Join(local, "new.txt"), []byte("new"), 0o644)
	placement := filepath.Join(box, "projects", "demo")
	if err := Sync(context.Background(), r, Host{destination: "box"}, local, placement, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := gitLine(t, placement, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Fatalf("box branch = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(placement, "one.txt")); string(got) != "edited" {
		t.Fatalf("uncommitted edit did not arrive: %q", got)
	}
	if _, err := os.Stat(filepath.Join(placement, "new.txt")); err != nil {
		t.Fatal("untracked file did not arrive")
	}
	if got := gitLine(t, placement, "config", "receive.denyCurrentBranch"); got != "updateInstead" {
		t.Fatalf("receive.denyCurrentBranch = %q", got)
	}
}

// TestPushCarriesADeletion: a file the operator removed here is removed there too, or the
// box builds against a file the Mac no longer has.
func TestPushCarriesADeletion(t *testing.T) {
	requireGit(t)
	box := t.TempDir()
	r := localRunner{home: box}
	local := gitRepo(t, "one")
	placement := filepath.Join(box, "projects", "demo")
	if err := Sync(context.Background(), r, Host{destination: "box"}, local, placement, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(local, "one.txt")); err != nil {
		t.Fatal(err)
	}
	if err := Sync(context.Background(), r, Host{destination: "box"}, local, placement, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(placement, "one.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a deleted file is still on the box: %v", err)
	}
}

func TestSyncLeavesADirtyBoxAlone(t *testing.T) {
	requireGit(t)
	box := t.TempDir()
	r := localRunner{home: box}
	local := gitRepo(t, "one")
	placement := filepath.Join(box, "projects", "demo")
	if err := Sync(context.Background(), r, Host{destination: "box"}, local, placement, io.Discard); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(placement, "one.txt"), []byte("box work"), 0o644)
	_ = os.WriteFile(filepath.Join(local, "one.txt"), []byte("mac work"), 0o644)
	var out bytes.Buffer
	if err := Sync(context.Background(), r, Host{destination: "box"}, local, placement, &out); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(placement, "one.txt")); string(got) != "box work" {
		t.Fatalf("a dirty box was overwritten: %q", got)
	}
	if !strings.Contains(out.String(), "dirty") {
		t.Fatalf("no notice about the dirty box: %q", out.String())
	}
}

func TestSyncRefusesDivergedBranches(t *testing.T) {
	requireGit(t)
	box := t.TempDir()
	r := localRunner{home: box}
	local := gitRepo(t, "one")
	placement := filepath.Join(box, "projects", "demo")
	if err := Sync(context.Background(), r, Host{destination: "box"}, local, placement, io.Discard); err != nil {
		t.Fatal(err)
	}
	commit(t, placement, "box side")
	commit(t, local, "mac side")
	err := Sync(context.Background(), r, Host{destination: "box"}, local, placement, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "rudy hosts pull") {
		t.Fatalf("diverged sync = %v, want a refusal naming rudy hosts pull", err)
	}
}

// TestSyncPushesABoxThatIsBehind: the ordinary second round. The box holds what the last
// sync left there, the Mac has moved on, and the new commit arrives without a notice.
func TestSyncPushesABoxThatIsBehind(t *testing.T) {
	requireGit(t)
	box := t.TempDir()
	r := localRunner{home: box}
	local := gitRepo(t, "one")
	placement := filepath.Join(box, "projects", "demo")
	if err := Sync(context.Background(), r, Host{destination: "box"}, local, placement, io.Discard); err != nil {
		t.Fatal(err)
	}
	commit(t, local, "second")
	if err := Sync(context.Background(), r, Host{destination: "box"}, local, placement, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, want := gitLine(t, placement, "rev-parse", "HEAD"), gitLine(t, local, "rev-parse", "HEAD"); got != want {
		t.Fatalf("box head = %s, want the local head %s", got, want)
	}
}

// TestSyncFromASubdirectoryPlacesTheRoot: `rudy --host box` typed inside a checkout moves
// the checkout, so the placement the tree lands at is the root's and not the one the cwd
// maps to. The session still opens at the cwd's placement, which the tree brings with it.
func TestSyncFromASubdirectoryPlacesTheRoot(t *testing.T) {
	requireGit(t)
	box := t.TempDir()
	r := localRunner{home: box}
	local := gitRepo(t, "one")
	sub := filepath.Join(local, "internal")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "deep.txt"), []byte("deep"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(box, "projects", "demo")
	if err := Sync(context.Background(), r, Host{destination: "box"}, sub, filepath.Join(root, "internal"), io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := gitLine(t, root, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Fatalf("the checkout landed somewhere else: box branch at %s = %q", root, got)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "internal", "deep.txt")); string(got) != "deep" {
		t.Fatalf("the subdirectory's file is not under the root's placement: %q", got)
	}
}

func TestSyncCopiesANonGitTreeOnce(t *testing.T) {
	requireTar(t)
	box := t.TempDir()
	r := localRunner{home: box}
	local := t.TempDir()
	_ = os.WriteFile(filepath.Join(local, "a.txt"), []byte("a"), 0o644)
	placement := filepath.Join(box, "projects", "plain")
	if err := Sync(context.Background(), r, Host{destination: "box"}, local, placement, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(placement, "a.txt")); string(got) != "a" {
		t.Fatalf("copy: %q", got)
	}
	_ = os.WriteFile(filepath.Join(local, "a.txt"), []byte("changed"), 0o644)
	if err := Sync(context.Background(), r, Host{destination: "box"}, local, placement, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(placement, "a.txt")); string(got) != "a" {
		t.Fatal("a present copy was overwritten; the box is truth")
	}
}

// TestQuoteSurvivesASingleQuote: every host line interpolates the placement, so the one
// character that could end the quoting is the one worth a test.
func TestQuoteSurvivesASingleQuote(t *testing.T) {
	requireGit(t) // for a shell to run the line under, nothing more
	r := localRunner{home: t.TempDir()}
	dir := filepath.Join(t.TempDir(), "it's here")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Inspect(context.Background(), r, dir)
	if err != nil || !got.Exists {
		t.Fatalf("Inspect(%q) = %+v, %v", dir, got, err)
	}
}

// localRunner runs a host line in a local shell under a pretend home, the way the ssh shim
// does for the cli tests. The push URL a real host gets (ssh://box/path) is rewritten to the
// path, since there is no ssh here: Sync asks the Runner for the push URL through Runner.URL
// so the rewrite lives in one place.
type localRunner struct{ home string }

func (l localRunner) Run(ctx context.Context, line string, stdin io.Reader) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", line)
	cmd.Env = append(hermeticGitEnv(), "HOME="+l.home, "PATH="+os.Getenv("PATH"))
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code, err = ee.ExitCode(), nil
	}
	return out.String(), errb.String(), code, err
}

func (l localRunner) URL(placement string) string { return placement }

// hermeticGitEnv is what every git in these tests runs with: nobody's global config, a fixed
// identity and no prompt. requireGit puts it on the test process too, so the local git calls
// the implementation makes are hermetic as well and a developer's ~/.gitconfig cannot decide
// whether a test passes.
func hermeticGitEnv() []string {
	return []string{
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=rudy test",
		"GIT_AUTHOR_EMAIL=rudy@test",
		"GIT_COMMITTER_NAME=rudy test",
		"GIT_COMMITTER_EMAIL=rudy@test",
		"GIT_TERMINAL_PROMPT=0",
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	for _, kv := range hermeticGitEnv() {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
}

func requireTar(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not on PATH")
	}
}

// gitRepo is a checkout with one commit on main and a pre-push hook that refuses everything:
// a Mac that signs its commits has one, it is right for GitHub and wrong for the box, and
// the fixture carries it so the --no-verify on the push is a tested property rather than a
// line nobody would miss.
func gitRepo(t *testing.T, file string) string {
	t.Helper()
	requireGit(t)
	dir := t.TempDir()
	gitLine(t, dir, "init", "-q", "-b", "main", ".")
	hook := filepath.Join(dir, ".git", "hooks", "pre-push")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'pre-push: refused' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file+".txt"), []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	gitLine(t, dir, "add", "-A")
	gitLine(t, dir, "commit", "-qm", file)
	return dir
}

// commit adds an empty commit, which moves the head and leaves the tree clean: the diverged
// and behind cases are about heads, and a commit that touched a file would make the box
// dirty and decide a different branch of the table.
func commit(t *testing.T, dir, msg string) {
	t.Helper()
	gitLine(t, dir, "commit", "-q", "--allow-empty", "-m", msg)
}

// gitLine runs one git command in dir and returns its first line of output.
func gitLine(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line
}
