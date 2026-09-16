package turn

import (
	"context"
	"errors"
	"testing"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

func TestTerminalSyncFailureDoesNotPublishState(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind session.Kind
		fail bool
	}{
		{"completion", session.KindAssistantMessage, false},
		{"failure", session.KindTurnFailed, true},
		{"steering_cancel", session.KindTurnInterrupted, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestSession(t, session.ModeOff)
			rec := &recorder{}
			p := &scripted{scripts: [][]provider.Part{{text("done"), stop(session.StopEndTurn, "end_turn")}}}
			if tc.fail {
				p.err = &provider.Error{Class: session.ErrProvider, Message: "provider failed"}
			}
			r := newRunner(t, s, p, nil, nil, rec)
			r.cfg.Sync = func() error { return errors.New("private sync failure") }
			if tc.name == "steering_cancel" {
				e, err := s.Append(userMsg(session.SourceTyped, "go"))
				if err != nil {
					t.Fatal(err)
				}
				r.turn, r.state = e.ID, Steering
				if err := r.Interrupt(session.InterruptCancel); !errors.Is(err, ErrTerminalDurability) {
					t.Errorf("cancel error = %v", err)
				}
			} else if err := r.Run(context.Background(), userMsg(session.SourceTyped, "go")); err == nil {
				t.Error("sync failure returned success")
			}
			for _, state := range rec.states {
				if state == Completed || state == Failed || state == Idle {
					t.Errorf("published terminal state %s after sync failure", state)
				}
			}
		})
	}
}

func TestTerminalEntryIsNotPublishedWhenSyncFails(t *testing.T) {
	s := openTestSession(t, session.ModeOff)
	rec := &recorder{}
	r := newRunner(t, s, &scripted{scripts: [][]provider.Part{{text("done"), stop(session.StopEndTurn, "end_turn")}}}, nil, nil, rec)
	r.cfg.Sync = func() error { return errors.New("private sync failure") }
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "go")); !errors.Is(err, ErrTerminalDurability) {
		t.Fatalf("Run error = %v", err)
	}
	if rec.count(session.KindAssistantMessage) != 0 {
		t.Fatal("published unsynchronized terminal assistant Entry")
	}
}
