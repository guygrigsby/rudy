package app

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/commands"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/server"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
	"github.com/guygrigsby/rudy/internal/tui/input"
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

var (
	testRef  = session.ModelRef{Provider: "fake", Model: "m1"}
	testRef2 = session.ModelRef{Provider: "fake", Model: "m2"}
)

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
// pushes notifications in, hands whatever the pump reads to Update, and records every
// request the client made, so a test can see what the model asked the server to do
// without a real server's timing in the way.
type harness struct {
	t   *testing.T
	m   *Model
	srv protocol.Conn
	cl  *protocol.Client

	mu   sync.Mutex
	reqs []recorded
}

// recorded is one request the client made, as the server side saw it.
type recorded struct {
	Method string
	Params json.RawMessage
}

func newHarness(t *testing.T, over map[string]any) *harness {
	t.Helper()
	return newHarnessPrompt(t, over, "")
}

// newHarnessPrompt is newHarness with a draft already in the editor, the way a positional
// prompt on the command line reaches the client.
func newHarnessPrompt(t *testing.T, over map[string]any, prompt string) *harness {
	t.Helper()
	cc, sc := protocol.Pipe()
	cl := protocol.NewClient(cc)
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
		Prompt:    prompt,
		Workspace: "rudy main*",
	})
	h := &harness{t: t, m: m, srv: sc, cl: cl}
	go h.answerEmpty(sc)
	h.update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return h
}

// answerEmpty is the server side of the pipe: every request is recorded and gets an empty
// result, so a call made as a command finishes instead of hanging.
func (h *harness) answerEmpty(conn protocol.Conn) {
	ctx := context.Background()
	for {
		raw, err := conn.Recv(ctx)
		if err != nil {
			return
		}
		var in struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(raw, &in); err != nil || len(in.ID) == 0 {
			continue
		}
		h.mu.Lock()
		h.reqs = append(h.reqs, recorded{Method: in.Method, Params: in.Params})
		h.mu.Unlock()
		resp, err := protocol.NewResponse(in.ID, nil)
		if err != nil {
			return
		}
		if err := conn.Send(ctx, resp); err != nil {
			return
		}
	}
}

