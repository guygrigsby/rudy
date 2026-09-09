package memory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	memory "github.com/aeryx-ai/memory/memory-go"
	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugin/plugintest"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

const (
	fixtureProject = "github.com/golden/alpha"
	fixtureSession = "01K4N0000000000000000000S1"
	entryA2        = "01K4N0000000000000000000A2"
	entryA6        = "01K4N0000000000000000000A6"
	entryA8        = "01K4N0000000000000000000A8"
)

// observerOut is the two observer lines in the shape the fold's OBS_OUT regex accepts:
// "[relevance] text | id,id". The brief's parenthesized form is not what src/fold.mjs
// parses, and the ids have to name entries the delta actually holds.
const observerOut = "[high] pnpm is the package manager here | " + entryA2 + "," + entryA6 + "\n" +
	"[medium] tests run with make test | " + entryA8 + "\n"

// fixtureModel is the model the rudy transcript fixture was recorded with; its slash is what
// makes actorFor's replacement observable.
var fixtureModel = session.ModelRef{Provider: "aperture", Model: "cline-pass/kimi-k3"}

const wantActor = "rudy/cline-pass-kimi-k3"

// recorder is the server side of the plugin: what it summarized, noted and noticed. Every
// field is behind the mutex because a hook handler runs on the runner's goroutine.
type recorder struct {
	mu      sync.Mutex
	notes   []session.Note
	notices []string
	prompts []string
	err     error
}

func (r *recorder) summarize(_ context.Context, prompt string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prompts = append(r.prompts, prompt)
	if r.err != nil {
		return "", r.err
	}
	return observerOut, nil
}

func (r *recorder) note(_ ulid.ULID, pluginName, text string, role session.NoteRole) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notes = append(r.notes, session.Note{Plugin: pluginName, Text: text, Role: role})
	return nil
}

func (r *recorder) notice(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notices = append(r.notices, text)
}

func (r *recorder) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *recorder) takeNotes() []session.Note {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]session.Note(nil), r.notes...)
	r.notes = nil
	return out
}

func (r *recorder) noticed(substr string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.notices {
		if strings.Contains(s, substr) {
			n++
		}
	}
	return n
}

type harness struct {
	t           *testing.T
	b           *memory.Bundle
	projDir     memory.Dir
	sessionsDir string
	home        string
	reg         *plugin.Registry
	runner      *plugin.HookRunner
	rec         *recorder
}

// newHarness builds a bundle, a project directory with one Project concept and a session
// directory holding the rudy transcript fixture, all under a temporary HOME so nothing
// reaches the real ~/.agents/memory and RecallObservation's home check has a real boundary.
func newHarness(t *testing.T, fold map[string]int) *harness {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	root := filepath.Join(home, "memory")
	b := memory.New(root)
	if err := b.Init("", now); err != nil {
		t.Fatalf("bundle init: %v", err)
	}
	projDir, err := b.Dir(fixtureProject)
	if err != nil {
		t.Fatalf("project dir: %v", err)
	}
	body := "Alpha is the golden project."
	res, err := memory.Remember(b, projDir, wantActor, now, memory.RememberInput{Type: "Project", Title: "Alpha", Body: &body})
	if err != nil {
		t.Fatalf("seed project concept: %v", err)
	}
	if _, err := res.Job.Run(b); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	sessionsDir := filepath.Join(home, "sessions")
	sessionDir := filepath.Join(sessionsDir, fixtureSession)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("testdata", "rudy.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, session.LogFile), fixture, 0o644); err != nil {
		t.Fatal(err)
	}

	rec := &recorder{}
	reg := plugin.NewRegistry(nil, rec.notice)
	reg.SetServices(plugin.Services{Note: rec.note})
	cfg := config.MemoryConfig{Dir: root, Enabled: true, Fold: fold}
	reg.Load(context.Background(), New(cfg, sessionsDir, "0.1.0", rec.summarize))
	if st := reg.Statuses()[0]; st.State != plugin.StateReady {
		t.Fatalf("plugin failed to load: %+v", st)
	}
	return &harness{
		t: t, b: b, projDir: projDir, sessionsDir: sessionsDir, home: home,
		reg: reg, runner: plugin.NewHookRunner(reg, 60*time.Second, rec.notice), rec: rec,
	}
}

