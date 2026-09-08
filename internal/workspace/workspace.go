// Package workspace identifies the directory a session acts on.
package workspace

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/guygrigsby/rudy/internal/session"
)

// ErrRefused marks an origin or id the memory project id rules reject.
var ErrRefused = errors.New("workspace: refused")

var (
	schemeRe = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://`)
	driveRe  = regexp.MustCompile(`(?i)^[a-z]:`)
	userRe   = regexp.MustCompile(`^[^@/]+@`)
	scpRe    = regexp.MustCompile(`^([^@\s]+@)?([^:/\s]+):(.+)$`)
	portRe   = regexp.MustCompile(`:\d+$`)
	spaceRe  = regexp.MustCompile(`\s`)
)

// Detect resolves cwd to a Workspace. Root is the absolute cwd, GitRoot the repository
// top level or empty, ProjectID the id the memory CLI derives for the same directory.
func Detect(cwd string) (session.Workspace, error) {
	root, err := filepath.Abs(cwd)
	if err != nil {
		return session.Workspace{}, fmt.Errorf("workspace: %w", err)
	}
	ws := session.Workspace{Root: filepath.Clean(root)}
	ws.GitRoot = git(ws.Root, "rev-parse", "--show-toplevel")
	if ws.GitRoot == "" {
		ws.ProjectID = LocalProjectID(ws.Root)
		return ws, nil
	}
	origin := git(ws.GitRoot, "remote", "get-url", "origin")
	if origin == "" {
		ws.ProjectID = LocalProjectID(ws.GitRoot)
		return ws, nil
	}
	id, err := ProjectIDFromOrigin(origin)
	if errors.Is(err, ErrRefused) {
		ws.ProjectID = LocalProjectID(ws.GitRoot)
		return ws, nil
	}
	if err != nil {
		return session.Workspace{}, err
	}
	ws.ProjectID = id
	return ws, nil
}

// git runs one git command in dir and returns its trimmed stdout, or "" on any failure.
func git(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// LocalProjectID is the id for a directory with no usable origin.
func LocalProjectID(dir string) string {
	return "local/" + filepath.Base(filepath.Clean(dir))
}

// ProjectIDFromOrigin turns a git remote URL into host/path. Filesystem paths and
// scheme-only URLs are refused with ErrRefused.
func ProjectIDFromOrigin(url string) (string, error) {
	u := strings.TrimSpace(url)
	if strings.HasPrefix(u, "/") || strings.HasPrefix(u, ".") || strings.HasPrefix(u, "~") || driveRe.MatchString(u) {
		return "", fmt.Errorf("%w: malformed project id origin %q", ErrRefused, u)
	}
	var host, p string
	if schemeRe.MatchString(u) {
		rest := schemeRe.ReplaceAllString(u, "")
		rest = userRe.ReplaceAllString(rest, "")
		i := strings.Index(rest, "/")
		if i < 0 {
			return "", fmt.Errorf("%w: malformed project id origin %q", ErrRefused, u)
		}
		host, p = rest[:i], rest[i+1:]
	} else {
		m := scpRe.FindStringSubmatch(u)
		if m == nil {
			return "", fmt.Errorf("%w: malformed project id origin %q", ErrRefused, u)
		}
		host, p = m[2], m[3]
	}
	host = portRe.ReplaceAllString(strings.ToLower(host), "")
	p = strings.TrimRight(p, "/")
	p = strings.TrimSuffix(p, ".git")
	return AssertProjectID(host + "/" + p)
}

// AssertProjectID refuses ids that could escape a bundle directory.
func AssertProjectID(id string) (string, error) {
	if id == "" || spaceRe.MatchString(id) || strings.HasPrefix(id, "/") {
		return "", fmt.Errorf("%w: malformed project id %q", ErrRefused, id)
	}
	for _, part := range strings.Split(id, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("%w: malformed project id %q", ErrRefused, id)
		}
	}
	return id, nil
}
