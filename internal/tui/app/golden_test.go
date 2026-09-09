package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	teatest "github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tui/keys"
)

// The frame every golden is taken at. Wide enough that the design's rows do not wrap on
// a word the transcript's own goldens keep whole, tall enough for the whole turn.
const (
	goldenWidth  = 100
	goldenHeight = 30
)

// The design's screen, entry by entry (docs/specs/2026-09-07-rudy-design.md, "Client").
// The same conversation internal/tui/transcript's screen_default golden renders, so the
// two files can be read side by side; the constants are repeated rather than shared
// because they are another package's test fixtures.
const (
	goldenPrompt = "fix the flaky fork test"
	goldenOpener = "Looking at the test first."
	goldenCloser = "Off by one in the slice bound. Fixed and green."
	goldenThink  = "weighing the fork bound"
	bashInput    = `{"command":"go test ./internal/session -run TestFork -count=3"}`
	bashOutput   = "--- FAIL: TestFork (0.01s)\nfork_test.go:41: want 3 entries, got 2\n"
	editInput    = `{"path":"internal/session/fork.go","old":"\tentries := s.entries[:at]\n","new":"\tentries := s.entries[:at+1]\n"}`
	editOutput   = "replaced 1 occurrence in internal/session/fork.go"
	dangerInput  = `{"command":"rm -rf ./bin"}`
)

// goldenUsage is the last assistant message's usage: 42000 prompt tokens of a 100000
// window and $0.08 spent, the two numbers the design's status line carries.
var goldenUsage = session.Usage{Input: 20000, Output: 1000, CacheRead: 22000}

// namedTB renames a test for the golden it writes. teatest names its files after the test
// that ran; a golden here is named for the config permutation it proves, so the whole set
// reads as the list of permutations rather than as a list of function names.
type namedTB struct {
	testing.TB
	name string
}

func (n namedTB) Name() string { return n.name }

// screen drives the design's conversation through a real Bubble Tea program under the
// config over describes and returns the final frame. drive is what this permutation does
// on top of the shared conversation: a key, another notification, nothing.
//
// The bytes are stable on any terminal. Colors come from lipgloss styles, which render
// the theme's hex values as truecolor sequences whatever the terminal is; the program is
// pinned to colorprofile.TrueColor so its own writer cannot downsample them either, and
// TestMain pins TERM, COLORTERM and CLICOLOR_FORCE for everything that still reads the
// environment (glamour's code fences).
func screen(t *testing.T, over map[string]any, drive func(tm *teatest.TestModel, sid string)) string {
	t.Helper()
	h := newHarness(t, over)
	tm := teatest.NewTestModel(t, h.m,
		teatest.WithInitialTermSize(goldenWidth, goldenHeight),
		teatest.WithProgramOptions(tea.WithColorProfile(colorprofile.TrueColor)),
	)
	sid := h.m.session.SessionID
	for _, msg := range designTurn(t, sid) {
		tm.Send(msg)
	}
	if drive != nil {
		drive(tm, sid)
	}
	if err := tm.Quit(); err != nil {
		t.Fatalf("quit: %v", err)
	}
	final, ok := tm.FinalModel(t, teatest.WithFinalTimeout(testTimeout)).(*Model)
	if !ok {
		t.Fatal("the program returned another model")
	}
	// The editor's cursor blinks on a 530ms timer, and a blink that landed while the
	// program ran would flip one cell of the frame. Focusing it again puts the cursor
	// back in its shown state, so the golden is the frame and not the clock.
	_ = final.ed.Focus()
	// The turn spinner is a clock too: the permutation that leaves a turn running spins
	// one, and a tick that landed before the program quit would move the glyph. A fresh
	// spinner stands at its first frame, which is the frame these goldens pin.
	final.spin = newSpinner()
	return final.View().Content
}

// golden compares got against testdata/<name>.golden through teatest, or rewrites it
// under -update (the flag charmbracelet/x/exp/golden registers).
func golden(t *testing.T, name, got string) {
	t.Helper()
	teatest.RequireEqualOutput(namedTB{TB: t, name: name}, []byte(got))
}

