// SPDX-License-Identifier: AGPL-3.0-or-later

package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	memory "github.com/aeryx-ai/memory/memory-go"
	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/session"
)

const (
	// foldTimeout bounds one fold. It is the plugin's own deadline rather than the hook
	// runner's because the fold's long part is a model call over the whole transcript delta,
	// which no hook budget can hold, and because a cancelled summarizer counts as a
	// summarizer failure against the SDK's give-up-after-three rule.
	foldTimeout = 120 * time.Second
	// closeTimeout bounds Close. A summarizer that never answers must not hold an exiting
	// process open past its own shutdown budget.
	closeTimeout = 30 * time.Second
	// writeCompletionPrefix is how the SDK reports a fold that recorded its observations and
	// advanced its checkpoint but could not index, commit or push afterwards.
	writeCompletionPrefix = "write completion: "
)

// sessionName is how a rudy session names itself to the bundle. It is the resource a Session
// Summary's sources carry and the key the fold checkpoint is stored under, so one function
// owns the spelling.
func sessionName(id string) string { return "rudy:session/" + id }

// actorClean is everything outside the OKF actor alphabet for the version half of
// <producer>/<version>.
var actorClean = regexp.MustCompile(`[^A-Za-z0-9._:-]`)

// actorFor is rudy/<model id>, with characters the OKF actor grammar refuses replaced by a
// dash: a model id like cline-pass/kimi-k3 would otherwise read as a second producer.
func actorFor(m session.ModelRef) string { return "rudy/" + actorClean.ReplaceAllString(m.Model, "-") }

// sdkSummarizer adapts the injected prompt runner to the SDK's Summarizer port. The context
// and the session travel on the struct because the port passes only a prompt; the context is
// the fold's own, so the model call is bounded by foldTimeout and by nothing else, and the
// session is the one being folded, which is what the request reports itself as.
type sdkSummarizer struct {
	ctx context.Context
	sid string
	run Summarize
}

func (s sdkSummarizer) Summarize(prompt string) (string, error) {
	return s.run(s.ctx, s.sid, prompt)
}

// settingsFrom maps the [memory.fold] config keys onto FoldSettings. A key the config omits
// indexes to zero, which is exactly how the SDK's own defaults are asked for.
func settingsFrom(fold map[string]int) memory.FoldSettings {
	return memory.FoldSettings{
		ObserveAfterTokens:       fold["observe_after_tokens"],
		ReflectAfterTokens:       fold["reflect_after_tokens"],
		ObservationsMaxTokens:    fold["observations_max_tokens"],
		ObservationsTargetTokens: fold["observations_target_tokens"],
		ObserverMaxTokens:        fold["observer_max_tokens"],
	}
}

// startFold puts a fold in flight for sid, off the caller's goroutine. It reports whether one
// will run, which is what tells session_closed whether the session's bookkeeping is now the
// fold's to forget.
//
// Folds are serialized per session, since the SDK takes a per-session lock anyway and a
// second fold would only find it held. A turn's fold that arrives while one is running is
// dropped, because FoldDue measures the transcript from the checkpoint and will cover the
// same bytes next turn; a finalize is remembered on the session instead, because nothing else
// will ever ask for it. A flag beats a per-session worker and queue here: the only thing a
// queue would buy is more than one pending fold, and that is precisely what FoldDue already
// collapses into one.
func (p *memPlugin) startFold(sid string, finalize bool) bool {
	p.mu.Lock()
	st, ok := p.sessions[sid]
	if !ok || st.project == "" || st.child {
		p.mu.Unlock()
		return false
	}
	if st.folding {
		st.finalize = st.finalize || finalize
		p.mu.Unlock()
		return finalize
	}
	st.folding = true
	p.mu.Unlock()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.foldUntilQuiet(sid, finalize)
	}()
	return true
}

