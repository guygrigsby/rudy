// Package memory is the memory plugin: it injects the bundle's context render on
// session_opened, folds the session transcript into a running Session Summary after every
// completed turn, finalizes that summary on session_closed, supplies it to the compactor on
// before_compaction, and registers the three memory tools plus /memory.
//
// Every bundle read and write goes through memory-go, which owns the OKF invariants; this
// package only decides when to call it and what a session means. It is the only package in
// the tree that imports the SDK.
package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	memory "github.com/aeryx-ai/memory/memory-go"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/session"
)

// Summarize runs one prompt through a model and returns the text. wire binds it to the
// provider registry and memory.summary_model; the plugin never sees a provider, so nothing
// outside this package has to know the SDK exists.
type Summarize func(ctx context.Context, prompt string) (string, error)

type memPlugin struct {
	cfg         config.MemoryConfig
	sessionsDir string
	version     string
	summarize   Summarize
	host        plugin.Host
	b           *memory.Bundle

	// mu guards the per-session bookkeeping: session_opened fills it, session_closed drops
	// it, and the tool invocations read it from whatever goroutine the loop is on.
	mu       sync.Mutex
	projects map[string]string           // session id -> project id
	models   map[string]session.ModelRef // session id -> the model the session opened with
}

// New builds the plugin. sessionsDir locates a session's entries.jsonl for the fold; version
// is the harness version, reported by /memory.
func New(cfg config.MemoryConfig, sessionsDir string, version string, summarize Summarize) plugin.Plugin {
	return &memPlugin{
		cfg: cfg, sessionsDir: sessionsDir, version: version, summarize: summarize,
		projects: map[string]string{}, models: map[string]session.ModelRef{},
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

// onOpened records what the session is working on and answers with the bundle's context
// render. A workspace with no project id has nowhere to read or write, so memory is off for
// that session; the operator hears about it once, not once per turn.
func (p *memPlugin) onOpened(ctx context.Context, call plugin.HookCall) (any, error) {
	payload, ok := call.Payload.(*plugin.SessionOpenedPayload)
	if !ok || payload == nil {
		return nil, nil
	}
	sid := payload.SessionID
	p.mu.Lock()
	_, seen := p.models[sid]
	p.projects[sid], p.models[sid] = payload.Workspace.ProjectID, payload.Model
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

// onTurnCompleted folds synchronously: the hook runner already bounds a handler by
// hook_timeout_ms, and the fold checkpoints only after a success, so a fold cut short by
// that deadline is retried on the next turn rather than losing the delta.
func (p *memPlugin) onTurnCompleted(ctx context.Context, call plugin.HookCall) (any, error) {
	payload, ok := call.Payload.(*plugin.TurnCompletedPayload)
	if !ok || payload == nil {
		return nil, nil
	}
	p.fold(ctx, payload.SessionID, false)
	return nil, nil
}

// onClosed folds one last time with Finalize, which promotes the summary out of draft, and
// then forgets the session.
func (p *memPlugin) onClosed(ctx context.Context, call plugin.HookCall) (any, error) {
	payload, ok := call.Payload.(*plugin.SessionClosedPayload)
	if !ok || payload == nil {
		return nil, nil
	}
	p.fold(ctx, payload.SessionID, true)
	p.mu.Lock()
	delete(p.projects, payload.SessionID)
	delete(p.models, payload.SessionID)
	p.mu.Unlock()
	return nil, nil
}

// onBeforeCompaction hands the compactor what memory already knows about this session, so
// the summary the model is about to write starts from the observations rather than from the
// transcript alone.
func (p *memPlugin) onBeforeCompaction(ctx context.Context, call plugin.HookCall) (any, error) {
	payload, ok := call.Payload.(*plugin.BeforeCompactionPayload)
	if !ok || payload == nil {
		return nil, nil
	}
	sid := payload.SessionID
	project, _ := p.session(sid)
	if project == "" {
		return nil, nil
	}
	c := p.sessionSummary(project, sessionName(sid))
	if c == nil {
		return nil, nil
	}
	body := memory.RenderSummaryBody(memory.ParseSummaryBody(c.Body))
	return &plugin.BeforeCompactionResult{Summary: "Memory of this session so far:\n\n" + body}, nil
}

// session reads the bookkeeping session_opened filled. An unknown session id, or one whose
// workspace had no project, reports an empty project: every caller treats that as off.
func (p *memPlugin) session(sid string) (project string, model session.ModelRef) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.projects[sid], p.models[sid]
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
