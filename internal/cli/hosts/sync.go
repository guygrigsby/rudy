package hosts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// Runner runs one shell line on a host and says where a git push to a placement goes. Two
// methods because the push is git's own connection rather than a line this package runs, and
// a test that has no ssh needs both rewritten together.
type Runner interface {
	Run(ctx context.Context, line string, stdin io.Reader) (stdout, stderr string, code int, err error)
	URL(placement string) string
}

// SSHBin is the ssh to run. RUDY_SSH exists for the tests, whose shim runs the remote line
// locally; it is not a config key because nothing but a test wants it.
func SSHBin() string {
	if v := os.Getenv("RUDY_SSH"); v != "" {
		return v
	}
	return "ssh"
}

// SSHRunner runs host lines over ssh, the same transport and the same ssh config the bridge
// is reached through.
func SSHRunner(host Host) Runner { return sshRunner{host: host} }

type sshRunner struct{ host Host }

// Run sends the line as one argument after --, as dialSSH does: ssh joins its command words
// with spaces and hands them to the box's shell, so a line that is already one word is the
// line the shell runs. A non-zero exit is the host's answer, not this call's failure; only
// an ssh that would not start is an error.
func (s sshRunner) Run(ctx context.Context, line string, stdin io.Reader) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, SSHBin(), "--", s.host.String(), line)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		return out.String(), errb.String(), ee.ExitCode(), nil
	case err != nil:
		return out.String(), errb.String(), 0, fmt.Errorf("%s: %w", SSHBin(), err)
	}
	return out.String(), errb.String(), 0, nil
}

// URL is where a git push to placement goes: ssh's own URL for the same destination, so the
// operator's ssh config resolves the host and no remote is added to either checkout.
func (s sshRunner) URL(placement string) string { return "ssh://" + s.host.String() + placement }

// Plan is what Sync decided to do with the tree.
type Plan int

const (
	PlanPush     Plan = iota // git: push the branch by URL, check it out there, copy uncommitted changes on top
	PlanCopy                 // not git: tar the tree over
	PlanOpenAsIs             // the box holds work or a copy already; leave it, open there
	PlanRefuse               // diverged, or the placement is not what the local tree is
)

func (p Plan) String() string {
	switch p {
	case PlanPush:
		return "push"
	case PlanCopy:
		return "copy"
	case PlanOpenAsIs:
		return "open as is"
	default:
		return "refuse"
	}
}

// LocalState is what the client knows about its own tree.
type LocalState struct {
	IsGit            bool
	Root             string // the workspace root: the checkout's toplevel, or the cwd itself
	Head, Branch     string
	Dirty            bool
	RemoteIsAncestor bool // the box head is an ancestor of the local head (box behind or equal)
	LocalIsAncestor  bool // the local head is an ancestor of the box head (box ahead or equal)
}

// State is what one round trip to the host says about the placement.
type State struct {
	Exists, IsGit, Dirty bool
	Head                 string
}

// Decide is the table in the spec. The second value is the reason for a refusal, or the
// notice printed when the box is left as it is.
func Decide(local LocalState, remote State) (Plan, string) {
	if !local.IsGit {
		if remote.Exists {
			return PlanOpenAsIs, "the host already holds a copy; opening it as it is (rudy hosts pull brings it back)"
		}
		return PlanCopy, ""
	}
	if !remote.Exists {
		return PlanPush, ""
	}
	if !remote.IsGit {
		return PlanRefuse, "the placement exists on the host and is not a git checkout; move it aside or pass --cwd"
	}
	switch {
	case remote.Dirty:
		return PlanOpenAsIs, "the host's tree is dirty; opening it as it is, your uncommitted changes stay here"
	case local.RemoteIsAncestor:
		return PlanPush, ""
	case local.LocalIsAncestor:
		return PlanOpenAsIs, "the host is ahead of this checkout; opening it as it is (rudy hosts pull brings the commits back)"
	default:
		return PlanRefuse, "the branch has diverged between here and the host; run rudy hosts pull, rebase, then try again"
	}
}

// inspectLine is the one round trip that reads a placement: whether it is there, whether it
// is a checkout of its own, and if so its head and whether its tree is dirty. A checkout of
// its own means its toplevel is itself: a plain directory inside somebody else's checkout
// answers plain, because pushing to it would land in that other repository and the tar on
// top would land in the wrong directory of it. Both paths come back physical, so the
// comparison is pwd -P against a toplevel git already resolved.
func inspectLine(placement string) string {
	return fmt.Sprintf(`if [ ! -e %[1]s ]; then echo absent; exit 0; fi
echo exists
top=$(git -C %[1]s rev-parse --show-toplevel 2>/dev/null)
here=$(cd %[1]s 2>/dev/null && pwd -P)
if [ -n "$top" ] && [ "$top" = "$here" ]; then
  echo git
  git -C %[1]s rev-parse HEAD 2>/dev/null || echo none
  if [ -n "$(git -C %[1]s status --porcelain)" ]; then echo dirty; else echo clean; fi
else
  echo plain
fi`, quote(placement))
}

