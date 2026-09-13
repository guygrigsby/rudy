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

// checkArchive reads the one thing that says an archive is one: the ustar magic at offset 257
// of the first header block. Inspect's reason, on a stream nobody can read by eye: what comes
// back from a host line is a shell's stdout, and a box that greets every ssh command writes
// into this one too. A greeting handed to tar fails naming neither the box nor the greeting,
// so it is named here instead.
func checkArchive(archive, placement string) error {
	const magic, at = "ustar", 257
	if len(archive) >= at+len(magic) && archive[at:at+len(magic)] == magic {
		return nil
	}
	first, _, _ := strings.Cut(strings.TrimSpace(archive), "\n")
	if len(first) > 80 {
		first = first[:80]
	}
	return fmt.Errorf("copying %s back: the host answered %q, which is not a tar archive; a shell that greets every ssh command writes into this stream and rudy cannot read past it", placement, first)
}

// extractTar unpacks over dir an archive the host wrote. The archive is a value rather than a
// stream because a Runner answers with its output: what comes back this way is a tree no git
// tracks, small enough that the box holds a copy of it at all, and streaming it would mean a
// second shape of Run for the one caller that reads bytes rather than words.
func extractTar(ctx context.Context, dir, archive string) error {
	cmd := exec.CommandContext(ctx, "tar", "-x", "-C", dir, "-f", "-")
	cmd.Stdin = strings.NewReader(archive)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("unpacking into %s: tar: %v: %s", dir, err, strings.TrimSpace(errb.String()))
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
