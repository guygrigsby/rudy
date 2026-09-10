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
	"github.com/guygrigsby/rudy/internal/tool"
)

// SystemPrompt is the base prompt, the tools this turn offers, and then the workspace's
// AGENTS.md and the global ~/.agents/AGENTS.md, each under its own heading, when they exist.
func SystemPrompt(ws session.Workspace, version string, tools ...tool.Tool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are rudy %s, a coding agent working in the workspace at %s.\n", version, ws.Root)
	b.WriteString("Use the tools the API gives you; their names and schemas are authoritative. ")
	b.WriteString("Prefer the edit tool over rewriting whole files. ")
	b.WriteString("Run the tests before claiming work is done. ")
	b.WriteString("Keep replies short and lead with the result.\n")
	return SystemPromptWith(b.String(), ws, tools...)
}

// SystemPromptWith is base, the same tool list and the same AGENTS.md sections. An agent
// definition's body replaces the base prompt and nothing else: a subagent still gets the
// workspace's instructions, which are about the repository rather than about who is reading
// them, and it gets its own narrower tool list, which is the point of the definition.
func SystemPromptWith(base string, ws session.Workspace, tools ...tool.Tool) string {
	var b strings.Builder
	b.WriteString(base)
	if !strings.HasSuffix(base, "\n") {
		b.WriteString("\n")
	}
	b.WriteString(toolSection(tools))

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

// toolSection names the tools this turn offers, one line each, or nothing at all when there
// are none. The schemas the API carries are still authoritative; this is so the model knows
// what it has without having to be told a schema is missing, and so an agent definition's
// narrower view reads as the whole set rather than as a subset of some other list.
//
// A tool with no description is listed by name: a nameless line is still a tool it has.
func toolSection(tools []tool.Tool) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Tools\n\n")
	for _, t := range tools {
		if t.Description == "" {
			fmt.Fprintf(&b, "- %s\n", t.Name)
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n", t.Name, t.Description)
	}
	return b.String()
}