// foldUntilQuiet runs the fold, then the finalize that arrived while it was running, and
// forgets the session once a finalize has run.
func (p *memPlugin) foldUntilQuiet(sid string, finalize bool) {
	for {
		p.guard(sid, "fold", func() { p.fold(sid, finalize) })
		p.mu.Lock()
		st, ok := p.sessions[sid]
		switch {
		case !ok:
			p.mu.Unlock()
			return
		case finalize:
			delete(p.sessions, sid)
			p.mu.Unlock()
			return
		case st.finalize:
			st.finalize, finalize = false, true
			p.mu.Unlock()
		default:
			st.folding = false
			p.mu.Unlock()
			return
		}
	}
}

// fold runs one fold for a session under its own deadline.
//
// A failure is a note, never a turn failure: memory is an addition to a session, and a bundle
// that cannot be written must not stop the model from answering.
func (p *memPlugin) fold(sid string, finalize bool) {
	state, ok := p.session(sid)
	if !ok || state.project == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), foldTimeout)
	defer cancel()
	o := memory.FoldOptions{
		Session:    sessionName(sid),
		Actor:      actorFor(state.model),
		Transcript: filepath.Join(p.sessionsDir, sid, session.LogFile),
		Format:     "rudy",
		ProjectID:  state.project,
		Finalize:   finalize,
		Summarizer: sdkSummarizer{ctx: ctx, sid: sid, run: p.summarize},
		Settings:   settingsFrom(p.cfg.Fold),
		Now:        time.Now(),
	}
	if due, _, err := memory.FoldDue(p.b, o); err == nil && !due && !finalize {
		return
	}
	st, err := memory.RunFoldJob(p.b, o)
	if err != nil {
		// RunFoldJob failed before it could checkpoint anything, so the plugin is the only
		// thing that can leave a trail for doctor and the next fold.
		_ = memory.MarkFoldError(p.b, o.Session, err.Error(), time.Now())
		p.note(sid, "memory: fold failed: "+err.Error(), session.NoteWarn)
		return
	}
	// A summarizer failure comes back in FoldStatus.Error rather than as an error return, and
	// the SDK has already checkpointed it: marking it again would only overwrite what it
	// wrote. A "write completion" error is not that. It means the observations were written
	// and the checkpoint advanced, and only the index, commit or push failed afterwards, so
	// it is reported beside the fold instead of instead of it.
	commitErr, committedNothing := strings.CutPrefix(st.Error, writeCompletionPrefix)
	if st.Error != "" && !committedNothing {
		p.note(sid, "memory: fold failed: "+st.Error, session.NoteWarn)
		return
	}
	if st.Status != "skipped" {
		p.note(sid, fmt.Sprintf("memory: folded %d observations", st.Observations), session.NoteMuted)
	}
	if committedNothing {
		p.note(sid, "memory: fold committed nothing: "+commitErr, session.NoteWarn)
	}
}

// runJob runs a write completion job off the caller's goroutine. The job regenerates the
// index, commits and pushes, which is up to a minute of network with no context behind it,
// and no tool call or hook may wait on that. A job that found the git lock held ran nothing
// and says nothing: the next job commits everything pending.
func (p *memPlugin) runJob(sid string, job memory.Job) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.guard(sid, "remember job", func() {
			if _, err := job.Run(p.b); err != nil {
				p.note(sid, "memory: remember job: "+err.Error(), session.NoteWarn)
			}
		})
	}()
}

// guard turns a panic in background work into a note. The hook runner and Load both recover
// around plugin code; moving the fold and the write job off those goroutines would otherwise
// give up that protection and let one bad fold take the whole harness down.
func (p *memPlugin) guard(sid, what string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			p.note(sid, fmt.Sprintf("memory: %s panicked: %v", what, r), session.NoteWarn)
		}
	}()
	fn()
}

// note appends to the session log, dropping the result: a note that cannot be written is
// display only and must not turn into a second failure on top of the one it reports.
func (p *memPlugin) note(sid, text string, role session.NoteRole) {
	id, err := ulid.Parse(sid)
	if err != nil {
		return
	}
	_ = p.host.Note(id, text, role)
}
