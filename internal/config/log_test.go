package config_test

import (
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/config"
)

func testPaths(t *testing.T) config.Paths {
	t.Helper()
	return config.Paths{Config: t.TempDir(), Data: t.TempDir(), Cache: t.TempDir(), Home: t.TempDir()}
}

func TestLogKeysHaveDefaults(t *testing.T) {
	d := config.Defaults()
	if d["log.level"] != "info" {
		t.Fatalf("log.level default = %v, want info", d["log.level"])
	}
	if _, ok := d["log.file"]; !ok {
		t.Fatal("log.file has no default; empty means $XDG_CACHE_HOME/rudy/rudy.log, filled at Load like sessions.dir")
	}
}

func TestLogLevelIsValidated(t *testing.T) {
	paths := testPaths(t)
	_, err := config.Load(paths, map[string]any{"log.level": "verbose"})
	if err == nil || !strings.Contains(err.Error(), "log.level") {
		t.Fatalf("log.level=verbose must be refused naming the key, got %v", err)
	}
}

func TestLogFileDefaultsUnderTheCacheDir(t *testing.T) {
	paths := testPaths(t)
	cfg, err := config.Load(paths, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cfg.Log.File, paths.Cache) || !strings.HasSuffix(cfg.Log.File, "rudy.log") {
		t.Fatalf("log.file = %q, want rudy.log under %s", cfg.Log.File, paths.Cache)
	}
}
