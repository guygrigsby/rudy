// Package memory is the memory plugin: it injects the bundle's context render on
// session_opened, folds the session transcript into a running Session Summary after every
// completed turn, finalizes that summary on session_closed, supplies it to the compactor on
// before_compaction, and registers the three memory tools plus /memory.
//
// Every bundle read and write goes through memory-go, which owns the OKF invariants; this
// package only decides when to call it and what a session means. It is the only package in
// the tree that imports the SDK.
//
// Nothing slow happens on a hook's or a tool's own goroutine. A fold makes a model call and a
// write job pushes to a git remote, either of which can take minutes; both run in the
// background and report through notes, and Close is what waits for them.
package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	memory "github.com/aeryx-ai/memory/memory-go"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/session"
)

// Summarize runs one prompt through a model for a session and returns the text. wire binds it
// to the provider registry and memory.summary_model; the plugin never sees a provider, so
// nothing outside this package has to know the SDK exists. The session id travels with the
// prompt because the request is made on that session's behalf and carries its X-Rudy-Session
// header like any other.
type Summarize func(ctx context.Context, sessionID string, prompt string) (string, error)

// sessionState is what session_opened learned about a session, plus the fold machinery's own
// bookkeeping for it. Every field is read and written under memPlugin.mu.
type sessionState struct {
	project string
	model   session.ModelRef
	// child is true when this session answers a parent's tool call. A subagent fires every
	// hook its parent does; letting each one fold would leave one Session Summary and one
	// push per subagent behind a single turn of the session that spawned them.
	child bool
	// folding is true while a fold goroutine owns this session, and finalize records a close
	// that arrived while one was running. A turn's fold may be dropped when one is already in
	// flight, since FoldDue covers the same delta next time; a finalize may not, because
	// nothing will ever ask for it again.
	folding  bool
	finalize bool
}

type memPlugin struct {
	cfg         config.MemoryConfig
	sessionsDir string
	version     string
	summarize   Summarize
	host        plugin.Host
	b           *memory.Bundle

	// wg counts the folds and write jobs in flight, which is what Close waits on.
	wg sync.WaitGroup

	mu       sync.Mutex
	sessions map[string]*sessionState

	// writes serializes memory_remember. memory.Remember reads a concept, revises it and
	// writes it back, so two at once lose a revision. Tool calls run concurrently (ADR 0028)
	// and a tool that is not reentrant guards itself rather than asking the scheduler to.
	// rudy-0lz fixes this properly in memory-go; this keeps rudy correct meanwhile.
	writes sync.Mutex
}

// New builds the plugin. sessionsDir locates a session's entries.jsonl for the fold; version
// is the harness version, reported by /memory.
func New(cfg config.MemoryConfig, sessionsDir string, version string, summarize Summarize) plugin.Plugin {
	return &memPlugin{
		cfg: cfg, sessionsDir: sessionsDir, version: version, summarize: summarize,
		sessions: map[string]*sessionState{},
	}
}

func (p *memPlugin) Name() string { return "memory" }

func (p *memPlugin) Init(ctx context.Context, h plugin.Host) error {
	if !p.cfg.Enabled {
		return nil
	}
	p.host = h
	p.b = memory.New(memory.ResolveRoot(p.cfg.Dir, os.Getenv))
	if !p.b.Exists() {
		h.Notice(fmt.Sprintf("memory: no bundle at %s; run memory init", p.b.Root))
		return nil
	}
	for _, hh := range []plugin.HookHandler{
		{Point: plugin.HookSessionOpened, Handle: p.onOpened},
		{Point: plugin.HookTurnCompleted, Handle: p.onTurnCompleted},
		{Point: plugin.HookBeforeCompaction, Handle: p.onBeforeCompaction},
		{Point: plugin.HookSessionClosed, Handle: p.onClosed},
	} {
		if err := h.RegisterHook(hh); err != nil {
			return err
		}
	}
	return errors.Join(p.registerTools(h), h.RegisterCommand(p.command()))
}

// Close is CloseContext with no deadline but the plugin's own, for a caller that has none to
// give.
func (p *memPlugin) Close() error { return p.CloseContext(context.Background()) }

// CloseContext waits for the folds and write jobs already in flight, so a process that is
// exiting does not leave a concept half written or a commit unmade. The wait ends at
// whichever comes first: the work, the plugin's own closeTimeout (a summarizer that never
// answers must not hold the exit open forever) or ctx, which is the process saying it has
// run out of patience, a second Ctrl-C during shutdown being exactly that. Registry.Close
// calls it.
func (p *memPlugin) CloseContext(ctx context.Context) error {
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("memory: stopped waiting for folds and write jobs: %w", ctx.Err())
	case <-time.After(closeTimeout):
		return fmt.Errorf("memory: gave up after %s waiting for folds and write jobs", closeTimeout)
	}
}

