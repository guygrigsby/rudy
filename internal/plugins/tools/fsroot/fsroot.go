// Package fsroot confines tool file access to the workspace root.
package fsroot

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

var ErrEscape = errors.New("path escapes the workspace")

// Rel converts p to a path relative to root for use with os.Root. Absolute paths inside root
// are rebased. A path that would leave root is ErrEscape. Symlink escapes are caught later
// by os.Root itself.
func Rel(root, p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(p) {
		r, err := filepath.Rel(root, p)
		if err != nil {
			return "", ErrEscape
		}
		p = r
	}
	p = filepath.Clean(p)
	if p == ".." || strings.HasPrefix(p, ".."+string(filepath.Separator)) {
		return "", ErrEscape
	}
	return p, nil
}

// tmpSeq gives every call to WriteAtomic in this process a distinct ordinal, so two
// concurrent writes to the same rel path never share a temp file name.
var tmpSeq atomic.Uint64

// tempName builds a temp file name for rel that is unique per call: pid, a process-wide
// counter and 8 random hex bytes, so concurrent writers (even across processes) never
// collide on the same tmp path.
func tempName(rel string) (string, error) {
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		return "", fmt.Errorf("fsroot: temp name: %w", err)
	}
	seq := tmpSeq.Add(1)
	return fmt.Sprintf("%s.rudy-%d-%d-%s.tmp", rel, os.Getpid(), seq, hex.EncodeToString(r[:])), nil
}

// WriteAtomic writes data to rel inside root through a temp file and rename.
func WriteAtomic(root *os.Root, rel string, data []byte) error {
	if dir := filepath.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp, err := tempName(rel)
	if err != nil {
		return err
	}
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = root.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return root.Rename(tmp, rel)
}

// IsBinary reports a NUL byte within the first 8000 bytes.
func IsBinary(b []byte) bool {
	n := min(len(b), 8000)
	return bytes.IndexByte(b[:n], 0) >= 0
}

func Text(s string) tool.Result {
	return tool.Result{Content: []session.Block{session.TextBlock(s)}}
}

func Fail(format string, args ...any) tool.Result {
	return tool.Result{Content: []session.Block{session.TextBlock(fmt.Sprintf(format, args...))}, IsError: true}
}
