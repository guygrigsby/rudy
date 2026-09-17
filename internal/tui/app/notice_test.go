// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/guygrigsby/rudy/internal/protocol"
)

// clock is a time a test moves by hand.
type clock struct{ at time.Time }

func (c *clock) now() time.Time { return c.at }

// newNoticeHarness is the pipe harness on a clock the test owns.
func newNoticeHarness(t *testing.T, over map[string]any) (*harness, *clock) {
	t.Helper()
	c := &clock{at: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)}
	h := newHarnessWith(t, over, func(o *Options) { o.Clock = c.now })
	return h, c
}

// TestANoticeGoesAwayOnItsOwn is the bug this fixed: a notice sat above the composer until
// three newer ones pushed it out, which for one notice was forever.
func TestANoticeGoesAwayOnItsOwn(t *testing.T) {
	h, c := newNoticeHarness(t, map[string]any{"ui.notices.ttl_ms": 1000})
	h.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: levelInfo, Text: "session named pluma"})
	if !strings.Contains(ansi.Strip(h.view()), "session named pluma") {
		t.Fatalf("a notice is drawn when it arrives:\n%s", ansi.Strip(h.view()))
	}
	c.at = c.at.Add(1500 * time.Millisecond)
	h.update(noticeSweepMsg{})
	if got := ansi.Strip(h.view()); strings.Contains(got, "session named pluma") {
		t.Errorf("and goes when its time is up:\n%s", got)
	}
}

// TestTheSweepStopsWhenThereIsNothingLeft: a client at rest schedules nothing.
func TestTheSweepStopsWhenThereIsNothingLeft(t *testing.T) {
	h, c := newNoticeHarness(t, map[string]any{"ui.notices.ttl_ms": 1000})
	if cmd := h.update(noticeSweepMsg{}); cmd != nil {
		if _, ok := runCmd(t, cmd).(noticeSweepMsg); ok {
			t.Fatal("with no notices there is nothing to sweep")
		}
	}
	h.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: levelInfo, Text: "one"})
	c.at = c.at.Add(1500 * time.Millisecond)
	h.update(noticeSweepMsg{})
	if len(h.m.notices) != 0 {
		t.Fatalf("notices %+v", h.m.notices)
	}
	if h.m.noticeTick {
		t.Error("and no further sweep is armed")
	}
}

// TestANoticeStaysWhenTheLifetimeIsZero is the escape: ui.notices.ttl_ms = 0 is what the
// client did before it had a clock.
func TestANoticeStaysWhenTheLifetimeIsZero(t *testing.T) {
	h, c := newNoticeHarness(t, map[string]any{"ui.notices.ttl_ms": 0})
	h.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: levelInfo, Text: "stays"})
	c.at = c.at.Add(time.Hour)
	h.update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if !strings.Contains(ansi.Strip(h.view()), "stays") {
		t.Errorf("zero means it never expires:\n%s", ansi.Strip(h.view()))
	}
}

// TestOnlyOneSweepIsEverInFlight: every message arms the sweep, and a second arming while
// one is pending would double the timers for the rest of the session.
func TestOnlyOneSweepIsEverInFlight(t *testing.T) {
	h, _ := newNoticeHarness(t, map[string]any{"ui.notices.ttl_ms": 1000})
	h.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: levelInfo, Text: "one"})
	if !h.m.noticeTick {
		t.Fatal("the first notice arms a sweep")
	}
	if cmd := h.m.noticeSweep(); cmd != nil {
		t.Error("a second arming while one is pending schedules nothing")
	}
}
