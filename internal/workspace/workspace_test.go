package workspace_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/guygrigsby/rudy/internal/workspace"
)

func TestProjectIDFromOriginNormalizes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"git@github.com:guygrigsby/x.git", "github.com/guygrigsby/x"},
		{"https://github.com/guygrigsby/x", "github.com/guygrigsby/x"},
		{"ssh://git@GitHub.com/guygrigsby/x.git", "github.com/guygrigsby/x"},
		{"https://user:tok@github.com/guygrigsby/x.git/", "github.com/guygrigsby/x"},
		{"ssh://git@github.com:2222/guygrigsby/x.git", "github.com/guygrigsby/x"},
		{"https://gitlab.example.com:8443/team/repo.git", "gitlab.example.com/team/repo"},
	}
	for _, c := range cases {
		got, err := workspace.ProjectIDFromOrigin(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.in, got, c.want)
		}
	}
}

func TestProjectIDFromOriginRefuses(t *testing.T) {
	bad := []string{
		"/Users/guy/repos/upstream.git",
		"./relative/path",
		"~/home/path",
		"C:/repos/x",
		"https://github.com",
		"https://github.com/",
		"not a url",
	}
	for _, in := range bad {
		if _, err := workspace.ProjectIDFromOrigin(in); err == nil {
			t.Errorf("%q: want refusal", in)
		}
	}
}

func TestAssertProjectIDRefusesTraversal(t *testing.T) {
	for _, bad := range []string{"", "../x", "/abs", "a b/c", "github.com/../x"} {
		if _, err := workspace.AssertProjectID(bad); err == nil {
			t.Errorf("%q: want refusal", bad)
		}
	}
	if got, err := workspace.AssertProjectID("local/x"); err != nil || got != "local/x" {
		t.Fatalf("local/x: got %q, %v", got, err)
	}
}

func TestLocalProjectID(t *testing.T) {
	if got := workspace.LocalProjectID("/tmp/some/repo/"); got != "local/repo" {
		t.Fatalf("got %q", got)
	}
}

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func gitRepo(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if origin != "" {
		run("remote", "add", "origin", origin)
	}
	return dir
}

func TestDetectRepoWithOrigin(t *testing.T) {
	needGit(t)
	dir := gitRepo(t, "git@github.com:aeryx-ai/memory.git")
	ws, err := workspace.Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ws.ProjectID != "github.com/aeryx-ai/memory" {
		t.Errorf("project id %q", ws.ProjectID)
	}
	wantRoot, _ := filepath.EvalSymlinks(dir)
	gotRoot, _ := filepath.EvalSymlinks(ws.Root)
	gotGit, _ := filepath.EvalSymlinks(ws.GitRoot)
	if gotRoot != wantRoot || gotGit != wantRoot {
		t.Errorf("root %q git root %q want %q", ws.Root, ws.GitRoot, wantRoot)
	}
}

func TestDetectRepoWithoutOriginAndPlainDir(t *testing.T) {
	needGit(t)
	bare := gitRepo(t, "")
	ws, err := workspace.Detect(bare)
	if err != nil {
		t.Fatal(err)
	}
	if ws.ProjectID != "local/"+filepath.Base(bare) {
		t.Errorf("bare repo: %q", ws.ProjectID)
	}
	plain := t.TempDir()
	ws, err = workspace.Detect(plain)
	if err != nil {
		t.Fatal(err)
	}
	if ws.GitRoot != "" {
		t.Errorf("plain dir has git root %q", ws.GitRoot)
	}
	if ws.ProjectID != "local/"+filepath.Base(plain) {
		t.Errorf("plain dir: %q", ws.ProjectID)
	}
}

func TestDetectSubdirectoryResolvesToRepo(t *testing.T) {
	needGit(t)
	repo := gitRepo(t, "https://github.com/a/b")
	sub := filepath.Join(repo, "deep", "er")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Detect(sub)
	if err != nil {
		t.Fatal(err)
	}
	if ws.ProjectID != "github.com/a/b" {
		t.Errorf("project id %q", ws.ProjectID)
	}
	if filepath.Base(ws.Root) != "er" {
		t.Errorf("root should stay the cwd, got %q", ws.Root)
	}
}

func TestDetectFilesystemOriginFallsBackToLocal(t *testing.T) {
	needGit(t)
	bareOrigin := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "--bare")
	cmd.Dir = bareOrigin
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	repo := gitRepo(t, bareOrigin)
	ws, err := workspace.Detect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if ws.ProjectID != "local/"+filepath.Base(repo) {
		t.Errorf("got %q", ws.ProjectID)
	}
}

func TestDetectRefusesMalformedLocalID(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) (dir, want string)
	}{
		{
			name: "filesystem root",
			setup: func(t *testing.T) (string, string) {
				return "/", ""
			},
		},
		{
			name: "basename with a space",
			setup: func(t *testing.T) (string, string) {
				dir := filepath.Join(t.TempDir(), "my project")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				return dir, ""
			},
		},
		{
			name: "normal temp dir",
			setup: func(t *testing.T) (string, string) {
				dir := t.TempDir()
				return dir, "local/" + filepath.Base(dir)
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, want := c.setup(t)
			ws, err := workspace.Detect(dir)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if ws.ProjectID != want {
				t.Errorf("%s: got %q want %q", c.name, ws.ProjectID, want)
			}
		})
	}
}
