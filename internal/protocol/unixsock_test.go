package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
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

func TestListenCreatesAPrivateDirAndSocket(t *testing.T) {
	// Umask 0 is the case the two chmods exist for: MkdirAll and bind both take the umask off
	// the mode they are given, so the modes here are the harness's doing and not the shell's.
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
		})
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
	path := filepath.Join(sockDir(t), "s.sock")
	first, err := ListenUnix(path)
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	defer func() { _ = first.Close() }()

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

func TestAcceptPeerAdmitsTheOwner(t *testing.T) {
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

	server, err := AcceptPeer(l)
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
	l, err := ListenUnix(path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()

	type dialed struct {
		conn net.Conn
		err  error
	}
	dials := make(chan dialed, 1)
	go func() {
		c, err := net.Dial("unix", path)
		dials <- dialed{conn: c, err: err}
	}()

	raw, err := l.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer func() { _ = raw.Close() }()
	d := <-dials
	if d.err != nil {
		t.Fatalf("dial: %v", d.err)
	}
	defer func() { _ = d.conn.Close() }()
	uc, ok := raw.(*net.UnixConn)
	if !ok {
		t.Fatalf("accepted a %T, want *net.UnixConn", raw)
	}
	uid, err := PeerUID(uc)
	if err != nil {
		t.Fatalf("peer uid: %v", err)
	}
	if uid != os.Getuid() {
		t.Fatalf("peer uid = %d, want %d", uid, os.Getuid())
	}
}

func TestAcceptPeerRefusesAnotherUID(t *testing.T) {
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

			conn, err := AcceptPeer(l)
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

func TestAcceptPeerPassesUpAClosedListener(t *testing.T) {
	path := filepath.Join(sockDir(t), "s.sock")
	l, err := ListenUnix(path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := AcceptPeer(l); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("err = %v, want net.ErrClosed so the accept loop ends", err)
	}
}

// oneConnListener hands AcceptPeer a connection that is not a unix socket, which is the one
// way its peer check has nothing to ask the kernel about.
type oneConnListener struct{ c net.Conn }

func (o oneConnListener) Accept() (net.Conn, error) { return o.c, nil }
func (o oneConnListener) Close() error              { return nil }
func (o oneConnListener) Addr() net.Addr            { return &net.UnixAddr{Name: "test", Net: "unix"} }

func TestAcceptPeerRefusesANonUnixConn(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = client.Close() }()
	conn, err := AcceptPeer(oneConnListener{c: server})
	if err == nil {
		_ = conn.Close()
		t.Fatal("accept admitted a connection with no peer credentials")
	}
	// The connection is closed, not left open unchecked.
	if err := server.SetReadDeadline(time.Now().Add(time.Second)); err == nil {
		t.Fatal("the connection was left open")
	}
}
