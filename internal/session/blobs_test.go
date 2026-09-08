package session

import (
	"bytes"
	"os"
	"path/filepath"
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
