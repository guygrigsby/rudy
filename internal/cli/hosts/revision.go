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
		return version[i+2:], nil
	}
	return version, nil
}
