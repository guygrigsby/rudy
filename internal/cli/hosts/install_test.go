package hosts

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallLineCheckoutsTheRevisionAndRunsMake(t *testing.T) {
	line := InstallLine("/home/guy/projects/rudy", "abc1234")
	for _, want := range []string{remotePath, "cd '/home/guy/projects/rudy'", "git fetch", "git checkout -q --detach 'abc1234'", "make install"} {
		if !strings.Contains(line, want) {
			t.Fatalf("install line %q lacks %q", line, want)
		}
	}
}

func TestInstallLineQuotesTheRevision(t *testing.T) {
	rev := "v1;touch${IFS}/tmp/rudy_pwn"
	line := InstallLine("/home/guy/projects/rudy", rev)
	if strings.Contains(line, "--detach "+rev) {
		t.Fatalf("install line interpolates the revision as shell syntax: %q", line)
	}
	if !strings.Contains(line, "--detach "+quote(rev)) {
		t.Fatalf("install line %q does not quote revision %q", line, rev)
	}
}

// TestInstallLineLeavesALeadingTildeForTheBoxsShell: remote.source names a path on the box,
// and the home it is relative to is the box's. Quoting the whole value would send the tilde
// over as a literal directory name; expanding it here would send this machine's home. So the
// tilde goes over bare and the rest is quoted, and the box's shell does the expansion.
func TestInstallLineLeavesALeadingTildeForTheBoxsShell(t *testing.T) {
	line := InstallLine("~/projects/rudy", "abc1234")
	if !strings.Contains(line, "cd ~/'projects/rudy'") {
		t.Fatalf("install line %q does not leave the tilde for the box", line)
	}
	if strings.Contains(line, "cd '~/") {
		t.Fatalf("install line %q quotes the tilde, which makes it a directory named ~", line)
	}
}

func TestInstallRunsOnTheHost(t *testing.T) {
	requireGit(t)
	requireMake(t)
	box := t.TempDir()
	source, first := fakeSource(t, box)
	if err := Install(context.Background(), localRunner{home: box}, source, first, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(source, "installed"))
	if strings.TrimSpace(string(got)) != first {
		t.Fatalf("installed %q, want %q", got, first)
	}
}

// TestInstallResolvesATildeAgainstTheBoxsHome is the line above proved end to end: the same
// install, with remote.source spelled the way the shipped default spells it.
func TestInstallResolvesATildeAgainstTheBoxsHome(t *testing.T) {
	requireGit(t)
	requireMake(t)
	box := t.TempDir()
	source, first := fakeSource(t, box)
	if err := Install(context.Background(), localRunner{home: box}, "~/projects/rudy", first, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(source, "installed"))
	if strings.TrimSpace(string(got)) != first {
		t.Fatalf("installed %q, want %q", got, first)
	}
}

// TestInstallReportsTheBuildThatFailed: a build that broke is the one thing an operator
// cannot see, because it happened on a machine they are not looking at. So the compiler's own
// words come back with the error, and the error says where to go and what to type to see the
// rest of it.
func TestInstallReportsTheBuildThatFailed(t *testing.T) {
	requireGit(t)
	requireMake(t)
	box := t.TempDir()
	source := filepath.Join(box, "projects", "rudy")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	// The shape of a real failure: go writes to stderr and make exits non-zero.
	if err := os.WriteFile(filepath.Join(source, "Makefile"), []byte("install:\n\techo 'undefined: Foo' >&2; exit 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInit(t, source)
	rev := gitLine(t, source, "rev-parse", "HEAD")

	var out bytes.Buffer
	err := Install(context.Background(), localRunner{home: box}, source, rev, &out)
	if err == nil {
		t.Fatal("a build that exited 2 was reported as an install")
	}
	for _, want := range []string{"undefined: Foo", "cd " + source, "make install"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("install error %q lacks %q", err, want)
		}
	}
	// The box's stderr reaches the writer as it arrives, not only through the error: a build
	// an operator is waiting on is the reason this streams at all.
	if !strings.Contains(out.String(), "undefined: Foo") {
		t.Fatalf("the box's output did not reach the writer: %q", out.String())
	}
}

// TestInstallRefusesWithoutASource: remote.source empty is a config nobody finished, and a
// cd with no argument would be a build in the box's home.
func TestInstallRefusesWithoutASource(t *testing.T) {
	err := Install(context.Background(), localRunner{home: t.TempDir()}, "", "abc1234", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "remote.source") {
		t.Fatalf("Install with no source = %v; want a refusal naming remote.source", err)
	}
}

// fakeSource is a box's rudy checkout: a Makefile whose install target records the commit it
// ran at, and two commits, so an install that landed on the first is an install that really
// checked out the revision it was given.
func fakeSource(t *testing.T, box string) (source, first string) {
	t.Helper()
	source = filepath.Join(box, "projects", "rudy")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "Makefile"), []byte("install:\n\tgit rev-parse HEAD > installed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInit(t, source)
	first = gitLine(t, source, "rev-parse", "HEAD")
	commit(t, source, "second")
	return source, first
}

// gitInit makes dir a checkout on main with everything in it committed.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	requireGit(t)
	gitLine(t, dir, "init", "-q", "-b", "main", ".")
	gitLine(t, dir, "add", "-A")
	gitLine(t, dir, "commit", "-qm", "first")
}

func requireMake(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not on PATH")
	}
}
