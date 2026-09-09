package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// sockDir is a short directory to put test sockets in. A socket path is 104 bytes on darwin
// and t.TempDir() under /var/folders spends most of that before the test has named a file.
func sockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rudy-sock-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// rawListen binds a socket the way a server that knows nothing of the lock would, for the
// tests about what ListenUnix does when it finds one.
func rawListen(t *testing.T, path string) *net.UnixListener {
	t.Helper()
	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// staleSocket leaves a socket file with no server behind it, the way a server that was
// killed does.
func staleSocket(t *testing.T, path string) {
	t.Helper()
	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	l.SetUnlinkOnClose(false)
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// holdLock takes the socket's lock the way another server holds it.
func holdLock(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock: %v", err)
	}
}

func TestListenCreatesAPrivateDirAndSocket(t *testing.T) {
	// Umask 0 is the case the chmods exist for: MkdirAll and bind both take the umask off the
	// mode they are given, so the modes here are the harness's doing and not the shell's.
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	cases := []struct {
		name    string
		predate func(t *testing.T, dir string)
	}{
		{"a dir that does not exist yet", func(*testing.T, string) {}},
		// MkdirAll leaves an existing dir's mode alone, and a socket under a dir anyone can
		// enter is a socket anyone can reach.
		{"a dir that already exists wider", func(t *testing.T, dir string) {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatalf("chmod: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(sockDir(t), "run")
			path := filepath.Join(dir, "s.sock")
			tc.predate(t, dir)
			l, err := ListenUnix(path)
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			defer func() { _ = l.Close() }()

			di, err := os.Stat(dir)
			if err != nil {
				t.Fatalf("stat dir: %v", err)
			}
			if got := di.Mode().Perm(); got != 0o700 {
				t.Errorf("dir mode = %o, want 700", got)
			}
			si, err := os.Lstat(path)
			if err != nil {
				t.Fatalf("stat socket: %v", err)
			}
			if si.Mode()&os.ModeSocket == 0 {
				t.Errorf("%s is %v, want a socket", path, si.Mode())
			}
			if got := si.Mode().Perm(); got != 0o600 {
				t.Errorf("socket mode = %o, want 600", got)
			}
			li, err := os.Lstat(path + ".lock")
			if err != nil {
				t.Fatalf("stat lock: %v", err)
			}
			if got := li.Mode().Perm(); got != 0o600 {
				t.Errorf("lock mode = %o, want 600", got)
			}
			if got := l.Addr().String(); got != path {
				t.Errorf("addr = %q, want %q", got, path)
			}
		})
	}
}

func TestListenFixesTheLockFileMode(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		// The lock file outlives the server, so a mode set once under a hostile umask would
		// lock every later server out of its own socket for good.
		{"a umask that narrows the create", func(t *testing.T, path string) {
			old := syscall.Umask(0o277)
			t.Cleanup(func() { syscall.Umask(old) })
		}},
		{"a lock file left narrow by an older run", func(t *testing.T, path string) {
			if err := os.WriteFile(path+".lock", nil, 0o400); err != nil {
				t.Fatalf("write lock: %v", err)
			}
		}},
		{"a lock file left wide by an older run", func(t *testing.T, path string) {
			if err := os.WriteFile(path+".lock", nil, 0o666); err != nil {
				t.Fatalf("write lock: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(sockDir(t), "s.sock")
			tc.setup(t, path)
			l, err := ListenUnix(path)
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			defer func() { _ = l.Close() }()
			li, err := os.Lstat(path + ".lock")
			if err != nil {
				t.Fatalf("stat lock: %v", err)
			}
			if got := li.Mode().Perm(); got != 0o600 {
				t.Fatalf("lock mode = %o, want 600", got)
			}
		})
	}
}

func TestListenRefusesASymlinkedDir(t *testing.T) {
	base := sockDir(t)
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	l, err := ListenUnix(filepath.Join(link, "s.sock"))
	if err == nil {
		_ = l.Close()
		t.Fatal("listen bound a socket under a symlinked dir")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v, want it to name the symlink", err)
	}
	if _, err := os.Lstat(filepath.Join(real, "s.sock")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a socket was bound through the link: %v", err)
	}
}

func TestListenReplacesAStaleSocket(t *testing.T) {
	cases := []struct {
		name  string
		leave func(t *testing.T, path string)
	}{
		{"a plain file", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
		}},
		{"a socket nobody serves", staleSocket},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(sockDir(t), "s.sock")
			tc.leave(t, path)
			l, err := ListenUnix(path)
			if err != nil {
				t.Fatalf("listen over %s: %v", tc.name, err)
			}
			defer func() { _ = l.Close() }()
			si, err := os.Lstat(path)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if si.Mode()&os.ModeSocket == 0 {
				t.Fatalf("%s is %v, want a socket", path, si.Mode())
			}
		})
	}
}

