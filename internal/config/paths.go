// Package config reads rudy's configuration. It never writes it.
package config

import (
	"os"
	"path/filepath"
	"strconv"
)

// Paths are the XDG base directories with /rudy appended.
type Paths struct {
	Config  string // $XDG_CONFIG_HOME/rudy
	Data    string // $XDG_DATA_HOME/rudy
	Runtime string // $XDG_RUNTIME_DIR/rudy, or a per-user dir under the OS temp dir
	Cache   string // $XDG_CACHE_HOME/rudy
	Home    string // the user's home directory, for expanding "~/" in config values
}

// XDG resolves the four directories from env, falling back to the XDG defaults under home.
// XDG_RUNTIME_DIR falls back to a uid-suffixed directory under the OS temp dir: macOS never
// sets XDG_RUNTIME_DIR, and /tmp is shared between users on Linux, so the uid keeps the
// directory private.
func XDG(env func(string) string, home string) Paths {
	pick := func(key, fallback string) string {
		if v := env(key); v != "" {
			return filepath.Join(v, "rudy")
		}
		return filepath.Join(fallback, "rudy")
	}
	runtime := filepath.Join(os.TempDir(), "rudy-"+strconv.Itoa(os.Getuid()))
	if v := env("XDG_RUNTIME_DIR"); v != "" {
		runtime = filepath.Join(v, "rudy")
	}
	return Paths{
		Config:  pick("XDG_CONFIG_HOME", filepath.Join(home, ".config")),
		Data:    pick("XDG_DATA_HOME", filepath.Join(home, ".local", "share")),
		Runtime: runtime,
		Cache:   pick("XDG_CACHE_HOME", filepath.Join(home, ".cache")),
		Home:    home,
	}
}

// ConfigFile is the one file rudy reads its configuration from, and the only one it ever
// adds a key to (Sync). It lives here so Load, the CLI and the tests all name it once.
func (p Paths) ConfigFile() string {
	return filepath.Join(p.Config, "config.toml")
}

// Socket is the path to rudy's unix socket, under the runtime directory.
func (p Paths) Socket() string {
	return filepath.Join(p.Runtime, "rudy.sock")
}
