package app

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/server"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tui/keys"
	"github.com/guygrigsby/rudy/internal/tui/theme"
	"github.com/guygrigsby/rudy/internal/tui/transcript"
)

// TestMain pins the color environment the way the transcript tests do, so nothing that
// consults it can move what a status line or a row renders to.
func TestMain(m *testing.M) {
	_ = os.Setenv("TERM", "xterm-256color")
	_ = os.Setenv("COLORTERM", "truecolor")
	_ = os.Setenv("CLICOLOR_FORCE", "1")
	os.Exit(m.Run())
}

const testTimeout = 5 * time.Second

var testRef = session.ModelRef{Provider: "fake", Model: "m1"}

// testModels is the registry the app is handed: one model, priced, with a round context
// window so a percent is exact.
func testModels() []provider.Model {
	return []provider.Model{{
		Ref:           testRef,
		DisplayName:   "Fake 1",
		ContextWindow: 100000,
		MaxOutput:     8192,
		Pricing: provider.Pricing{
			Input: "0.000003", Output: "0.000015", CacheRead: "0.0000003", CacheWrite: "0.00000375",
		},
	}}
}

// testConfig loads the real defaults from a temp XDG root, so a test sees the same UI
// config a run does rather than a hand-built struct that can drift from it.
func testConfig(t *testing.T, over map[string]any) *config.Config {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{Config: dir, Data: dir, Runtime: dir, Cache: dir, Home: dir}
	m := map[string]any{"default.provider": "fake", "default.model": "m1"}
	maps.Copy(m, over)
	cfg, err := config.Load(paths, m)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

// harness drives one Model over a real protocol.Client whose server side is the test: it
// pushes notifications in and hands whatever the pump reads to Update.
type harness struct {
	t   *testing.T
	m   *Model
	srv protocol.Conn
	cl  *protocol.Client
}

func newHarness(t *testing.T, over map[string]any) *harness {
	t.Helper()
	cc, sc := protocol.Pipe()
	cl := protocol.NewClient(cc)
	go answerEmpty(sc)
	t.Cleanup(func() { _ = cl.Close(); _ = sc.Close() })
	m := New(Options{
		Config: testConfig(t, over),
		Theme:  theme.Default(),
		Keys:   keys.Default(),
		Client: cl,
		Session: protocol.SessionInfo{
			SessionID: session.NewID().String(),
			Workspace: session.Workspace{Root: "/w", GitRoot: "/w", ProjectID: "local/w"},
			Model:     testRef,
			Mode:      session.ModeStrict,
			Thinking:  session.ThinkingHigh,
		},
		Models:    testModels(),
		Version:   "test",
		Cwd:       "/w",
		Workspace: "rudy main*",
	})
	h := &harness{t: t, m: m, srv: sc, cl: cl}
	h.update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return h
}

// answerEmpty is the server side of the pipe: every request gets an empty result, so a
// call made as a command finishes instead of hanging.
func answerEmpty(conn protocol.Conn) {
	ctx := context.Background()
	for {
		raw, err := conn.Recv(ctx)
		if err != nil {
			return
		}
		var in struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(raw, &in); err != nil || len(in.ID) == 0 {
			continue
		}
		resp, err := protocol.NewResponse(in.ID, nil)
		if err != nil {
			return
		}
		if err := conn.Send(ctx, resp); err != nil {
			return
		}
	}
}

func (h *harness) update(msg tea.Msg) tea.Cmd {
	h.t.Helper()
	next, cmd := h.m.Update(msg)
	if next != h.m {
		h.t.Fatalf("Update returned another model: %T", next)
	}
	return cmd
}

// send pushes one notification from the server side without pumping it.
func (h *harness) send(method string, params any) {
	h.t.Helper()
	n, err := protocol.NewNotification(method, params)
	if err != nil {
		h.t.Fatalf("notification %s: %v", method, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := h.srv.Send(ctx, n); err != nil {
		h.t.Fatalf("send %s: %v", method, err)
	}
}

// notify sends one notification and pumps it into Update, the way the program loop would.
func (h *harness) notify(method string, params any) tea.Cmd {
	h.t.Helper()
	h.send(method, params)
	return h.update(runCmd(h.t, pump(h.cl)))
}

func (h *harness) view() string {
	h.t.Helper()
	return h.m.View().Content
}

func (h *harness) lines() []string { return strings.Split(h.view(), "\n") }

// press sends one key by the spelling the [keys] grammar uses.
func (h *harness) press(spelling string) tea.Cmd {
	h.t.Helper()
	k, err := keys.Parse(spelling)
	if err != nil {
		h.t.Fatalf("key %q: %v", spelling, err)
	}
	return h.update(tea.KeyPressMsg(k))
}

// typeText presses each rune of s.
func (h *harness) typeText(s string) {
	h.t.Helper()
	for _, r := range s {
		h.update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// runCmd runs one command and returns its message, failing rather than hanging.
func runCmd(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	out := make(chan tea.Msg, 1)
	go func() { out <- cmd() }()
	select {
	case msg := <-out:
		return msg
	case <-time.After(testTimeout):
		t.Fatal("command did not finish")
		return nil
	}
}

// runAll runs cmd and every command a tea.Batch it returned holds, and collects the
// messages. Batched commands run concurrently in a real program; here they run in order,
// which is enough to see what a command produced.
func runAll(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	msg := runCmd(t, cmd)
	if msg == nil {
		return nil
	}
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	var out []tea.Msg
	for _, c := range batch {
		out = append(out, runAll(t, c)...)
	}
	return out
}

// entry builds a valid log entry around a payload.
func entry(t *testing.T, p session.Payload) session.Entry {
	t.Helper()
	if err := session.Validate(p); err != nil {
		t.Fatalf("invalid payload %T: %v", p, err)
	}
	return session.Entry{ID: session.NewID(), At: time.Unix(0, 0).UTC(), Kind: p.Kind(), Payload: p}
}

func (h *harness) appended(p session.Payload) tea.Cmd {
	h.t.Helper()
	return h.notify(protocol.NotifyEntryAppended, protocol.EntryAppended{
		SessionID: h.m.session.SessionID, Entry: entry(h.t, p),
	})
}

func rowKinds(rows []*transcript.Row) []transcript.RowKind {
	out := make([]transcript.RowKind, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Kind)
	}
	return out
}

func TestInitStartsThePumpAndReachesTheEditor(t *testing.T) {
	h := newHarness(t, nil)
	h.send(protocol.NotifyNotice, protocol.NoticeParams{Level: "info", Text: "hello"})
	var got bool
	for _, msg := range runAll(t, h.m.Init()) {
		if n, ok := msg.(NotificationMsg); ok && n.Method == protocol.NotifyNotice {
			got = true
		}
	}
	if !got {
		t.Fatal("Init must arm the notification pump")
	}
	// The editor is focused and takes typed text from the first key on.
	h.typeText("hi")
	if h.m.ed.Text() != "hi" {
		t.Fatalf("editor holds %q", h.m.ed.Text())
	}
}

func TestEntriesBecomeRowsAndAccumulateUsage(t *testing.T) {
	h := newHarness(t, nil)
	h.appended(session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("fix the flaky fork test")}})
	h.appended(session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopEndTurn,
		Content: []session.Block{session.TextBlock("Looking at the test first.")},
		Usage:   session.Usage{Input: 20000, Output: 100, CacheRead: 22000},
	})
	h.appended(session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopEndTurn,
		Content: []session.Block{session.TextBlock("Fixed and green.")},
		Usage:   session.Usage{Input: 30000, Output: 50, CacheRead: 12000},
	})
	if got := rowKinds(h.m.tr.Rows()); !slices.Equal(got, []transcript.RowKind{transcript.RowUser, transcript.RowAssistant, transcript.RowAssistant}) {
		t.Fatalf("rows %v", got)
	}
	want := session.Usage{Input: 50000, Output: 150, CacheRead: 34000}
	if h.m.usage != want {
		t.Errorf("usage %+v want %+v", h.m.usage, want)
	}
	// The context percent reads the last assistant message alone, not the sum.
	if h.m.lastPrompt != 42000 {
		t.Errorf("last prompt %d", h.m.lastPrompt)
	}
	if v := h.view(); !strings.Contains(ansi.Strip(v), "fix the flaky fork test") || !strings.Contains(ansi.Strip(v), "Fixed and green.") {
		t.Errorf("view %q", v)
	}
}

