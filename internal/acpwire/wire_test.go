package acpwire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/acpschema"
	"github.com/guygrigsby/rudy/internal/acptest"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }
func newTestWire(t *testing.T, raw string, opts Options) (*Wire, *bufio.Reader, *lockedBuffer) {
	t.Helper()
	out := new(lockedBuffer)
	w := New(io.NopCloser(strings.NewReader(raw)), out, opts)
	t.Cleanup(func() { w.Close() })
	w.Open()
	return w, bufio.NewReader(w.Input()), out
}
func readLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	v, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func waitAck(t *testing.T, a *Receipt) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestGateAndPartialReads(t *testing.T) {
	raw := string(acptest.InitializeRequest) + "\n"
	w := New(io.NopCloser(strings.NewReader(raw)), io.Discard, Options{})
	defer w.Close()
	done := make(chan []byte, 1)
	go func() { p := make([]byte, len(raw)); _, _ = io.ReadFull(w.Input(), p); done <- p }()
	select {
	case <-done:
		t.Fatal("gate released bytes")
	case <-time.After(20 * time.Millisecond):
	}
	w.Open()
	select {
	case p := <-done:
		if string(p) != raw {
			t.Fatal("changed raw input")
		}
	case <-time.After(time.Second):
		t.Fatal("gate stuck")
	}
}
func TestFrameByteLimits(t *testing.T) {
	for _, n := range []int{8 << 20, (8 << 20) + 1} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			base := string(acptest.InitializeRequest)
			raw := base + strings.Repeat(" ", n-len(base)) + "\n"
			w, r, _ := newTestWire(t, raw, Options{})
			p, err := r.ReadString('\n')
			if n == 8<<20 {
				if err != nil || len(p) != n+1 {
					t.Fatal("refused exact limit")
				}
			} else {
				if err == nil {
					t.Fatal("accepted overflow")
				}
				select {
				case <-w.Failed():
				default:
					t.Fatal("no supervisor failure")
				}
			}
		})
	}
}
func TestUnterminatedEOF(t *testing.T) {
	w, r, _ := newTestWire(t, string(acptest.InitializeRequest), Options{})
	if _, err := r.ReadByte(); err == nil {
		t.Fatal("dispatched unterminated frame")
	}
	select {
	case <-w.Failed():
	default:
		t.Fatal("no failure")
	}
}
func TestShapesAndSanitizedErrors(t *testing.T) {
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","result":"SECRET"}`,
		`[{"secret":"SECRET"}]`, `{"jsonrpc":"2.0","method":"initialize","params":{"secret":"SECRET"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/cancel","params":{"sessionId":"SECRET"}}`,
		`{"jsonrpc":"2.0","id":"SECRET","method":7}`, `{"secret":"SECRET",`,
		`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"SECRET","sessionId":"other"}}`,
	} {
		w, r, out := newTestWire(t, raw+"\n", Options{})
		if _, err := r.ReadByte(); err == nil {
			t.Fatal("invalid shape reached SDK")
		}
		if strings.Contains(out.String(), "SECRET") {
			t.Fatal("payload leaked")
		}
		select {
		case <-w.Failed():
		default:
			t.Fatal("missing failure")
		}
	}
}
func TestNotificationSchemaAndUnknownDrop(t *testing.T) {
	opts := Options{ValidateNotification: func(method string, p json.RawMessage) error {
		return acpschema.ValidateDefinition("CancelNotification", p)
	}}
	_, r, _ := newTestWire(t, `{"jsonrpc":"2.0","method":"session/cancel","params":{}}`+"\n", opts)
	if _, err := r.ReadByte(); err == nil {
		t.Fatal("invalid notification dispatched")
	}
	_, r, _ = newTestWire(t, `{"jsonrpc":"2.0","method":"unknown","params":{}}`+"\n"+string(acptest.InitializeRequest)+"\n", Options{})
	if !strings.Contains(readLine(t, r), "initialize") {
		t.Fatal("unknown notification dispatched")
	}
}
func TestInboundSlotsAndNull(t *testing.T) {
	var raw strings.Builder
	for i := range 65 {
		fmt.Fprintf(&raw, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":\"unknown\"}\n", i)
	}
	w, r, _ := newTestWire(t, raw.String(), Options{})
	for range 64 {
		readLine(t, r)
	}
	if _, err := r.ReadByte(); err == nil {
		t.Fatal("65th request reached SDK")
	}
	select {
	case <-w.Failed():
	default:
		t.Fatal("missing overload failure")
	}
	for _, ids := range [][2]string{{"null", "null"}, {"1", "1e0"}, {`"a"`, `"\u0061"`}} {
		_, r, _ := newTestWire(t, fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%s,\"method\":\"unknown\"}\n{\"jsonrpc\":\"2.0\",\"id\":%s,\"method\":\"unknown\"}\n", ids[0], ids[1]), Options{})
		readLine(t, r)
		if _, err := r.ReadByte(); err == nil {
			t.Fatal("duplicate id admitted")
		}
	}
}
func TestPhysicalWriteReleasesRequest(t *testing.T) {
	peerR, peerW := io.Pipe()
	outR, outW := io.Pipe()
	defer outR.Close()
	w := New(peerR, outW, Options{})
	defer w.Close()
	w.Open()
	go func() { _, _ = peerW.Write(append(bytes.Clone(acptest.InitializeRequest), '\n')) }()
	r := bufio.NewReader(w.Input())
	readLine(t, r)
	a, err := w.Send(acptest.InitializeResponse, 17)
	if err != nil {
		t.Fatal(err)
	}
	if w.Usage().Requests != 1 {
		t.Fatal("released before physical write")
	}
	if a.Tag != 17 {
		t.Fatal("lost carrier tag")
	}
	line := readLine(t, bufio.NewReader(outR))
	if !strings.Contains(line, "result") {
		t.Fatal("lost response")
	}
	waitAck(t, a)
	if w.Usage().Requests != 0 {
		t.Fatal("request retained after write")
	}
	peerW.Close()
}
func TestOutboundAdmissionAndLateResponses(t *testing.T) {
	w, r, _ := newTestWire(t, "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n"+string(acptest.InitializeRequest)+"\n", Options{})
	o, err := w.PrepareOutbound("session/new", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	o.Cancel()
	readLine(t, r)
	if w.Usage().Requests != 1 || w.Usage().Responses != 0 {
		t.Fatal("late response dispatched")
	}
	w, _, _ = newTestWire(t, "", Options{})
	for range 64 {
		if _, err := w.PrepareOutbound("session/new", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.PrepareOutbound("session/new", json.RawMessage(`{}`)); err == nil {
		t.Fatal("65th outbound admitted")
	}
}
func TestSeparateCountsAndSharedBytes(t *testing.T) {
	var raw strings.Builder
	for i := range 64 {
		fmt.Fprintf(&raw, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":\"unknown\"}\n", i)
		raw.Write(acptest.CancelNotification)
		raw.WriteByte('\n')
		fmt.Fprintf(&raw, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{}}\n", i+1)
	}
	w, r, _ := newTestWire(t, raw.String(), Options{})
	for range 64 {
		if _, err := w.PrepareOutbound("session/new", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	for range 192 {
		readLine(t, r)
	}
	u := w.Usage()
	if u.Requests != 64 || u.Notifications != 64 || u.Responses != 64 {
		t.Fatalf("wrong independent usage: %+v", u)
	}
}
func TestNotificationAndResponseRelease(t *testing.T) {
	w, r, _ := newTestWire(t, string(acptest.CancelNotification)+"\n{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n", Options{})
	o, err := w.PrepareOutbound("session/new", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	readLine(t, r)
	readLine(t, r)
	blocked, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() { w.HandleNotification(func() { close(blocked); <-release }); o.Complete(); close(done) }()
	<-blocked
	if u := w.Usage(); u.Notifications != 1 || u.Responses != 1 || u.InboundBytes == 0 {
		t.Fatal("released behind callback barrier")
	}
	close(release)
	<-done
	if u := w.Usage(); u.InboundBytes != 0 || u.OutboundBytes != 0 {
		t.Fatal("retained completed callbacks")
	}
}

func TestStructuralRefusalPrecedesSchemaAllocation(t *testing.T) {
	for _, params := range []string{strings.Repeat("[", 64) + "0" + strings.Repeat("]", 64), "[" + strings.Repeat("0,", 16384) + "0]"} {
		calls := 0
		raw := `{"jsonrpc":"2.0","method":"session/cancel","params":` + params + `}`
		raw += strings.Repeat(" ", (8<<20)-len(raw))
		_, r, _ := newTestWire(t, raw+"\n", Options{ValidateNotification: func(string, json.RawMessage) error { calls++; return nil }})
		if _, err := r.ReadByte(); err == nil {
			t.Fatal("structural overflow reached SDK")
		}
		if calls != 0 {
			t.Fatal("schema allocated before structural admission")
		}
	}
}

func TestCloseDoesNotReleasePendingInput(t *testing.T) {
	w, _, _ := newTestWire(t, string(acptest.InitializeRequest)+"\n", Options{})
	p := make([]byte, 1)
	if _, err := w.Input().Read(p); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if n, err := w.Input().Read(p); n != 0 || err == nil {
		t.Fatal("close released charged frame remainder")
	}
}

func TestPinnedMethodVocabulary(t *testing.T) {
	_, r, _ := newTestWire(t, "{\"jsonrpc\":\"2.0\",\"method\":\"logout\"}\n"+string(acptest.InitializeRequest)+"\n", Options{})
	if _, err := r.ReadByte(); err == nil {
		t.Fatal("known logout request accepted without id")
	}
	_, r, _ = newTestWire(t, "{\"jsonrpc\":\"2.0\",\"method\":\"session/fork\"}\n"+string(acptest.InitializeRequest)+"\n", Options{})
	if !strings.Contains(readLine(t, r), "initialize") {
		t.Fatal("unstable method entered stable vocabulary")
	}
}
