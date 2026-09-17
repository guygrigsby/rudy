// SPDX-License-Identifier: AGPL-3.0-or-later

package pluginstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// goDownload is the subset of what `go mod download -json` reports that stageGo needs. Error
// is where the go tool puts the cause of a failed download: a typo, a tag that does not exist,
// a private module with no credentials and a proxy that is down all report there, on stdout,
// with stderr left empty, so a caller that only reads stderr shows the operator nothing at all.
type goDownload struct {
	Dir     string
	Version string
	Sum     string
	Error   string
}

// stageGo fills dir with a go: source's module content the way `go install` resolves one: the
// module proxy answers module@ref, not a git host (ADR 0025 decision 1; no `git` invocation
// appears anywhere in this file). An empty src.Ref means "latest", per the contracts row for
// plugins.lock.toml, resolved here rather than by ParseSource.
//
// go mod download reports Dir inside GOMODCACHE, which the go tool makes read-only by design;
// os.CopyFS (already how stagePath copies a filesystem source) is what leaves the staged copy
// writable, since it creates files at 0o666 and directories at 0o777 before umask regardless of
// the source's own mode, which the manifest's build step then needs to write its binary into.
func (s *Store) stageGo(ctx context.Context, src Source, dir string) (string, error) {
	ref := src.Ref
	if ref == "" {
		ref = "latest"
	}
	dl, err := s.goModDownload(ctx, src.Location, ref)
	if err != nil {
		return "", err
	}
	if err := os.CopyFS(dir, os.DirFS(dl.Dir)); err != nil {
		return "", fmt.Errorf("pluginstore: copy %s: %w", dl.Dir, err)
	}
	return dl.Version + " " + dl.Sum, nil
}

// stageGoUpdate is stageGo for rudy plugins update. An empty src.Ref or the literal "latest"
// (an operator can type either; ParseSource leaves "latest" as typed rather than collapsing it
// with empty, since the lock records what was asked for) both re-resolve on every update and
// accept whatever that is now, same as a fresh install: "latest" naming something new is the
// entire point of asking for it, not a tamper signal. A concrete pinned version carries a
// stronger expectation: the module proxy and the checksum database both exist to guarantee a
// given module@version never changes content once published, so a pinned version that now
// resolves to a different digest than the lock already recorded means that guarantee broke
// somewhere between the operator's environment and the module they asked for. That is refused
// rather than silently swapped into the checkout: the live checkout and the lock are left
// exactly as they were, the same "advance both or advance neither" rule Update already keeps
// for a build failure.
func (s *Store) stageGoUpdate(ctx context.Context, src Source, dir, priorDigest string) (string, error) {
	resolved, err := s.stageGo(ctx, src, dir)
	if err != nil {
		return "", err
	}
	if pinned(src.Ref) && priorDigest != "" && resolved != priorDigest {
		return "", fmt.Errorf("pluginstore: %s@%s: proxy now reports %s, previously %s: refusing to replace a pinned module whose content changed", src.Location, src.Ref, resolved, priorDigest)
	}
	return resolved, nil
}

// pinned reports whether ref names one concrete version the tamper check in stageGoUpdate
// should hold immutable, as opposed to a moving target that is expected to resolve to
// something new over time: an empty ref and the literal "latest" are both the latter.
func pinned(ref string) bool { return ref != "" && ref != "latest" }

// goModDownload runs `go mod download -json module@ref` hermetically: GOFLAGS pinned to
// -mod=mod (there is no consuming go.mod here for the download to treat as read-only) and
// GOMODCACHE pointed at a cache this store owns, so a plugin's dependencies never land in the
// operator's real module cache. Everything else, GOPROXY, GOPRIVATE, GONOSUMDB, GOSUMDB, any
// credential helper a private module needs, comes from the operator's own environment
// unchanged; Store.GoEnv is the one override, and it exists for tests to point GOPROXY at a
// local fixture instead of t.Setenv, which would mutate every other test in this package that
// shells out to go while this one runs.
func (s *Store) goModDownload(ctx context.Context, module, ref string) (goDownload, error) {
	cmd := exec.CommandContext(ctx, "go", "mod", "download", "-json", module+"@"+ref)
	cmd.Env = s.goEnv()
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	// stdout is parsed whether or not the command succeeded: a failed download still prints its
	// JSON object, and that object's Error field is the only place the reason appears.
	var dl goDownload
	jsonErr := json.Unmarshal([]byte(stdout.String()), &dl)
	if runErr != nil {
		return goDownload{}, fmt.Errorf("pluginstore: go mod download %s@%s: %w: %s", module, ref, runErr, downloadReason(dl, stderr.String()))
	}
	if dl.Error != "" {
		// The go tool also reports some failures through Error alone, exiting zero, so an
		// exit status is not on its own evidence that a module was downloaded.
		return goDownload{}, fmt.Errorf("pluginstore: go mod download %s@%s: %s", module, ref, tail(dl.Error))
	}
	if jsonErr != nil {
		return goDownload{}, fmt.Errorf("pluginstore: go mod download %s@%s: %w", module, ref, jsonErr)
	}
	if dl.Dir == "" || dl.Version == "" || dl.Sum == "" {
		return goDownload{}, fmt.Errorf("pluginstore: go mod download %s@%s: incomplete result %+v", module, ref, dl)
	}
	return dl, nil
}

// downloadReason is what a failed `go mod download` gets to say for itself: the JSON object's
// own Error field first, since that is where the go tool writes the cause, falling back to
// stderr for a failure that produced no JSON at all (an unparseable module path, a missing go
// binary's own complaint) and to a plain statement when the tool reported nothing anywhere,
// rather than the empty string every go: failure used to end with.
func downloadReason(dl goDownload, stderr string) string {
	if reason := tail(dl.Error); reason != "" {
		return reason
	}
	if reason := tail(stderr); reason != "" {
		return reason
	}
	return "no reason reported"
}

// goEnv is the environment a go: source's subprocess runs with: the operator's own, with
// Store.GoEnv's overrides (a test's fixture GOPROXY and GONOSUMDB) applied on top, and GOFLAGS
// plus GOMODCACHE pinned last so neither the operator's environment nor a test can loosen the
// one guarantee that actually matters: nothing here ever writes into the operator's real
// module cache.
func (s *Store) goEnv() []string {
	env := os.Environ()
	for _, kv := range s.GoEnv {
		if key, val, ok := strings.Cut(kv, "="); ok {
			env = setEnvVar(env, key, val)
		}
	}
	env = setEnvVar(env, "GOFLAGS", "-mod=mod")
	env = setEnvVar(env, "GOMODCACHE", s.goModCacheDir())
	return env
}

// goModCacheDir is the module cache this store owns: a sibling of the per-plugin checkouts
// under Root/plugins, so sweepStaleStages' .install-*/.update-*/*.old glob leaves it alone and
// plugin.Discover skips it silently (it holds no plugin.toml). It persists across installs and
// updates rather than living inside one stage, since GOMODCACHE is meant to be reused; a fresh
// one per install would mean redownloading every dependency every time.
func (s *Store) goModCacheDir() string { return filepath.Join(s.pluginsDir(), ".gomodcache") }

// setEnvVar replaces key's entry in env (an os.Environ()-shaped slice) or appends one, so the
// result never carries two entries for the same variable: exec.Cmd passes its Env slice
// straight to the OS, and which of two duplicate entries a child process's own environment
// lookup would honor is not something to rely on.
func setEnvVar(env []string, key, val string) []string {
	prefix := key + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			env[i] = prefix + val
			return env
		}
	}
	return append(env, prefix+val)
}