func TestStreamDeltaFeedsTheLiveRowAndTurnStateMirrors(t *testing.T) {
	h := newHarness(t, nil)
	turn := session.NewID().String()
	h.notify(protocol.NotifyTurnState, protocol.TurnStateChanged{SessionID: h.m.session.SessionID, TurnID: turn, State: "streaming"})
	if h.m.turnState != "streaming" || h.m.turnID != turn {
		t.Fatalf("turn %q %q", h.m.turnState, h.m.turnID)
	}
	for _, text := range []string{"Looking ", "at the test."} {
		h.notify(protocol.NotifyStreamDelta, protocol.StreamDelta{
			SessionID: h.m.session.SessionID, TurnID: turn,
			Part: provider.Part{Type: provider.PartTextDelta, Text: text},
		})
	}
	rows := h.m.tr.Rows()
	if len(rows) != 1 || !rows[0].Live || rows[0].Text != "Looking at the test." {
		t.Fatalf("rows %+v", rows)
	}
	if !strings.Contains(ansi.Strip(h.view()), "Looking at the test.") {
		t.Errorf("view %q", h.view())
	}
}

func TestPermissionPromptStandsUntilTheDecisionArrives(t *testing.T) {
	h := newHarness(t, nil)
	h.appended(session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopToolUse,
		Content: []session.Block{session.ToolUseBlock("t1", "bash", json.RawMessage(`{"command":"go test ./..."}`))},
	})
	h.notify(protocol.NotifyPermissionRequested, protocol.PermissionRequested{
		SessionID: h.m.session.SessionID, TurnID: session.NewID().String(), ToolUseID: "t1",
		Tool: "bash", Input: json.RawMessage(`{"command":"go test ./..."}`),
		Matcher: session.Matcher{Tool: "bash", Prefix: "go test"},
	})
	if !strings.Contains(ansi.Strip(h.view()), "allow once [y]") {
		t.Fatalf("no question in %q", h.view())
	}
	h.appended(session.PermissionDecision{
		ToolUseID: "t1", Tool: "bash", Mode: session.ModeStrict,
		Matcher:  session.Matcher{Tool: "bash", Prefix: "go test"},
		Decision: session.Allow, DecidedBy: session.ByAsker, Scope: session.ScopeOnce, Reason: "asker",
	})
	if strings.Contains(ansi.Strip(h.view()), "allow once [y]") {
		t.Fatalf("the answered question must go: %q", h.view())
	}
	if got := rowKinds(h.m.tr.Rows()); !slices.Equal(got, []transcript.RowKind{transcript.RowTool}) {
		t.Fatalf("rows %v", got)
	}
}

