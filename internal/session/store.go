package session

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/oklog/ulid/v2"
)

// Store is the directory that holds one subdirectory per session.
type Store struct{ root string }

// OpenStore creates root when missing.
func OpenStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("session: open store: %w", err)
	}
	return &Store{root: root}, nil
}

// Root is the store directory.
func (st *Store) Root() string { return st.root }

// Dir is the directory of one session.
func (st *Store) Dir(id ulid.ULID) string { return filepath.Join(st.root, id.String()) }

// lock is replaced by a flock in Task 5. Until then no lock is taken.
func (st *Store) lock(id ulid.ULID) (func(), error) { return func() {}, nil }
