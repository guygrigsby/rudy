package memory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	parentSession  = "01K4N0000000000000000000P1"
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
// field is behind the mutex because folds and write jobs run on their own goroutines.
type recorder struct {
	mu      sync.Mutex
	notes   []session.Note
	notices []string
	prompts []string
	err     error

	gate    chan struct{} // non-nil makes summarize block until it is closed
	entered chan struct{} // closed the first time a summarize call reaches the gate
	once    *sync.Once
	boom    bool // makes summarize panic, standing in for a bug anywhere under the fold
}

func (r *recorder) summarize(ctx context.Context, prompt string) (string, error) {
	r.mu.Lock()
	r.prompts = append(r.prompts, prompt)
	gate, entered, once, err, boom := r.gate, r.entered, r.once, r.err, r.boom
	r.mu.Unlock()
	if boom {
		panic("summarizer exploded")
	}
	if gate != nil {
		once.Do(func() { close(entered) })
		select {
		case <-gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if err != nil {
		return "", err
	}
	return observerOut, nil
}

// hold makes every summarize call block. entered is closed once a call has reached the gate,
// so a test can wait for the fold to really be in flight rather than sleeping; release lets
// it and every later call through.
func (r *recorder) hold() (entered <-chan struct{}, release func()) {
	gate, arrived := make(chan struct{}), make(chan struct{})
	r.mu.Lock()
	r.gate, r.entered, r.once = gate, arrived, &sync.Once{}
	r.mu.Unlock()
	return arrived, func() { close(gate) }
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

func (r *recorder) panics(on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.boom = on
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
	plug        *memPlugin
}

func newHarness(t *testing.T, fold map[string]int) *harness {
	t.Helper()
	return newHarnessWithHookTimeout(t, fold, 60*time.Second)
}

// newHarnessWithHookTimeout builds a bundle, a project directory with one Project concept and
// a session directory holding the rudy transcript fixture, all under a temporary HOME so
// nothing reaches the real ~/.agents/memory and RecallObservation's home check has a real
// boundary. hookTimeout is what the runner gives a handler, which after fix round 1 must no
// longer bound the fold.
func newHarnessWithHookTimeout(t *testing.T, fold map[string]int, hookTimeout time.Duration) *harness {
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
	p := New(cfg, sessionsDir, "0.1.0", rec.summarize)
	reg.Load(context.Background(), p)
	if st := reg.Statuses()[0]; st.State != plugin.StateReady {
		t.Fatalf("plugin failed to load: %+v", st)
	}
	h := &harness{
		t: t, b: b, projDir: projDir, sessionsDir: sessionsDir, home: home,
		reg: reg, runner: plugin.NewHookRunner(reg, hookTimeout, rec.notice), rec: rec,
		plug: p.(*memPlugin),
	}
	t.Cleanup(func() { _ = h.plug.Close() })
	return h
}

// settle waits for every fold and write job the plugin has started, which is what Close does.
// Folds are asynchronous now, so a test asserting on their effects has to wait for one.
func (h *harness) settle() {
	h.t.Helper()
	if err := h.plug.Close(); err != nil {
		h.t.Fatalf("close: %v", err)
	}
}

// breakGit removes the bundle's repository, so every Job.Run fails at CommitAll while the
// concept writes underneath it keep working. It is how a "write completion" failure is
// produced without stubbing the SDK.
func (h *harness) breakGit() {
	h.t.Helper()
	if err := os.RemoveAll(filepath.Join(h.b.Root, ".git")); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) open(projectID string) []any { return h.openUnder(projectID, "") }

func (h *harness) openUnder(projectID, parent string) []any {
	h.t.Helper()
	return h.runner.Fire(context.Background(), plugin.HookCall{
		Point:     plugin.HookSessionOpened,
		SessionID: fixtureSession,
		Payload: &plugin.SessionOpenedPayload{
			SessionID:       fixtureSession,
			Workspace:       session.Workspace{Root: h.home, ProjectID: projectID},
			Model:           fixtureModel,
			ParentSessionID: parent,
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

// projectIndex is the project directory's index.md, which only a write job regenerates.
func (h *harness) projectIndex() string {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.projDir.Abs, "index.md"))
	if err != nil {
		h.t.Fatalf("index: %v", err)
	}
	return string(raw)
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

// noteWith finds the note whose text has prefix, or fails naming what was recorded.
func noteWith(t *testing.T, notes []session.Note, prefix string) session.Note {
	t.Helper()
	for _, n := range notes {
		if strings.HasPrefix(n.Text, prefix) {
			return n
		}
	}
	t.Fatalf("no note starting %q in %+v", prefix, notes)
	return session.Note{}
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
	h.settle()
	if notes := h.rec.takeNotes(); len(notes) != 0 {
		t.Fatalf("notes %+v", notes)
	}
	if h.summary() != nil {
		t.Fatal("wrote a session summary for a session with no project")
	}
}

// TestChildSessionReadsButNeverWrites is finding 1: a subagent's session fires the same hooks
// as the session it belongs to. It gets the context render, and its tools work, but it must
// not fold, summarize or finalize, or five subagents leave six session summaries and five
// pushes behind one turn.
func TestChildSessionReadsButNeverWrites(t *testing.T) {
	h := newHarness(t, map[string]int{"observe_after_tokens": 1})
	results := h.openUnder(fixtureProject, parentSession)
	if len(results) != 1 {
		t.Fatalf("a child session got no context render: %v", results)
	}
	if _, ok := results[0].(*plugin.SessionOpenedResult); !ok {
		t.Fatalf("result type %T", results[0])
	}
	h.turn()
	h.settle()
	if notes := h.rec.takeNotes(); len(notes) != 0 {
		t.Fatalf("a child session's turn noted %+v", notes)
	}
	if h.summary() != nil {
		t.Fatal("a child session's turn wrote a session summary")
	}
	if results := h.compaction(); len(results) != 0 {
		t.Fatalf("a child session supplied a compaction summary: %v", results)
	}
	// The tools still work: a subagent reads and records into the same project.
	if res := h.call("memory_recall", `{"query":"alpha"}`); res.IsError {
		t.Errorf("recall in a child session: %q", resultText(t, res))
	}
	h.closed()
	h.settle()
	if h.summary() != nil {
		t.Fatal("a child session's close created a session summary")
	}
	if notes := h.rec.takeNotes(); len(notes) != 0 {
		t.Fatalf("a child session's close noted %+v", notes)
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
	h.settle()
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
	h.settle()
	if notes := h.rec.takeNotes(); len(notes) != 0 {
		t.Fatalf("a skipped fold noted %+v", notes)
	}
	if h.summary() != nil {
		t.Fatal("a skipped fold wrote a session summary")
	}
}

// TestFoldRunsOffTheHookPath is finding 4: turn_completed hands the fold to a goroutine with
// its own deadline, so the hook returns at once and the runner's timeout, here 100ms against
// a summarizer that never answers until released, never reaches the fold.
func TestFoldRunsOffTheHookPath(t *testing.T) {
	h := newHarnessWithHookTimeout(t, map[string]int{"observe_after_tokens": 1}, 100*time.Millisecond)
	h.open(fixtureProject)
	entered, release := h.rec.hold()
	start := time.Now()
	h.turn()
	elapsed := time.Since(start)
	<-entered // the fold really is in flight, and really is blocked
	if elapsed > 500*time.Millisecond {
		t.Fatalf("turn_completed took %s; the fold is still on the hook's critical path", elapsed)
	}
	release()
	h.settle()
	if n := h.rec.noticed("timed out"); n != 0 {
		t.Errorf("the hook runner timed out %d times; its deadline still reaches the fold", n)
	}
	note := onlyNote(t, h.rec.takeNotes())
	if note.Text != "memory: folded 2 observations" {
		t.Errorf("note %+v", note)
	}
	if c := h.summary(); c == nil || len(memory.ParseSummaryBody(c.Body).Observations) != 2 {
		t.Errorf("the fold did not land: %+v", c)
	}
}

// TestSessionClosedQueuesBehindARunningFold is the other half of finding 4: a turn's fold may
// be dropped when one is already running, since FoldDue covers the same delta next time, but
// the finalize never can be. Nothing else will ever ask for it.
func TestSessionClosedQueuesBehindARunningFold(t *testing.T) {
	h := newHarness(t, map[string]int{"observe_after_tokens": 1})
	h.open(fixtureProject)
	entered, release := h.rec.hold()
	h.turn()
	<-entered
	h.closed() // queued behind the fold that is blocked in the summarizer
	release()
	h.settle()
	c := h.summary()
	if c == nil {
		t.Fatal("no session summary")
	}
	if c.Status != "stable" {
		t.Errorf("status %q, want stable: the finalize queued behind the running fold was lost", c.Status)
	}
}

// TestCloseWaitsForAFoldInFlight: Close is what a CLI exit path calls, and it must not return
// while a fold is still writing into the bundle.
func TestCloseWaitsForAFoldInFlight(t *testing.T) {
	h := newHarness(t, map[string]int{"observe_after_tokens": 1})
	h.open(fixtureProject)
	entered, release := h.rec.hold()
	h.turn()
	<-entered
	done := make(chan error, 1)
	go func() { done <- h.plug.Close() }()
	select {
	case err := <-done:
		t.Fatalf("Close returned (%v) while a fold was in flight", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("close: %v", err)
	}
	if c := h.summary(); c == nil || len(memory.ParseSummaryBody(c.Body).Observations) != 2 {
		t.Errorf("the fold did not finish before Close returned: %+v", c)
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
	h.settle()
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
	h.settle()
	h.rec.takeNotes()
	h.closed()
	h.settle()
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
	h.settle()
	note := onlyNote(t, h.rec.takeNotes())
	if note.Text != "memory: fold failed: boom" || note.Role != session.NoteWarn {
		t.Errorf("note %+v", note)
	}
	if msg := foldStateError(t, h.b, sessionName(fixtureSession)); msg != "boom" {
		t.Errorf("checkpoint lastError %q, want boom", msg)
	}
	h.rec.fail(nil)
	h.turn()
	h.settle()
	note = onlyNote(t, h.rec.takeNotes())
	if note.Text != "memory: folded 2 observations" || note.Role != session.NoteMuted {
		t.Errorf("note after recovery %+v", note)
	}
}

// TestFoldPanicIsANote: the fold no longer runs under the hook runner, which recovers around
// plugin code, so it has to recover for itself. A panic anywhere under a fold must cost one
// note and the next fold, not the process.
func TestFoldPanicIsANote(t *testing.T) {
	h := newHarness(t, map[string]int{"observe_after_tokens": 1})
	h.open(fixtureProject)
	h.rec.panics(true)
	h.turn()
	h.settle()
	note := onlyNote(t, h.rec.takeNotes())
	if !strings.HasPrefix(note.Text, "memory: fold panicked: ") || note.Role != session.NoteWarn {
		t.Fatalf("note %+v", note)
	}
	h.rec.panics(false)
	h.turn()
	h.settle()
	note = onlyNote(t, h.rec.takeNotes())
	if note.Text != "memory: folded 2 observations" {
		t.Errorf("note after recovery %+v", note)
	}
}

// TestFoldCommitFailureIsANoteBesideTheFold is finding 3. A commit that fails after the
// observations were written and the checkpoint advanced is not a failed fold: reporting it as
// one drops the folded note and clobbers the checkpoint the SDK just cleared, so the next
// fold re-reads a delta it already recorded.
func TestFoldCommitFailureIsANoteBesideTheFold(t *testing.T) {
	h := newHarness(t, map[string]int{"observe_after_tokens": 1})
	h.open(fixtureProject)
	h.breakGit()
	h.turn()
	h.settle()
	notes := h.rec.takeNotes()
	if len(notes) != 2 {
		t.Fatalf("notes %+v, want the fold and the commit failure", notes)
	}
	folded := noteWith(t, notes, "memory: folded ")
	if folded.Text != "memory: folded 2 observations" || folded.Role != session.NoteMuted {
		t.Errorf("fold note %+v", folded)
	}
	commit := noteWith(t, notes, "memory: fold committed nothing: ")
	if commit.Role != session.NoteWarn {
		t.Errorf("commit note %+v", commit)
	}
	if strings.Contains(commit.Text, "write completion:") {
		t.Errorf("commit note repeats the SDK's prefix: %q", commit.Text)
	}
	c := h.summary()
	if c == nil || len(memory.ParseSummaryBody(c.Body).Observations) != 2 {
		t.Fatalf("the observations were not written: %+v", c)
	}
	if msg := foldStateError(t, h.b, sessionName(fixtureSession)); msg != "" {
		t.Errorf("checkpoint lastError %q; a commit failure must not clobber a cleared checkpoint", msg)
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
	h.settle()
	note := onlyNote(t, h.rec.takeNotes())
	if !strings.HasPrefix(note.Text, "memory: fold failed: ") || note.Role != session.NoteWarn {
		t.Fatalf("note %+v", note)
	}
	want := strings.TrimPrefix(note.Text, "memory: fold failed: ")
	if msg := foldStateError(t, h.b, sessionName(fixtureSession)); msg != want {
		t.Errorf("checkpoint lastError %q, want %q", msg, want)
	}
}

// foldStateError reads the message the SDK's own checkpoint holds for a session, or "" when
// it holds none.
func foldStateError(t *testing.T, b *memory.Bundle, sess string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(b.Root, ".state"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
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
	h.settle()
	if !strings.Contains(h.projectIndex(), "use-pnpm") {
		t.Errorf("index does not name the concept:\n%s", h.projectIndex())
	}
	// A second write of the same title revises rather than creates.
	got = resultText(t, h.call("memory_remember", `{"type":"Feedback","title":"Use pnpm","body":"still never npm"}`))
	if got != "revised "+rel {
		t.Errorf("second result %q, want %q", got, "revised "+rel)
	}
}

// slowGit puts a git shim first on PATH that sleeps before handing off to the real git, so a
// write job takes long enough that a synchronous one is unmistakable. The real job's slow part
// is a push to a remote; this stands in for it without a network.
func slowGit(t *testing.T, d time.Duration) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep " + strconv.FormatFloat(d.Seconds(), 'f', 3, 64) + "\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestRememberJobRunsOffTheToolCall is finding 2: the job indexes, commits and pushes, up to
// sixty seconds of network with no context behind it, and a safe tool must not hold a turn
// open for that. With every git call sleeping 250ms the tool still answers at once, and the
// index it did not wait for appears afterwards.
func TestRememberJobRunsOffTheToolCall(t *testing.T) {
	h := newHarness(t, nil)
	h.open(fixtureProject)
	slowGit(t, 250*time.Millisecond)
	rel := "projects/" + fixtureProject + "/feedback/use-pnpm.md"
	start := time.Now()
	got := resultText(t, h.call("memory_remember", `{"type":"Feedback","title":"Use pnpm","body":"never npm"}`))
	elapsed := time.Since(start)
	if got != "remembered "+rel {
		t.Fatalf("result %q", got)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("the tool waited %s on the write job", elapsed)
	}
	if strings.Contains(h.projectIndex(), "use-pnpm") {
		t.Fatal("the index was regenerated before the tool answered")
	}
	h.settle()
	if !strings.Contains(h.projectIndex(), "use-pnpm") {
		t.Errorf("the job never ran:\n%s", h.projectIndex())
	}
}

// TestRememberJobSkipsSilentlyWhenTheGitLockIsHeld: a job that finds the lock held ran
// nothing and says nothing, because the next job commits everything pending.
func TestRememberJobSkipsSilentlyWhenTheGitLockIsHeld(t *testing.T) {
	h := newHarness(t, nil)
	h.open(fixtureProject)
	lock := filepath.Join(h.b.Root, ".locks", "git")
	if err := os.MkdirAll(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	h.call("memory_remember", `{"type":"Feedback","title":"Use pnpm","body":"never npm"}`)
	h.settle()
	if strings.Contains(h.projectIndex(), "use-pnpm") {
		t.Fatal("the job ran even though the git lock was held")
	}
	if notes := h.rec.takeNotes(); len(notes) != 0 {
		t.Errorf("a job that found the lock held said %+v", notes)
	}
	if err := os.RemoveAll(lock); err != nil {
		t.Fatal(err)
	}
	h.call("memory_remember", `{"type":"Reference","title":"Make test","body":"make test"}`)
	h.settle()
	index := h.projectIndex()
	if !strings.Contains(index, "use-pnpm") || !strings.Contains(index, "make-test") {
		t.Errorf("the next job did not commit what was pending:\n%s", index)
	}
}

// TestRememberJobFailureIsAWarnNote: the write succeeded and the model was told so; the
// index and commit that failed afterwards are the operator's problem, on the session.
func TestRememberJobFailureIsAWarnNote(t *testing.T) {
	h := newHarness(t, nil)
	h.open(fixtureProject)
	h.breakGit()
	if res := h.call("memory_remember", `{"type":"Feedback","title":"Use pnpm","body":"never npm"}`); res.IsError {
		t.Fatalf("the tool failed on a job that had not run yet: %q", resultText(t, res))
	}
	h.settle()
	note := onlyNote(t, h.rec.takeNotes())
	if !strings.HasPrefix(note.Text, "memory: remember job: ") || note.Role != session.NoteWarn {
		t.Errorf("note %+v", note)
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
	h.settle()
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
	h.settle()
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
