// Package turn builds provider requests from a session's log and drives one turn at a time
// through its state machine: streaming a completion, gating and running tools, and recording
// every step back to the session.
package turn

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/guygrigsby/rudy/internal/session"
)

// SystemPrompt is the base prompt followed by the workspace's AGENTS.md and the global
// ~/.agents/AGENTS.md, each under its own heading, when they exist.
func SystemPrompt(ws session.Workspace, version string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are rudy %s, a coding agent working in the workspace at %s.\n", version, ws.Root)
	b.WriteString("Use the tools the API gives you; their names and schemas are authoritative. ")
	b.WriteString("Prefer the edit tool over rewriting whole files. ")
	b.WriteString("Run the tests before claiming work is done. ")
	b.WriteString("Keep replies short and lead with the result.\n")

	paths := []string{filepath.Join(ws.Root, "AGENTS.md")}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".agents", "AGENTS.md"))
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "\n## AGENTS.md (%s)\n\n%s\n", p, strings.TrimRight(string(data), "\n"))
	}
	return b.String()
}