// notify is one server notification as the update loop receives it.
func notify(t *testing.T, method string, params any) NotificationMsg {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("%s params: %v", method, err)
	}
	return NotificationMsg{Method: method, Params: raw}
}

// appendedMsg is one entry.appended notification for sid.
func appendedMsg(t *testing.T, sid string, p session.Payload) NotificationMsg {
	t.Helper()
	return notify(t, protocol.NotifyEntryAppended, protocol.EntryAppended{SessionID: sid, Entry: entry(t, p)})
}

// designTurn is the design's turn: the prompt, the thinking the default config hides, an
// answer, a bash call that fails, the edit that fixes it and the closing paragraph. No
// turn.state comes with it: inline rendering commits a turn that rests to scrollback, and
// what these goldens pin is the live region, which is the screen the design draws.
func designTurn(t *testing.T, sid string) []tea.Msg {
	t.Helper()
	return []tea.Msg{
		appendedMsg(t, sid, session.UserMessage{Source: session.SourceTyped, Content: []session.Block{session.TextBlock(goldenPrompt)}}),
		appendedMsg(t, sid, session.AssistantMessage{
			Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopToolUse,
			Content: []session.Block{
				{Type: session.BlockThinking, Text: goldenThink},
				session.TextBlock(goldenOpener),
				session.ToolUseBlock("t1", "bash", json.RawMessage(bashInput)),
			},
		}),
		appendedMsg(t, sid, session.ToolResult{
			ToolUseID: "t1", Outcome: session.OutcomeError, DurationMS: 120,
			Content: []session.Block{session.TextBlock(bashOutput)},
		}),
		appendedMsg(t, sid, session.AssistantMessage{
			Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopToolUse,
			Content: []session.Block{session.ToolUseBlock("t2", "edit", json.RawMessage(editInput))},
		}),
		appendedMsg(t, sid, session.ToolResult{
			ToolUseID: "t2", Outcome: session.OutcomeOK, DurationMS: 42,
			Content: []session.Block{session.TextBlock(editOutput)},
		}),
		appendedMsg(t, sid, session.AssistantMessage{
			Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopEndTurn,
			Content: []session.Block{session.TextBlock(goldenCloser)},
			Usage:   goldenUsage,
		}),
	}
}

// key is one key press by the spelling the [keys] grammar uses.
func key(t *testing.T, spelling string) tea.Msg {
	t.Helper()
	k, err := keys.Parse(spelling)
	if err != nil {
		t.Fatalf("key %q: %v", spelling, err)
	}
	return tea.KeyPressMsg(k)
}

// TestNoPaintedBackgrounds gates the design's "no painted backgrounds, the terminal's
// black shows through" over every frame these goldens pin. ui.diff.style = "background"
// is the one path allowed to paint one.
func TestNoPaintedBackgrounds(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.golden"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no goldens: %v", err)
	}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Base(f) == "diff_background.golden"
		if got := bytes.Contains(body, []byte("48;2;")); got != want {
			t.Errorf("%s paints a background: %v, want %v", f, got, want)
		}
	}
}

func TestGoldenDefault(t *testing.T) {
	golden(t, "default_altscreen", screen(t, nil, nil))
}

func TestGoldenToolsExpanded(t *testing.T) {
	golden(t, "tools_expanded", screen(t, map[string]any{"ui.transcript.tool_collapsed": false}, nil))
}

func TestGoldenDiffBackground(t *testing.T) {
	golden(t, "diff_background", screen(t, map[string]any{"ui.diff.style": "background"}, nil))
}

func TestGoldenThinkingShown(t *testing.T) {
	golden(t, "thinking_shown", screen(t, map[string]any{"ui.transcript.thinking": "shown"}, nil))
}

// TestGoldenInline is the opt-in half of ADR 0015: the frame anchors at the bottom of
// whatever the terminal already held, and a rested turn's rows would have been committed
// out of it. Nothing here rests one, so what it pins is the live region.
func TestGoldenInline(t *testing.T) {
	golden(t, "inline", screen(t, map[string]any{"ui.render": "inline"}, nil))
}

