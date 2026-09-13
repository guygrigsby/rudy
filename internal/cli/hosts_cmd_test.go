package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHostsPushNeedsAHost: the verb acts on a machine, and with none named and no
// remote.host there is nothing to act on. A usage error rather than a dial, so it never
// reaches the probe that would otherwise start a kernel in this process.
func TestHostsPushNeedsAHost(t *testing.T) {
	o := BuildOptions{Stderr: io.Discard, Home: t.TempDir(), Env: func(string) string { return "" }}
	code, err := runHostsPush(context.Background(), refuseToBuild(t, "hosts push with no host wired a server"), o, dialOptions{}, io.Discard)
	if code != 2 || err == nil || !strings.Contains(err.Error(), "remote.host") {
		t.Fatalf("hosts push with no host = %d, %v; want 2 naming remote.host", code, err)
	}
}

// TestHostsPullNeedsAHost: the same as push, from the other direction. A usage error before
// any dial, so the verb never starts a kernel in this process.
func TestHostsPullNeedsAHost(t *testing.T) {
	o := BuildOptions{Stderr: io.Discard, Home: t.TempDir(), Env: func(string) string { return "" }}
	code, err := runHostsPull(context.Background(), refuseToBuild(t, "hosts pull with no host wired a server"), o, dialOptions{}, io.Discard)
	if code != 2 || err == nil || !strings.Contains(err.Error(), "remote.host") {
		t.Fatalf("hosts pull with no host = %d, %v; want 2 naming remote.host", code, err)
	}
}

// TestHostsPullBringsTheBoxsCommitBack is the return path end to end: the tree goes over, the
// box commits on it, and the verb fast-forwards this checkout onto the box's head. Both legs
// go through the ssh shim, git's own connection included.
func TestHostsPullBringsTheBoxsCommitBack(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and starts a daemon")
	}
	requireBoxGit(t)
	bin := builtRudy(t)
	upstream := fakeOpenAI(t, "unused")
	home, env := boxWithProvider(t, bin, upstream.URL)
	t.Cleanup(func() { stopBoxDaemon(t, env) })
	t.Setenv("RUDY_SSH", sshShim(t, env))

	localHome := t.TempDir()
	t.Setenv("HOME", localHome)
	local := filepath.Join(localHome, "projects", "demo")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRepoAt(t, local)
	t.Chdir(local)

	o := BuildOptions{Stderr: io.Discard}
	build := testBuilder(t, &fakeProvider{})
	if code, err := runHostsPush(context.Background(), build, o, dialOptions{Host: "box"}, io.Discard); err != nil || code != 0 {
		t.Fatalf("hosts push = %d, %v", code, err)
	}
	placement := filepath.Join(home, "projects", "demo")
	if err := os.WriteFile(filepath.Join(placement, "box.txt"), []byte("box work"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitAt(t, placement, "add", "-A")
	gitAt(t, placement, "commit", "-qm", "box work")

	var out bytes.Buffer
	code, err := runHostsPull(context.Background(), build, o, dialOptions{Host: "box"}, &out)
	if err != nil || code != 0 {
		t.Fatalf("hosts pull = %d, %v\n%s", code, err, out.String())
	}
	if got, err := os.ReadFile(filepath.Join(local, "box.txt")); err != nil || string(got) != "box work" {
		t.Fatalf("the box's commit did not come back: %q %v\n%s", got, err, out.String())
	}
	if got, want := gitAt(t, local, "rev-parse", "HEAD"), gitAt(t, placement, "rev-parse", "HEAD"); got != want {
		t.Fatalf("local head = %s, want the box's %s\n%s", got, want, out.String())
	}
}

// TestHostsPushMovesTheTree is the command's real path: the tree here, the placement there,
// and the same Sync a new session runs. It greets the box to learn its home, which is what
// makes the directory it pushes to the directory a session would open on.
func TestHostsPushMovesTheTree(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and starts a daemon")
	}
	requireBoxGit(t)
	bin := builtRudy(t)
	upstream := fakeOpenAI(t, "unused")
	home, env := boxWithProvider(t, bin, upstream.URL)
	t.Cleanup(func() { stopBoxDaemon(t, env) })
	t.Setenv("RUDY_SSH", sshShim(t, env))

	localHome := t.TempDir()
	t.Setenv("HOME", localHome)
	local := filepath.Join(localHome, "projects", "demo")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRepoAt(t, local)
	t.Chdir(local)

	var out bytes.Buffer
	o := BuildOptions{Stderr: io.Discard}
	code, err := runHostsPush(context.Background(), testBuilder(t, &fakeProvider{}), o, dialOptions{Host: "box"}, &out)
	if err != nil || code != 0 {
		t.Fatalf("hosts push = %d, %v", code, err)
	}
	placement := filepath.Join(home, "projects", "demo")
	if got := gitAt(t, placement, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Fatalf("the placement is not a checkout on main: %q\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), placement) {
		t.Fatalf("hosts push said nothing about where the tree went: %q", out.String())
	}
}
