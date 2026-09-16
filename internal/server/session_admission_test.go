package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/turn"
)

// blockingProvider holds its completion open until the test releases it, which is what lets a
// test decide the exact moment a turn reaches its terminal append.
type blockingProvider struct {
	entered chan struct{} // one signal per Complete that got started
	release chan struct{} // closed to let every waiting Complete finish
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{entered: make(chan struct{}, 4), release: make(chan struct{})}
}

func (p *blockingProvider) Name() string { return "fake" }

func (p *blockingProvider) ListModels(context.Context) ([]provider.Model, error) {
	return []provider.Model{{
		Ref:           session.ModelRef{Provider: "fake", Model: "m1"},
		DisplayName:   "Fake 1",
		ContextWindow: 100000,
		Capabilities:  provider.Capabilities{Tools: true},
	}}, nil
}

func (p *blockingProvider) Complete(ctx context.Context, _ provider.Request, emit func(provider.Part) error) error {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := emit(provider.Part{Type: provider.PartTextDelta, Text: "done"}); err != nil {
		return err
	}
	return emit(provider.Part{Type: provider.PartStop, StopReason: session.StopEndTurn, StopReasonRaw: "end_turn"})
}

type blockingPlugin struct{ p *blockingProvider }

func (blockingPlugin) Name() string { return "fake" }

func (bp blockingPlugin) Init(ctx context.Context, h plugin.Host) error {
	return h.RegisterProvider(bp.p)
}

// fixture is a Server over a real store with one blocking provider registered.
type fixture struct {
	t     *testing.T
	srv   *Server
	store *session.Store
	model provider.Model
	prov  *blockingProvider
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	store, err := session.OpenStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	prov := newBlockingProvider()
	plugins := plugin.NewRegistry(nil, func(string) {})
	plugins.Load(ctx, blockingPlugin{prov})
	reg := provider.NewRegistry(filepath.Join(dir, "registry.json"), plugins.Providers()...)
	if err := reg.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.MaxTokens = 1000
	srv := New(Deps{Version: "test", Config: cfg, Store: store, Registry: reg, Plugins: plugins, Gate: gate.New(nil)})
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	m, err := reg.Resolve("fake:m1")
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, srv: srv, store: store, model: m, prov: prov}
}

