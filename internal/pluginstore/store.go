// Package pluginstore is the record of what is installed: the plugin checkouts under the XDG
// data root and the lock file beside them. Only the lock's enabled flag is read here; rudy
// plugin install and its siblings own the rest of the file.
package pluginstore

import (
	"errors"
	"fmt"
	"os"
	"sort"

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

// DisabledFromLock returns the plugin names the lock at path marks disabled. A missing lock
// disables nothing: nothing has been installed yet, which is not a failure. A lock that will
// not parse is a failure, and it is returned as one: the file is the only record of which
// plugins the user turned off, so reading it as "none of them" would start child processes
// the user disabled.
func DisabledFromLock(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pluginstore: %s: %w", path, err)
	}
	var l lock
	if err := toml.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("pluginstore: %s: %w", path, err)
	}
	var out []string
	for name, e := range l.Plugins {
		if e.Enabled != nil && !*e.Enabled {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}
