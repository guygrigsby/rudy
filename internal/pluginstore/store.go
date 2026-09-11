// Package pluginstore is the record of what is installed: the plugin checkouts under the XDG
// data root and the lock file beside them. rudy plugin install and its siblings own the
// whole file; the server's own boot path only ever reads the enabled flag through
// DisabledFromLock.
package pluginstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/guygrigsby/rudy/internal/plugin"
)

// LockFile is the lock's name, next to the plugins directory under the data root.
const LockFile = "plugins.lock.toml"

// Installed is one [plugins.<name>] table.
type Installed struct {
	Name   string `toml:"-"`
	Source string `toml:"source"`
	// Kind is what Source resolved as: git, path, go or https. A lock written before this
	// field existed has none on disk; Read derives it (see the loop below) rather than
	// leaving it the zero value, so an old lock keeps meaning instead of failing dispatch.
	Kind Kind `toml:"kind"`
	// Ref is what the operator typed after "@", exactly as typed. rudy plugins update
	// re-resolves it every time; it is never rewritten to the tip of whatever the checkout
	// happens to track.
	Ref         string    `toml:"ref"`
	Commit      string    `toml:"commit"`
	Digest      string    `toml:"digest"`
	InstalledAt time.Time `toml:"installed_at"`
	Enabled     bool      `toml:"enabled"`
}

// lockFile is plugins.lock.toml as a whole.
type lockFile struct {
	Plugins map[string]Installed `toml:"plugins"`
}

// Store is the plugin checkouts and their lock file under one XDG data root: checkouts live
// at Root/plugins/<name>, the lock at Root/plugins.lock.toml.
type Store struct {
	Root string
	// Out receives a manifest's build command output as Install and Update run it. Left nil
	// everywhere but the CLI, which wires in its own stdout so the operator watching the
	// command sees what the build does.
	Out io.Writer
}

// buildOut is where a manifest's build command output goes: Out when the caller set one,
// discarded otherwise, so Install and Update never need to nil-check it themselves.
func (s *Store) buildOut() io.Writer {
	if s.Out != nil {
		return s.Out
	}
	return io.Discard
}

// New is a Store over root, normally $XDG_DATA_HOME/rudy. New does no filesystem clean-up
// itself: it runs on every session boot, through DisabledFromLock, and a session starting
// while another terminal's rudy plugin install is mid-clone must not go anywhere near that
// terminal's stage. See sweepStaleStages for where stale stages actually get removed.
func New(root string) *Store { return &Store{Root: root} }

func (s *Store) lockPath() string               { return filepath.Join(s.Root, LockFile) }
func (s *Store) pluginsDir() string             { return filepath.Join(s.Root, "plugins") }
func (s *Store) checkoutDir(name string) string { return filepath.Join(s.pluginsDir(), name) }

// staleStageAge is how old an .install-* or .update-* directory under Root/plugins has to be
// before Install or Update will remove it as abandoned rather than leave it alone as
// possibly still in use. An hour is generous next to how long even a large --depth 1 clone
// takes, and every write inside an active stage (git writing objects, os.CopyFS writing
// files) bumps the stage directory's own mtime, so a clone that is still actually making
// progress never ages past this no matter how long it runs.
const staleStageAge = time.Hour

// sweepStaleStages removes every .install-* and .update-* directory under Root/plugins whose
// modification time is older than staleStageAge. Called from Install and Update, never from
// New or DisabledFromLock: those run on every session boot and every rudy plugin list, and
// neither is the moment to go deleting another process's in-progress work. A missing plugins
// directory means nothing has ever been installed, not something to sweep; any other read
// failure is left for the caller's real operation to report, since sweeping is best-effort
// clean-up, not the thing being asked for.
func (s *Store) sweepStaleStages() {
	ents, err := os.ReadDir(s.pluginsDir())
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleStageAge)
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if !strings.HasPrefix(e.Name(), ".install-") && !strings.HasPrefix(e.Name(), ".update-") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(s.pluginsDir(), e.Name()))
	}
}

