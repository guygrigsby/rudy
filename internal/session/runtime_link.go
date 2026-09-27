// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/oklog/ulid/v2"
	toml "github.com/pelletier/go-toml/v2"
)

const RuntimeLinkFile = "runtime.toml"

var (
	ErrRuntimeLinkNotFound = errors.New("session: runtime link not found")
	ErrRuntimeLinkConflict = errors.New("session: runtime link conflicts with an existing binding")
)

type RuntimeLink struct {
	Runtime  string `toml:"runtime"`
	ThreadID string `toml:"thread_id"`
}

func (l RuntimeLink) validate() error {
	if l.Runtime == "" || l.ThreadID == "" {
		return errors.New("session: runtime link requires runtime and thread id")
	}
	return nil
}

type RuntimeLinkStore struct {
	mu   sync.Mutex
	root string
}

func NewRuntimeLinkStore(root string) *RuntimeLinkStore {
	return &RuntimeLinkStore{root: root}
}

func (s *RuntimeLinkStore) Path(id ulid.ULID) string {
	return filepath.Join(s.root, id.String(), RuntimeLinkFile)
}

func (s *RuntimeLinkStore) Read(id ulid.ULID) (RuntimeLink, error) {
	body, err := os.ReadFile(s.Path(id))
	if errors.Is(err, os.ErrNotExist) {
		return RuntimeLink{}, ErrRuntimeLinkNotFound
	}
	if err != nil {
		return RuntimeLink{}, fmt.Errorf("session: read runtime link: %w", err)
	}
	var link RuntimeLink
	decoder := toml.NewDecoder(bytes.NewReader(body)).DisallowUnknownFields()
	if err := decoder.Decode(&link); err != nil {
		return RuntimeLink{}, fmt.Errorf("session: decode runtime link: %w", err)
	}
	if err := link.validate(); err != nil {
		return RuntimeLink{}, err
	}
	return link, nil
}

func (s *RuntimeLinkStore) Write(id ulid.ULID, link RuntimeLink) error {
	if id.IsZero() {
		return errors.New("session: runtime link requires session id")
	}
	if err := link.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, err := s.Read(id); err == nil {
		if existing == link {
			return nil
		}
		return ErrRuntimeLinkConflict
	} else if !errors.Is(err, ErrRuntimeLinkNotFound) {
		return err
	}
	if err := s.rejectDuplicate(id, link); err != nil {
		return err
	}

	dir := filepath.Dir(s.Path(id))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("session: create runtime link dir: %w", err)
	}
	body, err := toml.Marshal(link)
	if err != nil {
		return fmt.Errorf("session: encode runtime link: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".runtime-*.toml")
	if err != nil {
		return fmt.Errorf("session: create runtime link: %w", err)
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("session: chmod runtime link: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		return fmt.Errorf("session: write runtime link: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("session: sync runtime link: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("session: close runtime link: %w", err)
	}
	if err := os.Rename(tmpName, s.Path(id)); err != nil {
		return fmt.Errorf("session: replace runtime link: %w", err)
	}
	dirFile, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("session: open runtime link dir: %w", err)
	}
	if err := dirFile.Sync(); err != nil {
		_ = dirFile.Close()
		return fmt.Errorf("session: sync runtime link dir: %w", err)
	}
	if err := dirFile.Close(); err != nil {
		return fmt.Errorf("session: close runtime link dir: %w", err)
	}
	keep = true
	return nil
}

func (s *RuntimeLinkStore) rejectDuplicate(id ulid.ULID, link RuntimeLink) error {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("session: list runtime links: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == id.String() {
			continue
		}
		otherID, err := ulid.ParseStrict(entry.Name())
		if err != nil {
			continue
		}
		other, err := s.Read(otherID)
		if errors.Is(err, ErrRuntimeLinkNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if other == link {
			return fmt.Errorf("%w: %s already owns %s:%s", ErrRuntimeLinkConflict, otherID, link.Runtime, link.ThreadID)
		}
	}
	return nil
}
