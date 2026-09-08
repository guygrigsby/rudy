package session

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Blobs is the content-addressed store under <session dir>/blobs.
type Blobs struct{ dir string }

// OpenBlobs creates the blobs directory when missing.
func OpenBlobs(dir string) (*Blobs, error) {
	d := filepath.Join(dir, "blobs")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return nil, fmt.Errorf("session: open blobs: %w", err)
	}
	return &Blobs{dir: d}, nil
}

// Put stores data under its sha256 hex and returns the hex. An existing
// blob is left alone; the write is a temp file renamed into place.
func (b *Blobs) Put(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:])
	dst := filepath.Join(b.dir, name)
	if _, err := os.Stat(dst); err == nil {
		return name, nil
	}
	tmp, err := os.CreateTemp(b.dir, "put-*")
	if err != nil {
		return "", fmt.Errorf("session: put blob: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("session: put blob: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("session: put blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("session: put blob: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("session: put blob: %w", err)
	}
	return name, nil
}

// Get reads a blob by its sha256 hex.
func (b *Blobs) Get(sha256hex string) ([]byte, error) {
	if len(sha256hex) != 64 {
		return nil, errors.New("session: get blob: not a sha256 hex")
	}
	data, err := os.ReadFile(filepath.Join(b.dir, sha256hex))
	if err != nil {
		return nil, fmt.Errorf("session: get blob: %w", err)
	}
	return data, nil
}