// onOpened records what the session is working on and answers with the bundle's context
// render. A child session gets the render too, cheaply and usefully, but is recorded as one
// so nothing later writes on its behalf. A workspace with no project id has nowhere to read
// or write, so memory is off for that session; the operator hears about it once, not once
// per turn.
func (p *memPlugin) onOpened(ctx context.Context, call plugin.HookCall) (any, error) {
	payload, ok := call.Payload.(*plugin.SessionOpenedPayload)
	if !ok || payload == nil {
		return nil, nil
	}
	sid := payload.SessionID
	p.mu.Lock()
	// The entry is updated in place rather than replaced: this point also fires on a resume,
	// which can land while a fold from the previous attach is still running, and a fresh
	// struct would drop the flags that fold is about to read.
	st, seen := p.sessions[sid]
	if !seen {
		st = &sessionState{}
		p.sessions[sid] = st
	}
	st.project, st.model, st.child = payload.Workspace.ProjectID, payload.Model, payload.ParentSessionID != ""
	p.mu.Unlock()
	if payload.Workspace.ProjectID == "" {
		if !seen {
			p.host.Notice("memory: workspace has no project id; memory is off for this session")
		}
		return nil, nil
	}
	text, err := memory.RenderContext(p.b, memory.ContextOptions{
		ProjectID: payload.Workspace.ProjectID,
		Session:   sessionName(sid),
	})
	if err != nil {
		p.host.Notice("memory: context: " + err.Error())
		return nil, nil
	}
	return &plugin.SessionOpenedResult{Context: text}, nil
}

// onTurnCompleted hands the fold to the background and returns. The fold's long part is a
// model call, which no hook deadline can accommodate: bounding it by hook_timeout_ms cancels
// the summarizer, which the SDK counts as a summarizer failure, and three of those abandon
// the delta outright.
func (p *memPlugin) onTurnCompleted(ctx context.Context, call plugin.HookCall) (any, error) {
	payload, ok := call.Payload.(*plugin.TurnCompletedPayload)
	if !ok || payload == nil {
		return nil, nil
	}
	p.startFold(payload.SessionID, false)
	return nil, nil
}

// onClosed queues the finalize fold, which promotes the summary out of draft, and hands the
// session's bookkeeping to it: whoever runs last forgets the session, so a finalize waiting
// behind a running fold still knows what project it is for.
func (p *memPlugin) onClosed(ctx context.Context, call plugin.HookCall) (any, error) {
	payload, ok := call.Payload.(*plugin.SessionClosedPayload)
	if !ok || payload == nil {
		return nil, nil
	}
	if !p.startFold(payload.SessionID, true) {
		p.mu.Lock()
		delete(p.sessions, payload.SessionID)
		p.mu.Unlock()
	}
	return nil, nil
}

// onBeforeCompaction hands the compactor what memory already knows about this session, so
// the summary the model is about to write starts from the observations rather than from the
// transcript alone. A child session has no summary of its own and never will.
func (p *memPlugin) onBeforeCompaction(ctx context.Context, call plugin.HookCall) (any, error) {
	payload, ok := call.Payload.(*plugin.BeforeCompactionPayload)
	if !ok || payload == nil {
		return nil, nil
	}
	sid := payload.SessionID
	st, ok := p.session(sid)
	if !ok || st.project == "" || st.child {
		return nil, nil
	}
	c := p.sessionSummary(st.project, sessionName(sid))
	if c == nil {
		return nil, nil
	}
	body := memory.RenderSummaryBody(memory.ParseSummaryBody(c.Body))
	return &plugin.BeforeCompactionResult{Summary: "Memory of this session so far:\n\n" + body}, nil
}

// session is a copy of the bookkeeping session_opened filled. ok is false for a session that
// never opened or has already been forgotten; an empty project means memory is off for it.
func (p *memPlugin) session(sid string) (sessionState, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.sessions[sid]
	if !ok {
		return sessionState{}, false
	}
	return *st, true
}

// sessionSummary is the running Session Summary concept for sess in the project directory,
// or nil. IsSessionSummaryFor is the SDK's own predicate, so the plugin never has to know
// how a summary names its session.
func (p *memPlugin) sessionSummary(projectID, sess string) *memory.Concept {
	dir, err := p.b.Dir(projectID)
	if err != nil {
		return nil
	}
	entries, err := p.b.ListConcepts(dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if memory.IsSessionSummaryFor(e.Concept, sess) {
			return e.Concept
		}
	}
	return nil
}

// command is /memory: where the bundle is, what this session is scoped to and what it holds.
func (p *memPlugin) command() plugin.Command {
	return plugin.Command{
		Name:        "memory",
		Description: "Show the memory bundle, this session's project and what it holds",
		Run: func(ctx context.Context, call plugin.CommandCall) (plugin.Action, error) {
			project := call.Workspace.ProjectID
			var b strings.Builder
			fmt.Fprintf(&b, "bundle %s (rudy %s)\n", p.b.Root, p.version)
			if project == "" {
				b.WriteString("project none; memory is off for this session\n")
			} else {
				fmt.Fprintf(&b, "project %s\n", project)
			}
			counts := p.counts(project)
			for _, typ := range memory.Types {
				fmt.Fprintf(&b, "%s %d\n", typ, counts[typ])
			}
			return plugin.Notice{Text: strings.TrimRight(b.String(), "\n")}, nil
		},
	}
}

// counts tallies concepts by type over the bundle root and the project directory together,
// which is the pair a session reads from. A directory that cannot be listed contributes
// nothing rather than failing the command: /memory is a report, not a check.
func (p *memPlugin) counts(projectID string) map[string]int {
	scopes := []string{""}
	if projectID != "" {
		scopes = append(scopes, projectID)
	}
	out := map[string]int{}
	for _, id := range scopes {
		dir, err := p.b.Dir(id)
		if err != nil {
			continue
		}
		entries, err := p.b.ListConcepts(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			out[e.Concept.Type]++
		}
	}
	return out
}
