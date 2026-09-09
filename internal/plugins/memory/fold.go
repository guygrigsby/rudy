package memory

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"time"

	memory "github.com/aeryx-ai/memory/memory-go"
	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/session"
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
// travels on the struct because Summarize takes only a prompt; that is the hook's context,
// so a fold that outruns hook_timeout_ms cancels the model call with it.
type sdkSummarizer struct {
	ctx context.Context
	run Summarize
}

func (s sdkSummarizer) Summarize(prompt string) (string, error) { return s.run(s.ctx, prompt) }

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

// fold runs one fold for a session. It is synchronous by design: the hook runner bounds it,
// and RunFoldJob checkpoints only after a success, so a fold that is cut short leaves the
// same delta for the next turn.
//
// A failure is a note, never a turn failure: memory is an addition to a session, and a
// bundle that cannot be written must not stop the model from answering.
func (p *memPlugin) fold(ctx context.Context, sid string, finalize bool) {
	project, model := p.session(sid)
	if project == "" {
		return
	}
	o := memory.FoldOptions{
		Session:    sessionName(sid),
		Actor:      actorFor(model),
		Transcript: filepath.Join(p.sessionsDir, sid, session.LogFile),
		Format:     "rudy",
		ProjectID:  project,
		Finalize:   finalize,
		Summarizer: sdkSummarizer{ctx: ctx, run: p.summarize},
		Settings:   settingsFrom(p.cfg.Fold),
		Now:        time.Now(),
	}
	if due, _, err := memory.FoldDue(p.b, o); err == nil && !due && !finalize {
		return
	}
	st, err := memory.RunFoldJob(p.b, o)
	// A summarizer failure is not an error return: the SDK checkpoints it and reports it in
	// FoldStatus.Error, so both shapes have to reach the same note.
	if err == nil && st.Error != "" {
		err = errors.New(st.Error)
	}
	if err != nil {
		_ = memory.MarkFoldError(p.b, o.Session, err.Error(), time.Now())
		p.note(sid, "memory: fold failed: "+err.Error(), session.NoteWarn)
		return
	}
	if st.Status == "skipped" {
		return
	}
	p.note(sid, fmt.Sprintf("memory: folded %d observations", st.Observations), session.NoteMuted)
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
