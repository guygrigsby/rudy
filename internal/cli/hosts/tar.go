package hosts

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
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