// Inspect reads the placement over one round trip.
func Inspect(ctx context.Context, r Runner, placement string) (State, error) {
	out, err := run(ctx, r, "reading "+placement, inspectLine(placement), nil)
	if err != nil {
		return State{}, err
	}
	f := strings.Fields(out)
	var s State
	if len(f) == 0 {
		return s, fmt.Errorf("reading %s: the host answered nothing", placement)
	}
	if f[0] != "exists" {
		return s, nil
	}
	s.Exists = true
	if len(f) < 2 || f[1] != "git" {
		return s, nil
	}
	s.IsGit = true
	if len(f) > 2 && f[2] != "none" {
		s.Head = f[2]
	}
	s.Dirty = len(f) > 3 && f[3] == "dirty"
	return s, nil
}

// Push makes the checkout at the placement if it is not there, pushes the branch into it by
// URL and checks it out, then copies the uncommitted changes on top: modified and untracked
// files as one tar, deletions as one rm. --no-verify for sand's reason: a Mac that signs has
// a pre-push hook that refuses unsigned commits, which is right for GitHub and wrong for a
// box that is this machine's own working copy.
func Push(ctx context.Context, r Runner, host Host, localRoot, branch, placement string, out io.Writer) error {
	p := quote(placement)
	init := ""
	if branch != "" {
		init = " -b " + quote(branch)
	}
	// The checkout is made once and configured every time: updateInstead is what lets a push
	// move the box's working tree instead of being refused for touching the current branch,
	// and a placement an operator made by hand needs it as much as one rudy made.
	line := fmt.Sprintf(`mkdir -p %[1]s || exit 1
top=$(git -C %[1]s rev-parse --show-toplevel 2>/dev/null)
here=$(cd %[1]s && pwd -P) || exit 1
if [ "$top" != "$here" ]; then git -C %[1]s init -q%[2]s || exit 1; fi
git -C %[1]s config receive.denyCurrentBranch updateInstead`, p, init)
	if _, err := run(ctx, r, "preparing "+placement, line, nil); err != nil {
		return err
	}
	// A branch with no commit on it yet has nothing to push, which is not a failure: the
	// files below are the whole of what this tree has and they still belong on the box.
	if branch != "" && gitOK(ctx, localRoot, "rev-parse", "--verify", "--quiet", "HEAD") {
		if _, err := gitOut(ctx, localRoot, "push", "--no-verify", "--quiet", r.URL(placement), branch+":refs/heads/"+branch); err != nil {
			return err
		}
		// A push into the branch the box already has checked out moved its tree through
		// updateInstead and this is a no-op; a push into any other branch is why it is here.
		checkout := fmt.Sprintf("git -C %s checkout -q %s", p, quote(branch))
		if _, err := run(ctx, r, "checking out "+branch, checkout, nil); err != nil {
			return err
		}
	}
	files, deleted, err := changed(ctx, localRoot)
	if err != nil {
		return err
	}
	if len(files) > 0 {
		unpack := fmt.Sprintf("tar -x -C %s -f -", p)
		if err := sendTar(ctx, r, "copying uncommitted changes", unpack, tarOf(ctx, localRoot, files)); err != nil {
			return err
		}
	}
	if len(deleted) > 0 {
		names := make([]string, len(deleted))
		for i, name := range deleted {
			names[i] = quote(name)
		}
		rm := fmt.Sprintf("cd %s && rm -f -- %s", p, strings.Join(names, " "))
		if _, err := run(ctx, r, "removing deleted files", rm, nil); err != nil {
			return err
		}
	}
	slog.Info("sync: push", "host", host.String(), "placement", placement, "files", len(files)+len(deleted))
	if n := len(files) + len(deleted); n > 0 {
		notice(out, fmt.Sprintf("pushed %s to %s:%s with %d uncommitted files", branch, host, placement, n))
	} else {
		notice(out, fmt.Sprintf("pushed %s to %s:%s", branch, host, placement))
	}
	return nil
}

