package tarx

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// entry is one header and its content, so a test archive reads as what it holds.
type entry struct {
	name string
	typ  byte
	body string
	link string
}

// archiveOf builds a tar in memory. Built in Go rather than shelled out to tar, because the
// archives worth testing are ones no tar on this machine would let anybody write.
func archiveOf(t *testing.T, entries ...entry) *tar.Reader {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Typeflag: typ, Mode: 0o644, Size: int64(len(e.body)), Linkname: e.link}
		if err := w.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return tar.NewReader(&buf)
}

func TestUnpackWritesFilesAndDirectories(t *testing.T) {
	dir := t.TempDir()
	tr := archiveOf(t,
		entry{name: "./", typ: tar.TypeDir},
		entry{name: "a.txt", body: "a"},
		entry{name: "sub/", typ: tar.TypeDir},
		entry{name: "sub/b.txt", body: "b"},
	)
	if err := Unpack(tr, dir, Options{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "a" {
		t.Fatalf("a.txt = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "sub", "b.txt")); string(got) != "b" {
		t.Fatalf("sub/b.txt = %q", got)
	}
}

// TestUnpackRefusesAnArchiveThatReachesOutside is the whole reason this package exists: the
// bytes came from somewhere else, and each of these names or types is a write outside the
// directory the caller named. Every case is refused naming the entry, and nothing lands.
func TestUnpackRefusesAnArchiveThatReachesOutside(t *testing.T) {
	cases := []struct {
		name  string
		entry entry
		want  string
	}{
		{"absolute", entry{name: "/victim/x", body: "owned"}, "escapes the stage"},
		{"parent", entry{name: "../victim/x", body: "owned"}, "escapes the stage"},
		{"deeper parent", entry{name: "sub/../../victim/x", body: "owned"}, "escapes the stage"},
		{"backslash", entry{name: `..\..\victim`, body: "owned"}, "backslash"},
		{"symlink", entry{name: "link", typ: tar.TypeSymlink, link: "/etc/passwd"}, "not a regular file or a directory"},
		{"hard link", entry{name: "hard", typ: tar.TypeLink, link: "/etc/passwd"}, "not a regular file or a directory"},
		{"fifo", entry{name: "pipe", typ: tar.TypeFifo}, "not a regular file or a directory"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			outer := t.TempDir()
			dir := filepath.Join(outer, "stage")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			err := Unpack(archiveOf(t, c.entry), dir, Options{})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Unpack(%s) = %v, want a refusal naming %q", c.name, err, c.want)
			}
			if !strings.Contains(err.Error(), strconv.Quote(c.entry.name)) {
				t.Fatalf("the refusal does not name the entry: %v", err)
			}
			left, _ := os.ReadDir(outer)
			for _, e := range left {
				if e.Name() != "stage" {
					t.Fatalf("%q was written beside the stage", e.Name())
				}
			}
			inside, _ := os.ReadDir(dir)
			if len(inside) != 0 {
				t.Fatalf("the stage is not empty: %v", inside)
			}
		})
	}
}

// TestUnpackCapsEntriesAndBytes: an archive that is cheap to send is not cheap to unpack.
func TestUnpackCapsEntriesAndBytes(t *testing.T) {
	dir := t.TempDir()
	many := make([]entry, 0, 4)
	for i := range 4 {
		many = append(many, entry{name: string(rune('a'+i)) + ".txt", body: "x"})
	}
	err := Unpack(archiveOf(t, many...), dir, Options{Entries: 2})
	if err == nil || !strings.Contains(err.Error(), "more than 2 entries") {
		t.Fatalf("entry cap = %v", err)
	}
	err = Unpack(archiveOf(t, entry{name: "big.txt", body: "0123456789"}), t.TempDir(), Options{Bytes: 4})
	if err == nil || !strings.Contains(err.Error(), "4 byte unpacked cap") {
		t.Fatalf("byte cap = %v", err)
	}
}

// TestUnpackStripsTheSoleTopLevelDirectory is what an archive git or GitHub made needs, and
// the prefix is applied only after a name has passed the checks above.
func TestUnpackStripsTheSoleTopLevelDirectory(t *testing.T) {
	dir := t.TempDir()
	tr := archiveOf(t,
		entry{name: "rudy-1.0/", typ: tar.TypeDir},
		entry{name: "rudy-1.0/plugin.toml", body: "name = 'x'"},
	)
	if err := Unpack(tr, dir, Options{Strip: "rudy-1.0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "plugin.toml")); err != nil {
		t.Fatalf("the prefix was not stripped: %v", err)
	}
}
