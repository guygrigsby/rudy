// Package theme resolves the twelve config.toml theme roles
// (docs/specs/rudy-contracts.md, "themes/<name>.toml") into lipgloss styles. Eleven
// roles are colors; the twelfth, code, names a chroma style for syntax highlighting
// rather than a paintable color, and so lives in Theme.Chroma instead of Theme.Colors.
// No role ever paints a background: Style and StyleFor only ever set foreground.
package theme

import (
	"errors"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strings"

	"github.com/alecthomas/chroma/v2/styles"
	toml "github.com/pelletier/go-toml/v2"

	lipgloss "charm.land/lipgloss/v2"

	"github.com/guygrigsby/rudy/internal/config"
)

// Role names one of the twelve paintable theme roles.
type Role string

// The twelve color roles. "code", the theme file's thirteenth key, resolves to
// Theme.Chroma instead and so has no Role constant.
const (
	RoleAccent    Role = "accent"
	RoleText      Role = "text"
	RoleMuted     Role = "muted"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
	RoleSuccess   Role = "success"
	RoleError     Role = "error"
	RoleWarning   Role = "warning"
	RoleDiffAdd   Role = "diff_add"
	RoleDiffDel   Role = "diff_del"
	// RoleShell paints the composer while a draft is a shell command, and the row that
	// command becomes. It is its own role rather than warning's so a person can tell a
	// shell from a warning at a glance (ADR 0023).
	RoleShell Role = "shell"
)

// Roles lists the twelve color roles, in the order Theme.Colors resolves them.
var Roles = []Role{
	RoleAccent, RoleText, RoleMuted, RoleUser, RoleAssistant, RoleTool,
	RoleSuccess, RoleError, RoleWarning, RoleDiffAdd, RoleDiffDel, RoleShell,
}

// codeKey is the theme file's thirteenth legal key; it names a chroma style, not a color.
const codeKey = "code"

// chromaPrefix is code's required value prefix: "chroma:<style>".
const chromaPrefix = "chroma:"

// legalKeys are the twelve keys a theme file (or an overrides map) may set: the eleven
// color roles plus code.
var legalKeys = func() map[string]bool {
	m := map[string]bool{codeKey: true}
	for _, r := range Roles {
		m[string(r)] = true
	}
	return m
}()

// Theme is a fully resolved set of roles: Colors for the eleven paintable roles,
// Chroma for the code role's style name (without the "chroma:" prefix).
type Theme struct {
	Name   string
	Colors map[Role]color.Color
	Chroma string
}

// Default is the design's built-in theme, ui.theme.name == "default", resolved from
// config.ThemeDefaults so the twelve values live in exactly one place.
func Default() Theme {
	th, err := resolve("default", config.ThemeDefaults())
	if err != nil {
		// config.ThemeDefaults is a compile-time constant covered by
		// TestDefaultResolvesEveryRole; resolve only fails on bad input, which this
		// is not.
		panic("theme: built-in default is invalid: " + err.Error())
	}
	return th
}

// Load resolves the theme named name. For name == "default" it starts from the same
// config.ThemeDefaults table Default builds from; for any other name it parses
// <dir>/<name>.toml, where only the twelve legal keys are allowed and a role missing
// from the file takes the built-in default (the contract's "every one required in a
// theme file or the built-in default applies" read as: a file gap falls back rather
// than errors, an unrecognized key does not). overrides then applies on top of either,
// at the same twelve-key legality, and typically comes straight from config.Config's
// ui.theme table (config.UIConfig.Theme), which also carries a "name" key theme roles
// have no use for; Load ignores that one key rather than erroring, since the name
// argument is already authoritative over which theme is loading.
func Load(dir, name string, overrides map[string]string) (Theme, error) {
	raw := config.ThemeDefaults()
	if name != "default" {
		file := filepath.Join(dir, name+".toml")
		body, err := os.ReadFile(file)
		if err != nil {
			return Theme{}, fmt.Errorf("theme: %w", err)
		}
		var fileRaw map[string]string
		if err := toml.Unmarshal(body, &fileRaw); err != nil {
			return Theme{}, fmt.Errorf("theme: parse %s: %w", file, err)
		}
		for k, v := range fileRaw {
			if !legalKeys[k] {
				return Theme{}, fmt.Errorf("theme: %s: unknown role %q", file, k)
			}
			raw[k] = v
		}
	}
	for k, v := range overrides {
		if k == "name" {
			continue
		}
		if !legalKeys[k] {
			return Theme{}, fmt.Errorf("theme: unknown role %q", k)
		}
		raw[k] = v
	}
	return resolve(name, raw)
}

// resolve turns raw role values into colors. A "#..." value is a literal color; a value
// equal to another legal key copies that key's own raw value one hop deep, so a second
// hop, including a cycle, is refused; anything else is refused by role name. code must
// be "chroma:<style>" naming a style validChromaStyle knows.
func resolve(name string, raw map[string]string) (Theme, error) {
	colors := make(map[Role]color.Color, len(Roles))
	var errs []error
	for _, r := range Roles {
		v := raw[string(r)]
		switch {
		case strings.HasPrefix(v, "#"):
			colors[r] = lipgloss.Color(v)
		case legalKeys[v]:
			target := raw[v]
			if !strings.HasPrefix(target, "#") {
				errs = append(errs, fmt.Errorf("theme: role %q points to %q, which must resolve to a color in one hop", r, v))
				continue
			}
			colors[r] = lipgloss.Color(target)
		default:
			errs = append(errs, fmt.Errorf("theme: role %q value %q is not a hex color or a role name", r, v))
		}
	}
	code := raw[codeKey]
	chromaName, ok := strings.CutPrefix(code, chromaPrefix)
	switch {
	case !ok:
		errs = append(errs, fmt.Errorf("theme: role %q value %q must be chroma:<style>", codeKey, code))
	case !validChromaStyle(chromaName):
		errs = append(errs, fmt.Errorf("theme: role %q names unknown chroma style %q", codeKey, chromaName))
	}
	if len(errs) > 0 {
		return Theme{}, errors.Join(errs...)
	}
	return Theme{Name: name, Colors: colors, Chroma: chromaName}, nil
}

// validChromaStyle reports whether styles.Names() registers name.
func validChromaStyle(name string) bool {
	for _, n := range styles.Names() {
		if n == name {
			return true
		}
	}
	return false
}

// Style is r's style: foreground only, so no theme ever paints a background.
func (t Theme) Style(r Role) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(t.Colors[r])
}

// StyleFor is Style by an untyped role name, such as a Span's role field
// (docs/specs/rudy-contracts.md, Span). An unknown name falls back to RoleText.
func (t Theme) StyleFor(name string) lipgloss.Style {
	r := Role(name)
	if _, ok := t.Colors[r]; ok {
		return t.Style(r)
	}
	return t.Style(RoleText)
}
