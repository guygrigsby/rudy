package cli

import (
	"io"
	"log/slog"
	"os"

	"github.com/guygrigsby/rudy/internal/config"
)

// openLog installs the process logger: JSON lines appended to cfg.Log.File at cfg.Log.Level,
// every record stamped with this pid, since a serve daemon and a client attaching to it are
// two processes writing one file. It is the process default, so anything still calling the
// standard log package lands in the same file.
//
// A file that cannot be opened is a notice and the trail goes to stderr instead: diagnostics
// must never be the reason a boot fails.
func openLog(cfg *config.Config, version string, stderr io.Writer, notice func(string)) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Log.Level)); err != nil {
		level = slog.LevelInfo
	}
	w := stderr
	f, err := os.OpenFile(cfg.Log.File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		notice("log file " + cfg.Log.File + ": " + err.Error() + "; logging to stderr")
	} else {
		w = f
	}
	logger := slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})).With("pid", os.Getpid())
	slog.SetDefault(logger)
	// min_level, not level: level is the key slog itself writes on every record.
	logger.Info("rudy: start", "version", version, "config", cfg.ConfigDir, "log", cfg.Log.File, "min_level", cfg.Log.Level)
	return logger
}