// interrupts are the how of every session.interrupt the client has made, in order.
func (h *harness) interrupts() []session.Interrupt {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []session.Interrupt
	for _, r := range h.reqs {
		if r.Method != protocol.MethodSessionInterrupt {
			continue
		}
		var p protocol.SessionInterruptParams
		if err := json.Unmarshal(r.Params, &p); err != nil {
			h.t.Fatalf("session.interrupt params: %v", err)
		}
		out = append(out, p.How)
	}
	return out
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

// TestPromptSeedsTheEditor pins the positional prompt as a draft, not a turn: it is in the
// editor, it is on screen, and nothing was submitted for it.
func TestPromptSeedsTheEditor(t *testing.T) {
	h := newHarnessPrompt(t, nil, "fix the flaky fork test")
	if got := h.m.ed.Text(); got != "fix the flaky fork test" {
		t.Fatalf("editor holds %q", got)
	}
	if !strings.Contains(ansi.Strip(h.view()), "fix the flaky fork test") {
		t.Errorf("the draft must be on screen:\n%s", h.view())
	}
	if len(h.m.tr.Rows()) != 0 {
		t.Errorf("a seeded draft is not a turn: %+v", h.m.tr.Rows())
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
	if h.m.turn.state != stateStreaming || h.m.turn.turnID != turn {
		t.Fatalf("turn %q %q", h.m.turn.state, h.m.turn.turnID)
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
	// A widget with nothing in it takes no line: emptying is how a plugin clears one.
	h.notify(protocol.NotifyWidgetUpdated, protocol.Widget{
		Owner: "skills", Key: "count", Slot: protocol.SlotBelowEditor, Content: nil,
	})
	lines = h.lines()
	if strings.Contains(ansi.Strip(h.view()), "12 skills") {
		t.Error("an emptied widget must go")
	}
	// The line it held is gone rather than blanked: the editor now sits against the
	// status line with nothing between them. Counting the frame would not show it, since
	// the client draws the whole screen (ADR 0015) however many lines its slots take.
	if editor, status := at("┃"), at("2 servers"); editor != status-1 {
		t.Fatalf("an emptied widget must leave no blank line: editor %d, status %d in\n%s", editor, status, strings.Join(lines, "\n"))
	}
	h.notify(protocol.NotifyWidgetUpdated, protocol.Widget{
		Owner: "skills", Key: "count", Slot: protocol.SlotBelowEditor,
		Content: []protocol.Span{{Text: "12 skills", Role: "muted"}},
	})

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

// TestACommandsNoticeIsDrawnOnce pins the one copy. The server sends a command's notice
// to every attached client as a notice notification and answers command.run with the same
// string, so a client that drew both put every command's notice on screen twice, which is
// what /model did on the real path.
func TestACommandsNoticeIsDrawnOnce(t *testing.T) {
	h := newHarness(t, nil)
	const notice = "/help  List the slash commands\n/plugins  List loaded plugins and their state"
	h.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: levelInfo, Text: notice})
	res, err := json.Marshal(protocol.CommandRunResult{Notice: notice})
	if err != nil {
		t.Fatal(err)
	}
	runAll(t, h.update(CallResultMsg{Method: protocol.MethodCommandRun, Name: "help", Result: res}))
	if len(h.m.notices) != 1 {
		t.Errorf("the notification drew the notice, the answer must not draw it again: %+v", h.m.notices)
	}
}

func TestNoticesAndTheLiveRegionStayInsideTheFrame(t *testing.T) {
	h := newHarness(t, nil)
	h.update(tea.WindowSizeMsg{Width: 80, Height: 12})
	for range 30 {
		h.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "warn", Text: "chatter"})
	}
	h.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "warn", Text: "the newest one"})
	lines := h.lines()
	if len(lines) > 12 {
		t.Fatalf("%d lines over a 12 row frame:\n%s", len(lines), h.view())
	}
	notices := 0
	for _, l := range lines {
		if strings.Contains(ansi.Strip(l), "chatter") {
			notices++
		}
	}
	// ui.notices.max is 3 by default, and the newest is the one kept.
	if notices != 2 || !strings.Contains(ansi.Strip(h.view()), "the newest one") {
		t.Fatalf("%d chatter lines in\n%s", notices, h.view())
	}
	if !strings.Contains(ansi.Strip(h.view()), "strict") {
		t.Errorf("the status line must survive a run of notices:\n%s", h.view())
	}

	long := newHarness(t, nil)
	long.update(tea.WindowSizeMsg{Width: 80, Height: 8})
	for i := range 20 {
		long.appended(session.UserMessage{
			Source:  session.SourceTyped,
			Content: []session.Block{session.TextBlock("line " + strconv.Itoa(i))},
		})
	}
	lines = long.lines()
	if len(lines) > 8 {
		t.Fatalf("%d lines over an 8 row frame:\n%s", len(lines), long.view())
	}
	v := ansi.Strip(long.view())
	if !strings.Contains(v, "line 19") {
		t.Errorf("the newest lines are the ones kept:\n%s", long.view())
	}
	if strings.Contains(v, "line 0") {
		t.Errorf("the oldest lines drop first:\n%s", long.view())
	}
	if !strings.Contains(v, "strict") {
		t.Errorf("the status line must survive a long turn:\n%s", long.view())
	}
}

