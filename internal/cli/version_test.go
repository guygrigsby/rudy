// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"runtime/debug"
	"testing"
)

// TestResolveVersion pins what a binary calls itself. "dev" was a word rudy made up for
// every build the linker did not stamp, which is what `go install` produces, so a released
// binary a user installed the documented way reported dev and so did their bug report. The
// toolchain already stamps the commit or the module version; this reads it.
func TestResolveVersion(t *testing.T) {
	info := func(main string, settings ...debug.BuildSetting) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Main: debug.Module{Version: main}, Settings: settings}, true
		}
	}
	rev := debug.BuildSetting{Key: "vcs.revision", Value: "b252ed33bb8f935b338ccc1aea369938fb405f24"}
	clean := debug.BuildSetting{Key: "vcs.modified", Value: "false"}
	dirty := debug.BuildSetting{Key: "vcs.modified", Value: "true"}

	cases := []struct {
		name   string
		linker string
		read   func() (*debug.BuildInfo, bool)
		want   string
	}{
		{"the linker wins", "v0.1.0", info("(devel)", rev, clean), "v0.1.0"},
		{"a checkout build names its commit", "", info("v0.0.0-20260917232333-b252ed33bb8f", rev, clean), "b252ed33bb8f"},
		{"a modified checkout says so", "", info("v0.0.0-20260917232333-b252ed33bb8f+dirty", rev, dirty), "b252ed33bb8f-dev"},
		{"go install names the release", "", info("v0.1.0"), "v0.1.0"},
		{"a binary with nothing stamped", "", info("(devel)"), "unknown"},
		{"no build info at all", "", func() (*debug.BuildInfo, bool) { return nil, false }, "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveVersion(c.linker, c.read); got != c.want {
				t.Errorf("resolveVersion(%q) = %q, want %q", c.linker, got, c.want)
			}
		})
	}
}

// TestVersionIsNeverTheWordDev: the default carries no invented word, so a build the linker
// did not stamp still reports something true about itself.
func TestVersionIsNeverTheWordDev(t *testing.T) {
	if version != "" {
		t.Errorf("the linker's variable defaults to %q, want it empty so the build info answers", version)
	}
	if got := Version(); got == "dev" {
		t.Errorf("Version() = %q", got)
	}
}
