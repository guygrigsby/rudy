// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// dialLog records the address each dial was handed and answers with serve.
type dialLog struct {
	mu    sync.Mutex
	addrs []string
	serve func(addr string) (net.Conn, error)
}

func (d *dialLog) dial(_ context.Context, _, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, addr)
	d.mu.Unlock()
	return d.serve(addr)
}

func (d *dialLog) dialed() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return strings.Join(d.addrs, ",")
}

// peerConn reports remote as its peer, since a pipe has no address of its own, and records
// whether it was closed.
type peerConn struct {
	net.Conn
	remote net.Addr
	closed bool
}

func (p *peerConn) RemoteAddr() net.Addr { return p.remote }
func (p *peerConn) Close() error         { p.closed = true; return p.Conn.Close() }

// pipeServing is one end of a pipe whose other end reads a request and writes resp. The
// returned conn claims to be connected to addr.
func pipeServing(resp, addr string) (net.Conn, error) {
	c, s := net.Pipe()
	ap, _ := netip.ParseAddrPort(addr)
	pc := &peerConn{Conn: c, remote: net.TCPAddrFromAddrPort(ap)}
	go func() {
		defer func() { _ = s.Close() }()
		if _, err := http.ReadRequest(bufio.NewReader(s)); err != nil {
			return
		}
		_, _ = s.Write([]byte(resp))
	}()
	return pc, nil
}

const (
	okResp       = "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nConnection: close\r\nContent-Length: 2\r\n\r\nhi"
	redirectResp = "HTTP/1.1 302 Found\r\nLocation: http://b.test/\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
)

// testFetcher is a fetcher wired the way Init wires one, with the policy's resolver and the
// dial seam under the test's control.
func testFetcher(resolve func(context.Context, string) ([]net.IP, error), d *dialLog) *fetcher {
	f := &fetcher{policy: policy{resolve: resolve}, maxBytes: 1 << 20, userAgent: "rudy/test", limiter: newLimiter(0), dial: d.dial}
	f.client = &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{DialContext: f.dialVetted, DisableKeepAlives: true, Proxy: nil},
	}
	return f
}

func ips(s ...string) []net.IP {
	out := make([]net.IP, 0, len(s))
	for _, a := range s {
		out = append(out, net.ParseIP(a))
	}
	return out
}

func TestFetchDialsTheVettedAddressNotTheName(t *testing.T) {
	d := &dialLog{serve: func(addr string) (net.Conn, error) { return pipeServing(okResp, addr) }}
	f := testFetcher(func(context.Context, string) ([]net.IP, error) { return ips("203.0.113.9"), nil }, d)
	pg, err := f.fetch(context.Background(), "http://example.test:8080/")
	if err != nil {
		t.Fatal(err)
	}
	if pg.Text != "hi" {
		t.Errorf("text = %q", pg.Text)
	}
	if got := d.dialed(); got != "203.0.113.9:8080" {
		t.Errorf("dialer was handed %q, want only the vetted address with the URL's port", got)
	}
}

func TestFetchNeverResolvesTwice(t *testing.T) {
	var calls []string
	resolve := func(_ context.Context, host string) ([]net.IP, error) {
		calls = append(calls, host)
		if host == "a.test" {
			return ips("203.0.113.1"), nil
		}
		return ips("203.0.113.2"), nil
	}
	d := &dialLog{serve: func(addr string) (net.Conn, error) {
		if strings.HasPrefix(addr, "203.0.113.1:") {
			return pipeServing(redirectResp, addr)
		}
		return pipeServing(okResp, addr)
	}}
	f := testFetcher(resolve, d)
	if _, err := f.fetch(context.Background(), "http://a.test/"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls, ","); got != "a.test,b.test" {
		t.Errorf("resolved %q, want one lookup per hop", got)
	}
	if got := d.dialed(); got != "203.0.113.1:80,203.0.113.2:80" {
		t.Errorf("dialed %q, want each hop's vetted address", got)
	}
}

func TestSecondVettedAddressIsTried(t *testing.T) {
	d := &dialLog{serve: func(addr string) (net.Conn, error) {
		if strings.HasPrefix(addr, "203.0.113.1:") {
			return nil, errors.New("connection refused")
		}
		return pipeServing(okResp, addr)
	}}
	f := testFetcher(func(context.Context, string) ([]net.IP, error) {
		return ips("203.0.113.1", "203.0.113.2"), nil
	}, d)
	if _, err := f.fetch(context.Background(), "http://example.test/"); err != nil {
		t.Fatal(err)
	}
	if got := d.dialed(); got != "203.0.113.1:80,203.0.113.2:80" {
		t.Errorf("dialed %q", got)
	}
}

func TestDialVettedWithoutAddressesFails(t *testing.T) {
	d := &dialLog{serve: func(string) (net.Conn, error) { t.Error("dialed"); return nil, nil }}
	f := &fetcher{dial: d.dial}
	if _, err := f.dialVetted(context.Background(), "tcp", "example.test:80"); err == nil {
		t.Fatal("a dial with no vetted address fell back to the name")
	}
}

func TestOwnInterfaceAddressRefused(t *testing.T) {
	addrs, _ := net.InterfaceAddrs()
	var own net.IP
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() {
			own = n.IP
			break
		}
	}
	if own == nil {
		t.Skip("no non-loopback interface address on this machine")
	}
	host := own.String()
	if own.To4() == nil {
		host = "[" + host + "]"
	}
	u, err := url.Parse("http://" + host + "/")
	if err != nil {
		t.Fatal(err)
	}
	err = policy{}.check(context.Background(), u)
	if err == nil || !strings.Contains(err.Error(), "not a public address") {
		t.Fatalf("check of own address %s = %v", own, err)
	}
}

func TestEmbeddedPrivateFormsRefused(t *testing.T) {
	// Teredo stores the client address XORed with 0xff.
	teredo := net.ParseIP("2001::")
	for i, b := range []byte{127, 0, 0, 1} {
		teredo[12+i] = b ^ 0xff
	}
	for _, tc := range []struct {
		ip     string
		public bool
	}{
		{"64:ff9b::7f00:1", false},
		{"64:ff9b::a00:1", false},
		{"64:ff9b:1::7f00:1", false},
		{"2002:7f00:1::1", false},
		{teredo.String(), false},
		{"::7f00:1", false},
		{"240.0.0.1", false},
		{"255.255.255.255", false},
		{"2001:4860:4860::8888", true},
		{"203.0.113.9", true},
	} {
		if got := public(net.ParseIP(tc.ip)); got != tc.public {
			t.Errorf("public(%s) = %v, want %v", tc.ip, got, tc.public)
		}
	}
}

func TestConnectionToAnUnvettedPeerIsRefused(t *testing.T) {
	var landed *peerConn
	d := &dialLog{serve: func(string) (net.Conn, error) {
		c, s := net.Pipe()
		go func() { _ = s.Close() }()
		landed = &peerConn{Conn: c, remote: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 80}}
		return landed, nil
	}}
	f := testFetcher(func(context.Context, string) ([]net.IP, error) { return ips("203.0.113.9"), nil }, d)
	_, err := f.fetch(context.Background(), "http://example.test/")
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("fetch = %v, want a refusal", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("refusal names the address: %v", err)
	}
	if landed == nil || !landed.closed {
		t.Error("the connection to the unvetted peer was not closed")
	}
}
