// SPDX-License-Identifier: AGPL-3.0-or-later

package hosts

import (
	"fmt"
	"strings"
)

// Revision is the commit a rudy version string names. make sets the version from
// git describe --tags --always --dirty: a tag, a tag plus -N-g<hash>, or a bare hash. A
// -dirty suffix or the dev fallback names nothing the box can check out.
func Revision(version string) (string, error) {
	switch {
	case version == "" || version == "dev":
		return "", fmt.Errorf("this binary's version is %q, not a commit; build it with make so the box can check out what you are running", version)
	case strings.HasSuffix(version, "-dirty"):
		return "", fmt.Errorf("this binary was built from a dirty tree (%s); commit and rebuild before installing it on a host", version)
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
