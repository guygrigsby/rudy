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
)

// Role names one of the eleven paintable theme roles.
type Role string

// The eleven color roles. "code", the theme file's twelfth key, resolves to
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
)

// Roles lists the eleven color roles, in the order Theme.Colors resolves them.
var Roles = []Role{
	RoleAccent, RoleText, RoleMuted, RoleUser, RoleAssistant, RoleTool,
	RoleSuccess, RoleError, RoleWarning, RoleDiffAdd, RoleDiffDel,
}

// codeKey is the theme file's twelfth legal key; it names a chroma style, not a color.
const codeKey = "code"

// chromaPrefix is code's required value prefix: "chroma:<style>".
const chromaPrefix = "chroma:"

// defaultChromaStyle is the harness's compiled-in default chroma style, "tokyonight"
// from docs/specs/2026-09-07-rudy-design.md. The pinned chroma v2.27.0 only registers
// variant names (tokyonight-night, tokyonight-storm, tokyonight-moon, tokyonight-day),
// not this bare design name, so it is accepted here regardless of styles.Names(); any
// other theme's code value must be a name styles.Names() actually returns.
const defaultChromaStyle = "tokyonight"

// legalKeys are the twelve keys a theme file (or an overrides map) may set: the eleven
// color roles plus code.
var legalKeys = func() map[string]bool {
	m := map[string]bool{codeKey: true}
	for _, r := range Roles {
		m[string(r)] = true
	}
	return m
}()

// defaultRaw is the design's default screen theme (2026-09-07-rudy-design.md,
// [ui.theme]), the single source both Default and Load(dir, "default", ...) resolve
// from.
var defaultRaw = map[string]string{
	"accent":    "#7aa2f7",
	"text":      "#c0caf5",
	"muted":     "#565f89",
	"user":      "accent",
	"assistant": "text",
	"tool":      "muted",
	"success":   "#9ece6a",
	"error":     "#f7768e",
	"warning":   "#e0af68",
	"diff_add":  "success",
	"diff_del":  "error",
	codeKey:     chromaPrefix + defaultChromaStyle,
}

// Theme is a fully resolved set of roles: Colors for the eleven paintable roles,
// Chroma for the code role's style name (without the "chroma:" prefix).
type Theme struct {
	Name   string
	Colors map[Role]color.Color
	Chroma string
}

// Default is the design's built-in theme, ui.theme.name == "default".
func Default() Theme {
	th, err := resolve("default", defaultRaw)
	if err != nil {
		// defaultRaw is a compile-time constant covered by TestDefaultResolvesEveryRole;
		// resolve only fails on bad input, which this is not.
		panic("theme: built-in default is invalid: " + err.Error())
	}
	return th
}

// Load resolves the theme named name. For name == "default" it starts from the same
// raw table Default builds from; for any other name it parses <dir>/<name>.toml, where
// only the twelve legal keys are allowed and a role missing from the file takes the
// built-in default (the contract's "every one required in a theme file or the built-in
// default applies" read as: a file gap falls back rather than errors, an unrecognized
// key does not). overrides then applies on top of either, at the same twelve-key
// legality, and typically comes from ui.theme.<role> in config.toml.
func Load(dir, name string, overrides map[string]string) (Theme, error) {
	raw := make(map[string]string, len(defaultRaw))
	for k, v := range defaultRaw {
		raw[k] = v
	}
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

// validChromaStyle reports whether name is the harness's own default or a style
// styles.Names() actually registers.
func validChromaStyle(name string) bool {
	if name == defaultChromaStyle {
		return true
	}
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
