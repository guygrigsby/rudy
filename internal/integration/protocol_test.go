// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// daemon is a running rudy serve and the socket it listens on.
type daemon struct {
	socket string
	cmd    *exec.Cmd
	out    *strings.Builder
	mu     sync.Mutex
}

// startDaemon runs rudy serve against the fake provider and waits for its socket.
func startDaemon(t *testing.T, h *home) *daemon {
	t.Helper()
	var runtimeDir string
	for _, kv := range h.env {
		if strings.HasPrefix(kv, "XDG_RUNTIME_DIR=") {
			runtimeDir = strings.TrimPrefix(kv, "XDG_RUNTIME_DIR=")
		}
	}
	d := &daemon{socket: filepath.Join(runtimeDir, "rudy", "rudy.sock"), out: &strings.Builder{}}
	d.cmd = exec.Command(rudyBin(t), "serve")
	d.cmd.Env = h.env
	d.cmd.Dir = h.root
	d.cmd.Stdout, d.cmd.Stderr = d.out, d.out
	if err := d.cmd.Start(); err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() {
		_ = d.cmd.Process.Kill()
		_, _ = d.cmd.Process.Wait()
	})
	c := dialSocket(t, d.socket, 30*time.Second)
	_ = c.Close()
	return d
}

// alive reports whether the daemon still answers a well-formed hello. Every attack below
// ends with this: one bad client is allowed to be refused, never to take the server down
// for everybody else.
func (d *daemon) alive(t *testing.T) bool {
	t.Helper()
	c, err := net.DialTimeout("unix", d.socket, 5*time.Second)
	if err != nil {
		return false
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(c, "%s\n", `{"jsonrpc":"2.0","id":1,"method":"client.hello","params":{"client":"integration","version":"test","asker":false}}`); err != nil {
		return false
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	return err == nil && strings.Contains(line, `"result"`)
}

// TestGarbageOnTheSocket throws bytes at the daemon that no client would send. Each one may
// be refused, ignored or close the connection. None of them may take the daemon with it.
func TestGarbageOnTheSocket(t *testing.T) {
	h := newHome(t)
	h.withProvider(t)
	d := startDaemon(t, h)

	deep := strings.Repeat(`{"a":`, 20000) + "1" + strings.Repeat("}", 20000)
	cases := []struct {
		name string
		send string
	}{
		{"not json", "hello there\n"},
		{"json that is not a request", "[1,2,3]\n"},
		{"an object with no method", `{"jsonrpc":"2.0","id":1}` + "\n"},
		{"an empty method", `{"jsonrpc":"2.0","id":1,"method":""}` + "\n"},
		{"the wrong jsonrpc version", `{"jsonrpc":"9.9","id":1,"method":"client.hello"}` + "\n"},
		{"a method nobody registered", `{"jsonrpc":"2.0","id":1,"method":"session.detonate"}` + "\n"},
		{"params as a string", `{"jsonrpc":"2.0","id":1,"method":"session.open","params":"nope"}` + "\n"},
		{"params as an array", `{"jsonrpc":"2.0","id":1,"method":"session.open","params":[1,2]}` + "\n"},
		{"an id that is an object", `{"jsonrpc":"2.0","id":{"a":1},"method":"client.hello"}` + "\n"},
		{"a session id that is a path", `{"jsonrpc":"2.0","id":1,"method":"session.resume","params":{"session_id":"../../../etc/passwd"}}` + "\n"},
		{"a cwd that does not exist", `{"jsonrpc":"2.0","id":1,"method":"session.open","params":{"cwd":"/nope/nowhere"}}` + "\n"},
		{"nul bytes inside a string", `{"jsonrpc":"2.0","id":1,"method":"session.resume","params":{"session_id":"a` + "\x00" + `b"}}` + "\n"},
		{"twenty thousand levels of nesting", deep + "\n"},
		{"a frame with no newline, then the door", `{"jsonrpc":"2.0","id":1,"method":"client.hello"`},
		{"a call before hello", `{"jsonrpc":"2.0","id":1,"method":"session.list","params":{}}` + "\n"},
		{"two hellos", `{"jsonrpc":"2.0","id":1,"method":"client.hello","params":{"client":"a","version":"t","asker":false}}` + "\n" + `{"jsonrpc":"2.0","id":2,"method":"client.hello","params":{"client":"a","version":"t","asker":false}}` + "\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn, err := net.DialTimeout("unix", d.socket, 10*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
			if _, err := conn.Write([]byte(c.send)); err != nil {
				t.Logf("write ended early, which is the server hanging up: %v", err)
			}
			// Read whatever comes back, if anything, then let the connection go.
			buf := make([]byte, 4096)
			_, _ = conn.Read(buf)
			_ = conn.Close()

			if !d.alive(t) {
				d.mu.Lock()
				out := d.out.String()
				d.mu.Unlock()
				t.Fatalf("the daemon stopped answering after %q\n%s", c.name, out)
			}
		})
	}
	assertNoPanic(t, d.out.String())
}

// TestAHugeFrameOnTheSocket sends more bytes in one frame than any client would: the
// question is whether the daemon refuses it or tries to hold all of it.
func TestAHugeFrameOnTheSocket(t *testing.T) {
	h := newHome(t)
	h.withProvider(t)
	d := startDaemon(t, h)

	conn, err := net.DialTimeout("unix", d.socket, 10*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	frame := `{"jsonrpc":"2.0","id":1,"method":"session.open","params":{"cwd":"` + strings.Repeat("x", 64<<20) + `"}}` + "\n"
	_, werr := conn.Write([]byte(frame))
	buf := make([]byte, 4096)
	_, _ = conn.Read(buf)
	_ = conn.Close()
	t.Logf("64MB frame: write err %v", werr)

	if !d.alive(t) {
		t.Fatalf("a 64MB frame took the daemon down\n%s", d.out.String())
	}
	assertNoPanic(t, d.out.String())
}

// TestManyConnectionsAtOnce opens more connections than a person ever would, half of them
// rude, and asks the daemon to still be there afterwards.
func TestManyConnectionsAtOnce(t *testing.T) {
	h := newHome(t)
	h.withProvider(t)
	d := startDaemon(t, h)

	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, err := net.DialTimeout("unix", d.socket, 10*time.Second)
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
			if i%2 == 0 {
				_, _ = fmt.Fprintf(conn, "%s\n", `{"jsonrpc":"2.0","id":1,"method":"client.hello","params":{"client":"flood","version":"test","asker":false}}`)
				buf := make([]byte, 1024)
				_, _ = conn.Read(buf)
				return
			}
			// The rude half: a partial frame and then the door, over and over.
			for range 20 {
				_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"meth`))
			}
		}(i)
	}
	wg.Wait()

	if !d.alive(t) {
		t.Fatalf("64 connections at once took the daemon down\n%s", d.out.String())
	}
	assertNoPanic(t, d.out.String())
}

// TestTheSocketIsNotWorldReachable: the boundary the trust model draws is the user account,
// and it is drawn with file modes.
func TestTheSocketPermissions(t *testing.T) {
	h := newHome(t)
	h.withProvider(t)
	d := startDaemon(t, h)

	si, err := os.Stat(d.socket)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := si.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode is %o, want 600: the trust model says only this uid", perm)
	}
	di, err := os.Stat(filepath.Dir(d.socket))
	if err != nil {
		t.Fatalf("stat socket dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("socket directory mode is %o, want 700", perm)
	}
}

// decodeLine is a JSON-RPC response read off a connection, for the tests that care what
// came back rather than only that the daemon lived.
func decodeLine(t *testing.T, r *bufio.Reader) map[string]any {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	return m
}