func TestNoticesMaxZeroDrawsNone(t *testing.T) {
	h := newHarness(t, map[string]any{"ui.notices.max": 0})
	h.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "warn", Text: "chatter"})
	if strings.Contains(ansi.Strip(h.view()), "chatter") {
		t.Fatalf("ui.notices.max 0 draws none:\n%s", h.view())
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
	// Editing still works: a draft that cannot be sent is still a draft to read back and
	// copy out. What refuses is submitting (see turn_test.go).
	h.typeText("nope")
	if h.m.ed.Text() != "nope" {
		t.Errorf("editing must keep working while disconnected: %q", h.m.ed.Text())
	}
	h.press("ctrl+c")
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
	// The terminal reports a click from the top of the screen. Altscreen owns the whole
	// screen; inline anchors its frame at the bottom, so the same row of the frame is a
	// different terminal row in each mode, and the test aims where the user's pointer
	// would actually be.
	for _, render := range []string{"inline", "altscreen"} {
		t.Run(render, func(t *testing.T) {
			h := newHarness(t, map[string]any{"ui.render": render})
			h.appended(session.AssistantMessage{
				Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopToolUse,
				Content: []session.Block{session.ToolUseBlock("t1", "bash", json.RawMessage(`{"command":"ls"}`))},
			})
			h.appended(session.ToolResult{
				ToolUseID: "t1", Outcome: session.OutcomeOK, DurationMS: 3,
				Content: []session.Block{session.TextBlock("a\nb\nc\nd\ne\nf")},
			})
			lines := h.lines()
			row := -1
			for i, l := range lines {
				if strings.Contains(ansi.Strip(l), "▸ bash") {
					row = i
				}
			}
			if row < 0 {
				t.Fatalf("no tool row in\n%s", h.view())
			}
			y := row
			if render == "inline" {
				y += h.m.height - len(lines)
			}
			h.update(tea.MouseClickMsg{X: 2, Y: y, Button: tea.MouseLeft})
			if rows := h.m.tr.Rows(); len(rows) != 1 || !rows[0].Expanded {
				t.Fatalf("click at terminal row %d (frame row %d) did not expand: %+v", y, row, rows)
			}
			h.update(tea.MouseClickMsg{X: 2, Y: y, Button: tea.MouseLeft})
			if rows := h.m.tr.Rows(); rows[0].Expanded {
				t.Fatal("a second click collapses")
			}
			// A click above an inline frame landed in scrollback, which this client does
			// not own and must not act on.
			if render == "inline" {
				h.update(tea.MouseClickMsg{X: 2, Y: 0, Button: tea.MouseLeft})
				if rows := h.m.tr.Rows(); rows[0].Expanded {
					t.Fatal("a click above the frame must do nothing")
				}
			}
		})
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
	if got != " fake:m2  permissive" {
		t.Fatalf("status %q", got)
	}
}

// The rest of the file is the same model over a real server, so the protocol the app
// speaks is the one the server answers rather than a test's idea of it.

// scripted is what the fake provider streams: one step per completion the server asks
// for, cycling once the steps run out so a turn that steers and resumes, and a second
// turn after this one, get the same script again.
type scripted []step

// step emits one completion. id is unique per completion, for the tool_use a step calls.
type step func(ctx context.Context, id string, emit func(provider.Part) error) error

// text is a step that answers with one text block.
func text(s string) step {
	return func(_ context.Context, _ string, emit func(provider.Part) error) error {
		if err := emit(provider.Part{Type: provider.PartTextDelta, Text: s}); err != nil {
			return err
		}
		return stop(emit, session.StopEndTurn)
	}
}

// slowText is a step that streams one delta and then holds the turn open for d, or until
// the turn is interrupted, whichever comes first. It is what lets a test press a key
// while a turn is genuinely running.
func slowText(s string, d time.Duration) step {
	return func(ctx context.Context, _ string, emit func(provider.Part) error) error {
		if err := emit(provider.Part{Type: provider.PartTextDelta, Text: s}); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
		return stop(emit, session.StopEndTurn)
	}
}

// toolCall is a step that calls one tool with input verbatim.
func toolCall(name, input string) step {
	return func(_ context.Context, id string, emit func(provider.Part) error) error {
		for _, part := range []provider.Part{
			{Type: provider.PartToolUseStart, ID: id, Name: name},
			{Type: provider.PartToolUseDelta, ID: id, Text: input},
			{Type: provider.PartToolUseEnd, ID: id},
		} {
			if err := emit(part); err != nil {
				return err
			}
		}
		return stop(emit, session.StopToolUse)
	}
}

// stop ends a step with the usage and stop reason every completion carries.
func stop(emit func(provider.Part) error, reason session.StopReason) error {
	if err := emit(provider.Part{Type: provider.PartUsage, Usage: session.Usage{Input: 10, Output: 2}}); err != nil {
		return err
	}
	return emit(provider.Part{Type: provider.PartStop, StopReason: reason, StopReasonRaw: string(reason)})
}

// fakeProvider runs a script. The server calls Complete from the turn goroutine, so the
// step counter is guarded: two turns in one test share this provider.
type fakeProvider struct {
	mu     sync.Mutex
	script scripted
	calls  int
}

func (*fakeProvider) Name() string { return "fake" }

// ListModels carries a second model the client's own snapshot does not, so a change to it
// is the model-not-found the registry refresh answers.
func (*fakeProvider) ListModels(context.Context) ([]provider.Model, error) {
	return append(testModels(), provider.Model{
		Ref: testRef2, DisplayName: "Fake 2", ContextWindow: 200000,
	}), nil
}

func (p *fakeProvider) Complete(ctx context.Context, _ provider.Request, emit func(provider.Part) error) error {
	p.mu.Lock()
	n := p.calls
	p.calls++
	s := p.script[n%len(p.script)]
	p.mu.Unlock()
	return s(ctx, fmt.Sprintf("call%d", n+1), emit)
}

// fakePlugin registers everything a turn in these tests can reach: the scripted provider,
// one unsafe tool and one safe one (so a test chooses whether the gate asks), and three
// commands, one that answers with a notice, one that submits a prompt and one that
// appends a note.
type fakePlugin struct{ provider *fakeProvider }

func (fakePlugin) Name() string { return "fake" }

func (f fakePlugin) Init(_ context.Context, h plugin.Host) error {
	if err := h.RegisterProvider(f.provider); err != nil {
		return err
	}
	if err := h.RegisterTool(fakeTool("bash", tool.Unsafe)); err != nil {
		return err
	}
	if err := h.RegisterTool(fakeTool("read", tool.Safe)); err != nil {
		return err
	}
	if err := h.RegisterCommand(plugin.Command{
		Name: "notice", Description: "answer with a notice",
		Run: func(_ context.Context, call plugin.CommandCall) (plugin.Action, error) {
			return plugin.Notice{Text: "noticed: " + call.Args}, nil
		},
	}); err != nil {
		return err
	}
	if err := h.RegisterCommand(plugin.Command{
		Name: "ask", Description: "submit a prompt",
		Run: func(_ context.Context, call plugin.CommandCall) (plugin.Action, error) {
			return plugin.SubmitPrompt{Text: call.Args}, nil
		},
	}); err != nil {
		return err
	}
	// A note is how an entry lands between turns, with no turn of its own: the row it
	// makes belongs to the turn that has already been committed.
	return h.RegisterCommand(plugin.Command{
		Name: "note", Description: "append a note",
		Run: func(_ context.Context, call plugin.CommandCall) (plugin.Action, error) {
			if err := h.Note(call.SessionID, call.Args, session.NoteInfo); err != nil {
				return nil, err
			}
			return plugin.NoAction{}, nil
		},
	})
}

// fakeTool answers with what it was asked to do, so a committed tool row has a preview to
// show and a test can see that the tool actually ran.
func fakeTool(name string, safety tool.Safety) tool.Tool {
	return tool.Tool{
		Name: name, Description: name, Safety: safety,
		Schema: json.RawMessage(`{"type":"object"}`),
		Invoke: func(_ context.Context, call tool.Call) (tool.Result, error) {
			var in struct {
				Command string `json:"command"`
				Path    string `json:"path"`
			}
			_ = json.Unmarshal(call.Input, &in)
			return tool.Result{Content: []session.Block{session.TextBlock("ran " + in.Command + in.Path)}}, nil
		},
	}
}

// newServerHarness wires a server the way internal/cli does, dials it as an asking client
// and opens a session on a temp workspace.
func newServerHarness(t *testing.T, script scripted) (*protocol.Client, protocol.SessionInfo) {
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
	// The kernel's own slash commands beside the fake ones, so /help, /fork and /model
	// answer here the way they do in a run: they are how a client discovers commands and
	// how a command opens another session.
	preg.Load(ctx, fakePlugin{provider: &fakeProvider{script: script}}, commands.New())
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

// appHarness is the model driven the way the program drives it: every command Update
// returns is run, in a goroutine of its own, and every message that comes back is fed to
// Update. The notification pump is one of those commands, so notifications arrive in
// order and one at a time, exactly as they do in a run, and a test waits on what the
// server actually said rather than on a sleep.
type appHarness struct {
	t *testing.T
	m *Model
	// msgs is the program's message queue: every command's result lands here and the
	// test goroutine is the only thing that takes them out and folds them in.
	msgs chan tea.Msg
	// prints is what tea.Println was asked to write above the program, which in inline
	// rendering is the committed transcript.
	prints []string
	// log is every entry the server announced, for a test that has to check what was
	// recorded rather than what was drawn. The client never appends one.
	log []session.Entry
	// rested counts the turns that reached a resting state, by turn id.
	rested map[string]bool
}

func newAppHarness(t *testing.T, script scripted) *appHarness {
	t.Helper()
	return newAppHarnessWith(t, script, nil)
}

// newAppHarnessWith is newAppHarness with a last word on the client's config, for the
// tests that turn on altscreen or shorten the double-press window.
func newAppHarnessWith(t *testing.T, script scripted, over func(*config.Config)) *appHarness {
	t.Helper()
	cl, info := newServerHarness(t, script)
	cfg := testConfig(t, nil)
	if over != nil {
		over(cfg)
	}
	m := New(Options{
		Config: cfg, Theme: theme.Default(), Keys: keys.Default(),
		Client: cl, Session: info, Models: testModels(), Version: "test",
		Cwd: info.Workspace.Root, Workspace: "rudy main",
	})
	h := &appHarness{
		t: t, m: m,
		msgs:   make(chan tea.Msg, 256),
		rested: make(map[string]bool),
	}
	h.dispatch(tea.WindowSizeMsg{Width: 80, Height: 24})
	// Init's other commands (the editor's cursor blink, a registry refresh) are not what
	// this harness is about; the pump is, and it is what a program arms first. The command
	// list is the other one a run asks for on connect, and the slash menu is empty without
	// it, so the harness asks too.
	h.exec(pump(cl))
	h.exec(m.call(protocol.MethodCommandList, nil))
	return h
}

// exec runs one command off the update loop, the way the program does, and queues what it
// returns. A batch fans out into one goroutine per command. A queue that has filled up
// drops rather than parking a goroutine forever: nothing here produces 256 pending
// messages, so a full queue means the test is already over.
func (h *appHarness) exec(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		if msg == nil {
			return
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				h.exec(c)
			}
			return
		}
		select {
		case h.msgs <- msg:
		default:
		}
	}()
}