func TestWidgetsAndStatusFillTheirSlots(t *testing.T) {
	h := newHarness(t, map[string]any{
		"ui.layout.slots": []string{"header", "transcript", "input", "status"},
		"ui.status.items": []string{"model", "memory:servers"},
	})
	h.notify(protocol.NotifyWidgetUpdated, protocol.Widget{
		Owner: "memory", Key: "banner", Slot: protocol.SlotHeader,
		Content: []protocol.Span{{Text: "rudy ", Role: "muted"}, {Text: "v0", Role: "accent"}},
	})
	h.notify(protocol.NotifyWidgetUpdated, protocol.Widget{
		Owner: "memory", Key: "hint", Slot: protocol.SlotAboveEditor,
		Content: []protocol.Span{{Text: "3 memories loaded", Role: "muted"}},
	})
	h.notify(protocol.NotifyWidgetUpdated, protocol.Widget{
		Owner: "skills", Key: "count", Slot: protocol.SlotBelowEditor,
		Content: []protocol.Span{{Text: "12 skills", Role: "muted"}},
	})
	h.notify(protocol.NotifyStatusUpdated, protocol.StatusUpdated{Items: []protocol.StatusItem{
		{Owner: "memory", Key: "servers", Content: []protocol.Span{{Text: "2 servers", Role: "success"}}},
		{Owner: "other", Key: "unplaced", Content: []protocol.Span{{Text: "never drawn", Role: "text"}}},
	}})
	lines := h.lines()
	at := func(want string) int {
		for i, l := range lines {
			if strings.Contains(ansi.Strip(l), want) {
				return i
			}
		}
		t.Fatalf("%q is not in %q", want, strings.Join(lines, "\n"))
		return -1
	}
	header, hint, skills, status := at("rudy v0"), at("3 memories loaded"), at("12 skills"), at("2 servers")
	if header >= hint || hint >= skills || skills != status-1 {
		t.Fatalf("slot order header %d above %d below %d status %d in\n%s", header, hint, skills, status, strings.Join(lines, "\n"))
	}
	if strings.Contains(ansi.Strip(h.view()), "never drawn") {
		t.Error("a status item the config did not place must not render")
	}
	// A widget replaces its own, keyed owner and key, and never another owner's.
	h.notify(protocol.NotifyWidgetUpdated, protocol.Widget{
		Owner: "memory", Key: "hint", Slot: protocol.SlotAboveEditor,
		Content: []protocol.Span{{Text: "4 memories loaded", Role: "muted"}},
	})
	v := ansi.Strip(h.view())
	if strings.Contains(v, "3 memories loaded") || !strings.Contains(v, "4 memories loaded") || !strings.Contains(v, "12 skills") {
		t.Errorf("widget replace %q", v)
	}
}

