// SPDX-License-Identifier: AGPL-3.0-or-later

// Package turn builds provider requests from a session's log and drives one turn at a time
// through its state machine: streaming a completion, gating and running tools, and recording
// every step back to the session.
package turn

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// Vars are the values a system prompt template may name. Every one is filled by the caller
// that knows it; a field left empty renders as nothing rather than as a hole (ADR 0024).
type Vars struct {
	// Base is the built-in opening paragraph, or an agent definition's body when one is in
	// force. A template that names ${base} wraps it; one that does not, replaces it.
	Base string
	// Tools is the tool list this turn offers, already rendered as "- name: description"
	// lines.
	Tools string
	// Agents is the AGENTS.md sections, each under its own heading.
	Agents string
	// The session's own facts.
	Version   string
	Workspace string
	Project   string
	Model     string
	// Date is today, in the model's own vocabulary. A model with a training cutoff behind
	// it guesses the date otherwise, and guesses wrong.
	Date string
	// OS is the platform the tools run on, which decides what a shell command may say.
	OS string
}

// varNames is the closed set a template may use, in the order `rudy prompt example` lists
// them. A name outside it is an error naming it, the way an unknown theme role or key
// action is: a typo in a prompt is silent otherwise, and a silent one is worse here than
// anywhere else in the config.
var varNames = []string{"base", "tools", "agents", "version", "workspace", "project", "model", "date", "os"}

// VarNames is the closed set, for the error a bad template gets and for the docs.
func VarNames() []string { return slices.Clone(varNames) }

// value is one variable's text.
func (v Vars) value(name string) (string, bool) {
	switch name {
	case "base":
		return v.Base, true
	case "tools":
		return v.Tools, true
	case "agents":
		return v.Agents, true
	case "version":
		return v.Version, true
	case "workspace":
		return v.Workspace, true
	case "project":
		return v.Project, true
	case "model":
		return v.Model, true
	case "date":
		return v.Date, true
	case "os":
		return v.OS, true
	}
	return "", false
}

// Render expands a system prompt template: `${name}` becomes that variable's text, `$${`
// is a literal `${`, and a name the set does not carry is an error naming it and listing
// what it could have been.
func Render(template string, v Vars) (string, error) {
	var b strings.Builder
	for i := 0; i < len(template); {
		j := strings.Index(template[i:], "${")
		if j < 0 {
			b.WriteString(template[i:])
			break
		}
		j += i
		// A doubled dollar escapes the opening, so a prompt can talk about ${tools}.
		if j > 0 && template[j-1] == '$' {
			b.WriteString(template[i : j-1])
			b.WriteString("${")
			i = j + 2
			continue
		}
		b.WriteString(template[i:j])
		end := strings.Index(template[j:], "}")
		if end < 0 {
			return "", fmt.Errorf("system prompt: %q is not closed", template[j:min(j+16, len(template))])
		}
		name := template[j+2 : j+end]
		text, ok := v.value(name)
		if !ok {
			return "", fmt.Errorf("system prompt: ${%s} is not a value; the set is ${%s}", name, strings.Join(varNames, "}, ${"))
		}
		b.WriteString(text)
		i = j + end + 1
	}
	return strings.TrimRight(b.String(), "\n") + "\n", nil
}

// DefaultTemplate is the prompt rudy ships, written in the same template language an
// operator's own file uses: what they override is a file exactly like this one.
const DefaultTemplate = `${base}
${tools}
${agents}`

// BaseParagraph is the built-in opening, which an agent definition's body replaces.
func BaseParagraph(ws session.Workspace, version string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are rudy %s, a coding agent working in the workspace at %s.\n", version, ws.Root)
	b.WriteString("Use the tools the API gives you; their names and schemas are authoritative. ")
	b.WriteString("Prefer the edit tool over rewriting whole files. ")
	b.WriteString("Run the tests before claiming work is done. ")
	b.WriteString("Keep replies short and lead with the result.")
	return b.String()
}

// SystemPrompt is the built-in template with the built-in base: what a session gets when
// nobody has written a prompt file.
func SystemPrompt(ws session.Workspace, version string, tools ...tool.Tool) string {
	out, err := Build(DefaultTemplate, Vars{
		Base:      BaseParagraph(ws, version),
		Tools:     ToolList(tools),
		Agents:    AgentsSections(ws),
		Version:   version,
		Workspace: ws.Root,
		Project:   ws.ProjectID,
		Date:      time.Now().Format("2006-01-02"),
		OS:        runtimeOS,
	})
	if err != nil {
		// DefaultTemplate is a constant in this package and its own test renders it, so a
		// failure here is a programming error rather than a configuration one.
		panic(err)
	}
	return out
}

// SystemPromptWith is the built-in template with base in place of the built-in paragraph,
// which is how an agent definition's body reaches a prompt: it replaces the opening and
// nothing else, so a subagent still gets the workspace's instructions and its own tools.
func SystemPromptWith(base string, ws session.Workspace, tools ...tool.Tool) string {
	out, err := Build(DefaultTemplate, Vars{
		Base:      strings.TrimRight(base, "\n"),
		Tools:     ToolList(tools),
		Agents:    AgentsSections(ws),
		Workspace: ws.Root,
		Project:   ws.ProjectID,
		Date:      time.Now().Format("2006-01-02"),
		OS:        runtimeOS,
	})
	if err != nil {
		panic(err)
	}
	return out
}

// Build renders a template, dropping the blank lines an empty variable leaves behind: a
// session with no tools and no AGENTS.md should not open with three empty paragraphs.
func Build(template string, v Vars) (string, error) {
	out, err := Render(template, v)
	if err != nil {
		return "", err
	}
	return collapse(out), nil
}

// collapse squeezes three or more newlines into two, so the sections read as paragraphs
// however many of them were empty.
func collapse(s string) string {
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return s
}

// ToolList is the tools a turn offers, one line each, or nothing at all when there are
// none. The schemas the API carries are still authoritative; this is so the model knows
// what it has, and so an agent definition's narrower view reads as the whole set.
//
// A tool with no description is listed by name: a nameless line is still a tool it has.
func ToolList(tools []tool.Tool) string {
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

// AgentsSections is the workspace's AGENTS.md and the global ~/.agents/AGENTS.md, each
// under its own heading, when they exist.
func AgentsSections(ws session.Workspace) string {
	var b strings.Builder
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
