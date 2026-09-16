package acpwire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/acpext"
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
	t.Cleanup(func() { _ = w.Close() })
	w.Open()
	return w, bufio.NewReader(w.Input()), out
}

// sdkPeer stands in for the SDK's request side. A reserved outbound id carries no response
// until the SDK writes its own request through Output and the wire binds the two ids; a
// response for an unbound id is refused on purpose (see acpagent's unbound-response test), so
// every test about a matched response has to reach the bound state the way production does.
type sdkPeer struct {
	mu   sync.Mutex
	next int64
}

// write is where acp.SendRequest would sit: inside Invoke, holding initiation, so the id it
// allocates binds to the outbound the wire is waiting on. The lock keeps ids monotonic when
// several callbacks are in flight, which the wire requires of the SDK.
func (p *sdkPeer) write(w *Wire, method string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next++
	_, err := fmt.Fprintf(w.Output(), "{\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":%q,\"params\":{}}\n", p.next, method)
	return err
}

// hold reserves one outbound request, binds it, and parks its SDK callback until release is
// closed. A parked callback is what keeps a matched response charged. It returns once the
// binding is visible to the reader, so the caller can feed the response straight away.
func (p *sdkPeer) hold(t *testing.T, w *Wire, method string, release <-chan struct{}, wg *sync.WaitGroup) *Outbound {
	t.Helper()
	o, err := w.PrepareOutbound(method, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	bound := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = o.Invoke(context.Background(), func(context.Context) error {
			if err := p.write(w, method); err != nil {
				return err
			}
			close(bound)
			<-release
			return nil
		})
	}()
	<-bound
	return o
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
	defer func() { _ = w.Close() }()
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
	defer func() { _ = outR.Close() }()
	w := New(peerR, outW, Options{})
	defer func() { _ = w.Close() }()
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
	_ = peerW.Close()
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

// A peer that closes its write side after a complete frame ended the input. The reader owes
// its consumer io.EOF for that, not a framing failure: the supervisor must not restart a
// connection that simply finished, and readFrame keeps the two cases apart for this reason.
// Ending is not abandoning, though. The connection is over, so it is released here as any
// other close releases it; a wire left open on a departed peer holds its write loop, its
// source and every admitted charge for the life of the process.
func TestCleanEOFEndsInputAndReleasesTheConnection(t *testing.T) {
	w, r, _ := newTestWire(t, string(acptest.InitializeRequest)+"\n", Options{})
	readLine(t, r)
	if _, err := r.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("end of input = %v, want io.EOF", err)
	}
	if !w.closed() {
		t.Fatal("the wire outlived the input it was reading")
	}
	if u := w.Usage(); u != (Usage{}) {
		t.Fatalf("admission survived the connection: %+v", u)
	}
	if _, err := r.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("read after the end = %v, want io.EOF", err)
	}
	select {
	case err := <-w.Failed():
		t.Fatalf("clean end of input reported %v", err)
	case <-time.After(20 * time.Millisecond):
	}
}

// The claim gate holds a request back until its callback claims it, so the end of input is not
// the end of the connection: what the peer already sent is still owed to the SDK, and only the
// last of it releases the wire.
func TestClaimGateDrainsBeforeTheEndOfInput(t *testing.T) {
	raw := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"unknown\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"unknown\"}\n"
	w, r, _ := newTestWire(t, raw, Options{RequireRequestClaims: true})
	for _, want := range []string{`"id":1`, `"id":2`} {
		if line := readLine(t, r); !strings.Contains(line, want) {
			t.Fatalf("line %q does not carry %s", line, want)
		}
		if w.ClaimRequest() == nil {
			t.Fatal("nothing to claim")
		}
	}
	if _, err := r.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("end of input = %v, want io.EOF", err)
	}
	if !w.closed() {
		t.Fatal("the wire outlived its drained ingress")
	}
}