func (h *harness) open(projectID string) []any {
	h.t.Helper()
	return h.runner.Fire(context.Background(), plugin.HookCall{
		Point:     plugin.HookSessionOpened,
		SessionID: fixtureSession,
		Payload: &plugin.SessionOpenedPayload{
			SessionID: fixtureSession,
			Workspace: session.Workspace{Root: h.home, ProjectID: projectID},
			Model:     fixtureModel,
		},
	})
}

func (h *harness) turn() []any {
	h.t.Helper()
	return h.runner.Fire(context.Background(), plugin.HookCall{
		Point:     plugin.HookTurnCompleted,
		SessionID: fixtureSession,
		Payload:   &plugin.TurnCompletedPayload{SessionID: fixtureSession, TurnID: "t1"},
	})
}

func (h *harness) closed() []any {
	h.t.Helper()
	return h.runner.Fire(context.Background(), plugin.HookCall{
		Point:     plugin.HookSessionClosed,
		SessionID: fixtureSession,
		Payload:   &plugin.SessionClosedPayload{SessionID: fixtureSession},
	})
}

func (h *harness) compaction() []any {
	h.t.Helper()
	return h.runner.Fire(context.Background(), plugin.HookCall{
		Point:     plugin.HookBeforeCompaction,
		SessionID: fixtureSession,
		Payload:   &plugin.BeforeCompactionPayload{SessionID: fixtureSession},
	})
}

// summary is the running Session Summary concept for the fixture session, or nil.
func (h *harness) summary() *memory.Concept {
	h.t.Helper()
	entries, err := h.b.ListConcepts(h.projDir)
	if err != nil {
		h.t.Fatalf("list concepts: %v", err)
	}
	for _, e := range entries {
		if memory.IsSessionSummaryFor(e.Concept, sessionName(fixtureSession)) {
			return e.Concept
		}
	}
	return nil
}

func (h *harness) tool(name string) tool.Tool {
	h.t.Helper()
	for _, tl := range h.reg.Tools() {
		if tl.Name == name {
			return tl
		}
	}
	h.t.Fatalf("tool %s not registered; have %v", name, h.reg.Tools())
	return tool.Tool{}
}

// call invokes a tool the way the loop does, with the fixture session attached.
func (h *harness) call(name, input string) tool.Result {
	h.t.Helper()
	res, err := h.tool(name).Invoke(context.Background(), tool.Call{
		Name:      name,
		Input:     json.RawMessage(input),
		Workspace: session.Workspace{Root: h.home, ProjectID: fixtureProject},
		SessionID: ulid.MustParse(fixtureSession),
	})
	if err != nil {
		h.t.Fatalf("%s: %v", name, err)
	}
	return res
}

func resultText(t *testing.T, res tool.Result) string {
	t.Helper()
	var b strings.Builder
	for _, blk := range res.Content {
		b.WriteString(blk.Text)
	}
	return b.String()
}

// onlyNote is the single note the caller expected, or a failure naming what was recorded.
func onlyNote(t *testing.T, notes []session.Note) session.Note {
	t.Helper()
	if len(notes) != 1 {
		t.Fatalf("notes %+v", notes)
	}
	return notes[0]
}

// TestSessionOpenedInjectsTheRenderedContext holds the SDK as the oracle: whatever
// RenderContext produces for this bundle and session is exactly what the hook returns.
func TestSessionOpenedInjectsTheRenderedContext(t *testing.T) {
	h := newHarness(t, nil)
	results := h.open(fixtureProject)
	if len(results) != 1 {
		t.Fatalf("results %v", results)
	}
	res, ok := results[0].(*plugin.SessionOpenedResult)
	if !ok {
		t.Fatalf("result type %T", results[0])
	}
	want, err := memory.RenderContext(h.b, memory.ContextOptions{ProjectID: fixtureProject, Session: sessionName(fixtureSession)})
	if err != nil {
		t.Fatalf("render context: %v", err)
	}
	if res.Context != want {
		t.Errorf("context\n got %q\nwant %q", res.Context, want)
	}
}

