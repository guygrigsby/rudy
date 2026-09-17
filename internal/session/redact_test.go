// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestAFilesystemFailureDoesNotCarryItsPath: an append or sync failure becomes a
// turn_failed message and a protocol error, both of which reach every subscriber of the
// session. The syscall's reason is what a caller can act on; the store's layout is the
// operator's and belongs in the log (rudy-wpa).
func TestAFilesystemFailureDoesNotCarryItsPath(t *testing.T) {
	secret := "/Users/someone/.local/share/rudy/sessions/01JZZ/entries.jsonl"
	err := opErr("append", &os.PathError{Op: "write", Path: secret, Err: syscall.ENOSPC})
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "/Users") {
		t.Errorf("the error carries the path: %q", err)
	}
	if !strings.Contains(err.Error(), "no space left on device") {
		t.Errorf("the error lost the reason a caller can act on: %q", err)
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Errorf("the error stopped matching its cause: %q", err)
	}
}

// TestOpeningALogThatCannotExistSaysWhyWithoutSayingWhere covers the real path through the
// same helper: a store directory that cannot be created.
func TestOpeningALogThatCannotExistSaysWhyWithoutSayingWhere(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := OpenLog(filepath.Join(blocker, "session"))
	if err == nil {
		t.Fatal("opening a log under a regular file succeeded")
	}
	if strings.Contains(err.Error(), dir) {
		t.Errorf("the error carries the store path: %q", err)
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("the error does not say what went wrong: %q", err)
	}
}
