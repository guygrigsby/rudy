// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlobsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	b, err := OpenBlobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("\x89PNG\r\n\x1a\nfake")
	sha, err := b.Put(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(sha) != 64 {
		t.Fatalf("sha = %q", sha)
	}
	again, err := b.Put(data)
	if err != nil || again != sha {
		t.Fatalf("second Put = %q, %v; want %q", again, err, sha)
	}
	got, err := b.Get(sha)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs", sha)); err != nil {
		t.Fatalf("blob file missing: %v", err)
	}
	if _, err := b.Get("deadbeef"); err == nil {
		t.Fatal("Get of a missing blob must fail")
	}
}

func TestBlobsGetRejectsInvalidHex(t *testing.T) {
	dir := t.TempDir()
	b, err := OpenBlobs(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Plant a file outside the blobs directory that a traversal name would
	// read if the hex check were length-only (the pre-fix behavior).
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("outside the blobs dir"), 0o600); err != nil {
		t.Fatal(err)
	}

	// "a/../" is a no-op path segment; 11 of them pad the traversal out to
	// exactly 64 characters (the length the old check alone accepted)
	// while "../secret" is what actually escapes the blobs directory, one
	// level up to dir/secret. This is depth-independent, unlike a long run
	// of "../", so it resolves the same regardless of how deeply t.TempDir
	// nests the blobs directory.
	traversal := strings.Repeat("a/../", 11) + "../secret"
	if len(traversal) != 64 {
		t.Fatalf("test setup: traversal len = %d, want 64", len(traversal))
	}
	if resolved := filepath.Clean(filepath.Join(dir, "blobs", traversal)); resolved != secret {
		t.Fatalf("test setup: traversal resolves to %q, want %q", resolved, secret)
	}
	if _, err := b.Get(traversal); err == nil || !strings.Contains(err.Error(), "invalid sha256") {
		t.Fatalf("Get(%q) = %v, want an invalid sha256 error", traversal, err)
	}

	upper := strings.ToUpper(strings.Repeat("a", 64))
	if _, err := b.Get(upper); err == nil || !strings.Contains(err.Error(), "invalid sha256") {
		t.Fatalf("Get(%q) = %v, want an invalid sha256 error", upper, err)
	}
}
