package protocol

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrSocketBusy means the path already has a server on it: a probe dial was answered, so
// binding would either fail or split one client population across two servers.
var ErrSocketBusy = errors.New("protocol: a server is already serving this socket")

// ErrPeerRefused means the process on the other end runs as another user. The socket lives
// in a 0700 directory, so reaching it at all takes either root or a hole in the filesystem,
// and neither is a peer the server talks to.
var ErrPeerRefused = errors.New("protocol: peer uid is not the server's")

// ErrNoServer means nothing was listening: the path is absent or the socket refused the
// connection. A probe needs this separate from a real failure, since "nobody there" is the
// case where starting a server is the right answer.
var ErrNoServer = errors.New("protocol: no server on the socket")

// staleProbeTimeout bounds the dial ListenUnix makes before it takes a path over. A local
// connect is answered by the kernel the moment a listener is bound, so a slow one means a
// server whose backlog is full, which is a server all the same.
const staleProbeTimeout = 50 * time.Millisecond

const (
	// socketDirMode keeps the whole directory to the owner, which is what makes the peer uid
	// check a second line of defense rather than the only one.
	socketDirMode = 0o700
	// socketMode is set after bind: the bind itself takes the umask off 0777.
	socketMode = 0o600
)

// peerUID is the credential check AcceptPeer runs. It is a variable so a test can refuse a
// connection without arranging a second user to connect as.
var peerUID = PeerUID

// ListenUnix listens on path, creating its directory 0700 and the socket 0600. A socket
// left behind by a server that died is removed; one with a server still on it is refused
// with ErrSocketBusy.
func ListenUnix(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, socketDirMode); err != nil {
		return nil, fmt.Errorf("protocol: socket dir %s: %w", dir, err)
	}
	// MkdirAll takes the umask off the mode it is given and leaves an existing directory's
	// mode alone, so neither case is private without saying so outright.
	if err := os.Chmod(dir, socketDirMode); err != nil {
		return nil, fmt.Errorf("protocol: socket dir %s: %w", dir, err)
	}
	if err := clearSocketPath(path); err != nil {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("protocol: listen %s: %w", path, err)
	}
	if err := os.Chmod(path, socketMode); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("protocol: socket mode %s: %w", path, err)
	}
	return l, nil
}

// clearSocketPath leaves path free to bind. Whatever sits there is removed only once a
// probe dial proves nothing is serving it: an unbound path answers with ECONNREFUSED, a
// path that is not a socket with ENOTSOCK. Any other failure is not evidence the server is
// gone, so it is reported rather than acted on.
func clearSocketPath(path string) error {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("protocol: socket %s: %w", path, err)
	}
	c, err := net.DialTimeout("unix", path, staleProbeTimeout)
	if err == nil {
		_ = c.Close()
		return fmt.Errorf("%w: %s", ErrSocketBusy, path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, syscall.ENOTSOCK) && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("protocol: probe %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("protocol: remove stale socket %s: %w", path, err)
	}
	return nil
}

// AcceptPeer accepts one connection and returns it as a Conn once the kernel says the peer
// runs as this process's user. A peer that does not is closed before a byte is read and
// reported as ErrPeerRefused, which the caller logs before accepting again. Accept's own
// errors come back unchanged, so a closed listener ends the caller's loop.
func AcceptPeer(l net.Listener) (Conn, error) {
	c, err := l.Accept()
	if err != nil {
		return nil, err
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		_ = c.Close()
		return nil, fmt.Errorf("protocol: accepted a %T, want a unix connection", c)
	}
	uid, err := peerUID(uc)
	if err != nil {
		_ = uc.Close()
		return nil, err
	}
	if uid != os.Getuid() {
		_ = uc.Close()
		return nil, fmt.Errorf("%w: peer uid %d, server uid %d", ErrPeerRefused, uid, os.Getuid())
	}
	return NewStreamConn(uc, uc, uc), nil
}

// DialUnix connects to a server on path. timeout bounds the connect alone, not what the
// returned Conn goes on to do. An absent path or a refused connection is ErrNoServer, which
// is how a probing client tells nobody there from a socket it cannot use.
func DialUnix(ctx context.Context, path string, timeout time.Duration) (Conn, error) {
	d := net.Dialer{Timeout: timeout}
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("%w: %s: %w", ErrNoServer, path, err)
		}
		return nil, fmt.Errorf("protocol: dial %s: %w", path, err)
	}
	return NewStreamConn(c, c, c), nil
}
