package protocol

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// ErrSocketBusy means the path already has a server on it: the lock beside the socket is
// held, or a probe dial was answered. Binding anyway would split one client population
// across two servers.
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
	// lockMode is the mode the lock file is created with. Nothing reads its contents; the
	// lock is the file's open description, not anything written in it.
	lockMode = 0o600
)

// peerUID is the credential check Listener.Accept runs. It is a variable so one test can
// refuse a connection without arranging a second user to connect as. The swap is
// package-wide while it stands, which is why no test in this package may call t.Parallel.
var peerUID = PeerUID

// Listener is a server's unix socket. It holds an exclusive lock beside the socket for as
// long as it lives, so a second server starting at the same moment cannot decide the socket
// is stale and take the path out from under this one, and its Accept runs the peer uid check
// on every connection so no caller can skip it.
type Listener struct {
	ln   *net.UnixListener
	lock *os.File
	path string

	closeOnce sync.Once
	closeErr  error
}

// ListenUnix listens on path, creating its directory 0700 and the socket 0600. A socket left
// behind by a server that died is removed; a path another server holds, by its lock or by
// answering a probe, is refused with ErrSocketBusy.
func ListenUnix(path string) (*Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, socketDirMode); err != nil {
		return nil, fmt.Errorf("protocol: socket dir %s: %w", dir, err)
	}
	// A symlinked directory is somebody else's decision about where the socket lives: the
	// 0700 below would land on whatever it points at, and the socket with it.
	di, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("protocol: socket dir %s: %w", dir, err)
	}
	if di.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("protocol: socket dir %s is a symlink", dir)
	}
	// MkdirAll takes the umask off the mode it is given and leaves an existing directory's
	// mode alone, so neither case is private without saying so outright.
	if err := os.Chmod(dir, socketDirMode); err != nil {
		return nil, fmt.Errorf("protocol: socket dir %s: %w", dir, err)
	}

	lock, err := takeLock(lockPath(path))
	if err != nil {
		return nil, err
	}
	ln, err := bindLocked(path)
	if err != nil {
		// Releasing the lock leaves the path as the next server finds it.
		_ = lock.Close()
		return nil, err
	}
	return &Listener{ln: ln, lock: lock, path: path}, nil
}

// lockPath is the lock that goes with a socket, in the same 0700 directory. The file is
// created once and never removed, not even by Close: a flock is held on an inode, not on a
// name, so a lock file that comes and goes is a lock two servers can hold at once. One
// server opening the file just before another unlinks it would take the lock on the inode
// nobody can reach any more, while a third creates a fresh file and takes that one. An empty
// 0600 file for the life of the installation is the price of the lock meaning one thing.
func lockPath(path string) string { return path + ".lock" }

// takeLock takes the socket's lock without waiting. The lock, not the socket file, is what
// makes taking a stale socket over safe: two servers starting at once both find the same
// dead socket, and only the one holding the lock gets as far as removing it.
func takeLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, lockMode)
	if err != nil {
		return nil, fmt.Errorf("protocol: socket lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s is locked", ErrSocketBusy, path)
		}
		return nil, fmt.Errorf("protocol: socket lock %s: %w", path, err)
	}
	return f, nil
}

// bindLocked clears the path and binds it. The caller holds the lock, so nothing else is
// deciding about this path at the same time.
func bindLocked(path string) (*net.UnixListener, error) {
	if err := clearSocketPath(path); err != nil {
		return nil, err
	}
	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		return nil, fmt.Errorf("protocol: socket %s: %w", path, err)
	}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		return nil, fmt.Errorf("protocol: listen %s: %w", path, err)
	}
	// Close removes the socket itself, in one step with the lock file.
	ln.SetUnlinkOnClose(false)
	if err := os.Chmod(path, socketMode); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("protocol: socket mode %s: %w", path, err)
	}
	return ln, nil
}

// clearSocketPath leaves path free to bind. Whatever sits there is removed only once a probe
// dial proves nothing is serving it: an unbound path answers with ECONNREFUSED, a path that
// is not a socket with ENOTSOCK. Any other failure is not evidence the server is gone, so it
// is reported rather than acted on.
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

// Accept returns the next connection whose peer runs as this process's user. A peer that
// does not is closed before a byte is read and reported as ErrPeerRefused, which the caller
// logs before accepting again. The listener's own errors come back unchanged, so a closed
// Listener ends the caller's loop with net.ErrClosed.
func (l *Listener) Accept() (Conn, error) {
	c, err := l.ln.AcceptUnix()
	if err != nil {
		return nil, err
	}
	uid, err := peerUID(c)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	if uid != os.Getuid() {
		_ = c.Close()
		return nil, fmt.Errorf("%w: peer uid %d, server uid %d", ErrPeerRefused, uid, os.Getuid())
	}
	return NewStreamConn(c, c, c), nil
}

// Addr is the address the listener is bound to, for logging.
func (l *Listener) Addr() net.Addr { return l.ln.Addr() }

// Close stops accepting, removes the socket and then gives up the lock, in that order: the
// next server to take the lock finds the path already clear. The lock file itself stays; see
// lockPath for why removing it would be the one way to let two servers hold the lock.
func (l *Listener) Close() error {
	l.closeOnce.Do(func() {
		err := l.ln.Close()
		err = errors.Join(err, remove(l.path))
		// Closing the file is what releases the flock.
		l.closeErr = errors.Join(err, l.lock.Close())
	})
	return l.closeErr
}

// remove deletes a file the caller expected to be there, and says nothing about one that was
// already gone.
func remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("protocol: remove %s: %w", path, err)
	}
	return nil
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