// dispatch folds one message in and runs whatever it produced.
func (h *appHarness) dispatch(msg tea.Msg) {
	h.t.Helper()
	h.record(msg)
	next, cmd := h.m.Update(msg)
	if next != h.m {
		h.t.Fatalf("Update returned another model: %T", next)
	}
	h.exec(cmd)
}

// printLine is the text a tea.Println command produced, false for any other message.
//
// printLineMessage is unexported in bubbletea, so it is recognized by type name and read
// through reflect: reflect.Value.String is the one getter that does not refuse an
// unexported field, which is enough to see what the program was asked to print.
func printLine(msg tea.Msg) (string, bool) {
	if !strings.Contains(fmt.Sprintf("%T", msg), "printLine") {
		return "", false
	}
	v := reflect.ValueOf(msg)
	for i := range v.NumField() {
		if v.Field(i).Kind() == reflect.String {
			return v.Field(i).String(), true
		}
	}
	return "", false
}

// record keeps what a test asks about later: the lines tea.Println was given, the entries
// the server announced and the turns that came to rest.
func (h *appHarness) record(msg tea.Msg) {
	if line, ok := printLine(msg); ok {
		h.prints = append(h.prints, line)
		return
	}
	n, ok := msg.(NotificationMsg)
	if !ok {
		return
	}
	switch n.Method {
	case protocol.NotifyEntryAppended:
		var p protocol.EntryAppended
		if err := json.Unmarshal(n.Params, &p); err != nil {
			h.t.Fatalf("entry.appended: %v", err)
		}
		h.log = append(h.log, p.Entry)
	case protocol.NotifyTurnState:
		var p protocol.TurnStateChanged
		if err := json.Unmarshal(n.Params, &p); err != nil {
			h.t.Fatalf("turn.state: %v", err)
		}
		switch p.State {
		case stateCompleted, stateFailed, stateIdle:
			h.rested[p.TurnID] = true
		}
	}
}