func TestNoticesFromTheServerAndFromAFailedPlugin(t *testing.T) {
	h := newHarness(t, nil)
	h.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "warn", Text: "registry refresh: boom"})
	h.notify(protocol.NotifyPluginState, protocol.PluginState{
		Name: "memory", Origin: "spawned", State: "failed", Reason: "exit status 1",
	})
	h.notify(protocol.NotifyPluginState, protocol.PluginState{Name: "skills", Origin: "linked", State: "ready"})
	v := ansi.Strip(h.view())
	if !strings.Contains(v, "registry refresh: boom") {
		t.Errorf("notice missing from %q", v)
	}
	if !strings.Contains(v, "plugin memory failed: exit status 1") {
		t.Errorf("plugin failure missing from %q", v)
	}
	if strings.Contains(v, "skills") {
		t.Errorf("a healthy plugin says nothing: %q", v)
	}
	for range maxNotices + 5 {
		h.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "info", Text: "chatter"})
	}
	if len(h.m.notices) != maxNotices {
		t.Errorf("notices %d want %d", len(h.m.notices), maxNotices)
	}
	if strings.Contains(ansi.Strip(h.view()), "registry refresh: boom") {
		t.Error("the oldest notices must fall off")
	}
}

func TestWindowSizeResizesTheTranscriptAndTheEditor(t *testing.T) {
	h := newHarness(t, nil)
	h.appended(session.UserMessage{Source: session.SourceTyped, Content: []session.Block{
		session.TextBlock(strings.Repeat("wide ", 40)),
	}})
	h.typeText(strings.Repeat("draft ", 30))
	h.update(tea.WindowSizeMsg{Width: 40, Height: 12})
	if h.m.width != 40 || h.m.height != 12 {
		t.Fatalf("size %dx%d", h.m.width, h.m.height)
	}
	for _, l := range h.lines() {
		if w := ansi.StringWidth(l); w > 40 {
			t.Fatalf("line of width %d over a 40 column screen: %q", w, l)
		}
	}
}

func TestDisconnectedNoticesAndRefusesInput(t *testing.T) {
	h := newHarness(t, nil)
	h.update(DisconnectedMsg{})
	if !h.m.disconnected {
		t.Fatal("disconnected")
	}
	if !strings.Contains(ansi.Strip(h.view()), "disconnected") {
		t.Errorf("no notice in %q", h.view())
	}
	h.typeText("nope")
	if h.m.ed.Text() != "" {
		t.Errorf("input must not reach a server that is gone: %q", h.m.ed.Text())
	}
	if _, ok := runCmd(t, h.press("ctrl+d")).(tea.QuitMsg); !ok {
		t.Error("the exit key still works while disconnected")
	}
}

func TestKeysClearExitAndExpandATool(t *testing.T) {
	h := newHarness(t, nil)
	h.typeText("some draft")
	h.press("ctrl+c") // app.clear
	if h.m.ed.Text() != "" {
		t.Errorf("app.clear leaves %q", h.m.ed.Text())
	}
	if _, ok := runCmd(t, h.press("ctrl+d")).(tea.QuitMsg); !ok {
		t.Error("app.exit on an empty editor quits")
	}
	h.typeText("busy")
	if msg := runCmd(t, h.press("ctrl+d")); msg != nil {
		if _, quit := msg.(tea.QuitMsg); quit {
			t.Error("app.exit with a draft must not quit")
		}
	}
	h.press("ctrl+c")

	h.appended(session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopToolUse,
		Content: []session.Block{session.ToolUseBlock("t1", "bash", json.RawMessage(`{"command":"ls"}`))},
	})
	h.appended(session.ToolResult{
		ToolUseID: "t1", Outcome: session.OutcomeOK, DurationMS: 3,
		Content: []session.Block{session.TextBlock("a\nb\nc\nd\ne\nf")},
	})
	h.press("ctrl+o") // app.tools.expand
	rows := h.m.tr.Rows()
	if len(rows) != 1 || !rows[0].Expanded {
		t.Fatalf("newest tool row not expanded: %+v", rows)
	}
}

