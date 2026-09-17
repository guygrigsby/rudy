// SPDX-License-Identifier: AGPL-3.0-or-later

package pluginstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/guygrigsby/rudy/internal/plugin"
)

// TrustFile is where the workspaces an operator has agreed to run plugins from are
// recorded, next to the plugin checkouts under the data root.
const TrustFile = "trust.toml"

// Trusted is one workspace the operator agreed to. Digest is what they agreed to: the
// manifests that were there when they said yes. A workspace whose plugins have changed
// since is trusted for what it was, not for what it has become, so it asks again. ADR 0025.
type Trusted struct {
	Root      string    `toml:"root"`
	Digest    string    `toml:"digest"`
	Plugins   []string  `toml:"plugins"`
	TrustedAt time.Time `toml:"trusted_at"`
}

// trustFile is trust.toml as a whole. Roots are keyed by their path, hashed, because a TOML
// key may not carry a slash and a path is not a name.
type trustFile struct {
	Workspaces map[string]Trusted `toml:"workspaces"`
}

// trustPath is the trust file under the store's root.
func (s *Store) trustPath() string { return filepath.Join(s.Root, TrustFile) }

// key is a workspace root as a TOML key: its hash, since the path itself carries slashes.
func trustKey(root string) string {
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:8])
}

// ReadTrust is every workspace the operator has trusted, keyed by root. A file that is not
// there is nobody trusted yet, which is not an error.
func (s *Store) ReadTrust() (map[string]Trusted, error) {
	body, err := os.ReadFile(s.trustPath())
	if os.IsNotExist(err) {
		return map[string]Trusted{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pluginstore: read %s: %w", s.trustPath(), err)
	}
	var f trustFile
	if err := toml.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("pluginstore: parse %s: %w", s.trustPath(), err)
	}
	out := make(map[string]Trusted, len(f.Workspaces))
	for _, t := range f.Workspaces {
		if t.Root != "" {
			out[t.Root] = t
		}
	}
	return out, nil
}

// Trust records that the operator agreed to run root's own plugins, as they stand now.
func (s *Store) Trust(root string, manifests []plugin.Manifest, now time.Time) error {
	all, err := s.ReadTrust()
	if err != nil {
		return err
	}
	all[root] = Trusted{
		Root:      root,
		Digest:    TrustDigest(manifests),
		Plugins:   manifestNames(manifests),
		TrustedAt: now.UTC(),
	}
	return s.writeTrust(all)
}

// Untrust forgets a workspace, so the next session in it asks again.
func (s *Store) Untrust(root string) error {
	all, err := s.ReadTrust()
	if err != nil {
		return err
	}
	if _, ok := all[root]; !ok {
		return fmt.Errorf("pluginstore: %s was not trusted", root)
	}
	delete(all, root)
	return s.writeTrust(all)
}

// IsTrusted reports whether the operator agreed to exactly these plugins in this workspace.
// A workspace that has gained, lost or changed one is not trusted for what it is now.
func (s *Store) IsTrusted(root string, manifests []plugin.Manifest) (bool, error) {
	all, err := s.ReadTrust()
	if err != nil {
		return false, err
	}
	t, ok := all[root]
	if !ok {
		return false, nil
	}
	return t.Digest == TrustDigest(manifests), nil
}

// TrustDigest is what a workspace's plugins are, as one string: each manifest's name, the
// command it runs and the build it runs first, in a fixed order. A manifest that changes
// what it executes changes the digest, which is what makes trust specific rather than a
// blanket yes to a directory.
func TrustDigest(manifests []plugin.Manifest) string {
	lines := make([]string, 0, len(manifests))
	for _, m := range manifests {
		lines = append(lines, strings.Join([]string{
			m.Name, m.Version, m.Command, strings.Join(m.Args, " "), m.Build,
		}, "\x00"))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// manifestNames are the plugin names in a workspace, sorted, for the record a person reads.
func manifestNames(manifests []plugin.Manifest) []string {
	out := make([]string, 0, len(manifests))
	for _, m := range manifests {
		out = append(out, m.Name)
	}
	sort.Strings(out)
	return out
}

// writeTrust replaces the trust file, keyed by the hash of each root.
func (s *Store) writeTrust(all map[string]Trusted) error {
	f := trustFile{Workspaces: make(map[string]Trusted, len(all))}
	for root, t := range all {
		t.Root = root
		f.Workspaces[trustKey(root)] = t
	}
	body, err := toml.Marshal(f)
	if err != nil {
		return fmt.Errorf("pluginstore: write %s: %w", s.trustPath(), err)
	}
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return fmt.Errorf("pluginstore: %w", err)
	}
	if err := os.WriteFile(s.trustPath(), body, 0o600); err != nil {
		return fmt.Errorf("pluginstore: write %s: %w", s.trustPath(), err)
	}
	return nil
}
