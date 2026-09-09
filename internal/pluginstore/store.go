// Package pluginstore is the record of what is installed: the plugin checkouts under the XDG
// data root and the lock file beside them. Only the lock's enabled flag is read here; rudy
// plugin install and its siblings own the rest of the file.
package pluginstore

import (
	"os"

	toml "github.com/pelletier/go-toml/v2"
)

// LockFile is the lock's name, next to the plugins directory under the data root.
const LockFile = "plugins.lock.toml"

// lock is plugins.lock.toml as far as loading a plugin cares.
type lock struct {
	Plugins map[string]entry `toml:"plugins"`
}

// entry is one [plugins.<name>] table. Enabled is a pointer so an entry that does not
// mention it is left enabled: the flag is what disables a plugin, and its absence is not a
// decision anybody made.
type entry struct {
	Enabled *bool `toml:"enabled"`
}

// DisabledFromLock returns the plugin names the lock at path marks disabled. A missing or
// unparsable lock disables nothing: a plugin the user installed should not silently
// disappear because the file that records it is gone.
func DisabledFromLock(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var l lock
	if err := toml.Unmarshal(b, &l); err != nil {
		return nil
	}
	var out []string
	for name, e := range l.Plugins {
		if e.Enabled != nil && !*e.Enabled {
			out = append(out, name)
		}
	}
	return out
}
