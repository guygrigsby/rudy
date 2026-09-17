// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"bytes"
	"testing"
)

// TestRootVersion: --version prints what the build carries. A test binary is built with no
// linker value and, unlike a go build of the command, no vcs stamp, so what it carries is
// the unstamped answer. That answer used to be the word "dev", which a `go install` build
// reported as well, so a released binary lied about itself in every bug report.
func TestRootVersion(t *testing.T) {
	root := NewRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got, want := out.String(), "rudy "+unknownVersion+"\n"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}