func TestNewLineReachesTheEditor(t *testing.T) {
	h := newHarness(t, nil)
	h.typeText("one")
	h.press("shift+enter") // tui.input.newLine
	h.typeText("two")
	if got := h.m.ed.Text(); got != "one\ntwo" {
		t.Fatalf("editor holds %q", got)
	}
}

func TestMouseClickTogglesTheToolRowUnderIt(t *testing.T) {
	h := newHarness(t, nil)
	h.appended(session.AssistantMessage{
		Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopToolUse,
		Content: []session.Block{session.ToolUseBlock("t1", "bash", json.RawMessage(`{"command":"ls"}`))},
	})
	h.appended(session.ToolResult{
		ToolUseID: "t1", Outcome: session.OutcomeOK, DurationMS: 3,
		Content: []session.Block{session.TextBlock("a\nb\nc\nd\ne\nf")},
	})
	y := -1
	for i, l := range h.lines() {
		if strings.Contains(ansi.Strip(l), "▸ bash") {
			y = i
		}
	}
	if y < 0 {
		t.Fatalf("no tool row in\n%s", h.view())
	}
	h.update(tea.MouseClickMsg{X: 2, Y: y, Button: tea.MouseLeft})
	if rows := h.m.tr.Rows(); len(rows) != 1 || !rows[0].Expanded {
		t.Fatalf("click did not expand: %+v", rows)
	}
	h.update(tea.MouseClickMsg{X: 2, Y: y, Button: tea.MouseLeft})
	if rows := h.m.tr.Rows(); rows[0].Expanded {
		t.Fatal("a second click collapses")
	}
}

func TestMouseWheelScrollsTheAltscreenOnly(t *testing.T) {
	for _, render := range []string{"inline", "altscreen"} {
		t.Run(render, func(t *testing.T) {
			h := newHarness(t, map[string]any{"ui.render": render})
			for range 40 {
				h.appended(session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock("line")}})
			}
			h.view() // the viewport sizes itself while composing
			h.update(tea.MouseWheelMsg{Y: 1, Button: tea.MouseWheelDown})
			scrolled := h.m.vp.YOffset() > 0
			if scrolled != (render == "altscreen") {
				t.Fatalf("%s scrolled %v at offset %d", render, scrolled, h.m.vp.YOffset())
			}
			if render == "altscreen" && !h.m.View().AltScreen {
				t.Error("altscreen config must set the view's AltScreen")
			}
			if render == "inline" && h.m.View().AltScreen {
				t.Error("inline config must not set AltScreen")
			}
		})
	}
}

func TestRegistryRefreshesWhenTheSessionModelIsUnknown(t *testing.T) {
	h := newHarness(t, nil)
	h.m.session.Model = session.ModelRef{Provider: "fake", Model: "gone"}
	h.m.model = provider.Model{}
	// Init's pump is one of the commands runAll walks, and it reads one notification
	// before it returns.
	h.send(protocol.NotifyNotice, protocol.NoticeParams{Level: "info", Text: "hello"})
	var called bool
	for _, msg := range runAll(t, h.m.Init()) {
		if r, ok := msg.(CallResultMsg); ok && r.Method == protocol.MethodRegistryList {
			called = true
		}
	}
	if !called {
		t.Fatal("a model the caller did not list must refresh the registry")
	}
	models := []provider.Model{{Ref: session.ModelRef{Provider: "fake", Model: "gone"}, ContextWindow: 200000}}
	res, err := json.Marshal(protocol.RegistryListResult{Models: models})
	if err != nil {
		t.Fatal(err)
	}
	h.update(CallResultMsg{Method: protocol.MethodRegistryList, Result: res})
	if h.m.model.ContextWindow != 200000 {
		t.Fatalf("model %+v", h.m.model)
	}
}

func TestModelAndModeChangesMoveTheStatusLine(t *testing.T) {
	h := newHarness(t, map[string]any{"ui.status.items": []string{"model", "permission_mode"}})
	h.appended(session.ModelChange{Model: session.ModelRef{Provider: "fake", Model: "m2"}})
	h.appended(session.ModeChange{Mode: session.ModePermissive})
	got := ansi.Strip(h.m.statusLine())
	if got != "m2  permissive" {
		t.Fatalf("status %q", got)
	}
}

// The rest of the file is the same model over a real server, so the protocol the app
// speaks is the one the server answers rather than a test's idea of it.

