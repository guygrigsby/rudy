// SPDX-License-Identifier: AGPL-3.0-or-later

package hosts

import (
	"fmt"
	"strings"
)

// Revision is the commit a rudy version string names. make sets the version from
// git describe --tags --always --dirty=-dev and an unstamped build takes the commit the
// toolchain recorded: a tag, a tag plus -N-g<hash>, or a bare hash. A -dev suffix names
// nothing the box can check out, and neither does "unknown", which is what a binary with no
// version and no commit in it calls itself. The older spellings, a bare "dev" and a -dirty
// suffix, are refused too: a box can be asked to install from a binary built before this.
func Revision(version string) (string, error) {
	switch {
	case version == "" || version == "dev" || version == "unknown":
		return "", fmt.Errorf("this binary's version is %q, not a commit; build it with make so the box can check out what you are running", version)
	case strings.HasSuffix(version, "-dev"), strings.HasSuffix(version, "-dirty"):
		return "", fmt.Errorf("this binary was built from a tree with uncommitted changes (%s); commit and rebuild before installing it on a host", version)
	}
	if i := strings.LastIndex(version, "-g"); i >= 0 && strings.Count(version, "-") >= 2 {
		version = version[i+2:]
	}
	if !revisionToken(version) {
		return "", fmt.Errorf("this binary's version %q is not a safe revision token", version)
	}
	return version, nil
}

// revisionToken is the portable subset of Git tag and object names that can cross the SSH
// shell boundary as a single, non-option token. InstallLine quotes it too; keeping this gate
// makes a malformed build version fail before any remote command is assembled.
func revisionToken(s string) bool {
	if s == "" || !asciiAlphaNumeric(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if asciiAlphaNumeric(c) || strings.ContainsRune("-._/+@", rune(c)) {
			continue
		}
		return false
	}
	return true
}

func asciiAlphaNumeric(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
