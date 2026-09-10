// Package skills is the skills plugin: it indexes the configured skill directories into a
// session's context on session_opened, and answers /skills with the same list. It has no
// private path to the loader either, internal/skills.Load and internal/skills.Migrate do the
// reading and copying; this package is only how a session sees the result.
package skills

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/skills"
)

type skillsPlugin struct {
	dirs []string

	mu      sync.Mutex
	noticed map[string]bool
}

// New is the skills plugin over dirs (skills.dirs from config): an absolute entry is used as
// is, a relative one resolves against the workspace root the hook or command fires for.
func New(dirs []string) plugin.Plugin {
	return &skillsPlugin{dirs: dirs, noticed: map[string]bool{}}
}

func (p *skillsPlugin) Name() string { return "skills" }

func (p *skillsPlugin) Init(ctx context.Context, h plugin.Host) error {
	if err := h.RegisterHook(plugin.HookHandler{
		Point: plugin.HookSessionOpened,
		Handle: func(ctx context.Context, call plugin.HookCall) (any, error) {
			payload, ok := call.Payload.(*plugin.SessionOpenedPayload)
			if !ok || payload == nil {
				return nil, nil
			}
			list, errs := p.load(payload.Workspace.Root)
			p.notice(h, errs)
			text := render(list)
			if text == "" {
				return nil, nil
			}
			return &plugin.SessionOpenedResult{Context: text}, nil
		},
	}); err != nil {
		return err
	}
	return h.RegisterCommand(plugin.Command{
		Name:        "skills",
		Description: "List the skills available in this workspace",
		Run: func(ctx context.Context, call plugin.CommandCall) (plugin.Action, error) {
			list, errs := p.load(call.Workspace.Root)
			p.notice(h, errs)
			var b strings.Builder
			for _, s := range list {
				fmt.Fprintf(&b, "%s  %s  %s\n", s.Name, s.Description, s.Dir)
			}
			return plugin.Notice{Text: strings.TrimRight(b.String(), "\n")}, nil
		},
	})
}

// load resolves p.dirs against root and loads the skills they hold.
func (p *skillsPlugin) load(root string) ([]skills.Skill, []error) {
	resolved := make([]string, len(p.dirs))
	for i, d := range p.dirs {
		if filepath.IsAbs(d) {
			resolved[i] = d
		} else {
			resolved[i] = filepath.Join(root, d)
		}
	}
	return skills.Load(resolved)
}

// notice surfaces each distinct message from Load once per process: a skill that keeps
// failing to parse costs the operator one notice, not one per session it fires for.
func (p *skillsPlugin) notice(h plugin.Host, errs []error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, err := range errs {
		msg := err.Error()
		if p.noticed[msg] {
			continue
		}
		p.noticed[msg] = true
		h.Notice(msg)
	}
}

// render turns the loaded skills into the context block a session_opened hook adds. No
// skills found means no block: an empty section would only waste context.
func render(list []skills.Skill) string {
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Skills\n\n")
	b.WriteString("Skills are instructions for specific tasks. When one matches the task, read its SKILL.md with the read tool before acting.\n")
	// The rule a skill's own author relies on: a skill that says `references/api.md` means
	// the file beside its SKILL.md, not one under the workspace the session happens to be
	// in. Without this the model reads the wrong path, or nothing.
	b.WriteString("A relative path inside a skill resolves against that skill's own directory, the one holding its SKILL.md; use the absolute path in tool calls.\n\n")
	for _, s := range list {
		fmt.Fprintf(&b, "- %s: %s (read %s before using it)\n", s.Name, s.Description, filepath.Join(s.Dir, "SKILL.md"))
	}
	return strings.TrimRight(b.String(), "\n")
}
