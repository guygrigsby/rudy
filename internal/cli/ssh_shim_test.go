package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sshShim writes a script that stands in for ssh: it drops "--" and the host, then runs the
// remaining argument as a shell line under the box's env. RUDY_SSH names it. What the real
// ssh adds (auth, a network) is exactly what these tests do not want, and what it does not
// add (a login shell's PATH, a home) is what the remote line is written to survive, so the
// shim starts from an empty environment and gives the line only the box's own.
func sshShim(t *testing.T, env []string) string {
	t.Helper()
	dir := t.TempDir()
	envFile := filepath.Join(dir, "env")
	if err := os.WriteFile(envFile, []byte(strings.Join(env, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"[ \"$1\" = \"--\" ] && shift\n" +
		"shift\n" + // the host
		"exec env -i $(cat " + envFile + ") sh -c \"$*\"\n"
	shim := filepath.Join(dir, "ssh")
	if err := os.WriteFile(shim, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return shim
}