func TestGoldenVimModes(t *testing.T) {
	golden(t, "vim_normal", screen(t, nil, func(tm *teatest.TestModel, _ string) {
		tm.Send(key(t, "escape"))
	}))
	golden(t, "vim_visual", screen(t, nil, func(tm *teatest.TestModel, _ string) {
		tm.Send(key(t, "escape"))
		tm.Send(tea.KeyPressMsg{Code: 'v', Text: "v"})
	}))
	golden(t, "vim_off", screen(t, map[string]any{"ui.vim": false}, nil))
}

// TestGoldenPermissionPrompt pins the question standing where the tool's row will be,
// which is what an unsafe tool costs before it runs. The session opened strict (the
// harness's SessionInfo), which is the only mode the client reads.
func TestGoldenPermissionPrompt(t *testing.T) {
	golden(t, "permission_prompt", screen(t, nil, func(tm *teatest.TestModel, sid string) {
		turn := session.NewID().String()
		tm.Send(appendedMsg(t, sid, session.AssistantMessage{
			Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopToolUse,
			Content: []session.Block{session.ToolUseBlock("t3", "bash", json.RawMessage(dangerInput))},
			Usage:   goldenUsage,
		}))
		tm.Send(notify(t, protocol.NotifyTurnState, protocol.TurnStateChanged{
			SessionID: sid, TurnID: turn, State: stateAwaitingPermission,
		}))
		tm.Send(notify(t, protocol.NotifyPermissionRequested, protocol.PermissionRequested{
			SessionID: sid, TurnID: turn, ToolUseID: "t3", Tool: "bash",
			Input:   json.RawMessage(dangerInput),
			Matcher: session.Matcher{Tool: "bash", Prefix: "rm -rf"},
		}))
	}))
}

// TestGoldenSlashMenu pins the completion above the editor: what command.list answered
// with what each command does, the keyboard's row accented, the client's own two under the
// registered ones, and the draft still in the editor under it all (ADR 0015 decision 4).
func TestGoldenSlashMenu(t *testing.T) {
	golden(t, "slash_menu", screen(t, nil, func(tm *teatest.TestModel, _ string) {
		list, err := json.Marshal(protocol.CommandListResult{Commands: []protocol.CommandInfo{
			{Name: "model", Description: "Switch this session's model: /model <provider:id or unique id>"},
			{Name: "help", Description: "List the slash commands"},
			{Name: "fork", Description: "Fork this session at an entry: /fork [entry id], default the newest"},
			{Name: "plugins", Description: "List loaded plugins and their state"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		tm.Send(CallResultMsg{Method: protocol.MethodCommandList, Result: list})
		tm.Send(tea.KeyPressMsg{Code: '/', Text: "/"})
	}))
}

// TestGoldenWidgetsAndStatus pins a plugin's status item and its widgets where config put
// them: the header widget in the header slot ui.layout.slots added, the hint above the
// editor, and the memory:servers cell in the place ui.status.items gave it.
func TestGoldenWidgetsAndStatus(t *testing.T) {
	over := map[string]any{
		"ui.layout.slots": []string{"header", "transcript", "input", "status"},
		"ui.status.items": []string{"vim_mode", "model", "memory:servers", "workspace"},
	}
	golden(t, "widgets_and_status", screen(t, over, func(tm *teatest.TestModel, _ string) {
		tm.Send(notify(t, protocol.NotifyWidgetUpdated, protocol.Widget{
			Owner: "memory", Key: "banner", Slot: protocol.SlotHeader,
			Content: []protocol.Span{{Text: "rudy ", Role: "muted"}, {Text: "v0", Role: "accent"}},
		}))
		tm.Send(notify(t, protocol.NotifyWidgetUpdated, protocol.Widget{
			Owner: "memory", Key: "hint", Slot: protocol.SlotAboveEditor,
			Content: []protocol.Span{{Text: "3 memories loaded", Role: "muted"}},
		}))
		tm.Send(notify(t, protocol.NotifyStatusUpdated, protocol.StatusUpdated{Items: []protocol.StatusItem{
			{Owner: "memory", Key: "servers", Content: []protocol.Span{{Text: "2 servers", Role: "success"}}},
		}}))
	}))
}
