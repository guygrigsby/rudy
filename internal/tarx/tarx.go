// Package tarx unpacks a tar archive whose bytes came from somewhere this machine does not
// control: a tarball an https: URL served, or a tar a host wrote down an ssh connection. Both
// callers write the result onto disk next to the operator's own files, so both need the same
// rules, and the rules are here rather than twice over.
//
// Every entry name is resolved against the destination and refused if it could land outside
// it. Entry types that can name anything on disk (a symlink, a hard link) are refused outright
// rather than validated case by case: neither caller's archive has a legitimate need for one,
// and refusing the type removes a class of link-resolution bugs from ever having to be gotten
// right against bytes somebody else produced. The entry count and the bytes written are both
// capped, since an archive that is cheap to send can be enormous to unpack.
package tarx

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// MaxEntries bounds how many entries Unpack accepts by default, alongside MaxBytes: an empty
// file or directory costs nothing against a budget of bytes written and compresses to almost
// nothing, so a byte cap alone leaves the entry count bounded only by the archive's length,
// each entry a MkdirAll or an OpenFile and an inode (rudy-79q).
const MaxEntries = 20000

// MaxBytes bounds the total bytes Unpack writes to disk by default, independent of whatever
// bounds the archive itself: a stream compresses, and a sparse entry declares a size no
// archive bytes back at all, so only what lands on disk is worth counting.
const MaxBytes = 256 << 20

// Options is what one caller wants that the other does not.
type Options struct {
	// Strip is a sole top-level directory removed from every entry name, "" for none. The
	// caller decides there is one (an archive git or GitHub made wraps its contents in
	// exactly one); Unpack only applies it.
	Strip string
	// Entries and Bytes replace MaxEntries and MaxBytes when non-zero. A working tree is
	// bigger than a plugin bundle, and the cap is there for a runaway or hostile host rather
	// than to say how large somebody's project may be.
	Entries int
	Bytes   int64
}

func (o Options) entries() int {
	if o.Entries > 0 {
		return o.Entries
	}
	return MaxEntries
}

func (o Options) bytes() int64 {
	if o.Bytes > 0 {
		return o.Bytes
	}
	return MaxBytes
}

// Clean resolves a tar entry's name to a destination-relative path, refusing anything that
// could land outside the destination. This is the security property of unpacking an archive
// somebody else produced: a hostile or merely careless one can name an entry "../../evil" or
// "/etc/cron.d/whatever" and the caller's own tar would happily follow it. It runs before any
// prefix is stripped, so a prefix can never be derived from a name that would have been
// refused.
func Clean(name string) (string, error) {
	// "..\..\x" is one legal, backslash-containing filename to filepath.Clean on a unix build
	// (backslash is not a separator there), so it survives every check below unchanged; refuse
	// it outright rather than rely on this running only on unix, since the path handling here
	// is the OS-provided filepath, not a unix-only one.
	if strings.ContainsRune(name, '\\') {
		return "", fmt.Errorf("entry %q contains a backslash", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("entry %q escapes the stage", name)
	}
	return clean, nil
}

// Unpack writes every entry of tr under dir, which the caller has already made. It returns on
// the first entry it refuses, with whatever earlier entries wrote still on disk: dir is a
// staging directory both callers throw away on any error, which is what makes a refusal leave
// nothing behind.
func Unpack(tr *tar.Reader, dir string, o Options) error {
	entries, remaining := 0, o.bytes()
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// A PAX global header (typeflag 'g') is the entry git archive and GitHub's release
		// tarballs emit ahead of the real content. It names no file of the archive's own, so
		// it is skipped rather than falling into the refusal below.
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		entries++
		if entries > o.entries() {
			return fmt.Errorf("more than %d entries", o.entries())
		}
		// Refusing up front on the declared size is what catches a sparse entry, whose Size
		// can be far larger than the archive bytes behind it: the reader synthesizes the
		// declared holes without consuming any. The LimitReader below is the enforcement,
		// since only bytes actually written are counted against the budget.
		if hdr.Size < 0 || hdr.Size > remaining {
			return fmt.Errorf("entry %q would exceed the %d byte unpacked cap", hdr.Name, o.bytes())
		}
		rel, err := Clean(hdr.Name)
		if err != nil {
			return err
		}
		if rel = strip(rel, o.Strip); rel == "." {
			continue
		}
		target, err := stageTarget(dir, rel)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, dirMode(hdr)); err != nil {
				return err
			}
		case tar.TypeReg:
			n, err := writeFile(target, io.LimitReader(tr, remaining), hdr)
			remaining -= n
			if err != nil {
				return fmt.Errorf("entry %q: %w", hdr.Name, err)
			}
		default:
			return fmt.Errorf("entry %q is not a regular file or a directory; a symlink or a hard link names whatever it likes on disk and neither is unpacked", hdr.Name)
		}
	}
}

// strip removes prefix, the archive's sole top-level directory, from one cleaned entry name.
// The prefix's own directory entry becomes ".", the destination itself, which Unpack skips.
func strip(rel, prefix string) string {
	switch {
	case prefix == "":
		return rel
	case rel == prefix:
		return "."
	default:
		return strings.TrimPrefix(rel, prefix+string(filepath.Separator))
	}
}

// stageTarget joins a cleaned, destination-relative entry name onto dir.
func stageTarget(dir, rel string) (string, error) {
	target := filepath.Join(dir, rel)
	// Defense in depth: Clean should already make this impossible, but code touching
	// arbitrary paths on disk on somebody else's input is exactly where to check twice rather
	// than trust one path to have gotten it right.
	if target != dir && !strings.HasPrefix(target, dir+string(filepath.Separator)) {
		return "", fmt.Errorf("entry %q escapes the stage", rel)
	}
	return target, nil
}

// dirMode is a directory entry's own permission bits, so a tree arrives as it left rather than
// as whatever this package felt like, with the owner's three forced on: a directory this
// process cannot enter is one nothing below it can be written into, and an archive is free to
// carry one (or to carry no mode at all, which is then 0o700). A caller that moves the result
// somewhere carries these on, as it carries a file's.
func dirMode(hdr *tar.Header) fs.FileMode {
	return fs.FileMode(hdr.Mode&0o777) | 0o700 //nolint:gosec // the header's own bits, masked to permissions
}

// writeFile extracts one regular-file entry to target, creating its parent directory (a tar
// stream is not required to list a directory entry before a file inside it) and preserving the
// entry's own permission bits so an executable stays executable. It reports how many bytes it
// actually wrote, which is what the caller's budget is decremented by, rather than hdr.Size.
func writeFile(target string, r io.Reader, hdr *tar.Header) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return 0, err
	}
	mode := fs.FileMode(hdr.Mode & 0o777) //nolint:gosec // the tar header's own mode bits, masked to permissions only
	if mode == 0 {
		mode = 0o644
	}
	w, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(w, r)
	closeErr := w.Close()
	if copyErr != nil {
		return n, copyErr
	}
	return n, closeErr
}
