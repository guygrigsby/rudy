// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// ResolveSecret turns an auth reference into its value. "" resolves to "", "env:NAME"
// reads the environment, "cache:KEY" reads KEY=value lines from cachePath, the file
// secrets.file names.
func ResolveSecret(ref string, env func(string) string, cachePath string) (string, error) {
	switch {
	case ref == "":
		return "", nil
	case strings.HasPrefix(ref, "env:"):
		name := strings.TrimPrefix(ref, "env:")
		v := env(name)
		if v == "" {
			return "", fmt.Errorf("config: secret %s: %s is unset", ref, name)
		}
		return v, nil
	case strings.HasPrefix(ref, "cache:"):
		key := strings.TrimPrefix(ref, "cache:")
		f, err := os.Open(cachePath)
		if err != nil {
			return "", fmt.Errorf("config: secret %s: %w; secrets.file names it", ref, err)
		}
		defer func() { _ = f.Close() }()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimPrefix(line, "export ")
			k, val, ok := strings.Cut(line, "=")
			if !ok || strings.TrimSpace(k) != key {
				continue
			}
			val = strings.TrimSpace(val)
			if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
				val = val[1 : len(val)-1]
			}
			return val, nil
		}
		if err := sc.Err(); err != nil {
			return "", fmt.Errorf("config: secret %s: %w", ref, err)
		}
		return "", fmt.Errorf("config: secret %s: %s not in %s; secrets.file names it", ref, key, cachePath)
	default:
		return "", fmt.Errorf("config: secret reference %q must be empty, env:NAME or cache:KEY", ref)
	}
}