// TestSessionOpenedWithoutProjectIDNoticesOnce covers the workspace with no derivable
// project id: memory is off for that session and the operator hears about it exactly once.
func TestSessionOpenedWithoutProjectIDNoticesOnce(t *testing.T) {
	h := newHarness(t, map[string]int{"observe_after_tokens": 1})
	if results := h.open(""); len(results) != 0 {
		t.Fatalf("results %v", results)
	}
	if n := h.rec.noticed("memory: workspace has no project id; memory is off for this session"); n != 1 {
		t.Fatalf("notices %d, want 1", n)
	}
	if results := h.open(""); len(results) != 0 {
		t.Fatalf("second open results %v", results)
	}
	if n := h.rec.noticed("memory: workspace has no project id"); n != 1 {
		t.Fatalf("notices after second open %d, want 1", n)
	}
	// The hooks stay quiet for that session rather than folding into the bundle root.
	h.turn()
	h.closed()
	if notes := h.rec.takeNotes(); len(notes) != 0 {
		t.Fatalf("notes %+v", notes)
	}
	if h.summary() != nil {
		t.Fatal("wrote a session summary for a session with no project")
	}
}

// TestTurnCompletedFoldsTheTranscript is the whole fold path: the settings from config make
// it due, the rudy format parses the fixture, the actor is the model id cleaned to the OKF
// alphabet, and the observations land in the project's Session Summary.
func TestTurnCompletedFoldsTheTranscript(t *testing.T) {
	h := newHarness(t, map[string]int{"observe_after_tokens": 1})
	h.open(fixtureProject)
	if results := h.turn(); len(results) != 0 {
		t.Fatalf("turn_completed returns nothing, got %v", results)
	}
	note := onlyNote(t, h.rec.takeNotes())
	if note.Text != "memory: folded 2 observations" || note.Role != session.NoteMuted {
		t.Errorf("note %+v", note)
	}
	c := h.summary()
	if c == nil {
		t.Fatal("no session summary")
	}
	if c.Generated.By != wantActor {
		t.Errorf("actor %q, want %q", c.Generated.By, wantActor)
	}
	if c.Status != "draft" {
		t.Errorf("status %q, want draft", c.Status)
	}
	body := memory.ParseSummaryBody(c.Body)
	if len(body.Observations) != 2 {
		t.Fatalf("observations %+v", body.Observations)
	}
	if body.Observations[0].Content != "pnpm is the package manager here" || body.Observations[0].Relevance != "high" {
		t.Errorf("first observation %+v", body.Observations[0])
	}
	if body.Observations[1].Content != "tests run with make test" || body.Observations[1].Relevance != "medium" {
		t.Errorf("second observation %+v", body.Observations[1])
	}
	transcript := filepath.Join(h.sessionsDir, fixtureSession, session.LogFile)
	var haveSession, haveTranscript bool
	for _, s := range c.Sources {
		haveSession = haveSession || s.Resource == sessionName(fixtureSession)
		haveTranscript = haveTranscript || s.Resource == transcript
	}
	if !haveSession || !haveTranscript {
		t.Errorf("sources %+v want %s and %s", c.Sources, sessionName(fixtureSession), transcript)
	}
}

// TestTurnCompletedSkipsWhenNotDue is the other half of the settings assertion: with the
// SDK's default observe_after_tokens the fixture's delta is too small, so the fold is
// skipped, nothing is written and nothing is noted.
func TestTurnCompletedSkipsWhenNotDue(t *testing.T) {
	h := newHarness(t, nil)
	h.open(fixtureProject)
	h.turn()
	if notes := h.rec.takeNotes(); len(notes) != 0 {
		t.Fatalf("a skipped fold noted %+v", notes)
	}
	if h.summary() != nil {
		t.Fatal("a skipped fold wrote a session summary")
	}
}

