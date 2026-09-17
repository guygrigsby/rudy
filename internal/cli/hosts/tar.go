// SPDX-License-Identifier: AGPL-3.0-or-later

package hosts

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/guygrigsby/rudy/internal/tarx"
)

// Moving files is ssh plus tar, sand's transport: one round trip per direction, nothing
// installed on the box, and both ends have a tar that takes -C, -f - and a list of names on
// stdin (bsdtar on the Mac, GNU tar on the box).

// tarOf is tar reading the names to archive on its stdin and writing the archive to its
// stdout. The names are given rather than a directory to walk so the caller decides what
// travels (git's idea of what changed, or the whole tree) and knows the count without a
// second walk. NUL separated for the same reason git's -z is: a path may hold a newline.
func tarOf(ctx context.Context, dir string, names []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "tar", "-c", "-C", dir, "--null", "-T", "-", "-f", "-")
	cmd.Stdin = strings.NewReader(strings.Join(names, "\x00") + "\x00")
	return cmd
}

// sendTar runs tar here and feeds its archive to line's stdin on the host: a stream on both
// ends, so a tree bigger than memory is not a problem and nothing is written to a temporary
// file on either machine.
func sendTar(ctx context.Context, r Runner, what, line string, tar *exec.Cmd) error {
	var tarErr bytes.Buffer
	tar.Stderr = &tarErr
	archive, err := tar.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if err := tar.Start(); err != nil {
		return fmt.Errorf("%s: tar: %w", what, err)
	}
	_, runErr := run(ctx, r, what, line, archive)
	// Reaped whatever happened, and after the host line has stopped reading: a line that
	// failed early leaves tar writing into a pipe nobody holds, so its own error is EPIPE
	// and the host's is the one that says why.
	waitErr := tar.Wait()
	if runErr != nil {
		return runErr
	}
	if waitErr != nil {
		return fmt.Errorf("%s: tar: %v: %s", what, waitErr, strings.TrimSpace(tarErr.String()))
	}
	return nil
}

// maxPullEntries and maxPullBytes bound what a host can make this client write when a copied
// tree comes home. Generous next to a working tree and mean next to a box that has gone wrong:
// the caps are here so a hostile or broken host cannot fill the operator's disk, not to say how
// large anybody's project may be. tarx's own defaults are a plugin bundle's, far too small for
// a tree somebody works in.
const (
	maxPullEntries = 200_000
	maxPullBytes   = 2 << 30
)

// unpackInto stages the host's archive inside the local tree and moves it in only once the
// whole thing has been read and accepted. Unpacking straight over the operator's files would
// mean a refused entry halfway through an archive had already written the half before it, and
// the archive is bytes a box produced: what it names is not this client's to trust. The stage
// is inside the root so every move is a rename within one directory tree, which is what lets
// the move go through os.Root and never leave it; it is removed on every path out of here.
func unpackInto(localRoot, placement string, stream io.Reader) (int, error) {
	buf := bufio.NewReader(stream)
	if err := checkArchive(buf, placement); err != nil {
		return 0, err
	}
	stage, err := os.MkdirTemp(localRoot, ".rudy-pull-")
	if err != nil {
		return 0, fmt.Errorf("staging %s inside %s: %w", placement, localRoot, err)
	}
	defer func() { _ = os.RemoveAll(stage) }()
	opts := tarx.Options{Entries: maxPullEntries, Bytes: maxPullBytes}
	if err := tarx.Unpack(tar.NewReader(buf), stage, opts); err != nil {
		return 0, fmt.Errorf("copying %s back: %w; nothing was written to %s", placement, err, localRoot)
	}
	return moveInto(localRoot, stage)
}

// staged is one entry of the stage on its way into the tree: where it goes relative to the
// root, whether it is a directory, and the mode it arrived with, which is the archive's own.
type staged struct {
	rel  string
	dir  bool
	mode fs.FileMode
}

