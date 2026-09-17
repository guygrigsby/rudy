// SPDX-License-Identifier: AGPL-3.0-or-later

package config_test

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/guygrigsby/rudy/internal/config"
)

func TestRuntimeFallsBackToAPerUserTempDir(t *testing.T) {
	p := config.XDG(envOf(nil), "/home/guy")
	want := filepath.Join(os.TempDir(), "rudy-"+strconv.Itoa(os.Getuid()))
	if p.Runtime != want {
		t.Errorf("runtime %q want %q", p.Runtime, want)
	}

	p = config.XDG(envOf(map[string]string{"XDG_RUNTIME_DIR": "/x/run"}), "/home/guy")
	if p.Runtime != "/x/run/rudy" {
		t.Errorf("runtime %q want /x/run/rudy", p.Runtime)
	}
}

func TestSocketIsUnderRuntime(t *testing.T) {
	p := config.Paths{Runtime: "/run/rudy"}
	want := filepath.Join("/run/rudy", "rudy.sock")
	if got := p.Socket(); got != want {
		t.Errorf("socket %q want %q", got, want)
	}
}
