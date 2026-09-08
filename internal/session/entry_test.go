package session

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func mustULID(t *testing.T, s string) ulid.ULID {
	t.Helper()
	id, err := ulid.ParseStrict(s)
	if err != nil {
		t.Fatalf("parse ulid %q: %v", s, err)
	}
	return id
}

func TestEnumsValid(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
		v    interface{ Valid() bool }
	}{
		{"mode strict", true, ModeStrict},
		{"mode bogus", false, Mode("loose")},
		{"thinking high", true, ThinkingHigh},
		{"thinking bogus", false, ThinkingLevel("max")},
		{"source steer", true, SourceSteer},
		{"stop interrupted", true, StopInterrupted},
		{"stop bogus", false, StopReason("done")},
		{"outcome lost", true, OutcomeLost},
		{"decision allow", true, Allow},
		{"decided by hook", true, ByHook},
		{"decided by bogus", false, DecidedBy("user")},
		{"scope session", true, ScopeSession},
		{"interrupt cancel", true, InterruptCancel},
		{"error class transport", true, ErrTransport},
		{"error class plugin", true, ErrPlugin},
		{"note role muted", true, NoteMuted},
		{"note role bogus", false, NoteRole("loud")},
		{"block tool_use", true, BlockToolUse},
		{"block bogus", false, BlockType("audio")},
		{"kind note", true, KindNote},
		{"kind bogus", false, Kind("event")},
	}
	for _, c := range cases {
		if got := c.v.Valid(); got != c.ok {
			t.Errorf("%s: Valid() = %v, want %v", c.name, got, c.ok)
		}
	}
}

func TestModelRefString(t *testing.T) {
	ref := ModelRef{Provider: "aperture", Model: "cline-pass/kimi-k3"}
	if got := ref.String(); got != "aperture:cline-pass/kimi-k3" {
		t.Fatalf("String() = %q", got)
	}
	back, ok := ParseModelRef("aperture:cline-pass/kimi-k3")
	if !ok || back != ref {
		t.Fatalf("ParseModelRef = %+v, %v", back, ok)
	}
	if _, ok := ParseModelRef("kimi-k3"); ok {
		t.Fatal("bare id must not parse as a ModelRef")
	}
}

func TestUsageAdd(t *testing.T) {
	a := Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4}
	b := Usage{Input: 10, Output: 20, CacheRead: 30, CacheWrite: 40}
	if got := a.Add(b); got != (Usage{11, 22, 33, 44}) {
		t.Fatalf("Add = %+v", got)
	}
}