func TestListenRefusesABusySocket(t *testing.T) {
	cases := []struct {
		name  string
		serve func(t *testing.T, path string)
	}{
		// The lock is the answer when the server on the path is one of ours.
		{"a server of ours holds the path", func(t *testing.T, path string) {
			l, err := ListenUnix(path)
			if err != nil {
				t.Fatalf("first listen: %v", err)
			}
			t.Cleanup(func() { _ = l.Close() })
		}},
		// The probe is the answer when it is not: an answering socket is a served socket
		// whoever bound it.
		{"a socket answers with no lock held", func(t *testing.T, path string) { rawListen(t, path) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(sockDir(t), "s.sock")
			tc.serve(t, path)
			second, err := ListenUnix(path)
			if err == nil {
				_ = second.Close()
				t.Fatal("second listen took a socket a server is on")
			}
			if !errors.Is(err, ErrSocketBusy) {
				t.Fatalf("err = %v, want ErrSocketBusy", err)
			}
			// The refusal leaves the first server's socket where it was.
			si, err := os.Lstat(path)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if si.Mode()&os.ModeSocket == 0 {
				t.Fatalf("%s is %v, want the first server's socket", path, si.Mode())
			}
		})
	}
}

func TestListenRefusesALockedPath(t *testing.T) {
	// Two servers starting at once find the same dead socket. Without the lock both remove it
	// and both bind, and the clients divide between them.
	path := filepath.Join(sockDir(t), "s.sock")
	staleSocket(t, path)
	holdLock(t, path)

	l, err := ListenUnix(path)
	if err == nil {
		_ = l.Close()
		t.Fatal("listen bound a path another server holds the lock on")
	}
	if !errors.Is(err, ErrSocketBusy) {
		t.Fatalf("err = %v, want ErrSocketBusy", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("the stale socket was removed under a lock we do not hold: %v", err)
	}
}

func TestListenKeepsAPathItCannotProbe(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root connects to a socket whatever its mode")
	}
	// A probe that neither answers nor refuses says nothing about whether a server is there,
	// and a path that might still be served is not one to delete.
	path := filepath.Join(sockDir(t), "s.sock")
	rawListen(t, path)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	l, err := ListenUnix(path)
	if err == nil {
		_ = l.Close()
		t.Fatal("listen took a path it could not probe")
	}
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("err = %v, want the probe's own EACCES", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("the socket was removed on a probe that proved nothing: %v", err)
	}
}

func TestDialReportsNoServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dir := sockDir(t)

	t.Run("nothing at the path", func(t *testing.T) {
		c, err := DialUnix(ctx, filepath.Join(dir, "absent.sock"), time.Second)
		if err == nil {
			_ = c.Close()
			t.Fatal("dial to an absent path returned a conn")
		}
		if !errors.Is(err, ErrNoServer) {
			t.Fatalf("err = %v, want ErrNoServer", err)
		}
	})

	t.Run("a socket that refuses", func(t *testing.T) {
		path := filepath.Join(dir, "stale.sock")
		staleSocket(t, path)
		c, err := DialUnix(ctx, path, time.Second)
		if err == nil {
			_ = c.Close()
			t.Fatal("dial to a stale socket returned a conn")
		}
		if !errors.Is(err, ErrNoServer) {
			t.Fatalf("err = %v, want ErrNoServer", err)
		}
	})
}

func TestAcceptAdmitsTheOwner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := filepath.Join(sockDir(t), "s.sock")
	l, err := ListenUnix(path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()

	type dialed struct {
		conn Conn
		err  error
	}
	dials := make(chan dialed, 1)
	go func() {
		c, err := DialUnix(ctx, path, time.Second)
		dials <- dialed{conn: c, err: err}
	}()

	server, err := l.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer func() { _ = server.Close() }()
	d := <-dials
	if d.err != nil {
		t.Fatalf("dial: %v", d.err)
	}
	defer func() { _ = d.conn.Close() }()

	if err := d.conn.Send(ctx, Request{JSONRPC: Version, ID: json.RawMessage(`1`), Method: "ping"}); err != nil {
		t.Fatalf("client send: %v", err)
	}
	raw, err := server.Recv(ctx)
	if err != nil {
		t.Fatalf("server recv: %v", err)
	}
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if req.Method != "ping" {
		t.Fatalf("method = %q, want ping", req.Method)
	}

	if err := server.Send(ctx, Response{JSONRPC: Version, ID: req.ID, Result: json.RawMessage(`"pong"`)}); err != nil {
		t.Fatalf("server send: %v", err)
	}
	raw, err = d.conn.Recv(ctx)
	if err != nil {
		t.Fatalf("client recv: %v", err)
	}
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if string(resp.Result) != `"pong"` {
		t.Fatalf("result = %s, want \"pong\"", resp.Result)
	}
}

