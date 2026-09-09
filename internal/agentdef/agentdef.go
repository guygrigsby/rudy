// Package agentdef reads agent definitions: the markdown files under an agents/ directory
// whose YAML frontmatter names a subagent's tools, model, thinking level and turn budget, and
// whose body is its system prompt.
package agentdef

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	yaml "go.yaml.in/yaml/v3"

	"github.com/guygrigsby/rudy/internal/frontmatter"
	"github.com/guygrigsby/rudy/internal/session"
)

// Definition is one agents/<name>.md, parsed.
type Definition struct {
	Name        string
	Description string
	Prompt      string                // the body; "" means the default base prompt
	Tools       []string              // nil means every tool; empty means none
	Model       string                // "" inherits
	Thinking    session.ThinkingLevel // "" inherits
	MaxTurns    int
}

// header is the YAML frontmatter. Tools is a pointer so an absent key (every tool) is
// distinguishable from an explicit empty list (no tool).
type header struct {
	Name        string    `yaml:"name"`
	Description string    `yaml:"description"`
	Tools       *[]string `yaml:"tools"`
	Model       string    `yaml:"model"`
	Thinking    string    `yaml:"thinking"`
	MaxTurns    int       `yaml:"max_turns"`
}

// Load reads every <root>/<stem>.md, in root order; the first definition for a name wins, so
// a user definition is not replaced by a workspace one of the same name. A file that fails to
// parse is reported in errs and skipped: one bad definition never costs the caller the rest.
// A root that does not exist is not an error.
func Load(roots []string) (map[string]Definition, []error) {
	defs := map[string]Definition{}
	var errs []error
	for _, root := range roots {
		ents, err := os.ReadDir(root)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("agents: read %s: %w", root, err))
			}
			continue
		}
		for _, ent := range ents {
			if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".md") {
				continue
			}
			path := filepath.Join(root, ent.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				errs = append(errs, fmt.Errorf("agents: read %s: %w", path, err))
				continue
			}
			d, err := parse(strings.TrimSuffix(ent.Name(), ".md"), data)
			if err != nil {
				errs = append(errs, fmt.Errorf("agents: %s: %w", path, err))
				continue
			}
			if _, seen := defs[d.Name]; !seen {
				defs[d.Name] = d
			}
		}
	}
	return defs, errs
}

// Resolve returns defs[name], or the implicit default definition for "default" or "".
func Resolve(defs map[string]Definition, name string) (Definition, bool) {
	if name == "" || name == "default" {
		if d, ok := defs["default"]; ok {
			return d, true
		}
		return Definition{Name: "default"}, true
	}
	d, ok := defs[name]
	return d, ok
}

// parse splits the frontmatter from the body and validates the header. stem is the file name
// without its extension, which is the definition's name unless the header repeats it.
func parse(stem string, data []byte) (Definition, error) {
	head, body, err := frontmatter.Split(string(data))
	if err != nil {
		return Definition{}, err
	}
	var fm header
	if err := yaml.Unmarshal([]byte(head), &fm); err != nil {
		return Definition{}, fmt.Errorf("frontmatter: %w", err)
	}
	if fm.Name != "" && fm.Name != stem {
		return Definition{}, fmt.Errorf("name %q does not match the file name %q", fm.Name, stem)
	}
	if fm.Description == "" {
		return Definition{}, errors.New("description is required")
	}
	thinking := session.ThinkingLevel(fm.Thinking)
	if fm.Thinking != "" && !thinking.Valid() {
		return Definition{}, fmt.Errorf("thinking %q is not off, low, medium or high", fm.Thinking)
	}
	if fm.MaxTurns < 0 {
		return Definition{}, fmt.Errorf("max_turns %d must be zero or positive", fm.MaxTurns)
	}
	d := Definition{
		Name:        stem,
		Description: fm.Description,
		Prompt:      strings.TrimSpace(body),
		Model:       fm.Model,
		Thinking:    thinking,
		MaxTurns:    fm.MaxTurns,
	}
	if fm.Tools != nil {
		// The agent tool is never among a subagent's tools: depth stays one, and a
		// definition that asks for it gets the rest of its list rather than a refusal.
		tools := slices.DeleteFunc(slices.Clone(*fm.Tools), func(t string) bool { return t == "agent" })
		d.Tools = tools
	}
	return d, nil
}
