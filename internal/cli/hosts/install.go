// SPDX-License-Identifier: AGPL-3.0-or-later

package hosts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/guygrigsby/rudy/internal/protocol"
)

// installTailLines is how much of a failed build comes back with the error. Enough for the
// compiler's own complaint and the recipe that produced it, short enough that an operator
// reading it through ssh still sees the error at the bottom.
const installTailLines = 20

// InstallLine is the one shell line that puts this client's rudy on the box: the checkout
// under remote.source fetched, detached at the revision this binary was built from, and
// built. remotePath is prepended for the same reason the bridge's line has it: ssh runs a
// non-interactive shell with the compiled-in PATH, and a box's go and make are usually under
// its own home.
//
// --detach rather than a branch: the checkout on the box is a build directory and not
// somebody's work, and a detached head is the honest shape for "whatever the Mac is running".
// The fetch is what makes a revision this machine has just pushed reachable there; it does not
// clone, so a box with no checkout at remote.source is a failure that names the directory
// rather than a repository rudy picked.
func InstallLine(source, rev string) string {
	return remotePath + "; cd " + hostPath(source) + " && git fetch -q && git checkout -q --detach " + quote(rev) + " && make install"
}

// hostPath is one of the operator's paths inside a host line. Quoted like every other value,
// except a leading ~, which goes over bare so the box's shell expands it: remote.source names
// a directory on the box, and this machine's home is not the box's home. Everything after the
// first slash is quoted, so a tilde path with a space or a quote in it is still one word.
func hostPath(p string) string {
	switch {
	case p == "~":
		return "~"
	case strings.HasPrefix(p, "~/"):
		return "~/" + quote(p[2:])
	}
	return quote(p)
}

// Install builds rudy on the host from its own checkout, at rev.
//
// The box's output is streamed to out as it arrives rather than collected and printed at the
// end: a make install is a go build, which is a minute of silence on a cold module cache, and
// an operator waiting on a machine they cannot see needs to know it is working. stdout and
// stderr are merged on the box, before ssh, so the build reads in the order it happened;
// Runner.Stream hands stderr back only once the line has exited, which would put the
// compiler's complaint underneath the recipe that produced it.
func Install(ctx context.Context, r Runner, source, rev string, out io.Writer) error {
	if source == "" {
		return errors.New("no rudy checkout on the host: set remote.source to the path of one")
	}
	stream, wait, err := r.Stream(ctx, "{ "+InstallLine(source, rev)+"; } 2>&1", nil)
	if err != nil {
		return fmt.Errorf("installing rudy %s on the host: %w", rev, err)
	}
	// The tail is kept whatever the caller does with the stream, because the caller's writer is
	// a terminal that has already scrolled by the time the error is written.
	tail := protocol.NewTail(protocol.TailBytes)
	dst := io.Writer(tail)
	if out != nil {
		dst = io.MultiWriter(out, tail)
	}
	_, copyErr := io.Copy(dst, stream)
	_ = stream.Close()
	errText, code, waitErr := wait()
	switch {
	case waitErr != nil:
		return fmt.Errorf("installing rudy %s on the host: %w", rev, waitErr)
	case code != 0:
		// source unquoted here: this is a sentence for a person to read and retype, not a line
		// for a shell.
		return fmt.Errorf("installing rudy %s on the host: exit %d\n%s\non the host, cd %s and run make install to see the whole build",
			rev, code, lastLines(tail.String()+errText, installTailLines), source)
	case copyErr != nil:
		return fmt.Errorf("reading the install on the host: %w", copyErr)
	}
	return nil
}

// lastLines is the end of the box's output, whole lines only: the tail is bounded in bytes, so
// its first line is usually half a line, and a half line of a compiler error is worse than
// none.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