// open returns a live session installed in the Server, with no connection attached.
func (f *fixture) open() *liveSession {
	f.t.Helper()
	sess, err := session.Open(f.store, session.SessionOpened{
		SchemaVersion: 1, RudyVersion: "test",
		Workspace: session.Workspace{Root: f.t.TempDir(), ProjectID: "local/test"},
		Model:     f.model.Ref, Thinking: session.ThinkingOff, Mode: session.ModeOff, Agent: "default",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	ls := newLive(sess, f.model)
	f.srv.mu.Lock()
	f.srv.live[sess.ID()] = ls
	f.srv.mu.Unlock()
	return ls
}

// attach installs ls and subscribes a fresh connection to it, the way an open or a resume
// does, and fails the test if the session refuses.
func (f *fixture) attach(ls *liveSession, asker bool) *testConn {
	f.t.Helper()
	tc := f.conn(asker)
	if _, e := f.srv.installAndAttach(tc.cn, ls); e != nil {
		f.t.Fatalf("attach: %v", e)
	}
	return tc
}

// testConn is one connection wired the way serveConn wires it, with a reader draining the
// client end so the pump never blocks and the test can see what the client was told.
type testConn struct {
	cn     *conn
	client protocol.Conn

	mu   sync.Mutex
	msgs []string
	eof  bool
	done chan struct{}
}

func (f *fixture) conn(asker bool) *testConn {
	f.t.Helper()
	clientEnd, serverEnd := protocol.Pipe()
	f.srv.mu.Lock()
	f.srv.nextID++
	cn := newConn(f.srv.nextID, serverEnd)
	cn.hello, cn.greeted, cn.asker = true, true, asker
	f.srv.conns[cn.id] = cn
	f.srv.mu.Unlock()
	pumpCtx, stop := context.WithCancel(context.Background())
	cn.abortPump = stop
	go cn.pump(pumpCtx)
	tc := &testConn{cn: cn, client: clientEnd, done: make(chan struct{})}
	go tc.read()
	f.t.Cleanup(func() { stop(); _ = clientEnd.Close() })
	return tc
}

func (c *testConn) read() {
	defer close(c.done)
	for {
		raw, err := c.client.Recv(context.Background())
		if err != nil {
			c.mu.Lock()
			c.eof = errors.Is(err, io.EOF) || errors.Is(err, protocol.ErrConnClosed) || errors.Is(err, io.ErrClosedPipe)
			c.mu.Unlock()
			return
		}
		c.mu.Lock()
		c.msgs = append(c.msgs, string(raw))
		c.mu.Unlock()
	}
}

// closed reports whether the server closed this connection within the budget.
func (c *testConn) closed(d time.Duration) bool {
	select {
	case <-c.done:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.eof
	case <-time.After(d):
		return false
	}
}

func typed(text string) session.UserMessage {
	return session.UserMessage{Source: session.SourceTyped, Content: []session.Block{{Type: session.BlockText, Text: text}}}
}

// runTerminalFailure starts a turn, breaks the log under it, releases the provider and waits
// for the turn to end. The terminal assistant Entry cannot be written, which is the durability
// failure ADR 0037 quarantines on.
func (f *fixture) runTerminalFailure(ls *liveSession) {
	f.t.Helper()
	if _, e := f.srv.startTurn(ls, typed("go")); e != nil {
		f.t.Fatalf("startTurn: %v", e)
	}
	select {
	case <-f.prov.entered:
	case <-time.After(2 * time.Second):
		f.t.Fatal("the provider was never called")
	}
	if err := ls.sess.Close(); err != nil {
		f.t.Fatalf("close the log under the turn: %v", err)
	}
	close(f.prov.release)
	f.waitIdle(ls)
}

// waitIdle waits for the runner to let go of the session.
func (f *fixture) waitIdle(ls *liveSession) {
	f.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ls.mu.Lock()
		r := ls.runner
		ls.mu.Unlock()
		if r == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatal("the turn never released the session")
}

func TestTerminalFailureQuarantinesTheSessionAndClosesItsSubscribers(t *testing.T) {
	f := newFixture(t)
	ls := f.open()
	sid := ls.sess.ID()
	tc := f.attach(ls, true)

	f.runTerminalFailure(ls)

	if st, _ := ls.mirroredState(); st == turn.Completed || st == turn.Idle || st == turn.Failed {
		t.Errorf("published terminal state %s after a terminal Entry that never landed", st)
	}
	if !tc.closed(2 * time.Second) {
		t.Error("the subscribed connection was not closed by the quarantine")
	}
	if _, e := f.srv.lookup(sid.String()); e == nil || e.Code != protocol.CodeUnavailable {
		t.Errorf("lookup after quarantine = %v, want unavailable", e)
	}
	if _, err := f.srv.appendNote(sid, "fake", "note", session.NoteInfo); err == nil {
		t.Error("a plugin Host.Note reached a quarantined session")
	}
	if _, _, e := f.srv.attachIfLive(f.conn(false).cn, sid); e == nil || e.Code != protocol.CodeUnavailable {
		t.Errorf("attach after quarantine = %v, want unavailable", e)
	}
}

func TestQuarantineSurvivesDetachAndReload(t *testing.T) {
	f := newFixture(t)
	ls := f.open()
	sid := ls.sess.ID()
	tc := f.attach(ls, true)
	f.runTerminalFailure(ls)

	f.srv.detach(tc.cn, ls)
	if _, e := f.srv.loadCold(f.conn(false).cn, sid); e == nil || e.Code != protocol.CodeUnavailable {
		t.Errorf("reload of a quarantined session = %v, want unavailable", e)
	}
	if _, e := f.srv.lookup(sid.String()); e == nil || e.Code != protocol.CodeUnavailable {
		t.Errorf("lookup after reload = %v, want unavailable", e)
	}
}

func TestQuarantineLeavesAnotherSessionAlone(t *testing.T) {
	f := newFixture(t)
	bad, good := f.open(), f.open()
	f.attach(bad, true)
	tcGood := f.attach(good, true)

	f.runTerminalFailure(bad)

	if _, e := f.srv.lookup(bad.sess.ID().String()); e == nil || e.Code != protocol.CodeUnavailable {
		t.Fatalf("the session whose log failed = %v, want unavailable; the rest of this test proves nothing without it", e)
	}
	if _, e := f.srv.lookup(good.sess.ID().String()); e != nil {
		t.Errorf("an unrelated session became %v", e)
	}
	if _, err := f.srv.appendNote(good.sess.ID(), "fake", "note", session.NoteInfo); err != nil {
		t.Errorf("Host.Note on an unrelated session: %v", err)
	}
	if tcGood.closed(200 * time.Millisecond) {
		t.Error("an unrelated session's subscriber was closed")
	}
}

func TestQuarantineCancelsTheTurnStillRunning(t *testing.T) {
	f := newFixture(t)
	ls := f.open()
	tc := f.attach(ls, true)
	if _, e := f.srv.startTurn(ls, typed("go")); e != nil {
		t.Fatalf("startTurn: %v", e)
	}
	select {
	case <-f.prov.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the provider was never called")
	}
	// The provider is still streaming: nothing releases it but the cancellation quarantine
	// owes this session.
	f.srv.quarantine(ls, turn.DurabilityError(errors.New("private cause")))
	f.waitIdle(ls)
	if !tc.closed(2 * time.Second) {
		t.Error("the subscribed connection was not closed by the quarantine")
	}
}

func TestQuarantineRefusalCarriesNoCause(t *testing.T) {
	f := newFixture(t)
	ls := f.open()
	sid := ls.sess.ID()
	f.srv.quarantine(ls, turn.DurabilityError(errors.New("private cause")))
	_, e := f.srv.lookup(sid.String())
	if e == nil || e.Code != protocol.CodeUnavailable {
		t.Fatalf("lookup = %v, want unavailable", e)
	}
	if strings.Contains(e.Message, "private cause") {
		t.Errorf("the refusal leaked its cause: %q", e.Message)
	}
	if e.Message != turn.ErrTerminalDurability.Error() {
		t.Errorf("refusal text = %q, want the fixed %q", e.Message, turn.ErrTerminalDurability.Error())
	}
}

// TestAttachRacingQuarantineObservesIt is the race ADR 0037 exists for: an attach that passed
// an availability check pauses, a terminal failure quarantines the session, and the attach
// then commits anyway. Under the fence the attach either lands in the subscriber set the
// quarantine closes or observes the cause and changes nothing.
func TestAttachRacingQuarantineObservesIt(t *testing.T) {
	f := newFixture(t)
	ls := f.open()
	sid := ls.sess.ID()
	tc := f.conn(false)

	held, release := make(chan struct{}), make(chan struct{})
	failed := make(chan error, 1)
	go func() {
		failed <- f.srv.withSessionOperation(ls, func() error {
			close(held)
			<-release
			return turn.DurabilityError(errors.New("private cause"))
		})
	}()
	<-held

	attached := make(chan *protocol.Error, 1)
	go func() {
		_, _, e := f.srv.attachIfLive(tc.cn, sid)
		attached <- e
	}()
	// The attach must not commit while the fenced operation holds the fence.
	select {
	case e := <-attached:
		t.Fatalf("attach committed through a held fence: %v", e)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-failed; !errors.Is(err, turn.ErrTerminalDurability) {
		t.Fatalf("fenced operation = %v, want a durability cause", err)
	}
	e := <-attached
	if e == nil || e.Code != protocol.CodeUnavailable {
		t.Fatalf("attach after the cause was installed = %v, want unavailable", e)
	}
	if tc.cn.subscribed(sid) {
		t.Error("the refused attach subscribed anyway")
	}
}

// TestHostNoteRacingQuarantineObservesIt is the same race on the linked plugin path, which
// reaches the session through appendNote rather than through a protocol handler.
func TestHostNoteRacingQuarantineObservesIt(t *testing.T) {
	f := newFixture(t)
	ls := f.open()
	sid := ls.sess.ID()

	held, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = f.srv.withSessionOperation(ls, func() error {
			close(held)
			<-release
			return turn.DurabilityError(errors.New("private cause"))
		})
	}()
	<-held

	noted := make(chan error, 1)
	go func() {
		_, err := f.srv.appendNote(sid, "fake", "note", session.NoteInfo)
		noted <- err
	}()
	select {
	case err := <-noted:
		t.Fatalf("Host.Note committed through a held fence: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-noted; !errors.Is(err, turn.ErrTerminalDurability) {
		t.Fatalf("Host.Note after the cause was installed = %v, want the durability refusal", err)
	}
	if n := len(ls.snapshotEntries()); n != 1 {
		t.Errorf("the refused note appended anyway: %d entries", n)
	}
}

// TestFencedOperationsAreSerialized proves the fence is one linearization boundary: a second
// operation cannot commit while the first holds it.
func TestFencedOperationsAreSerialized(t *testing.T) {
	f := newFixture(t)
	ls := f.open()
	var order []int
	var mu sync.Mutex
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.srv.withSessionOperation(ls, func() error {
			close(held)
			<-release
			mu.Lock()
			order = append(order, 1)
			mu.Unlock()
			return nil
		})
	}()
	<-held
	second := make(chan struct{})
	go func() {
		defer close(second)
		_ = f.srv.withSessionOperation(ls, func() error {
			mu.Lock()
			order = append(order, 2)
			mu.Unlock()
			return nil
		})
	}()
	select {
	case <-second:
		t.Fatal("a second operation entered a held fence")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-done
	<-second
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Errorf("fenced operations ran %v, want 1 then 2", order)
	}
}

// TestTerminalDurabilityCapabilityMeansTheWholeGuarantee holds the advertised name to what
// ADR 0033 says it promises: the terminal Entry syncs before its state publishes, the failure
// propagates, the Session is quarantined and its subscribers are closed. An edge adapter reads
// the name and admits Session work on the strength of it, so the name and the behavior are
// asserted together, in one test, rather than in two that can drift apart.
func TestTerminalDurabilityCapabilityMeansTheWholeGuarantee(t *testing.T) {
	f := newFixture(t)
	if got := f.srv.capabilities(); !slices.Contains(got, protocol.CapabilityTerminalTurnDurabilityV1) {
		t.Fatalf("capabilities = %q, want the terminal durability guarantee", got)
	}
	ls := f.open()
	sid := ls.sess.ID()
	tc := f.attach(ls, true)

	f.runTerminalFailure(ls)

	if st, _ := ls.mirroredState(); st == turn.Completed || st == turn.Idle || st == turn.Failed {
		t.Errorf("terminal state %s published without its Entry", st)
	}
	if !tc.closed(2 * time.Second) {
		t.Error("the guarantee names subscriber closure; the subscriber stayed open")
	}
	if _, e := f.srv.lookup(sid.String()); e == nil || e.Code != protocol.CodeUnavailable {
		t.Errorf("the guarantee names quarantine; a later operation got %v", e)
	}
}

func TestTerminalDurabilityCapabilityIsAbsentWithoutItsHooks(t *testing.T) {
	for _, tc := range []struct {
		name string
		srv  *Server
	}{
		{"no admission fence", &Server{d: Deps{Store: &session.Store{}}}},
		{"no session store", New(Deps{Version: "test", Config: &config.Config{}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.srv.capabilities(); len(got) != 0 {
				t.Errorf("capabilities = %q, want none from a Server missing a hook", got)
			}
		})
	}
}

// syncWriter serializes the log lines every goroutine in a fixture writes.
type syncWriter struct {
	mu  sync.Mutex
	buf []byte
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

// captureLog sends the process log into a buffer for the length of one test.
func captureLog(t *testing.T) *syncWriter {
	t.Helper()
	w := &syncWriter{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(w, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return w
}

// TestQuarantineLogsTheCauseItWillNotSend is the other half of the refusal text: the client is
// told the fixed sentence, and the operator is told which disk failed. A durability error
// renders as the fixed text through %v, so logging the error itself says nothing at all.
func TestQuarantineLogsTheCauseItWillNotSend(t *testing.T) {
	log := captureLog(t)
	f := newFixture(t)
	ls := f.open()
	f.srv.quarantine(ls, turn.DurabilityError(errors.New("no space left on device")))
	if got := log.String(); !strings.Contains(got, "no space left on device") {
		t.Errorf("the quarantine log carries no cause:\n%s", got)
	}
}

// TestQuarantineSparesThePluginConnectionThatOpenedTheChild holds quarantine to ADR 0037's
// "affects only the failed Session". A plugin connection is that plugin's whole runtime, so
// closing it would withdraw its tools, commands and providers from every session in the
// process because one child session lost its log.
func TestQuarantineSparesThePluginConnectionThatOpenedTheChild(t *testing.T) {
	f := newFixture(t)
	child := f.open()
	pluginConn := f.conn(false)
	pluginConn.cn.plugin = "fake"
	if _, e := f.srv.installAndAttach(pluginConn.cn, child); e != nil {
		t.Fatalf("attach the plugin connection: %v", e)
	}
	clientConn := f.attach(child, true)

	f.srv.quarantine(child, turn.DurabilityError(errors.New("private cause")))

	if !clientConn.closed(2 * time.Second) {
		t.Error("the subscribed client was not closed")
	}
	if pluginConn.closed(200 * time.Millisecond) {
		t.Error("the plugin connection that opened the child was closed with it")
	}
}

// TestQuarantineSparesTheShutdownControlConnection keeps ADR 0031's terminal proof. Shutdown
// is when cancelled turns write the terminal Entries that can fail, so the connection waiting
// to send server.stopped is exactly the one a quarantine could silence.
func TestQuarantineSparesTheShutdownControlConnection(t *testing.T) {
	f := newFixture(t)
	ls := f.open()
	control := f.conn(false)
	control.cn.claimShutdownProof()
	if _, e := f.srv.installAndAttach(control.cn, ls); e != nil {
		t.Fatalf("attach the control connection: %v", e)
	}

	f.srv.quarantine(ls, turn.DurabilityError(errors.New("private cause")))

	if control.closed(200 * time.Millisecond) {
		t.Error("the shutdown control connection was closed, so server.stopped can never be sent")
	}
}

// TestQuarantinedSubmitIsUnavailableNotInternal covers the window between a handler's lookup
// and the runner's first append: the turn dies on a refusal, and a refusal is unavailable
// however late it lands (rudy-contracts.md, "Requests, client to server").
func TestQuarantinedSubmitIsUnavailableNotInternal(t *testing.T) {
	f := newFixture(t)
	ls := f.open()
	f.srv.quarantine(ls, turn.DurabilityError(errors.New("private cause")))
	_, e := f.srv.startTurn(ls, typed("go"))
	if e == nil || e.Code != protocol.CodeUnavailable {
		t.Fatalf("submit on a quarantined session = %v, want unavailable", e)
	}
	if e.Message != turn.ErrTerminalDurability.Error() {
		t.Errorf("refusal text = %q, want the fixed %q", e.Message, turn.ErrTerminalDurability.Error())
	}
}

// TestQuarantinedCompactIsUnavailableNotInternal is the same window on session.compact, whose
// commit reaches the fence through ModelCompactor.
func TestQuarantinedCompactIsUnavailableNotInternal(t *testing.T) {
	f := newFixture(t)
	ls := f.open()
	f.attach(ls, true)
	if _, e := f.srv.startTurn(ls, typed("go")); e != nil {
		t.Fatalf("startTurn: %v", e)
	}
	<-f.prov.entered
	close(f.prov.release)
	f.waitIdle(ls)
	f.srv.quarantine(ls, turn.DurabilityError(errors.New("private cause")))
	_, _, e := f.srv.compact(context.Background(), ls, "")
	if e == nil || e.Code != protocol.CodeUnavailable {
		t.Fatalf("compact on a quarantined session = %v, want unavailable", e)
	}
}
