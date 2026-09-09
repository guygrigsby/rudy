package app

import (
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/guygrigsby/rudy/internal/tui/banner"
)

// frameEvery is how long one step of the mark's reveal lasts. The whole reveal is
// banner.Frames of these, about a second, which is long enough to read as motion and short
// enough that nobody waits for it. ADR 0016.
const frameEvery = 40 * time.Millisecond

// headerTickMsg advances the reveal by one step.
type headerTickMsg struct{}

// headerState is what the client keeps about the startup header: whether it is still being
// drawn in the frame, which step of the reveal it is on, and whether the inline print is
// still owed. Everything else the header says is read at draw time, so the facts in it
// cannot go stale behind a session switch.
type headerState struct {
	// live is the header still being drawn as the top of the transcript, which is what
	// altscreen does until the conversation scrolls it away.
	live bool
	// pending is the print inline rendering owes: the header goes into the terminal's own
	// scrollback once, when the first window size says how wide to draw it.
	pending bool
	// step is the reveal, banner.Settled once it is over.
	step int
	// name is the greeting's name and now is the clock the greeting and the tips were
	// resolved against, both fixed at startup: a header that changed its mind about the
	// time of day while a person read it would be worse than one that is a minute stale.
	name string
	now  time.Time
	home string
}

// newHeader resolves everything the header says that does not change: the name to greet,
// the hour to greet it in, and the home directory the workspace is shortened against. The
// two flags decide where it is drawn, which is a render mode question.
func (m *Model) newHeader(o Options) headerState {
	h := headerState{step: banner.Settled}
	if !m.cfg.UI.Header.Show {
		return h
	}
	if m.inline() {
		// Printed above the live region rather than drawn in it: an inline frame that grew
		// to hold a header and then shrank would strand its rows (ADR 0015, rudy-wbf).
		h.pending = true
	} else {
		h.live = true
		if m.cfg.UI.Header.Animate {
			h.step = 0
		}
	}
	h.now = o.Now
	if h.now.IsZero() {
		h.now = time.Now()
	}
	h.name = banner.Name(m.cfg.UI.Header.Name, gitLine(o.Cwd, "config", "user.name"), os.Getenv("USER"))
	h.home, _ = os.UserHomeDir()
	return h
}

// headerOptions is the header as it stands right now. The session's own facts are read
// here rather than remembered, so a model change or a session switch moves them the way it
// moves the status line.
func (m *Model) headerOptions(width int) banner.Options {
	return banner.Options{
		Version:   m.version,
		Name:      m.header.name,
		Now:       m.header.now,
		Model:     m.session.Model.String(),
		Thinking:  string(m.session.Thinking),
		Cwd:       m.session.Workspace.Root,
		Home:      m.header.home,
		Width:     width,
		MaxWidth:  m.cfg.UI.Header.MaxWidth,
		Tips:      m.cfg.UI.Header.Tips,
		Updates:   m.cfg.UI.Header.Updates,
		Changelog: m.changelog,
		Step:      m.header.step,
	}
}

// headerLines is the header where the transcript's own rows begin, or nothing once it has
// scrolled away or was never live. It is the top of the transcript rather than a slot: a
// slot would cost those rows for the whole session, and what the header says that is worth
// keeping is already in the status line.
func (m *Model) headerLines() []string {
	if !m.header.live {
		return nil
	}
	return banner.Render(m.headerOptions(m.width), m.th)
}

// headerStart is what the header owes once the terminal's width is known: the inline print,
// or the first tick of the reveal. Both need a width, so neither can happen in Init.
func (m *Model) headerStart() tea.Cmd {
	if m.header.pending {
		m.header.pending = false
		if lines := banner.Render(m.headerOptions(m.width), m.th); len(lines) > 0 {
			return tea.Println(strings.Join(lines, "\n"))
		}
		return nil
	}
	if m.header.live && m.header.step != banner.Settled {
		return headerTick()
	}
	return nil
}

// headerTicked advances the reveal one step and asks for the next, or settles and stops.
// A client at rest schedules nothing, the same rule the turn spinner keeps.
func (m *Model) headerTicked() tea.Cmd {
	if !m.header.live || m.header.step == banner.Settled {
		return nil
	}
	m.header.step++
	if m.header.step > banner.Frames {
		m.header.step = banner.Settled
		return nil
	}
	return headerTick()
}

// settleHeader ends the reveal wherever it had got to, which is what any keystroke does:
// somebody typing has stopped watching the animation.
func (m *Model) settleHeader() {
	if m.header.step != banner.Settled {
		m.header.step = banner.Settled
	}
}

func headerTick() tea.Cmd {
	return tea.Tick(frameEvery, func(time.Time) tea.Msg { return headerTickMsg{} })
}