// MethodKinds is the whole ACP vocabulary, but every method belongs to the side that handles
// it. session/update is the client's, so an agent wire receiving one has a frame no agent
// adapter has a callback for: nothing would ever run HandleNotification and the slot it was
// charged would never come back. MaxItems of them would end the connection, which makes the
// whole vocabulary a remote denial of service on the side that does not own half of it.
func TestNotificationForTheOtherSideIsDropped(t *testing.T) {
	rudyUpdate := []byte(`{"jsonrpc":"2.0","method":"` + string(acpext.MethodSessionUpdate) + `","params":{}}`)
	for _, tc := range []struct {
		name  string
		side  Side
		wrong []byte
	}{
		{"agent is sent the client's update", AgentSide, acptest.UpdateNotification},
		{"agent is sent the client's Rudy update", AgentSide, rudyUpdate},
		{"client is sent the agent's cancel", ClientSide, acptest.CancelNotification},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw strings.Builder
			for range MaxItems {
				raw.Write(tc.wrong)
				raw.WriteByte('\n')
			}
			raw.Write(acptest.InitializeRequest)
			raw.WriteByte('\n')
			w, r, _ := newTestWire(t, raw.String(), Options{Side: tc.side})
			if line := readLine(t, r); !strings.Contains(line, "initialize") {
				t.Fatalf("delivered the other side's notification: %q", line)
			}
			if u := w.Usage(); u.Notifications != 0 {
				t.Fatalf("charged a slot nothing can release: %+v", u)
			}
		})
	}
}

// The mirror of the rule: each side still receives the notifications that are its own.
func TestEachSideReceivesItsOwnNotifications(t *testing.T) {
	for _, tc := range []struct {
		name  string
		side  Side
		frame []byte
	}{
		{"agent is sent a cancel", AgentSide, acptest.CancelNotification},
		{"client is sent an update", ClientSide, acptest.UpdateNotification},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, r, _ := newTestWire(t, string(tc.frame)+"\n", Options{Side: tc.side})
			readLine(t, r)
			if u := w.Usage(); u.Notifications != 1 {
				t.Fatalf("dropped a notification this side owns: %+v", u)
			}
		})
	}
}

// The ledger charges what the SDK is handed, not what the peer sent. A session/cancel is
// re-encoded from its validated fields, and json.Marshal escapes <, > and & to six bytes each,
// so charging the frame as it arrived lets the ingress queue hold several times what the
// ledger says it does.
func TestReEncodedCancelIsChargedAsDelivered(t *testing.T) {
	frame := `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"` + strings.Repeat("<", 4096) + `"}}`
	w, r, _ := newTestWire(t, frame+"\n", Options{})
	delivered := len(readLine(t, r)) - 1
	if u := w.Usage(); u.InboundBytes < delivered {
		t.Fatalf("charged %d bytes for a frame delivered at %d", u.InboundBytes, delivered)
	}
}

func TestSeparateCountsAndSharedBytes(t *testing.T) {
	var raw strings.Builder
	for i := range MaxItems {
		fmt.Fprintf(&raw, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":\"unknown\"}\n", i)
		raw.Write(acptest.CancelNotification)
		raw.WriteByte('\n')
		fmt.Fprintf(&raw, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{}}\n", i+1)
	}
	w, r, _ := newTestWire(t, raw.String(), Options{})
	peer := &sdkPeer{}
	release := make(chan struct{})
	var wg sync.WaitGroup
	defer func() { close(release); wg.Wait() }()
	for range MaxItems {
		peer.hold(t, w, "session/new", release, &wg)
	}
	for range 3 * MaxItems {
		readLine(t, r)
	}
	u := w.Usage()
	if u.Requests != MaxItems || u.Notifications != MaxItems || u.Responses != MaxItems {
		t.Fatalf("wrong independent usage: %+v", u)
	}
}
func TestNotificationAndResponseRelease(t *testing.T) {
	w, r, _ := newTestWire(t, string(acptest.CancelNotification)+"\n{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n", Options{})
	peer := &sdkPeer{}
	callback := make(chan struct{})
	var wg sync.WaitGroup
	peer.hold(t, w, "session/new", callback, &wg)
	readLine(t, r)
	readLine(t, r)
	blocked, resume := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		w.HandleNotification(func() { close(blocked); <-resume })
		close(callback)
		wg.Wait()
		close(done)
	}()
	<-blocked
	if u := w.Usage(); u.Notifications != 1 || u.Responses != 1 || u.InboundBytes == 0 {
		t.Fatal("released behind callback barrier")
	}
	close(resume)
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
	_ = w.Close()
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