// settle folds in every message already waiting, without blocking on one that is not.
func (h *appHarness) settle() {
	h.t.Helper()
	for {
		select {
		case msg := <-h.msgs:
			h.dispatch(msg)
		default:
			return
		}
	}
}

// wait folds messages in until ok reports the state the test is waiting for, or fails
// after testTimeout naming what never happened.
func (h *appHarness) wait(what string, ok func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(testTimeout)
	for !ok() {
		left := time.Until(deadline)
		if left <= 0 {
			h.t.Fatalf("waited %s for %s; turn %q %q\n%s", testTimeout, what, h.m.turn.state, h.m.turn.turnID, h.view())
		}
		timer := time.NewTimer(left)
		select {
		case msg := <-h.msgs:
			timer.Stop()
			h.dispatch(msg)
		case <-timer.C:
		}
	}
}

// waitTurn waits for the mirrored turn state to be want, the server's own vocabulary.
func (h *appHarness) waitTurn(want string) {
	h.t.Helper()
	h.wait("turn state "+want, func() bool { return h.m.turn.state == want })
}

// waitTurnCount waits until n turns have come to rest.
func (h *appHarness) waitTurnCount(n int) {
	h.t.Helper()
	h.wait(fmt.Sprintf("%d rested turns", n), func() bool { return len(h.rested) >= n })
}

