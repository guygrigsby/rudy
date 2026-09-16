package acpwire

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestLoggerDropsAllUntrustedData(t *testing.T) {
	var out strings.Builder
	l := NewLogger(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	l.With("SECRET", "SECRET").WithGroup("SECRET").Error("SECRET", "raw", "SECRET", "id", "SECRET", "params", map[string]any{"nested": errors.New("SECRET")})
	l.Error("connection closed", "cause", errors.New("SECRET"), "queued", 3)
	l.Log(context.Background(), slog.LevelError, "failed to parse incoming message", "err", errors.New("SECRET"), "raw", "SECRET")
	if strings.Contains(out.String(), "SECRET") {
		t.Fatal("credentials reached log")
	}
	if !strings.Contains(out.String(), "connection closed") {
		t.Fatal("fixed diagnostic missing")
	}
}