func TestSettingsFromConfigMapsTheFiveKeys(t *testing.T) {
	got := settingsFrom(map[string]int{
		"observe_after_tokens":       1,
		"reflect_after_tokens":       2,
		"observations_max_tokens":    3,
		"observations_target_tokens": 4,
		"observer_max_tokens":        5,
	})
	want := memory.FoldSettings{
		ObserveAfterTokens: 1, ReflectAfterTokens: 2, ObservationsMaxTokens: 3,
		ObservationsTargetTokens: 4, ObserverMaxTokens: 5,
	}
	if got != want {
		t.Errorf("settings %+v, want %+v", got, want)
	}
	if got := settingsFrom(nil); got != (memory.FoldSettings{}) {
		t.Errorf("empty config settings %+v, want the zero value so the SDK defaults win", got)
	}
}

func TestActorForCleansTheModelID(t *testing.T) {
	got := actorFor(fixtureModel)
	if got != wantActor {
		t.Fatalf("actor %q, want %q", got, wantActor)
	}
	if _, err := memory.ParseActor(got); err != nil {
		t.Fatalf("parse actor %q: %v", got, err)
	}
}

// TestBeforeCompactionSuppliesTheRunningSummary covers both sides: nothing to say before a
// fold has run, the rendered body afterward.
func TestBeforeCompactionSuppliesTheRunningSummary(t *testing.T) {
	h := newHarness(t, map[string]int{"observe_after_tokens": 1})
	h.open(fixtureProject)
	if results := h.compaction(); len(results) != 0 {
		t.Fatalf("results before any fold %v", results)
	}
	h.turn()
	h.rec.takeNotes()
	results := h.compaction()
	if len(results) != 1 {
		t.Fatalf("results %v", results)
	}
	res, ok := results[0].(*plugin.BeforeCompactionResult)
	if !ok {
		t.Fatalf("result type %T", results[0])
	}
	want := "Memory of this session so far:\n\n" + memory.RenderSummaryBody(memory.ParseSummaryBody(h.summary().Body))
	if res.Summary != want {
		t.Errorf("summary\n got %q\nwant %q", res.Summary, want)
	}
}

// TestSessionClosedFinalizesTheSummary: the close fold runs with Finalize, which promotes the
// concept out of draft whether or not the delta was big enough to be due.
func TestSessionClosedFinalizesTheSummary(t *testing.T) {
	h := newHarness(t, map[string]int{"observe_after_tokens": 1})
	h.open(fixtureProject)
	h.turn()
	h.rec.takeNotes()
	h.closed()
	c := h.summary()
	if c == nil {
		t.Fatal("no session summary")
	}
	if c.Status != "stable" {
		t.Errorf("status %q, want stable", c.Status)
	}
}

// TestFoldFailureNotesAndRecovers: a summarizer error is a warn note, never a turn failure,
// and it leaves the checkpoint where the next turn can fold the same delta again.
func TestFoldFailureNotesAndRecovers(t *testing.T) {
	h := newHarness(t, map[string]int{"observe_after_tokens": 1})
	h.open(fixtureProject)
	h.rec.fail(errors.New("boom"))
	if results := h.turn(); len(results) != 0 {
		t.Fatalf("results %v", results)
	}
	note := onlyNote(t, h.rec.takeNotes())
	if note.Text != "memory: fold failed: boom" || note.Role != session.NoteWarn {
		t.Errorf("note %+v", note)
	}
	if msg := foldStateError(t, h.b, sessionName(fixtureSession)); msg != "boom" {
		t.Errorf("checkpoint lastError %q, want boom", msg)
	}
	h.rec.fail(nil)
	h.turn()
	note = onlyNote(t, h.rec.takeNotes())
	if note.Text != "memory: folded 2 observations" || note.Role != session.NoteMuted {
		t.Errorf("note after recovery %+v", note)
	}
}

