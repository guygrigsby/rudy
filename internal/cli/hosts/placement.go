// SPDX-License-Identifier: AGPL-3.0-or-later

package hosts

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// Place is the workspace path on the host for a local cwd: the same path relative to home,
// on the host's home. Outside the local home there is no mapping and cwdFlag (--cwd) names
// the placement itself. Host paths are always forward-slash, so path rather than filepath
// on the way out.
func Place(localCwd, localHome, remoteHome, cwdFlag string) (string, error) {
	if cwdFlag != "" {
		if !path.IsAbs(cwdFlag) {
			return "", fmt.Errorf("--cwd %q is not absolute; name the workspace path on the host", cwdFlag)
		}
		return path.Clean(cwdFlag), nil
	}
	rel, err := filepath.Rel(localHome, localCwd)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside %s, so it has no place on the host; pass --cwd <path on host>", localCwd, localHome)
	}
	if rel == "." {
		return remoteHome, nil
	}
	return path.Join(remoteHome, filepath.ToSlash(rel)), nil
}