func TestPeerUIDIsTheCaller(t *testing.T) {
	path := filepath.Join(sockDir(t), "s.sock")
	l := rawListen(t, path)

	type dialed struct {
		conn net.Conn
		err  error
	}
	dials := make(chan dialed, 1)
	go func() {
		c, err := net.Dial("unix", path)
		dials <- dialed{conn: c, err: err}
	}()

	raw, err := l.AcceptUnix()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer func() { _ = raw.Close() }()
	d := <-dials
	if d.err != nil {
		t.Fatalf("dial: %v", d.err)
	}
	defer func() { _ = d.conn.Close() }()

	uid, err := PeerUID(raw)
	if err != nil {
		t.Fatalf("peer uid: %v", err)
	}
	if uid != os.Getuid() {
		t.Fatalf("peer uid = %d, want %d", uid, os.Getuid())
	}
}

func TestAcceptRefusesAnotherUID(t *testing.T) {
	cases := []struct {
		name  string
		check func(*net.UnixConn) (int, error)
		want  error
	}{
		{"a uid that is not ours", func(*net.UnixConn) (int, error) { return os.Getuid() + 1, nil }, ErrPeerRefused},
		{"a check that fails", func(*net.UnixConn) (int, error) { return 0, errors.New("getsockopt: broken") }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			path := filepath.Join(sockDir(t), "s.sock")
			l, err := ListenUnix(path)
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			defer func() { _ = l.Close() }()

			saved := peerUID
			peerUID = tc.check
			t.Cleanup(func() { peerUID = saved })

			type dialed struct {
				conn Conn
				err  error
			}
			dials := make(chan dialed, 1)
			go func() {
				c, err := DialUnix(ctx, path, time.Second)
				dials <- dialed{conn: c, err: err}
			}()

			conn, err := l.Accept()
			if err == nil {
				_ = conn.Close()
				t.Fatal("accept admitted the peer")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want ErrPeerRefused", err)
			}
			if tc.want == nil && errors.Is(err, ErrPeerRefused) {
				t.Fatalf("err = %v, want the check's own error", err)
			}

			d := <-dials
			if d.err != nil {
				t.Fatalf("dial: %v", d.err)
			}
			defer func() { _ = d.conn.Close() }()
			// The refusal closes the connection, so the client sees the end of it without
			// having sent or been sent a byte.
			if _, err := d.conn.Recv(ctx); !errors.Is(err, io.EOF) {
				t.Fatalf("client recv = %v, want io.EOF from a closed connection", err)
			}
		})
	}
}

func TestAcceptPassesUpAClosedListener(t *testing.T) {
	path := filepath.Join(sockDir(t), "s.sock")
	l, err := ListenUnix(path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("err = %v, want net.ErrClosed so the accept loop ends", err)
	}
}

func TestTheListenerHoldsItsLockUntilClose(t *testing.T) {
	path := filepath.Join(sockDir(t), "s.sock")
	l, err := ListenUnix(path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// A second handle on the same lock file, taken while the listener is alive, is what every
	// other server gets: the file is never removed, so the lock is always on this one inode.
	f, err := os.OpenFile(path+".lock", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Fatal("the lock was free while the listener was serving")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("close left the lock held: %v", err)
	}
	if _, err := os.Lstat(path + ".lock"); err != nil {
		t.Fatalf("lock file after close: %v, want it kept so the next server locks this inode", err)
	}
}

func TestCloseRemovesTheSocketAndKeepsTheLockFile(t *testing.T) {
	path := filepath.Join(sockDir(t), "s.sock")
	l, err := ListenUnix(path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("socket after close: %v, want it gone", err)
	}
	if _, err := os.Lstat(path + ".lock"); err != nil {
		t.Errorf("lock file after close: %v, want it kept", err)
	}
	if err := l.Close(); err != nil {
		t.Errorf("second close: %v, want nothing to do", err)
	}
	// The lock came off with the socket, so the path is there to be taken again, and the next
	// server takes it on the lock file this one left behind.
	again, err := ListenUnix(path)
	if err != nil {
		t.Fatalf("listen again: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatalf("close again: %v", err)
	}
}
