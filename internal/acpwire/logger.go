package acpwire

import (
	"context"
	"log/slog"
)

// NewLogger installs a deny-by-default SDK logger. Only reviewed fixed messages
// and bounded counts cross this boundary. It never resolves attacker LogValuers.
func NewLogger(sink slog.Handler) *slog.Logger { return slog.New(sanitizer{sink: sink}) }

type sanitizer struct{ sink slog.Handler }

func (s sanitizer) Enabled(ctx context.Context, l slog.Level) bool { return s.sink.Enabled(ctx, l) }
func (s sanitizer) Handle(ctx context.Context, r slog.Record) error {
	switch r.Message {
	case "connection closed", "failed to parse incoming message", "failed to queue notification; closing connection", "failed to handle notification", "dropping $/cancel_request due to full queue":
	default:
		return nil
	}
	clean := slog.NewRecord(r.Time, r.Level, r.Message, 0)
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "capacity", "queued", "queue_len":
			if a.Value.Kind() == slog.KindInt64 {
				n := a.Value.Int64()
				if n >= 0 && n <= MaxItems {
					clean.AddAttrs(slog.Int64(a.Key, n))
				}
			}
		}
		return true
	})
	// The sink owns local I/O recovery. Never return its unstructured error to SDK.
	_ = s.sink.Handle(ctx, clean)
	return nil
}
func (s sanitizer) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s sanitizer) WithGroup(string) slog.Handler      { return s }
