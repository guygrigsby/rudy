// Package config reads rudy's configuration. It never writes it.
package config

import (
	"os"
	"path/filepath"
)

// Paths are the XDG base directories with /rudy appended.
type Paths struct {
	Config  string // $XDG_CONFIG_HOME/rudy
	Data    string // $XDG_DATA_HOME/rudy
	Runtime string // $XDG_RUNTIME_DIR/rudy
	Cache   string // $XDG_CACHE_HOME/rudy
	Home    string // the user's home directory, for expanding "~/" in config values
}

// XDG resolves the four directories from env, falling back to the XDG defaults under home.
// XDG_RUNTIME_DIR falls back to the OS temp dir because macOS never sets it.
func XDG(env func(string) string, home string) Paths {
	pick := func(key, fallback string) string {
		if v := env(key); v != "" {
			return filepath.Join(v, "rudy")
		}
		return filepath.Join(fallback, "rudy")
	}
	return Paths{
		Config:  pick("XDG_CONFIG_HOME", filepath.Join(home, ".config")),
		Data:    pick("XDG_DATA_HOME", filepath.Join(home, ".local", "share")),
		Runtime: pick("XDG_RUNTIME_DIR", os.TempDir()),
		Cache:   pick("XDG_CACHE_HOME", filepath.Join(home, ".cache")),
		Home:    home,
	}
}
