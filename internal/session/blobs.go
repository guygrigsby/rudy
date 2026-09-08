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
	if !isSHA256Hex(sha256hex) {
		return nil, errors.New("session: blobs: invalid sha256")
	}
	data, err := os.ReadFile(filepath.Join(b.dir, sha256hex))
	if err != nil {
		return nil, fmt.Errorf("session: get blob: %w", err)
	}
	return data, nil
}

// isSHA256Hex reports whether s is exactly 64 lowercase hex characters. Get
// rejects anything else before it ever reaches the filesystem, so a name
// carrying "../" or uppercase characters cannot escape the blobs directory.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