// Copy tars a tree no git tracks to the placement. Once: a placement that exists is the box's
// own, and Decide never asks for a second copy over it.
func Copy(ctx context.Context, r Runner, host Host, localRoot, placement string, out io.Writer) error {
	names, err := treeFiles(localRoot)
	if err != nil {
		return err
	}
	p := quote(placement)
	if len(names) == 0 {
		// Nothing to archive, and tar told to archive nothing is an error. The directory is
		// still made, because the session opens there.
		if _, err := run(ctx, r, "making "+placement, "mkdir -p "+p, nil); err != nil {
			return err
		}
	} else {
		unpack := fmt.Sprintf("mkdir -p %[1]s && tar -x -C %[1]s -f -", p)
		if err := sendTar(ctx, r, "copying "+localRoot, unpack, tarOf(ctx, localRoot, names)); err != nil {
			return err
		}
	}
	slog.Info("sync: copy", "host", host.String(), "placement", placement, "files", len(names))
	notice(out, fmt.Sprintf("copied %d files to %s:%s", len(names), host, placement))
	return nil
}

// Sync moves the working tree to the placement before a session opens on it. The box is
// truth whenever it holds work: a dirty or ahead placement is opened as it is, and only a
// divergence, or a placement that is not the kind of thing the local tree is, refuses.
func Sync(ctx context.Context, r Runner, host Host, localCwd, placement string, out io.Writer) error {
	local, err := Local(ctx, localCwd)
	if err != nil {
		return err
	}
	if local.IsGit && local.Branch == "" {
		return fmt.Errorf("HEAD is detached in %s, so there is no branch to push to %s:%s; check out a branch, or pass --no-sync", local.Root, host, placement)
	}
	// The tree that moves is the root, so the placement that receives it is the root's, not
	// the cwd's: from inside a checkout the repository goes to the repository's own place
	// rather than into a directory named after where the operator was standing. The session
	// still opens at the cwd's placement, which the tree brings with it.
	root, err := rootPlacement(local.Root, localCwd, placement)
	if err != nil {
		return err
	}
	remote, err := Inspect(ctx, r, root)
	if err != nil {
		return err
	}
	// The ancestry is the extra round trip the git case pays, and only where it decides
	// something: a dirty box is opened as it is whatever the heads say.
	if local.IsGit && remote.Exists && remote.IsGit && !remote.Dirty {
		ancestry(ctx, &local, r.URL(root), local.Branch)
	}
	plan, note := Decide(local, remote)
	switch plan {
	case PlanPush:
		return Push(ctx, r, host, local.Root, local.Branch, root, out)
	case PlanCopy:
		return Copy(ctx, r, host, local.Root, root, out)
	case PlanOpenAsIs:
		notice(out, note)
		return nil
	default:
		return fmt.Errorf("%s:%s: %s", host, root, note)
	}
}

// rootPlacement is where the tree's root goes on the host. The placement maps the cwd, and a
// cwd inside a checkout is a subdirectory of what moves, so the same number of segments come
// off. Symlinks are resolved on the way in because a toplevel git reports is physical and a
// cwd need not be, and on a Mac /var and /private/var are the same directory twice.
func rootPlacement(localRoot, localCwd, placement string) (string, error) {
	cwd, err := filepath.EvalSymlinks(localCwd)
	if err != nil {
		cwd = localCwd
	}
	root, err := filepath.EvalSymlinks(localRoot)
	if err != nil {
		root = localRoot
	}
	rel, err := filepath.Rel(root, cwd)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// Nothing sane to compute: the cwd is not under its own workspace root. Sending the
		// tree to a placement that has no relation to it would be worse than saying so.
		return "", fmt.Errorf("%s is not inside %s, so there is no placement for the tree; pass --cwd or --no-sync", localCwd, localRoot)
	}
	if rel == "." {
		return placement, nil
	}
	for range strings.Count(filepath.ToSlash(rel), "/") + 1 {
		placement = path.Dir(placement)
	}
	return placement, nil
}

// run is one host line, with the host's exit status and stderr folded into an error that
// names what was being done.
func run(ctx context.Context, r Runner, what, line string, stdin io.Reader) (string, error) {
	out, errText, code, err := r.Run(ctx, line, stdin)
	if err != nil {
		return out, fmt.Errorf("%s: %w", what, err)
	}
	if code != 0 {
		if text := strings.TrimSpace(errText); text != "" {
			return out, fmt.Errorf("%s: exit %d: %s", what, code, text)
		}
		return out, fmt.Errorf("%s: exit %d", what, code)
	}
	return out, nil
}

// notice is how the sync talks: one line on the writer the caller gave it, which is the
// client's stderr when a session is opening and its stdout under rudy hosts push.
func notice(out io.Writer, text string) {
	if text == "" || out == nil {
		return
	}
	_, _ = fmt.Fprintln(out, "rudy:", text)
}