type fakeProvider struct{ text string }

func (fakeProvider) Name() string { return "fake" }

func (fakeProvider) ListModels(context.Context) ([]provider.Model, error) { return testModels(), nil }

func (p fakeProvider) Complete(_ context.Context, _ provider.Request, emit func(provider.Part) error) error {
	for _, part := range []provider.Part{
		{Type: provider.PartTextDelta, Text: p.text},
		{Type: provider.PartUsage, Usage: session.Usage{Input: 10, Output: 2}},
		{Type: provider.PartStop, StopReason: session.StopEndTurn, StopReasonRaw: "stop"},
	} {
		if err := emit(part); err != nil {
			return err
		}
	}
	return nil
}

type fakePlugin struct{ text string }

func (fakePlugin) Name() string { return "fake" }

func (f fakePlugin) Init(_ context.Context, h plugin.Host) error {
	return h.RegisterProvider(fakeProvider(f))
}

// newServerHarness wires a server the way internal/cli does, dials it as an asking client
// and opens a session on a temp workspace.
func newServerHarness(t *testing.T, text string) (*protocol.Client, protocol.SessionInfo) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	store, err := session.OpenStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Default.Provider = "fake"
	cfg.Default.Model = "m1"
	cfg.Default.Thinking = "off"
	cfg.Permissions.Mode = "strict"
	cfg.MaxTokens = 1000
	preg := plugin.NewRegistry(nil, func(string) {})
	reg := provider.NewRegistry(filepath.Join(dir, "registry.json"))
	srv := server.New(server.Deps{
		Version: "test", Config: cfg, Store: store, Registry: reg, Plugins: preg,
		Gate:  gate.New(nil),
		Hooks: plugin.NewHookRunner(preg, testTimeout, func(string) {}),
	})
	services := srv.PluginServices()
	services.ProvidersChanged = func(ps []provider.Provider) { reg.SetProviders(ps...) }
	preg.SetServices(services)
	preg.Load(ctx, fakePlugin{text: text})
	reg.SetProviders(preg.Providers()...)
	if err := reg.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	serveCtx, cancel := context.WithCancel(ctx)
	cc, sc := protocol.Pipe()
	go func() { _ = srv.Serve(serveCtx, sc) }()
	cl := protocol.NewClient(cc)
	t.Cleanup(func() {
		_ = cl.Close()
		cancel()
		_ = srv.Shutdown(context.Background())
	})
	var hello protocol.ClientHelloResult
	if err := cl.Call(ctx, protocol.MethodClientHello, protocol.ClientHelloParams{Client: "rudy-tui", Version: "test", Asker: true}, &hello); err != nil {
		t.Fatalf("hello: %v", err)
	}
	var info protocol.SessionInfo
	if err := cl.Call(ctx, protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: dir}, &info); err != nil {
		t.Fatalf("open: %v", err)
	}
	return cl, info
}

func TestAgainstARealServerATurnBecomesRows(t *testing.T) {
	cl, info := newServerHarness(t, "Looking at the test first.")
	m := New(Options{
		Config: testConfig(t, nil), Theme: theme.Default(), Keys: keys.Default(),
		Client: cl, Session: info, Models: testModels(), Version: "test", Cwd: info.Workspace.Root,
		Workspace: "rudy main",
	})
	if _, cmd := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24}); cmd != nil {
		t.Fatal("a resize needs no command")
	}
	msg := runCmd(t, m.call(protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID,
		Content:   []session.Block{session.TextBlock("fix the flaky fork test")},
		Source:    session.SourceTyped,
	}))
	res, ok := msg.(CallResultMsg)
	if !ok || res.Err != nil {
		t.Fatalf("submit %v %+v", ok, msg)
	}
	deadline := time.Now().Add(testTimeout)
	for m.turnState != "completed" {
		if time.Now().After(deadline) {
			t.Fatalf("turn stuck in %q after\n%s", m.turnState, m.View().Content)
		}
		n, ok := runCmd(t, pump(cl)).(NotificationMsg)
		if !ok {
			t.Fatal("the pump stopped early")
		}
		m.Update(n)
	}
	v := ansi.Strip(m.View().Content)
	if !strings.Contains(v, "fix the flaky fork test") || !strings.Contains(v, "Looking at the test first.") {
		t.Fatalf("view %q", v)
	}
	if m.usage != (session.Usage{Input: 10, Output: 2}) {
		t.Errorf("usage %+v", m.usage)
	}
}
