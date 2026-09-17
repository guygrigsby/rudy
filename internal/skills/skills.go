// SPDX-License-Identifier: AGPL-3.0-or-later

// Package skills reads the skill directories a user or a workspace keeps under skills.dirs:
// one subdirectory per skill, each with a SKILL.md whose frontmatter names it and describes
// when to use it. Skills are read, never written, except by Migrate, which copies them in
// from an older tool's layout and never overwrites.
package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	yaml "go.yaml.in/yaml/v3"

	"github.com/guygrigsby/rudy/internal/frontmatter"
)

// Skill is one skill directory, parsed.
type Skill struct {
	Name         string
	Description  string
	AllowedTools []string // nil means every tool
	Dir          string   // absolute path to the skill's directory
}

// header is the SKILL.md YAML frontmatter. AllowedTools is a pointer so an absent key is
// distinguishable from an explicit empty list.
type header struct {
	Name         string    `yaml:"name"`
	Description  string    `yaml:"description"`
	AllowedTools *[]string `yaml:"allowed-tools"`
}

// Load reads every <root>/<name>/SKILL.md, skipping dot-prefixed directories, in root order;
// the first skill for a name wins,
// so a workspace skill never replaces a user skill of the same name (or the reverse,
// depending on the order the caller passes). A subdirectory that fails to parse is reported
// in errs and skipped: one broken skill never costs the caller the rest. A root that does
// not exist is not an error, and a subdirectory with no SKILL.md is not a skill.
func Load(roots []string) ([]Skill, []error) {
	var out []Skill
	seen := map[string]bool{}
	var errs []error
	for _, root := range roots {
		ents, err := os.ReadDir(root)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("skills: read %s: %w", root, err))
			}
			continue
		}
		for _, ent := range ents {
			// A dot-prefixed directory is never a skill: .git, .DS_Store and an editor's
			// backup directory all sit in a skills root, and reading one is at best wasted
			// work and at worst a stray SKILL.md loaded from something nobody installed.
			if !ent.IsDir() || strings.HasPrefix(ent.Name(), ".") {
				continue
			}
			dir := filepath.Join(root, ent.Name())
			path := filepath.Join(dir, "SKILL.md")
			data, err := os.ReadFile(path)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				errs = append(errs, fmt.Errorf("skills: read %s: %w", path, err))
				continue
			}
			s, err := parse(ent.Name(), dir, data)
			if err != nil {
				errs = append(errs, fmt.Errorf("skills: %s: %w", dir, err))
				continue
			}
			if seen[s.Name] {
				continue
			}
			seen[s.Name] = true
			out = append(out, s)
		}
	}
	return out, errs
}

// parse splits the frontmatter from the body and validates the header. name is the
// directory's own name, which is the skill's name unless the header gives one.
func parse(name, dir string, data []byte) (Skill, error) {
	head, _, err := frontmatter.Split(string(data))
	if err != nil {
		return Skill{}, err
	}
	var h header
	if err := yaml.Unmarshal([]byte(head), &h); err != nil {
		return Skill{}, fmt.Errorf("frontmatter: %w", err)
	}
	if h.Description == "" {
		return Skill{}, errors.New("description is required")
	}
	skillName := h.Name
	if skillName == "" {
		skillName = name
	}
	s := Skill{Name: skillName, Description: h.Description, Dir: dir}
	if h.AllowedTools != nil {
		s.AllowedTools = *h.AllowedTools
	}
	return s, nil
}

// Migrate copies every skill directory under each from root that <to> does not already have,
// in root order; a name already copied from an earlier root, like a name <to> already has, is
// skipped rather than overwritten and is counted once in skipped. A from root that does not
// exist is not an error. Whole directories are copied, so a skill's other assets travel with
// its SKILL.md.
func Migrate(from []string, to string) (copied, skipped []string, err error) {
	done := map[string]bool{}
	for _, root := range from {
		ents, rerr := os.ReadDir(root)
		if rerr != nil {
			if errors.Is(rerr, os.ErrNotExist) {
				continue
			}
			return copied, skipped, fmt.Errorf("skills: read %s: %w", root, rerr)
		}
		for _, ent := range ents {
			if !ent.IsDir() {
				continue
			}
			name := ent.Name()
			src := filepath.Join(root, name)
			if _, err := os.Stat(filepath.Join(src, "SKILL.md")); err != nil {
				continue
			}
			if done[name] {
				skipped = append(skipped, name)
				continue
			}
			dst := filepath.Join(to, name)
			if _, err := os.Stat(dst); err == nil {
				done[name] = true
				skipped = append(skipped, name)
				continue
			}
			if err := copyStaged(src, dst, to, name); err != nil {
				return copied, skipped, err
			}
			done[name] = true
			copied = append(copied, name)
		}
	}
	return copied, skipped, nil
}

// copyStaged copies src into a staging directory beside dst and renames it into place only
// once the copy finishes without error. os.CopyFS stops at the first error and leaves
// whatever it already wrote behind; copying into a scratch directory first, rather than
// straight into dst, keeps a partial copy from ever landing at the path a later Migrate's
// os.Stat treats as "already migrated". Any failure removes the staging directory and
// reports the skill name, and dst is left untouched for the next attempt.
func copyStaged(src, dst, to, name string) (err error) {
	if err := os.MkdirAll(to, 0o755); err != nil {
		return fmt.Errorf("skills: migrate %s: %w", name, err)
	}
	staging, err := os.MkdirTemp(to, ".migrate-"+name+"-*")
	if err != nil {
		return fmt.Errorf("skills: migrate %s: %w", name, err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(staging)
		}
	}()
	if err = os.CopyFS(staging, os.DirFS(src)); err != nil {
		return fmt.Errorf("skills: migrate %s: %w", name, err)
	}
	if err = os.Rename(staging, dst); err != nil {
		return fmt.Errorf("skills: migrate %s: %w", name, err)
	}
	return nil
}
