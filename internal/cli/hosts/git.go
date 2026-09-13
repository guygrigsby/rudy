package hosts

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// quote is one value inside a host line's single quotes. Every line this package builds is
// text a shell on the box will read, and the placement in it is the operator's own path, so
// there is one helper and one place to be right about a quote in a directory name.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// gitEnv is what the local git runs with. RUDY_SSH names the ssh this client reaches the box
// with, and a push by URL is that same reach, so git is told to use it too: otherwise the
// transport the operator chose (the test shim, a wrapper with options) would be honoured for
// the bridge and ignored for the tree.
func gitEnv() []string {
	env := os.Environ()
	if v := SSHBin(); v != "ssh" {
		env = append(env, "GIT_SSH_COMMAND="+v)
	}
	return env
}

// gitRaw runs git in dir and returns its stdout untouched. An error names the arguments and
// carries git's own stderr, which is the only thing that says why.
func gitRaw(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnv()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if text := strings.TrimSpace(errb.String()); text != "" {
			return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, text)
		}
		return out.String(), fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out.String(), nil
}

// gitOut is gitRaw trimmed, for the commands whose answer is one line.
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitRaw(ctx, dir, args...)
	return strings.TrimSpace(out), err
}

// gitOK reports whether git exited 0, for the questions whose whole answer is the exit
// status: merge-base --is-ancestor, and whether a fetch found the branch at all.
func gitOK(ctx context.Context, dir string, args ...string) bool {
	_, err := gitRaw(ctx, dir, args...)
	return err == nil
}

// gitNames runs a git command that writes NUL separated paths and returns them. -z is what
// makes a path with a newline or a space in it one name rather than two.
func gitNames(ctx context.Context, dir string, args ...string) ([]string, error) {
	out, err := gitRaw(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, name := range strings.Split(out, "\x00") {
		if name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// Local is what the client knows about its own tree: where its root is, whether it is a
// checkout, and what branch and head it is on. A cwd that is in no checkout answers IsGit
// false with itself as the root, which is the tree Copy sends.
func Local(ctx context.Context, localCwd string) (LocalState, error) {
	s := LocalState{Root: localCwd}
	root, err := gitOut(ctx, localCwd, "rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return s, nil
	}
	s.IsGit, s.Root = true, root
	// branch --show-current rather than rev-parse --abbrev-ref HEAD: a branch with no commit
	// yet has no HEAD to abbreviate and the other spelling is a fatal, and a detached head
	// answers empty here rather than the word HEAD, which Sync has to refuse rather than
	// push to a branch of that name.
	if s.Branch, err = gitOut(ctx, root, "branch", "--show-current"); err != nil {
		return s, err
	}
	// An unborn branch has no head, which is not an error: there is nothing to push and the
	// files in the tree are still worth carrying.
	s.Head, _ = gitOut(ctx, root, "rev-parse", "HEAD")
	status, err := gitRaw(ctx, root, "status", "--porcelain")
	if err != nil {
		return s, err
	}
	s.Dirty = strings.TrimSpace(status) != ""
	return s, nil
}

// ancestry fills in the two bits Decide reads, on the Mac, because it is the only machine
// holding both commits: the box does not have the local head yet. One fetch by URL into
// FETCH_HEAD and two merge-base questions, the extra round trip the git case pays. No remote
// is added to either checkout, so a host renamed in ssh config leaves nothing behind.
func ancestry(ctx context.Context, local *LocalState, url, branch string) {
	if local.Head == "" || branch == "" {
		// Nothing here for the box to be behind, and nothing to fetch against.
		return
	}
	if !gitOK(ctx, local.Root, "fetch", "--no-tags", "--quiet", url, branch) {
		// The branch is not on the box: a checkout rudy just made, or one holding something
		// else. There is nothing there to lose, so the push is the answer.
		local.RemoteIsAncestor = true
		return
	}
	local.RemoteIsAncestor = gitOK(ctx, local.Root, "merge-base", "--is-ancestor", "FETCH_HEAD", local.Head)
	local.LocalIsAncestor = gitOK(ctx, local.Root, "merge-base", "--is-ancestor", local.Head, "FETCH_HEAD")
}

// changed is what the Mac has that no commit carries: files modified or untracked, and
// separately the ones deleted here. ls-files --modified reports a deleted file too, so the
// deletions come out of the first list; a tar told to archive a file that is not there fails
// and takes the whole sync with it.
func changed(ctx context.Context, root string) (files, deleted []string, err error) {
	files, err = gitNames(ctx, root, "ls-files", "-z", "--modified", "--others", "--exclude-standard")
	if err != nil {
		return nil, nil, err
	}
	deleted, err = gitNames(ctx, root, "ls-files", "-z", "--deleted")
	if err != nil {
		return nil, nil, err
	}
	gone := make(map[string]bool, len(deleted))
	for _, name := range deleted {
		gone[name] = true
	}
	kept := files[:0]
	for _, name := range files {
		if !gone[name] {
			kept = append(kept, name)
		}
	}
	return kept, deleted, nil
}
