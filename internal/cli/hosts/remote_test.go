// SPDX-License-Identifier: AGPL-3.0-or-later

package hosts

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestRemoteLinePrependsUserBinsAndExits111WithoutRudy(t *testing.T) {
	line := RemoteLine()
	for _, want := range []string{`PATH="$HOME/.local/bin:$HOME/bin:$HOME/go/bin:$PATH"`, "command -v rudy >/dev/null 2>&1 || exit 111", "exec rudy bridge"} {
		if !strings.Contains(line, want) {
			t.Fatalf("remote line %q lacks %q", line, want)
		}
	}
	if !strings.HasSuffix(RemoteLine("--no-start"), "exec rudy bridge --no-start") {
		t.Fatalf("remote line with args = %q", RemoteLine("--no-start"))
	}
}

func TestRemoteLineRunsInAShell(t *testing.T) {
	// The line is what ssh hands the box's shell. Run it under sh with a PATH that lacks
	// rudy and check the exit code is the one the client keys on.
	cmd := exec.Command("sh", "-c", RemoteLine())
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != ExitNoRudy {
		t.Fatalf("remote line without rudy exited %v, want %d", err, ExitNoRudy)
	}
}
