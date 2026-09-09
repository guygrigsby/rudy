// Package pluginstore is the record of what is installed: the plugin checkouts under the XDG
// data root and the lock file beside them. rudy plugin install and its siblings own the
// whole file; the server's own boot path only ever reads the enabled flag through
// DisabledFromLock.
package pluginstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	Name        string    `toml:"-"`
	Source      string    `toml:"source"`
	Commit      string    `toml:"commit"`
	InstalledAt time.Time `toml:"installed_at"`
	Enabled     bool      `toml:"enabled"`
}

// lockFile is plugins.lock.toml as a whole.
type lockFile struct {
	Plugins map[string]Installed `toml:"plugins"`
}

// Store is the plugin checkouts and their lock file under one XDG data root: checkouts live
// at Root/plugins/<name>, the lock at Root/plugins.lock.toml.
type Store struct{ Root string }

// New is a Store over root, normally $XDG_DATA_HOME/rudy.
func New(root string) *Store { return &Store{Root: root} }

func (s *Store) lockPath() string               { return filepath.Join(s.Root, LockFile) }
func (s *Store) pluginsDir() string             { return filepath.Join(s.Root, "plugins") }
func (s *Store) checkoutDir(name string) string { return filepath.Join(s.pluginsDir(), name) }

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

// Install stages source under Root/plugins/.install-*, reads its manifest, and on success
// renames the stage to Root/plugins/<name>, name being the manifest's own name rather than
// the stage's random one. Every error path removes the stage: a failed install leaves no
// checkout and no lock entry behind.
func (s *Store) Install(ctx context.Context, source string, now time.Time) (Installed, plugin.Manifest, error) {
	if err := os.MkdirAll(s.pluginsDir(), 0o700); err != nil {
		return Installed{}, plugin.Manifest{}, fmt.Errorf("pluginstore: %w", err)
	}
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

	isClone, err := stageSource(ctx, source, stage)
	if err != nil {
		return Installed{}, plugin.Manifest{}, err
	}
	m, err := plugin.ReadManifest(stage)
	if err != nil {
		return Installed{}, plugin.Manifest{}, fmt.Errorf("pluginstore: %s: %w", source, err)
	}

	locked, err := s.Read()
	if err != nil {
		return Installed{}, plugin.Manifest{}, err
	}
	if _, ok := locked[m.Name]; ok {
		return Installed{}, plugin.Manifest{}, fmt.Errorf("%s is already installed", m.Name)
	}
	dest := s.checkoutDir(m.Name)
	if _, err := os.Stat(dest); err == nil {
		return Installed{}, plugin.Manifest{}, fmt.Errorf("%s is already installed", m.Name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Installed{}, plugin.Manifest{}, fmt.Errorf("pluginstore: %w", err)
	}

	var commit string
	if isClone {
		if commit, err = headCommit(ctx, stage); err != nil {
			return Installed{}, plugin.Manifest{}, err
		}
	}
	if err := os.Rename(stage, dest); err != nil {
		return Installed{}, plugin.Manifest{}, fmt.Errorf("pluginstore: %w", err)
	}
	keep = true
	m.Dir = dest

	inst := Installed{Name: m.Name, Source: source, Commit: commit, InstalledAt: now, Enabled: true}
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

// Update brings an installed plugin's checkout to the latest commit: a clone fetches and
// hard-resets to the remote's default branch, a path-copied plugin is removed and re-copied
// from its original source through a stage, the same way Install lands a fresh copy.
func (s *Store) Update(ctx context.Context, name string, now time.Time) (Installed, error) {
	locked, err := s.Read()
	if err != nil {
		return Installed{}, err
	}
	inst, ok := locked[name]
	if !ok {
		return Installed{}, notInstalled(name)
	}
	dir := s.checkoutDir(name)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if _, err := runGit(ctx, "-C", dir, "fetch", "--depth", "1", "origin"); err != nil {
			return Installed{}, err
		}
		if _, err := runGit(ctx, "-C", dir, "reset", "--hard", "FETCH_HEAD"); err != nil {
			return Installed{}, err
		}
		commit, err := headCommit(ctx, dir)
		if err != nil {
			return Installed{}, err
		}
		inst.Commit = commit
	} else {
		if err := s.recopy(inst.Source, dir); err != nil {
			return Installed{}, err
		}
		inst.Commit = ""
	}
	inst.InstalledAt = now
	locked[name] = inst
	if err := s.Write(locked); err != nil {
		return Installed{}, err
	}
	return inst, nil
}

// recopy replaces dir with a fresh copy of source, staged beside dir first so a failed copy
// never leaves dir half written or missing.
func (s *Store) recopy(source, dir string) error {
	stage, err := os.MkdirTemp(s.pluginsDir(), ".update-*")
	if err != nil {
		return fmt.Errorf("pluginstore: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := os.CopyFS(stage, os.DirFS(source)); err != nil {
		return fmt.Errorf("pluginstore: copy %s: %w", source, err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("pluginstore: %w", err)
	}
	if err := os.Rename(stage, dir); err != nil {
		return fmt.Errorf("pluginstore: %w", err)
	}
	keep = true
	return nil
}

// stageSource fills stage (already created, empty) with source's contents: a directory with
// no .git is copied as plain files, everything else (a URL, or a local path that is itself a
// git checkout) is cloned. isClone tells the caller whether stage is a git checkout it can
// read a commit out of.
func stageSource(ctx context.Context, source, stage string) (isClone bool, err error) {
	if fi, statErr := os.Stat(source); statErr == nil && fi.IsDir() {
		if _, gitErr := os.Stat(filepath.Join(source, ".git")); gitErr != nil {
			if err := os.CopyFS(stage, os.DirFS(source)); err != nil {
				return false, fmt.Errorf("pluginstore: copy %s: %w", source, err)
			}
			return false, nil
		}
	}
	if _, err := runGit(ctx, "clone", "--depth", "1", source, stage); err != nil {
		return false, err
	}
	return true, nil
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
