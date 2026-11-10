// SPDX-License-Identifier: AGPL-3.0-or-later

package trust

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// File is where the workspaces an operator has agreed to are recorded, under the data root.
const File = "trust.toml"

// Record is one trust.toml entry: what the operator agreed to. Digest is the workspace
// digest at that moment; a workspace that has changed since is trusted for what it was, not
// for what it has become, so it asks again (ADR 0025, ADR 0050). Declared is the kind:name
// list for a person reading the file. A record from before ADR 0050 carries plugins instead
// of declared, reads fine and never matches, because its digest covered different inputs.
type Record struct {
	Root      string    `toml:"root"`
	Digest    string    `toml:"digest"`
	Declared  []string  `toml:"declared"`
	TrustedAt time.Time `toml:"trusted_at"`
}

// file is trust.toml as a whole. Roots are keyed by their path, hashed, because a TOML key
// may not carry a slash and a path is not a name.
type file struct {
	Workspaces map[string]Record `toml:"workspaces"`
}

// Store is trust.toml under the data directory.
type Store struct{ Root string }

// NewStore is the trust record under dataDir.
func NewStore(dataDir string) *Store { return &Store{Root: dataDir} }

func (s *Store) path() string { return filepath.Join(s.Root, File) }

// key is a workspace root as a TOML key: its hash, since the path itself carries slashes.
func key(root string) string {
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:8])
}

// Read is every workspace the operator has trusted, keyed by root. A file that is not there
// is nobody trusted yet, which is not an error.
func (s *Store) Read() (map[string]Record, error) {
	body, err := os.ReadFile(s.path())
	if os.IsNotExist(err) {
		return map[string]Record{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("trust: read %s: %w", s.path(), err)
	}
	var f file
	if err := toml.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("trust: parse %s: %w", s.path(), err)
	}
	out := make(map[string]Record, len(f.Workspaces))
	for _, r := range f.Workspaces {
		if r.Root != "" {
			out[r.Root] = r
		}
	}
	return out, nil
}

// Trust records that the operator agreed to run what root ships, as it stands now.
func (s *Store) Trust(root string, decls []Declaration, now time.Time) error {
	all, err := s.Read()
	if err != nil {
		return err
	}
	declared := make([]string, 0, len(decls))
	for _, d := range decls {
		declared = append(declared, d.String())
	}
	all[root] = Record{Root: root, Digest: Digest(decls), Declared: declared, TrustedAt: now.UTC()}
	return s.write(all)
}

// Untrust forgets a workspace, so the next session in it asks again.
func (s *Store) Untrust(root string) error {
	all, err := s.Read()
	if err != nil {
		return err
	}
	if _, ok := all[root]; !ok {
		return fmt.Errorf("trust: %s was not trusted", root)
	}
	delete(all, root)
	return s.write(all)
}

// IsTrusted reports whether the operator agreed to exactly these declarations in this
// workspace. One gained, lost or changed is not trusted for what it is now.
func (s *Store) IsTrusted(root string, decls []Declaration) (bool, error) {
	all, err := s.Read()
	if err != nil {
		return false, err
	}
	r, ok := all[root]
	if !ok {
		return false, nil
	}
	return r.Digest == Digest(decls), nil
}

// write replaces the trust file, keyed by the hash of each root.
func (s *Store) write(all map[string]Record) error {
	f := file{Workspaces: make(map[string]Record, len(all))}
	for root, r := range all {
		r.Root = root
		f.Workspaces[key(root)] = r
	}
	body, err := toml.Marshal(f)
	if err != nil {
		return fmt.Errorf("trust: write %s: %w", s.path(), err)
	}
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return fmt.Errorf("trust: %w", err)
	}
	if err := os.WriteFile(s.path(), body, 0o600); err != nil {
		return fmt.Errorf("trust: write %s: %w", s.path(), err)
	}
	return nil
}