func TestBlockMarshalEmitsOnlyVariantFields(t *testing.T) {
	img := Block{Type: BlockImage, MediaType: "image/png", SHA256: "abc", Text: "leak", ID: "leak"}
	b, err := json.Marshal(img)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"type":"image","media_type":"image/png","sha256":"abc"}`; got != want {
		t.Fatalf("image block = %s, want %s", got, want)
	}
	var bad Block
	if err := json.Unmarshal([]byte(`{"type":"audio"}`), &bad); err == nil {
		t.Fatal("unknown block type must fail to unmarshal")
	}
}

func TestNewIDMonotonic(t *testing.T) {
	prev := NewID()
	for range 1000 {
		next := NewID()
		if next.Compare(prev) <= 0 {
			t.Fatalf("ids not increasing: %s then %s", prev, next)
		}
		prev = next
	}
}

func TestEntryRoundTripAllKinds(t *testing.T) {
	id1 := mustULID(t, "01K4M0A7Q8ZJ3N6R9T2V5X8B1D")
	id2 := mustULID(t, "01K4M0A8Q8ZJ3N6R9T2V5X8B1D")
	at := time.Date(2026, 9, 7, 20, 30, 0, 123456789, time.FixedZone("MDT", -6*3600))
	payloads := []Payload{
		SessionOpened{SchemaVersion: 1, RudyVersion: "0.1.0", Workspace: Workspace{Root: "/w", GitRoot: "/w", ProjectID: "local/w"}, Model: ModelRef{"aperture", "cline-pass/kimi-k3"}, Thinking: ThinkingHigh, Mode: ModeStrict, Agent: "default"},
		ForkPoint{ParentSessionID: id1, ParentEntryID: id2},
		UserMessage{Source: SourceTyped, Content: []Block{TextBlock("fix the flaky fork test")}},
		AssistantMessage{Model: ModelRef{"aperture", "cline-pass/kimi-k3"}, Thinking: ThinkingHigh, Content: []Block{TextBlock("Looking."), ToolUseBlock("toolu_01", "bash", json.RawMessage(`{"command":"go test ./..."}`))}, Usage: Usage{Input: 1200, Output: 80}, StopReason: StopToolUse, StopReasonRaw: "tool_calls"},
		PermissionDecision{ToolUseID: "toolu_01", Tool: "bash", Mode: ModeStrict, Matcher: Matcher{Tool: "bash", Prefix: "go test"}, Decision: Allow, DecidedBy: ByAsker, Scope: ScopeSession, Reason: "allow for session"},
		ToolResult{ToolUseID: "toolu_01", Outcome: OutcomeOK, Content: []Block{TextBlock("ok\n")}, DurationMS: 412},
		ModelChange{Model: ModelRef{"aperture", "gpt-5.6-sol"}},
		ModeChange{Mode: ModePermissive},
		ThinkingChange{Thinking: ThinkingMedium},
		TitleChange{Title: "fork off-by-one"},
		Compaction{Summary: "earlier work", FirstEntryID: id1, LastEntryID: id2, Model: ModelRef{"aperture", "cline-pass/kimi-k3"}, Usage: Usage{Input: 40000, Output: 900}},
		TurnInterrupted{TurnID: id1, How: InterruptCancel},
		TurnFailed{TurnID: id1, Class: ErrProvider, Message: "502 after 5 attempts", Retries: 5},
		Note{Plugin: "memory", Text: "3 concepts folded", Role: NoteMuted},
	}
	for _, p := range payloads {
		e := Entry{ID: id1, At: at, Kind: p.Kind(), Payload: p}
		line, err := e.MarshalJSON()
		if err != nil {
			t.Fatalf("%s: marshal: %v", p.Kind(), err)
		}
		if !strings.HasPrefix(string(line), `{"id":"01K4M0A7Q8ZJ3N6R9T2V5X8B1D","at":"2026-09-07T20:30:00.123456789-06:00","kind":"`+string(p.Kind())+`"`) {
			t.Fatalf("%s: envelope prefix wrong: %s", p.Kind(), line)
		}
		var back Entry
		if err := back.UnmarshalJSON(line); err != nil {
			t.Fatalf("%s: unmarshal: %v\n%s", p.Kind(), err, line)
		}
		again, err := back.MarshalJSON()
		if err != nil {
			t.Fatalf("%s: re-marshal: %v", p.Kind(), err)
		}
		if !bytes.Equal(line, again) {
			t.Fatalf("%s: not stable:\n%s\n%s", p.Kind(), line, again)
		}
	}
}

func TestEntryToolUseInputByteExact(t *testing.T) {
	raw := json.RawMessage(`{"path": "go.mod",  "html":"<b>&</b>", "z":1, "a":2}`)
	e := Entry{ID: NewID(), At: time.Now(), Kind: KindAssistantMessage, Payload: AssistantMessage{
		Model: ModelRef{"aperture", "m"}, Thinking: ThinkingOff,
		Content:    []Block{ToolUseBlock("t1", "read", raw)},
		StopReason: StopToolUse, StopReasonRaw: "tool_calls",
	}}
	line, err := e.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(line, []byte(`"input":`+string(raw))) {
		t.Fatalf("input bytes changed on the line:\n%s", line)
	}
	var back Entry
	if err := back.UnmarshalJSON(line); err != nil {
		t.Fatal(err)
	}
	got := back.Payload.(AssistantMessage).Content[0].Input
	if !bytes.Equal(got, raw) {
		t.Fatalf("input after round trip = %s, want %s", got, raw)
	}
}

func TestEntryTextAndSignatureVerbatim(t *testing.T) {
	e := Entry{ID: NewID(), At: time.Now(), Kind: KindAssistantMessage, Payload: AssistantMessage{
		Model: ModelRef{"aperture", "m"}, Thinking: ThinkingHigh,
		Content: []Block{
			{Type: BlockThinking, Text: "plan <x>", Signature: "EqQBCkYIBRgCIkD+/abc=="},
			TextBlock("see <b>bold</b> & done"),
		},
		StopReason: StopEndTurn, StopReasonRaw: "stop",
	}}
	line, err := e.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"signature":"EqQBCkYIBRgCIkD+/abc=="`, `"text":"see <b>bold</b> & done"`, `"text":"plan <x>"`} {
		if !bytes.Contains(line, []byte(want)) {
			t.Fatalf("missing %s in\n%s", want, line)
		}
	}
}

func TestEntryUnmarshalRejects(t *testing.T) {
	cases := map[string]string{
		"unknown kind":  `{"id":"01K4M0A7Q8ZJ3N6R9T2V5X8B1D","at":"2026-09-07T20:30:00Z","kind":"event"}`,
		"invalid enum":  `{"id":"01K4M0A7Q8ZJ3N6R9T2V5X8B1D","at":"2026-09-07T20:30:00Z","kind":"mode_change","mode":"loose"}`,
		"invalid block": `{"id":"01K4M0A7Q8ZJ3N6R9T2V5X8B1D","at":"2026-09-07T20:30:00Z","kind":"user_message","source":"typed","content":[{"type":"audio"}]}`,
		"bad id":        `{"id":"nope","at":"2026-09-07T20:30:00Z","kind":"mode_change","mode":"off"}`,
	}
	for name, line := range cases {
		var e Entry
		if err := e.UnmarshalJSON([]byte(line)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
