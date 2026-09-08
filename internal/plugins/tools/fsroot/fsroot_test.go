package fsroot_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugins/tools/fsroot"
)

func TestRel(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		in   string
		want string
		err  bool
	}{
		{"a.txt", "a.txt", false},
		{"./sub/b.txt", "sub/b.txt", false},
		{filepath.Join(root, "sub", "c.txt"), "sub/c.txt", false},
		{"../etc/passwd", "", true},
		{"sub/../../x", "", true},
		{"/etc/passwd", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := fsroot.Rel(root, c.in)
		if (err != nil) != c.err {
			t.Errorf("Rel(%q) err = %v, want err %v", c.in, err, c.err)
			continue
		}
		if !c.err && filepath.ToSlash(got) != c.want {
			t.Errorf("Rel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWriteAtomicCreatesParents(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := fsroot.WriteAtomic(root, "a/b/c.txt", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "a", "b", "c.txt"))
	if err != nil || string(b) != "hi" {
		t.Fatalf("read back %q, %v", b, err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "a", "b"))
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %d entries", len(entries))
	}
}

func TestWriteAtomicConcurrentSameRelDoesNotCollide(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	const n = 16
	contents := make([][]byte, n)
	for i := range n {
		contents[i] = []byte(fmt.Sprintf("content-%02d-%s", i, strings.Repeat("x", 200)))
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = fsroot.WriteAtomic(root, "same.txt", contents[i])
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("WriteAtomic goroutine %d: %v", i, err)
		}
	}

	got, err := os.ReadFile(filepath.Join(dir, "same.txt"))
	if err != nil {
		t.Fatalf("read final file: %v", err)
	}
	matched := false
	for _, c := range contents {
		if string(got) == string(c) {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("final file (%d bytes) does not match any of the %d written contents in full", len(got), n)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestIsBinary(t *testing.T) {
	if fsroot.IsBinary([]byte("plain text\n")) {
		t.Fatal("text flagged binary")
	}
	if !fsroot.IsBinary([]byte("ab\x00cd")) {
		t.Fatal("NUL not flagged")
	}
}
