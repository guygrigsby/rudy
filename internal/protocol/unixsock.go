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

// ErrSocketNotOurs means the socket, or the directory holding it, is somebody else's: owned
// by another uid, reached through a symlink, or sitting in a directory group or other can
// write. On linux with XDG_RUNTIME_DIR unset the default lives under /tmp, which every local
// user can write, so a socket answering at the expected path is not by itself evidence that
// the process behind it is the user's own. This is the client's half of the same trust
// boundary ErrPeerRefused is the server's half of.
var ErrSocketNotOurs = errors.New("protocol: the socket is not ours")

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
	// lockMode is set on the lock file at every open, not just the one that creates it.
	// Nothing reads its contents; the lock is the file's open description, not anything
	// written in it, and the file outlives the server, so a mode a stale umask left behind
	// would outlive it too.
	lockMode = 0o600
)

// peerUID is the credential check Listener.Accept runs. It is a variable so one test can
// refuse a connection without arranging a second user to connect as. The swap is
// package-wide while it stands, which is why no test in this package may call t.Parallel.
var peerUID = PeerUID

// statUID is the stat-to-uid step the ownership checks run, a variable for the same reason
// peerUID is one: a test cannot arrange a second user's file, and the branch that refuses one
// is the branch worth having a test for.
var statUID = fileUID

// fileUID is the owner of a stat the caller already took. The syscall.Stat_t is the only
// place the uid is, and every unix this builds for has it.
func fileUID(fi os.FileInfo) (int, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("protocol: %s: stat carries no uid (%T)", fi.Name(), fi.Sys())
	}
	return int(st.Uid), nil
}

// CheckSocketOwner refuses a socket path that is not this user's before anything connects to
// it. The server checks its peer's uid; without this the client checks nothing, and a socket
// under a directory another local user could create first (linux, XDG_RUNTIME_DIR unset,
// /tmp world-writable, the path a bare uid away from guessable) would take every rudy run on
// the machine into that user's process, prompts, tool calls and all.
//
// Both the socket and its directory have to be this uid's, neither may be a symlink, and the
// directory may not be writable by group or other. Nothing here follows a link: a check that
// resolved one would be checking the target while the connect went through the link. An
// absent path is ErrNoServer, not a refusal: nobody there is the case where starting a server
// is the right answer.
func CheckSocketOwner(path string) error {
	if err := checkSocketDir(filepath.Dir(path)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s: %w", ErrNoServer, path, err)
		}
		return err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s: %w", ErrNoServer, path, err)
		}
		return fmt.Errorf("protocol: socket %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symlink", ErrSocketNotOurs, path)
	}
	uid, err := statUID(fi)
	if err != nil {
		return err
	}
	if uid != os.Getuid() {
		return fmt.Errorf("%w: %s is owned by uid %d, not %d", ErrSocketNotOurs, path, uid, os.Getuid())
	}
	return nil
}

// checkSocketDir is the predicate both halves of the trust boundary apply to the directory a
// socket lives in: this uid's, not a symlink, and closed to group and other writes. A
// directory anyone else can write is one they can put a socket in, whoever owns the one
// there now.
func checkSocketDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("protocol: socket dir %s: %w", dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: socket dir %s is a symlink", ErrSocketNotOurs, dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: socket dir %s is not a directory", ErrSocketNotOurs, dir)
	}
	uid, err := statUID(fi)
	if err != nil {
		return err
	}
	if uid != os.Getuid() {
		return fmt.Errorf("%w: socket dir %s is owned by uid %d, not %d", ErrSocketNotOurs, dir, uid, os.Getuid())
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("%w: socket dir %s is mode %04o, writable by group or other", ErrSocketNotOurs, dir, perm)
	}
	return nil
}

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

// ListenUnix listens on path, creating its directory 0700 and the socket 0600. A directory
// that was already there is checked rather than narrowed, and refused with ErrSocketNotOurs
// when it is another uid's or open to a group or other write. A socket left behind by a
// server that died is removed; a path another server holds, by its lock or by answering a
// probe, is refused with ErrSocketBusy.
func ListenUnix(path string) (*Listener, error) {
	if err := prepareSocketDir(filepath.Dir(path)); err != nil {
		return nil, err
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

// prepareSocketDir leaves dir fit to bind a socket in: created 0700, or vouched for if it was
// already there. Only a directory this call made is chmod'd. A chmod on any directory the
// caller named is a chmod on a directory that is not the socket's to narrow: `rudy serve
// --socket ~/rudy.sock` would take $HOME to 0700, and a root `--socket /tmp/x.sock` would
// take /tmp with it, breaking every other program on the machine that expected to write
// there. A directory that already existed is checked against the same predicate the client
// applies before it attaches, so neither half of the trust boundary is the loose one.
//
// The leaf is created with Mkdir rather than MkdirAll so "we made it" is the syscall's answer
// and not a stat that another process could have raced.
func prepareSocketDir(dir string) error {
	if parent := filepath.Dir(dir); parent != dir {
		if err := os.MkdirAll(parent, socketDirMode); err != nil {
			return fmt.Errorf("protocol: socket dir %s: %w", parent, err)
		}
	}
	switch err := os.Mkdir(dir, socketDirMode); {
	case err == nil:
		// Mkdir takes the umask off the mode it is given, so the mode is set outright rather
		// than left to whatever the shell that started this process was carrying. Nothing else
		// can be in the directory: it did not exist a syscall ago.
		if err := os.Chmod(dir, socketDirMode); err != nil {
			return fmt.Errorf("protocol: socket dir %s: %w", dir, err)
		}
		return nil
	case errors.Is(err, fs.ErrExist):
		return checkSocketDir(dir)
	default:
		return fmt.Errorf("protocol: socket dir %s: %w", dir, err)
	}
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
	// O_RDONLY is all a flock asks for, so the widest set of existing modes can still be
	// opened, and the mode is set afterwards rather than left to the umask: this file is
	// never removed, so a umask of 0200 on the run that created it would otherwise leave a
	// 0400 file that every later server fails to open, for good.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, lockMode)
	if err != nil {
		return nil, fmt.Errorf("protocol: socket lock %s: %w", path, err)
	}
	if err := os.Chmod(path, lockMode); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("protocol: socket lock mode %s: %w", path, err)
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
// returned Conn goes on to do. An absent path, a refused connection or a path that is not a
// socket at all is ErrNoServer, which is how a probing client tells nobody there from a
// socket it cannot use: a plain file or a directory where the socket goes is a leftover, not
// a server, and the caller starting its own is the right answer to all three.
func DialUnix(ctx context.Context, path string, timeout time.Duration) (Conn, error) {
	d := net.Dialer{Timeout: timeout}
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOTSOCK) {
			return nil, fmt.Errorf("%w: %s: %w", ErrNoServer, path, err)
		}
		return nil, fmt.Errorf("protocol: dial %s: %w", path, err)
	}
	return NewStreamConn(c, c, c), nil
}