// Read is every entry the lock holds, keyed by name with Name filled in. A missing lock is
// empty, not an error: nothing has been installed yet.
func (s *Store) Read() (map[string]Installed, error) {
	b, err := os.ReadFile(s.lockPath())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Installed{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pluginstore: %w", err)
	}
	var lf lockFile
	if err := toml.Unmarshal(b, &lf); err != nil {
		return nil, fmt.Errorf("pluginstore: %s: %w", s.lockPath(), err)
	}
	out := make(map[string]Installed, len(lf.Plugins))
	for name, inst := range lf.Plugins {
		inst.Name = name
		if inst.Kind == "" {
			// Predates the kind column: derive it from fields every lock has always
			// carried, exactly as the plugins.lock.toml contracts row says, so an old lock
			// keeps meaning rather than failing the per-kind dispatch on a zero value.
			if inst.Commit != "" {
				inst.Kind = KindGit
			} else {
				inst.Kind = KindPath
			}
		}
		out[name] = inst
	}
	return out, nil
}

// Write replaces the lock with exactly m, through a temp file and rename so a reader never
// sees a partial write. The file is 0600: it names the machine paths and commits every
// installed plugin came from. go-toml sorts a map's keys when it encodes it, so the
// [plugins.<name>] tables land in name order without this having to sort them itself.
func (s *Store) Write(m map[string]Installed) error {
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return fmt.Errorf("pluginstore: %w", err)
	}
	b, err := toml.Marshal(lockFile{Plugins: m})
	if err != nil {
		return fmt.Errorf("pluginstore: %w", err)
	}
	tmp, err := os.CreateTemp(s.Root, ".plugins-lock-*.toml")
	if err != nil {
		return fmt.Errorf("pluginstore: %w", err)
	}
	discard := func(err error) error {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("pluginstore: %s: %w", s.lockPath(), err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return discard(err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return discard(err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return discard(err)
	}
	if err := tmp.Close(); err != nil {
		return discard(err)
	}
	if err := os.Rename(tmp.Name(), s.lockPath()); err != nil {
		return discard(err)
	}
	return nil
}

// Disabled is the names the lock marks disabled, sorted. A lock that will not parse is
// treated as disabling nothing here: a caller that needs to tell "nothing disabled" apart
// from "the lock is broken" reads it through Read instead. DisabledFromLock is that caller.
func (s *Store) Disabled() []string {
	m, err := s.Read()
	if err != nil {
		return nil
	}
	return disabledNames(m)
}

// disabledNames is the shared tail of Store.Disabled and DisabledFromLock: the names an
// already-read lock marks disabled, sorted.
func disabledNames(m map[string]Installed) []string {
	var out []string
	for name, inst := range m {
		if !inst.Enabled {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// DisabledFromLock returns the plugin names the lock at path marks disabled. A missing lock
// disables nothing: nothing has been installed yet, which is not a failure. A lock that will
// not parse is a failure, and it is returned as one: the file is the only record of which
// plugins the user turned off, so reading it as "none of them" would start child processes
// the user disabled.
func DisabledFromLock(path string) ([]string, error) {
	m, err := New(filepath.Dir(path)).Read()
	if err != nil {
		return nil, err
	}
	return disabledNames(m), nil
}

// notInstalled is what every verb but Install returns for a name the lock does not hold, the
// same message rudy plugin's CLI commands surface for an unknown name.
func notInstalled(name string) error { return fmt.Errorf("no plugin named %s", name) }

// validateName refuses an operator-supplied name before it is used to look anything up in
// the lock or touch a path under Root/plugins (Uninstall's os.RemoveAll, SetEnabled and
// Update's checkoutDir(name)). plugin.ValidateManifestName is the same rule ReadManifest
// holds every plugin.toml's own name to; a name is not trustworthy just because it was typed
// on a command line rather than read out of a file. Install never needs this itself: its
// name always comes from a manifest ReadManifest has already validated.
func validateName(name string) error {
	if err := plugin.ValidateManifestName(name); err != nil {
		return fmt.Errorf("pluginstore: %w", err)
	}
	return nil
}

// Install stages source under Root/plugins/.install-*, reads its manifest, and on success
// renames the stage to Root/plugins/<name>, name being the manifest's own name rather than
// the stage's random one. Every error path removes the stage: a failed install leaves no
// checkout and no lock entry behind.
func (s *Store) Install(ctx context.Context, source string, now time.Time) (Installed, plugin.Manifest, error) {
	src, err := ParseSource(source)
	if err != nil {
		return Installed{}, plugin.Manifest{}, fmt.Errorf("pluginstore: %w", err)
	}
	if err := os.MkdirAll(s.pluginsDir(), 0o700); err != nil {
		return Installed{}, plugin.Manifest{}, fmt.Errorf("pluginstore: %w", err)
	}
	s.sweepStaleStages()
	stage, err := os.MkdirTemp(s.pluginsDir(), ".install-*")
	if err != nil {
		return Installed{}, plugin.Manifest{}, fmt.Errorf("pluginstore: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(stage)
		}
	}()

	resolved, err := stageForInstall(ctx, src, stage)
	if err != nil {
		return Installed{}, plugin.Manifest{}, err
	}
	m, err := plugin.ReadManifest(stage)
	if err != nil {
		return Installed{}, plugin.Manifest{}, fmt.Errorf("pluginstore: %s: %w", src.AsTyped, err)
	}
	locked, err := s.Read()
	if err != nil {
		return Installed{}, plugin.Manifest{}, err
	}
	if _, ok := locked[m.Name]; ok {
		return Installed{}, plugin.Manifest{}, fmt.Errorf("%s is already installed", m.Name)
	}
	announceBuild(s.buildOut(), m.Name, m.Build)
	if err := runBuild(ctx, stage, m.Build, s.buildOut()); err != nil {
		return Installed{}, plugin.Manifest{}, fmt.Errorf("pluginstore: %w", err)
	}
	dest := s.checkoutDir(m.Name)
	// Defense in depth: plugin.ReadManifest already refuses a name that could steer dest
	// outside pluginsDir(), so this can only trip if that validation is ever weakened or
	// bypassed. Better an install refused here than a rename that lands outside the plugins
	// directory Uninstall later os.RemoveAll's by name.
	if filepath.Dir(filepath.Clean(dest)) != filepath.Clean(s.pluginsDir()) {
		return Installed{}, plugin.Manifest{}, fmt.Errorf("pluginstore: refusing to install %q outside %s", m.Name, s.pluginsDir())
	}
	if _, err := os.Stat(dest); err == nil {
		return Installed{}, plugin.Manifest{}, fmt.Errorf("%s is already installed", m.Name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Installed{}, plugin.Manifest{}, fmt.Errorf("pluginstore: %w", err)
	}

	if err := os.Rename(stage, dest); err != nil {
		return Installed{}, plugin.Manifest{}, fmt.Errorf("pluginstore: %w", err)
	}
	keep = true
	m.Dir = dest

	inst := Installed{Name: m.Name, Source: src.AsTyped, Kind: src.Kind, Ref: src.Ref, InstalledAt: now, Enabled: true}
	switch src.Kind {
	case KindGit:
		inst.Commit = resolved
	case KindGo, KindHTTPS:
		inst.Digest = resolved
	}
	locked[m.Name] = inst
	if err := s.Write(locked); err != nil {
		// The checkout landed but the lock did not record it: leaving it behind would be a
		// plugin Discover picks up with no way to disable or uninstall it, which is worse
		// than losing the checkout and reporting the write failure.
		_ = os.RemoveAll(dest)
		return Installed{}, plugin.Manifest{}, err
	}
	return inst, m, nil
}

// Uninstall removes a plugin's checkout and its lock entry.
func (s *Store) Uninstall(name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	locked, err := s.Read()
	if err != nil {
		return err
	}
	if _, ok := locked[name]; !ok {
		return notInstalled(name)
	}
	if err := os.RemoveAll(s.checkoutDir(name)); err != nil {
		return fmt.Errorf("pluginstore: %w", err)
	}
	delete(locked, name)
	return s.Write(locked)
}

// SetEnabled flips a plugin's enabled flag in the lock.
func (s *Store) SetEnabled(name string, on bool) error {
	if err := validateName(name); err != nil {
		return err
	}
	locked, err := s.Read()
	if err != nil {
		return err
	}
	inst, ok := locked[name]
	if !ok {
		return notInstalled(name)
	}
	inst.Enabled = on
	locked[name] = inst
	return s.Write(locked)
}

// Update brings an installed plugin's checkout to the latest commit or content, staged the
// same way Install lands a fresh copy: fetched or copied into Root/plugins/.update-*, built
// there, and only a build that succeeds gets swapped into Root/plugins/<name> and recorded in
// the lock. A build that fails leaves the live checkout, and the lock, exactly as they were:
// Update either advances both together or advances neither. Every error path removes the
// stage.
func (s *Store) Update(ctx context.Context, name string, now time.Time) (Installed, error) {
	if err := validateName(name); err != nil {
		return Installed{}, err
	}
	if err := os.MkdirAll(s.pluginsDir(), 0o700); err != nil {
		return Installed{}, fmt.Errorf("pluginstore: %w", err)
	}
	s.sweepStaleStages()
	locked, err := s.Read()
	if err != nil {
		return Installed{}, err
	}
	inst, ok := locked[name]
	if !ok {
		return Installed{}, notInstalled(name)
	}
	src, err := ParseSource(inst.Source)
	if err != nil {
		return Installed{}, fmt.Errorf("pluginstore: %w", err)
	}
	// kind and ref are the lock's own columns, not re-derived from source: a checkout
	// installed before the kind column existed can have a source string that alone reads
	// ambiguously (a local git checkout given as a bare path parses as path, not git), and
	// ref is re-resolved every update, never rewritten from whatever the checkout tracks.
	src.Kind = inst.Kind
	src.Ref = inst.Ref

	stage, err := os.MkdirTemp(s.pluginsDir(), ".update-*")
	if err != nil {
		return Installed{}, fmt.Errorf("pluginstore: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(stage)
		}
	}()

	resolved, err := stageForUpdate(ctx, src, stage)
	if err != nil {
		return Installed{}, err
	}
	m, err := plugin.ReadManifest(stage)
	if err != nil {
		return Installed{}, fmt.Errorf("pluginstore: %s: %w", src.AsTyped, err)
	}
	announceBuild(s.buildOut(), name, m.Build)
	if err := runBuild(ctx, stage, m.Build, s.buildOut()); err != nil {
		return Installed{}, fmt.Errorf("pluginstore: %w", err)
	}

	if err := swapCheckout(s.checkoutDir(name), stage); err != nil {
		return Installed{}, err
	}
	keep = true

	switch src.Kind {
	case KindGit:
		inst.Commit = resolved
	case KindGo, KindHTTPS:
		inst.Digest = resolved
	}
	inst.InstalledAt = now
	locked[name] = inst
	if err := s.Write(locked); err != nil {
		return Installed{}, err
	}
	return inst, nil
}

// swapCheckout replaces dir's live contents with stage's. It renames dir aside before
// renaming stage into its place, rather than os.RemoveAll(dir) then os.Rename(stage, dir):
// that older two-step version could lose the checkout entirely if either half failed, since
// the deferred stage cleanup then removed the only remaining copy too (rudy-xpe). Here, the
// worst a failure between the two renames leaves behind is dir+".old" orphaned next to a
// missing dir, not a destroyed plugin.
func swapCheckout(dir, stage string) error {
	old := dir + ".old"
	if err := os.RemoveAll(old); err != nil {
		return fmt.Errorf("pluginstore: %w", err)
	}
	if err := os.Rename(dir, old); err != nil {
		return fmt.Errorf("pluginstore: %w", err)
	}
	if err := os.Rename(stage, dir); err != nil {
		return fmt.Errorf("pluginstore: %w", err)
	}
	// Best effort: the swap itself already succeeded, and a leftover .old backup is a
	// nuisance to clean up next time, not a reason to fail an update that otherwise worked.
	_ = os.RemoveAll(old)
	return nil
}

// runBuild runs a manifest's build command once in the staged checkout, with the checkout as
// its working directory and the operator's environment. It executes arbitrary code by
// construction: that is what installing from a source means, and the install command says so
// before it happens rather than implying otherwise (ADR 0025).
func runBuild(ctx context.Context, dir, command string, out io.Writer) error {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build %q: %w", command, err)
	}
	return nil
}

// announceBuild tells the operator a manifest's build is about to run, before runBuild
// actually executes it. A manifest with no build command says nothing.
func announceBuild(out io.Writer, name, command string) {
	if strings.TrimSpace(command) == "" {
		return
	}
	_, _ = fmt.Fprintf(out, "building %s: %s\n", name, command)
}

// sourceSchemeRe matches an explicit URL scheme (https://, git://, ssh://, file://, ...).
var sourceSchemeRe = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://`)

// isRemoteSource reports whether source names a remote git endpoint rather than a
// filesystem path, mirroring git's own transport detection. An explicit scheme:// is always
// remote. Otherwise, a ":" appearing before source's first "/" (or with no "/" at all) is
// git's scp-like shorthand, with or without a "user@" (build.example.com:team/plugin.git is
// as valid to git as git@build.example.com:team/plugin.git); that shorthand is remote unless
// source is in fact an existing local path, since a filename with a colon in it is legal,
// if rare, and a real directory on disk beats the shorthand's guess.
func isRemoteSource(source string) bool {
	if sourceSchemeRe.MatchString(source) {
		return true
	}
	colon := strings.IndexByte(source, ':')
	if colon < 0 {
		return false
	}
	if slash := strings.IndexByte(source, '/'); slash >= 0 && slash < colon {
		return false
	}
	if _, err := os.Stat(source); err == nil {
		return false
	}
	return true
}

// stageForInstall fills stage (already created, empty) with src's contents for a fresh
// install, and reports what it resolved to: the checked-out commit for git, empty for path
// (there is no version concept to record), and an error for go and https, not yet
// implemented (tasks 4 and 5 add a stager for each behind this same dispatch).
func stageForInstall(ctx context.Context, src Source, stage string) (resolved string, err error) {
	switch src.Kind {
	case KindGit:
		return stageGitInstall(ctx, src, stage)
	case KindPath:
		return "", stagePath(ctx, src, stage)
	case KindGo:
		return "", fmt.Errorf("pluginstore: go: sources are not yet supported")
	case KindHTTPS:
		return "", fmt.Errorf("pluginstore: https sources are not yet supported")
	default:
		return "", fmt.Errorf("pluginstore: unknown source kind %q", src.Kind)
	}
}

// stageForUpdate is stageForInstall's counterpart for rudy plugins update: the same dispatch,
// but git re-resolves a pinned ref instead of taking whatever a fresh clone's default branch
// happens to be at (see stageGitUpdate).
func stageForUpdate(ctx context.Context, src Source, stage string) (resolved string, err error) {
	switch src.Kind {
	case KindGit:
		return stageGitUpdate(ctx, src, stage)
	case KindPath:
		return "", stagePath(ctx, src, stage)
	case KindGo:
		return "", fmt.Errorf("pluginstore: go: sources are not yet supported")
	case KindHTTPS:
		return "", fmt.Errorf("pluginstore: https sources are not yet supported")
	default:
		return "", fmt.Errorf("pluginstore: unknown source kind %q", src.Kind)
	}
}

// stagePath fills stage with a path source's contents: a directory with no .git is copied as
// plain files, one that is itself a git checkout is cloned instead, so an install or update
// never picks up uncommitted changes or ignored files sitting in a developer's working copy.
// Either way this is the path kind: no ref, no commit recorded, per the contracts row.
func stagePath(ctx context.Context, src Source, stage string) error {
	if fi, statErr := os.Stat(src.Location); statErr == nil && fi.IsDir() {
		if _, gitErr := os.Stat(filepath.Join(src.Location, ".git")); gitErr != nil {
			if err := os.CopyFS(stage, os.DirFS(src.Location)); err != nil {
				return fmt.Errorf("pluginstore: copy %s: %w", src.Location, err)
			}
			return nil
		}
	}
	// -- before the location: it is whatever the operator typed, and a value starting with a
	// dash would otherwise be read as an option to git rather than as a repository.
	if _, err := runGit(ctx, "clone", "--depth", "1", "--", src.Location, stage); err != nil {
		return err
	}
	return nil
}

// stageGitInstall lands a git source into stage for a fresh install and returns the commit
// it landed on. With no ref this is the plain shallow clone stageSource always did. With a
// ref, a shallow clone can name it directly only when it is a branch or a tag; a bare commit
// is not something --branch understands, so that shallow attempt is retried as a full clone
// plus checkout.
func stageGitInstall(ctx context.Context, src Source, stage string) (string, error) {
	if src.Ref == "" {
		if _, err := runGit(ctx, "clone", "--depth", "1", "--", src.Location, stage); err != nil {
			return "", err
		}
		return headCommit(ctx, stage)
	}
	if _, err := runGit(ctx, "clone", "--depth", "1", "--branch", src.Ref, "--", src.Location, stage); err != nil {
		// git refuses to clone into a non-empty directory, and a failed --branch attempt can
		// still have created .git before failing; clear it before the full-clone fallback.
		if rmErr := os.RemoveAll(stage); rmErr != nil {
			return "", fmt.Errorf("pluginstore: %w", rmErr)
		}
		if mkErr := os.MkdirAll(stage, 0o700); mkErr != nil {
			return "", fmt.Errorf("pluginstore: %w", mkErr)
		}
		if _, err := runGit(ctx, "clone", "--", src.Location, stage); err != nil {
			return "", err
		}
		if _, err := runGit(ctx, "-C", stage, "checkout", src.Ref); err != nil {
			return "", err
		}
	}
	return headCommit(ctx, stage)
}

// stageGitUpdate lands a git source into stage for rudy plugins update. With no ref this is
// the existing default-branch behaviour: a fresh shallow clone lands wherever origin's
// default branch tip is now. With a ref, the clone establishes the repository and its origin
// remote, then a shallow fetch of exactly that ref plus a hard reset onto it re-resolves the
// pin, rather than drifting to the tip of whatever branch the clone's default happened to
// track.
func stageGitUpdate(ctx context.Context, src Source, stage string) (string, error) {
	if _, err := runGit(ctx, "clone", "--depth", "1", "--", src.Location, stage); err != nil {
		return "", err
	}
	if src.Ref != "" {
		if _, err := runGit(ctx, "-C", stage, "fetch", "--depth", "1", "--", "origin", src.Ref); err != nil {
			return "", err
		}
		if _, err := runGit(ctx, "-C", stage, "reset", "--hard", "FETCH_HEAD"); err != nil {
			return "", err
		}
	}
	return headCommit(ctx, stage)
}

// headCommit is the checked-out commit of a git directory.
func headCommit(ctx context.Context, dir string) (string, error) {
	return runGit(ctx, "-C", dir, "rev-parse", "HEAD")
}

// runGit runs one git command hermetically: no user or system config, and no prompt for
// credentials a script can never answer, so an unreachable or private source fails fast
// instead of hanging. Its stdout is trimmed and returned; a failure's error carries the
// tail of stderr, which is where git puts the reason.
func runGit(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("pluginstore: git %s: %w: %s", strings.Join(args, " "), err, tail(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// tail is the last few lines of a git command's stderr, enough to name the failure without
// dumping a whole clone's progress output into an error string.
func tail(s string) string {
	s = strings.TrimSpace(s)
	const maxLines = 5
	lines := strings.Split(s, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}