// waitPrinted waits until what has been committed to scrollback carries sub. A commit is
// a command like any other: the turn resting is what starts it, and the print lands a
// message later, so a test waits for the print rather than for the state that caused it.
func (h *appHarness) waitPrinted(sub string) {
	h.t.Helper()
	h.wait("the print of "+strconv.Quote(sub), func() bool { return strings.Contains(h.printed(), sub) })
}

// waitFor waits until the view says what the test is looking for.
func (h *appHarness) waitFor(what string, ok func(view string) bool) {
	h.t.Helper()
	h.wait(what, func() bool { return ok(h.view()) })
}

// press sends one key by the spelling the [keys] grammar uses and folds in whatever came
// back at once.
func (h *appHarness) press(spelling string) {
	h.t.Helper()
	k, err := keys.Parse(spelling)
	if err != nil {
		h.t.Fatalf("key %q: %v", spelling, err)
	}
	h.dispatch(tea.KeyPressMsg(k))
	h.settle()
}

// typeText types s into the editor, leaving normal mode first the way a user would: a
// test that steered a turn left the editor in normal, where letters are vim verbs.
func (h *appHarness) typeText(s string) {
	h.t.Helper()
	if h.m.ed.Mode() == input.ModeNormal {
		h.dispatch(tea.KeyPressMsg{Code: 'i', Text: "i"})
	}
	for _, r := range s {
		h.dispatch(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	h.settle()
}

// view is the frame as drawn, styling stripped: what the user reads.
func (h *appHarness) view() string { return ansi.Strip(h.m.View().Content) }

// printed is everything committed to scrollback so far, styling stripped.
func (h *appHarness) printed() string { return ansi.Strip(strings.Join(h.prints, "\n")) }

func (h *appHarness) editor() *input.Editor { return h.m.ed }

// entries are the log entries the server announced, in order.
func (h *appHarness) entries() []session.Entry { return h.log }

func TestModelChangeToAnUnknownModelRefreshesTheRegistry(t *testing.T) {
	cl, info := newServerHarness(t, scripted{text("ok")})
	m := New(Options{
		Config: testConfig(t, nil), Theme: theme.Default(), Keys: keys.Default(),
		Client: cl, Session: info, Models: testModels(), Version: "test",
		Cwd: info.Workspace.Root, Workspace: "rudy main",
	})
	params, err := json.Marshal(protocol.EntryAppended{
		SessionID: info.SessionID, Entry: entry(t, session.ModelChange{Model: testRef2}),
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := m.notification(protocol.Notification{Method: protocol.NotifyEntryAppended, Params: params})
	if cmd == nil {
		t.Fatal("a model the snapshot does not carry must refresh the registry")
	}
	if m.model.ContextWindow != 0 {
		t.Fatalf("the model is unknown until the refresh answers: %+v", m.model)
	}
	res, ok := runCmd(t, cmd).(CallResultMsg)
	if !ok || res.Err != nil || res.Method != protocol.MethodRegistryList {
		t.Fatalf("registry.list %v %+v", ok, res)
	}
	m.Update(res)
	if m.model.Ref != testRef2 || m.model.ContextWindow != 200000 {
		t.Fatalf("model %+v", m.model)
	}
	// A model the fresh registry still does not carry must not ask again, or a bad id
	// would loop the client against the server for the rest of the session.
	m.session.Model = session.ModelRef{Provider: "fake", Model: "never"}
	if again := m.callResult(res); again != nil {
		t.Error("a refresh that did not help must not refresh again")
	}
}

// TestAgainstARealServerATurnBecomesRows is the whole path in altscreen, where a rested
// turn's rows stay where they are: what the server said becomes rows and usage. Inline
// commits the same rows to scrollback instead, which turn_test.go covers.
func TestAgainstARealServerATurnBecomesRows(t *testing.T) {
	h := newAppHarnessWith(t, scripted{text("Looking at the test first.")}, func(c *config.Config) {
		c.UI.Render = renderAltscreen
	})
	h.typeText("fix the flaky fork test")
	h.press("enter")
	h.waitTurn(stateCompleted)
	v := h.view()
	if !strings.Contains(v, "fix the flaky fork test") || !strings.Contains(v, "Looking at the test first.") {
		t.Fatalf("view %q", v)
	}
	if got := h.printed(); got != "" {
		t.Errorf("altscreen prints nothing to scrollback, printed %q", got)
	}
	if h.m.usage != (session.Usage{Input: 10, Output: 2}) {
		t.Errorf("usage %+v", h.m.usage)
	}
}

// rowsOn are the "row N" indices the frame is showing, in the order they are drawn.
func rowsOn(v string) []int {
	var out []int
	for _, l := range strings.Split(ansi.Strip(v), "\n") {
		_, after, ok := strings.Cut(l, "row ")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(after)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// TestAltscreenFollowsTheNewestRowAndScrolls is ui.render = "altscreen"'s half of the
// transcript. Inline lives in the terminal's own scrollback, so a row that lands is
// already at the bottom; altscreen keeps every row in a viewport, which has to be moved
// to them and moved back by the tui.altScreen actions ADR 0013 decision 5 binds.
func TestAltscreenFollowsTheNewestRowAndScrolls(t *testing.T) {
	h := newHarness(t, map[string]any{"ui.render": renderAltscreen})
	const rows = 60
	for i := range rows {
		h.appended(session.UserMessage{
			Source: session.SourceTyped, Content: []session.Block{session.TextBlock("row " + strconv.Itoa(i))},
		})
	}
	before := rowsOn(h.view())
	if len(before) == 0 || before[len(before)-1] != rows-1 {
		t.Fatalf("altscreen shows the newest row, showing %v", before)
	}
	h.press("pageUp")
	after := rowsOn(h.view())
	if len(after) == 0 || after[0] >= before[0] {
		t.Fatalf("pageUp reveals earlier rows: showed %v, now %v", before, after)
	}
	if slices.Contains(after, rows-1) {
		t.Errorf("a scrolled viewport stays where it was left, showing %v", after)
	}
	// A row landing under a viewport the user scrolled up does not drag it back down; one
	// landing under a viewport at the bottom does.
	h.appended(session.UserMessage{
		Source: session.SourceTyped, Content: []session.Block{session.TextBlock("row " + strconv.Itoa(rows))},
	})
	if got := rowsOn(h.view()); slices.Contains(got, rows) {
		t.Errorf("a scrolled viewport is not dragged to the newest row, showing %v", got)
	}
	h.press("end")
	if got := rowsOn(h.view()); !slices.Contains(got, rows) {
		t.Errorf("end returns to the newest row, showing %v", got)
	}
	h.press("home")
	if got := rowsOn(h.view()); len(got) == 0 || got[0] != 0 {
		t.Errorf("home goes to the oldest row, showing %v", got)
	}
}

// TestInlineLeavesTheScrollKeysToTheEditor is the other side of that binding: pi binds
// home to tui.editor.cursorLineStart as well as to tui.altScreen.top, and inline has no
// viewport, so the key has to reach the editor exactly as it did before.
func TestInlineLeavesTheScrollKeysToTheEditor(t *testing.T) {
	h := newHarness(t, inlineOver())
	h.typeText("hello")
	h.press("home")
	h.typeText("X")
	if got := h.m.ed.Text(); got != "Xhello" {
		t.Errorf("inline leaves home to the editor, text %q", got)
	}
	if off := h.m.vp.YOffset(); off != 0 {
		t.Errorf("inline scrolls no viewport, offset %d", off)
	}
	al := newHarness(t, map[string]any{"ui.render": renderAltscreen})
	al.typeText("hello")
	al.press("home")
	al.typeText("X")
	if got := al.m.ed.Text(); got != "helloX" {
		t.Errorf("altscreen takes home for the viewport, text %q", got)
	}
}

// TestSuspendSuspendsTheProgram is app.suspend, bound to ctrl+z by pi's defaults.
func TestSuspendSuspendsTheProgram(t *testing.T) {
	h := newHarness(t, nil)
	if _, ok := runCmd(t, h.press("ctrl+z")).(tea.SuspendMsg); !ok {
		t.Fatal("app.suspend must suspend the program")
	}
}
