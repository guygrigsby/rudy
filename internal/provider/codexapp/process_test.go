// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"os"
	"strings"
	"testing"
)

func TestCodexEnvironmentAllowlistMatchesNormativeDocs(t *testing.T) {
	quoted := make([]string, len(codexEnvironmentKeys))
	for i, name := range codexEnvironmentKeys {
		quoted[i] = "`" + name + "`"
	}
	list := strings.Join(quoted[:len(quoted)-1], ", ") + " and " + quoted[len(quoted)-1]
	want := "The operational environment allowlist is " + list + "."
	for _, path := range []string{
		"../../../docs/specs/codex-app-server-runtime.md",
		"../../../docs/adr/0047-isolate-codex-process-environment.md",
	} {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		normalized := strings.Join(strings.Fields(string(contents)), " ")
		if !strings.Contains(normalized, want) {
			t.Errorf("%s must record the codexEnvironmentKeys allowlist as %q", path, want)
		}
	}
}