// TestFoldHardErrorMarksTheCheckpoint drives the path where RunFoldJob itself fails rather
// than the summarizer: the plugin has to record it against the session with MarkFoldError,
// since the SDK never got far enough to checkpoint anything.
func TestFoldHardErrorMarksTheCheckpoint(t *testing.T) {
	h := newHarness(t, map[string]int{"observe_after_tokens": 1})
	h.open("bad project id")
	if results := h.turn(); len(results) != 0 {
		t.Fatalf("results %v", results)
	}
	note := onlyNote(t, h.rec.takeNotes())
	if !strings.HasPrefix(note.Text, "memory: fold failed: ") || note.Role != session.NoteWarn {
		t.Fatalf("note %+v", note)
	}
	want := strings.TrimPrefix(note.Text, "memory: fold failed: ")
	if msg := foldStateError(t, h.b, sessionName(fixtureSession)); msg != want {
		t.Errorf("checkpoint lastError %q, want %q", msg, want)
	}
}

// foldStateError reads the message the SDK's own checkpoint holds for a session.
func foldStateError(t *testing.T, b *memory.Bundle, sess string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(b.Root, ".state"))
	if err != nil {
		t.Fatalf("state dir: %v", err)
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(b.Root, ".state", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var st struct {
			Session   string `json:"session"`
			LastError *struct {
				Message string `json:"message"`
			} `json:"lastError"`
		}
		if json.Unmarshal(raw, &st) != nil || st.Session != sess || st.LastError == nil {
			continue
		}
		return st.LastError.Message
	}
	t.Fatalf("no checkpoint with an error for %s", sess)
	return ""
}

func TestRememberWritesAndRunsTheJob(t *testing.T) {
	h := newHarness(t, nil)
	h.open(fixtureProject)
	got := resultText(t, h.call("memory_remember", `{"type":"Feedback","title":"Use pnpm","body":"never npm"}`))
	rel := "projects/" + fixtureProject + "/feedback/use-pnpm.md"
	if got != "remembered "+rel {
		t.Errorf("result %q, want %q", got, "remembered "+rel)
	}
	if _, err := os.Stat(filepath.Join(h.projDir.Abs, "feedback", "use-pnpm.md")); err != nil {
		t.Errorf("concept file: %v", err)
	}
	index, err := os.ReadFile(filepath.Join(h.projDir.Abs, "index.md"))
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if !strings.Contains(string(index), "use-pnpm") {
		t.Errorf("index does not name the concept:\n%s", index)
	}
	// A second write of the same title revises rather than creates.
	got = resultText(t, h.call("memory_remember", `{"type":"Feedback","title":"Use pnpm","body":"still never npm"}`))
	if got != "revised "+rel {
		t.Errorf("second result %q, want %q", got, "revised "+rel)
	}
}

func TestRememberRootScopeWritesUnderTheBundleRoot(t *testing.T) {
	h := newHarness(t, nil)
	h.open(fixtureProject)
	got := resultText(t, h.call("memory_remember", `{"type":"Feedback","title":"Use pnpm","body":"never npm","scope":"root"}`))
	if got != "remembered feedback/use-pnpm.md" {
		t.Errorf("result %q", got)
	}
	if _, err := os.Stat(filepath.Join(h.b.Root, "feedback", "use-pnpm.md")); err != nil {
		t.Errorf("root concept file: %v", err)
	}
}

func TestRememberMalformedTypeIsAnErrorResult(t *testing.T) {
	h := newHarness(t, nil)
	h.open(fixtureProject)
	res := h.call("memory_remember", `{"type":"Nonsense","title":"Use pnpm"}`)
	if !res.IsError {
		t.Fatalf("result %+v", res)
	}
	if !strings.Contains(resultText(t, res), "Nonsense") {
		t.Errorf("result %q", resultText(t, res))
	}
}

func TestToolsRefuseASessionWithNoProject(t *testing.T) {
	h := newHarness(t, nil)
	h.open("")
	for _, tc := range []struct{ name, input string }{
		{"memory_remember", `{"type":"Feedback","title":"Use pnpm"}`},
		{"memory_recall", `{"query":"pnpm"}`},
		{"memory_recall_observation", `{"id":"abc"}`},
	} {
		res := h.call(tc.name, tc.input)
		if !res.IsError || !strings.Contains(resultText(t, res), "memory is off for this session") {
			t.Errorf("%s: %+v %q", tc.name, res, resultText(t, res))
		}
	}
}

