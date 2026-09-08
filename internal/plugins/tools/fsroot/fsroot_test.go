package fsroot_test

import (
	"os"
	"path/filepath"
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
	defer root.Close()
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

func TestIsBinary(t *testing.T) {
	if fsroot.IsBinary([]byte("plain text\n")) {
		t.Fatal("text flagged binary")
	}
	if !fsroot.IsBinary([]byte("ab\x00cd")) {
		t.Fatal("NUL not flagged")
	}
}