// moveInto moves a staged tree into the operator's own, entry by entry: the pull overlays
// rather than replaces, since a file the box deleted stays here and a file this machine has
// that the box never saw is not the box's to remove. An existing file is replaced by the
// rename, which is atomic per file.
//
// Every write goes through os.Root, and that is the security property of this leg rather than
// a tidiness: the plain os calls resolve every path component, so a symlink already sitting in
// the operator's tree (sub -> /etc) turns an archive entry named sub/passwd into a write to
// /etc/passwd, without the archive ever carrying a link for tarx to refuse. Root.MkdirAll and
// Root.Rename refuse to traverse a symlink at all. The pass below it refuses first and names
// what it found, so the whole-archive-or-nothing property survives: nothing moves until every
// destination has been looked at.
func moveInto(localRoot, stage string) (int, error) {
	root, err := os.OpenRoot(localRoot)
	if err != nil {
		return 0, fmt.Errorf("opening %s: %w", localRoot, err)
	}
	defer func() { _ = root.Close() }()

	var entries []staged
	err = filepath.WalkDir(stage, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(stage, p)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entries = append(entries, staged{rel: rel, dir: d.IsDir(), mode: info.Mode().Perm()})
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("reading what %s sent: %w", localRoot, err)
	}
	for _, e := range entries {
		if err := checkPlace(root, e); err != nil {
			return 0, fmt.Errorf("copying back into %s: %w; nothing was moved", localRoot, err)
		}
	}

	stageRel := filepath.Base(stage)
	files := 0
	for _, e := range entries {
		if e.dir {
			if err := root.MkdirAll(e.rel, e.mode); err != nil {
				return files, fmt.Errorf("moving the host's files into %s: %w", localRoot, err)
			}
			continue
		}
		if dir := filepath.Dir(e.rel); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return files, fmt.Errorf("moving the host's files into %s: %w", localRoot, err)
			}
		}
		if err := root.Rename(filepath.Join(stageRel, e.rel), e.rel); err != nil {
			return files, fmt.Errorf("moving the host's files into %s: %w", localRoot, err)
		}
		files++
	}
	return files, nil
}

// checkPlace looks at where one staged entry is going, before anything moves. A symlink
// anywhere along the path is a refusal rather than something to follow: it is the operator's
// own link, and the host's archive is not what decides to write through it. So is an existing
// entry of the other kind, a directory where the archive has a file or the reverse, which no
// rename can do anyway and which would otherwise fail halfway through the move.
func checkPlace(root *os.Root, e staged) error {
	parts := strings.Split(filepath.ToSlash(e.rel), "/")
	for i := range parts {
		prefix := filepath.Join(parts[:i+1]...)
		fi, err := root.Lstat(prefix)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// Nothing here, so nothing below it either: the move makes the rest.
			return nil
		case err != nil:
			return fmt.Errorf("%s: %w", prefix, err)
		case fi.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s is a symlink here, and the host's %s is not written through it", prefix, e.rel)
		case i < len(parts)-1 && !fi.IsDir():
			return fmt.Errorf("%s is a file here and a directory on the host, which holds %s", prefix, e.rel)
		case i == len(parts)-1 && fi.IsDir() != e.dir:
			return fmt.Errorf("%s is a %s here and a %s on the host", prefix, kind(fi.IsDir()), kind(e.dir))
		}
	}
	return nil
}

func kind(dir bool) string {
	if dir {
		return "directory"
	}
	return "file"
}

// checkArchive reads the one thing that says an archive is one: the ustar magic at offset 257
// of the first header block, peeked without taking it off the stream. Inspect's reason, on
// bytes nobody can read by eye: what comes back from a host line is a shell's stdout, and a box
// that greets every ssh command writes into this one too. A greeting handed to the unpacker
// fails naming neither the box nor the greeting, so it is named here instead.
func checkArchive(buf *bufio.Reader, placement string) error {
	const magic, at = "ustar", 257
	head, err := buf.Peek(at + len(magic))
	if err == nil && string(head[at:]) == magic {
		return nil
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(head)), "\n")
	if len(first) > 80 {
		first = first[:80]
	}
	return fmt.Errorf("copying %s back: the host answered %q, which is not a tar archive; a shell that greets every ssh command writes into this stream and rudy cannot read past it", placement, first)
}

// treeFiles is every file under root, as paths relative to it. Symlinks are names, not the
// things they point at: WalkDir does not follow them and neither does tar, so a link arrives
// as a link.
func treeFiles(root string) ([]string, error) {
	var names []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		names = append(names, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", root, err)
	}
	return names, nil
}