func TestRecallAnswersJSONHits(t *testing.T) {
	h := newHarness(t, nil)
	h.open(fixtureProject)
	h.call("memory_remember", `{"type":"Feedback","title":"Use pnpm","body":"never npm"}`)
	var hits []memory.Hit
	if err := json.Unmarshal([]byte(resultText(t, h.call("memory_recall", `{"query":"pnpm"}`))), &hits); err != nil {
		t.Fatalf("unmarshal hits: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	var found bool
	for _, hit := range hits {
		found = found || hit.Title == "Use pnpm"
	}
	if !found {
		t.Errorf("hits %+v", hits)
	}
}

func TestRecallObservationAnswersTheSourceEntries(t *testing.T) {
	h := newHarness(t, map[string]int{"observe_after_tokens": 1})
	h.open(fixtureProject)
	h.turn()
	h.rec.takeNotes()
	obs := memory.ParseSummaryBody(h.summary().Body).Observations
	if len(obs) != 2 {
		t.Fatalf("observations %+v", obs)
	}
	var hit memory.ObservationHit
	raw := resultText(t, h.call("memory_recall_observation", `{"id":"`+obs[0].ID+`"}`))
	if err := json.Unmarshal([]byte(raw), &hit); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}
	if hit.ID != obs[0].ID {
		t.Errorf("id %q, want %q", hit.ID, obs[0].ID)
	}
	if hit.Reason != "" {
		t.Fatalf("reason %q; the transcript should be readable under HOME", hit.Reason)
	}
	if len(hit.Entries) != 2 {
		t.Fatalf("entries %+v, want the two cited transcript entries", hit.Entries)
	}
	if !strings.Contains(hit.Entries[0].Text, "use pnpm here, never npm") {
		t.Errorf("entry text %q", hit.Entries[0].Text)
	}
}

func TestMemoryCommandReportsRootProjectAndCounts(t *testing.T) {
	h := newHarness(t, nil)
	h.open(fixtureProject)
	h.call("memory_remember", `{"type":"Feedback","title":"Use pnpm","body":"never npm"}`)
	cmds := h.reg.Commands()
	if len(cmds) != 1 || cmds[0].Name != "memory" {
		t.Fatalf("commands %+v", cmds)
	}
	action, err := cmds[0].Run(context.Background(), plugin.CommandCall{
		SessionID: ulid.MustParse(fixtureSession),
		Workspace: session.Workspace{Root: h.home, ProjectID: fixtureProject},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	notice, ok := action.(plugin.Notice)
	if !ok {
		t.Fatalf("action %T", action)
	}
	for _, want := range []string{h.b.Root, fixtureProject, "Project 1", "Feedback 1"} {
		if !strings.Contains(notice.Text, want) {
			t.Errorf("notice %q missing %q", notice.Text, want)
		}
	}
}

func TestDisabledRegistersNothing(t *testing.T) {
	h := &plugintest.Host{Name: "memory"}
	cfg := config.MemoryConfig{Dir: t.TempDir(), Enabled: false}
	if err := New(cfg, t.TempDir(), "0.1.0", nil).Init(context.Background(), h); err != nil {
		t.Fatalf("init: %v", err)
	}
	if len(h.Hooks)+len(h.RegisteredTools)+len(h.RegisteredCommands)+len(h.Notices) != 0 {
		t.Errorf("registered %+v", h)
	}
}

func TestMissingBundleNoticesAndRegistersNothing(t *testing.T) {
	h := &plugintest.Host{Name: "memory"}
	dir := t.TempDir()
	cfg := config.MemoryConfig{Dir: dir, Enabled: true}
	if err := New(cfg, t.TempDir(), "0.1.0", nil).Init(context.Background(), h); err != nil {
		t.Fatalf("init: %v", err)
	}
	if len(h.Hooks)+len(h.RegisteredTools)+len(h.RegisteredCommands) != 0 {
		t.Errorf("registered %+v", h)
	}
	want := "memory: no bundle at " + memory.New(memory.ResolveRoot(dir, os.Getenv)).Root + "; run memory init"
	if len(h.Notices) != 1 || h.Notices[0] != want {
		t.Errorf("notices %q, want %q", h.Notices, want)
	}
}
