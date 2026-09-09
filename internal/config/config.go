package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/spf13/viper"

	"github.com/guygrigsby/rudy/internal/session"
)

// ProviderConfig is one [providers.<name>] table.
type ProviderConfig struct {
	Wire    string            `mapstructure:"wire"`     // openai_chat, anthropic_messages, custom
	BaseURL string            `mapstructure:"base_url"` // ends with /v1
	Auth    string            `mapstructure:"auth"`     // "", "env:NAME", "cache:KEY"
	Headers map[string]string `mapstructure:"headers"`  // extra request headers, verbatim
	Dialect string            `mapstructure:"dialect"`  // "" or clinepass; clinepass requires wire == openai_chat
}

// LayoutConfig is the [ui.layout] table.
type LayoutConfig struct {
	Slots []string `mapstructure:"slots"`
}

// TranscriptConfig is the [ui.transcript] table.
type TranscriptConfig struct {
	ToolCollapsed    bool   `mapstructure:"tool_collapsed"`
	ToolPreviewLines int    `mapstructure:"tool_preview_lines"`
	Thinking         string `mapstructure:"thinking"`
	UserPrefix       string `mapstructure:"user_prefix"`
	BlockGap         int    `mapstructure:"block_gap"`
}

// DiffConfig is the [ui.diff] table.
type DiffConfig struct {
	Style string `mapstructure:"style"`
}

// StatusConfig is the [ui.status] table.
type StatusConfig struct {
	Items []string `mapstructure:"items"`
}

// NoticesConfig is the [ui.notices] table. Max is how many notice lines the client draws,
// newest first: a notice is chrome under the transcript and must never push the editor
// off the frame, so the count is a config field like every other render choice.
type NoticesConfig struct {
	Max int `mapstructure:"max"`
}

// UIConfig is the [ui] table. Theme holds ui.theme.name plus role overrides, merged
// over ThemeDefaults by hand in Load since viper replaces a nested default table
// wholesale rather than merging it key by key with a partial file table.
type UIConfig struct {
	Render     string            `mapstructure:"render"`
	Vim        bool              `mapstructure:"vim"`
	Layout     LayoutConfig      `mapstructure:"layout"`
	Transcript TranscriptConfig  `mapstructure:"transcript"`
	Diff       DiffConfig        `mapstructure:"diff"`
	Status     StatusConfig      `mapstructure:"status"`
	Notices    NoticesConfig     `mapstructure:"notices"`
	Theme      map[string]string `mapstructure:"theme"`
}

// themeDefaults are the design's twelve ui.theme role values
// (docs/specs/2026-09-07-rudy-design.md, [ui.theme]): the eleven color roles plus code.
// ThemeDefaults is the one exported copy of this data; internal/tui/theme.Default and
// Load's built-in "default" theme resolve from it too, so it lives here once rather
// than once per package. Kept out of Defaults() (and so out of viper's SetDefault)
// because viper replaces a nested default table wholesale the moment the file sets any
// key under the same table, instead of merging it key by key; Load merges these by
// hand, in ui.theme's case alongside the "name" key, which is config's alone (a theme
// file has no use for it).
var themeDefaults = map[string]string{
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
	"code":      "chroma:tokyonight-night",
}

// ThemeDefaults is the design's twelve ui.theme role values, a fresh copy each call.
func ThemeDefaults() map[string]string {
	out := make(map[string]string, len(themeDefaults))
	for k, v := range themeDefaults {
		out[k] = v
	}
	return out
}

// MemoryConfig is the [memory] table.
type MemoryConfig struct {
	Dir          string         `mapstructure:"dir"`
	Enabled      bool           `mapstructure:"enabled"`
	SummaryModel string         `mapstructure:"summary_model"`
	Fold         map[string]int `mapstructure:"fold"`
}

// Config is config.toml after defaults, environment and overrides.
type Config struct {
	Default struct {
		Provider string `mapstructure:"provider"`
		Model    string `mapstructure:"model"`
		Thinking string `mapstructure:"thinking"`
	} `mapstructure:"default"`
	Agent         string `mapstructure:"agent"`
	HookTimeoutMS int    `mapstructure:"hook_timeout_ms"`
	ToolTimeoutMS int    `mapstructure:"tool_timeout_ms"`
	Permissions   struct {
		Mode          string   `mapstructure:"mode"`
		Dangerous     []string `mapstructure:"dangerous"`
		DoublePressMS int      `mapstructure:"double_press_ms"`
	} `mapstructure:"permissions"`
	Sessions struct {
		Dir       string  `mapstructure:"dir"`
		CompactAt float64 `mapstructure:"compact_at"`
	} `mapstructure:"sessions"`
	Providers map[string]ProviderConfig `mapstructure:"providers"`
	// Plugins and PluginsDisabled are filled by hand from the raw [plugins] table after
	// Unmarshal, since "disabled" is a sibling key of the per-plugin tables under the same
	// [plugins] section rather than a table itself; mapstructure has no way to split that.
	Plugins         map[string]map[string]any `mapstructure:"-"`
	PluginsDisabled []string                  `mapstructure:"-"`
	Skills          struct {
		Dirs        []string `mapstructure:"dirs"`
		MigrateFrom []string `mapstructure:"migrate_from"`
	} `mapstructure:"skills"`
	Memory MemoryConfig `mapstructure:"memory"`
	MCP    struct {
		ConnectTimeoutMS int `mapstructure:"connect_timeout_ms"`
	} `mapstructure:"mcp"`
	MaxTokens int      `mapstructure:"max_tokens"`
	UI        UIConfig `mapstructure:"ui"`
	// Keys is the [keys] table: pi action id to bound keys. Filled by hand from the raw
	// TOML, like Plugins above, because viper lowercases map keys and pi's action ids
	// are case-sensitive (app.model.cycleForward).
	Keys map[string][]string `mapstructure:"-"`
	// ConfigDir is paths.Config, filled by Load. Anything that reads a file next to
	// config.toml (agent definitions) has the Config but not the Paths.
	ConfigDir string `mapstructure:"-"`
}

// Defaults are the values in force when neither the file nor the environment sets a key.
// Keys with empty defaults exist so RUDY_* environment variables can set them.
func Defaults() map[string]any {
	return map[string]any{
		"default.provider":            "",
		"default.model":               "",
		"default.thinking":            "high",
		"agent":                       "default",
		"hook_timeout_ms":             5000,
		"tool_timeout_ms":             600000,
		"permissions.mode":            "strict",
		"permissions.double_press_ms": 500,
		"permissions.dangerous": []string{
			"rm -rf", "rm -r", "git push --force", "git push -f", "git reset --hard",
			"git clean", "sudo", "chmod -R", "chown -R", "mkfs", "dd",
		},
		"sessions.dir":                     "", // filled from paths.Data when still empty after Load
		"sessions.compact_at":              0.8,
		"skills.dirs":                      []string{"~/.agents/skills", ".agents/skills"},
		"skills.migrate_from":              []string{"~/.claude/skills", "~/.pi/agent/skills"},
		"memory.dir":                       "~/.agents/memory",
		"memory.enabled":                   true,
		"memory.summary_model":             "",
		"mcp.connect_timeout_ms":           10000,
		"max_tokens":                       8192,
		"ui.render":                        "inline",
		"ui.vim":                           true,
		"ui.layout.slots":                  []string{"transcript", "input", "status"},
		"ui.transcript.tool_collapsed":     true,
		"ui.transcript.tool_preview_lines": 2,
		"ui.transcript.thinking":           "hidden",
		"ui.transcript.user_prefix":        "›",
		"ui.transcript.block_gap":          1,
		"ui.diff.style":                    "text",
		"ui.status.items":                  []string{"vim_mode", "model", "permission_mode", "context", "cost", "workspace", "turn"},
		"ui.notices.max":                   3,
	}
}

// Load reads <paths.Config>/config.toml when it exists, applies RUDY_* environment
// variables (RUDY_PERMISSIONS_MODE sets permissions.mode) and then overrides, validates
// and returns the result. It never writes.
func Load(paths Paths, overrides map[string]any) (*Config, error) {
	v := viper.New()
	for k, val := range Defaults() {
		v.SetDefault(k, val)
	}
	v.SetEnvPrefix("RUDY")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	file := filepath.Join(paths.Config, "config.toml")
	body, statErr := os.ReadFile(file)
	fileExists := statErr == nil
	if fileExists {
		v.SetConfigFile(file)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("config: read %s: %w", file, err)
		}
	}
	for k, val := range overrides {
		v.Set(k, val)
	}
	var c Config
	if err := v.Unmarshal(&c); err != nil {
		return nil, fmt.Errorf("config: decode: %w", err)
	}
	// viper lowercases every map key it reads from the config file, which would
	// corrupt HTTP header names. Re-read the file directly to restore their case.
	if fileExists {
		if err := restoreHeaderCase(body, c.Providers); err != nil {
			return nil, fmt.Errorf("config: read %s: %w", file, err)
		}
	}
	// mapstructure cannot split "disabled" (a plain key) from the per-plugin tables that
	// share its [plugins] parent, so both come from viper's raw view instead of Unmarshal.
	c.Plugins = map[string]map[string]any{}
	for name, raw := range v.GetStringMap("plugins") {
		if name == "disabled" {
			continue
		}
		if table, ok := raw.(map[string]any); ok {
			c.Plugins[name] = table
		}
	}
	c.PluginsDisabled = v.GetStringSlice("plugins.disabled")
	// ui.theme is a table of strings (name plus role overrides); merge the file's and
	// overrides' values over the built-in defaults by hand, see themeDefaults.
	c.UI.Theme = ThemeDefaults()
	c.UI.Theme["name"] = "default"
	for k, val := range v.GetStringMapString("ui.theme") {
		c.UI.Theme[k] = val
	}
	// permissions.double_press_ms is the contract's canonical key; the design also
	// shows it under [ui], so that spelling is accepted as an alias when the canonical
	// key is absent from both the file and overrides.
	permSet := isSetIn(overrides, "permissions.double_press_ms") || (fileExists && v.InConfig("permissions.double_press_ms"))
	uiAliasSet := isSetIn(overrides, "ui.double_press_ms") || (fileExists && v.InConfig("ui.double_press_ms"))
	if !permSet && uiAliasSet {
		c.Permissions.DoublePressMS = v.GetInt("ui.double_press_ms")
	}
	// keys is read from the raw TOML, not through viper: viper lowercases every map key
	// it reads, which would corrupt pi's case-sensitive action ids (app.model.cycleForward).
	c.Keys = map[string][]string{}
	if fileExists {
		keys, err := parseKeysTable(body)
		if err != nil {
			return nil, err
		}
		c.Keys = keys
	}
	c.ConfigDir = paths.Config
	if c.Sessions.Dir == "" {
		c.Sessions.Dir = filepath.Join(paths.Data, "sessions")
	}
	c.Sessions.Dir = ExpandHome(c.Sessions.Dir, paths.Home)
	c.Memory.Dir = ExpandHome(c.Memory.Dir, paths.Home)
	for i, d := range c.Skills.Dirs {
		c.Skills.Dirs[i] = ExpandHome(d, paths.Home)
	}
	for i, d := range c.Skills.MigrateFrom {
		c.Skills.MigrateFrom[i] = ExpandHome(d, paths.Home)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// ExpandHome replaces a leading "~/" (or a bare "~") with home. Anything else is returned as
// is, so a relative skills dir still resolves against the workspace later.
func ExpandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// isSetIn reports whether overrides sets key directly, used to test whether an alias
// key (ui.double_press_ms) should be read at all: an explicit override or file value
// for the canonical key always wins.
func isSetIn(overrides map[string]any, key string) bool {
	_, ok := overrides[key]
	return ok
}

// parseKeysTable reads the [keys] table straight from the raw TOML, the way
// restoreHeaderCase does for header casing, since viper lowercases every map key it
// reads and pi's action ids are case-sensitive (app.model.cycleForward). A string value
// becomes a one-element list, a list of strings stays a list, an empty list stays
// present and empty (it unbinds the action), and anything else is an error naming the
// action id.
func parseKeysTable(body []byte) (map[string][]string, error) {
	var raw struct {
		Keys map[string]any `toml:"keys"`
	}
	if err := toml.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("config: keys: %w", err)
	}
	keys := make(map[string][]string, len(raw.Keys))
	var errs []error
	for action, val := range raw.Keys {
		switch bound := val.(type) {
		case string:
			keys[action] = []string{bound}
		case []any:
			list := make([]string, 0, len(bound))
			ok := true
			for _, item := range bound {
				s, isStr := item.(string)
				if !isStr {
					ok = false
					break
				}
				list = append(list, s)
			}
			if !ok {
				errs = append(errs, fmt.Errorf("config: keys.%s must be a string or a list of strings", action))
				continue
			}
			keys[action] = list
		default:
			errs = append(errs, fmt.Errorf("config: keys.%s must be a string or a list of strings", action))
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return keys, nil
}

// restoreHeaderCase re-parses the raw TOML to recover the original casing of
// providers.<name>.headers keys, which viper's Unmarshal lowercases.
func restoreHeaderCase(body []byte, providers map[string]ProviderConfig) error {
	var raw struct {
		Providers map[string]struct {
			Headers map[string]string `toml:"headers"`
		} `toml:"providers"`
	}
	if err := toml.Unmarshal(body, &raw); err != nil {
		return err
	}
	for name, rp := range raw.Providers {
		if len(rp.Headers) == 0 {
			continue
		}
		key := strings.ToLower(name)
		p, ok := providers[key]
		if !ok {
			continue
		}
		p.Headers = rp.Headers
		providers[key] = p
	}
	return nil
}

// validWires and validDialects gate providers.<name>.wire and .dialect. A non-empty dialect
// is a rewrite of the openai_chat wire format, so it makes no sense paired with any other wire.
var validWires = map[string]bool{"openai_chat": true, "anthropic_messages": true, "custom": true}
var validDialects = map[string]bool{"": true, "clinepass": true}

func (c *Config) validate() error {
	var errs []error
	if !session.Mode(c.Permissions.Mode).Valid() {
		errs = append(errs, fmt.Errorf("config: permissions.mode %q is not strict, permissive or off", c.Permissions.Mode))
	}
	if !session.ThinkingLevel(c.Default.Thinking).Valid() {
		errs = append(errs, fmt.Errorf("config: default.thinking %q is not off, low, medium or high", c.Default.Thinking))
	}
	if len(c.Providers) > 0 {
		if _, ok := c.Providers[c.Default.Provider]; !ok {
			errs = append(errs, fmt.Errorf("config: default.provider %q is not a [providers.*] table", c.Default.Provider))
		}
	}
	for name, p := range c.Providers {
		if !validWires[p.Wire] {
			errs = append(errs, fmt.Errorf("config: providers.%s.wire %q is not openai_chat, anthropic_messages or custom", name, p.Wire))
		}
		if p.BaseURL == "" {
			errs = append(errs, fmt.Errorf("config: providers.%s.base_url is empty", name))
		}
		if !validDialects[p.Dialect] {
			errs = append(errs, fmt.Errorf("config: providers.%s.dialect %q is not \"\" or clinepass", name, p.Dialect))
		}
		if p.Dialect != "" && p.Wire != "openai_chat" {
			errs = append(errs, fmt.Errorf("config: providers.%s.dialect %q requires wire openai_chat, got %q", name, p.Dialect, p.Wire))
		}
	}
	if c.Permissions.DoublePressMS <= 0 {
		// Zero is not "no window": it is a window no two presses can fall inside, which
		// leaves the double-Esc cancel of the TurnControl table unreachable.
		errs = append(errs, fmt.Errorf("config: permissions.double_press_ms %d must be positive", c.Permissions.DoublePressMS))
	}
	if c.HookTimeoutMS <= 0 {
		errs = append(errs, fmt.Errorf("config: hook_timeout_ms %d must be positive", c.HookTimeoutMS))
	}
	if c.ToolTimeoutMS < 0 {
		errs = append(errs, fmt.Errorf("config: tool_timeout_ms %d must be zero or positive; zero means no timeout", c.ToolTimeoutMS))
	}
	if c.Sessions.CompactAt <= 0 || c.Sessions.CompactAt > 1 {
		errs = append(errs, fmt.Errorf("config: sessions.compact_at %v must be greater than 0 and at most 1", c.Sessions.CompactAt))
	}
	if c.MCP.ConnectTimeoutMS <= 0 {
		errs = append(errs, fmt.Errorf("config: mcp.connect_timeout_ms %d must be positive", c.MCP.ConnectTimeoutMS))
	}
	if c.MaxTokens <= 0 {
		errs = append(errs, fmt.Errorf("config: max_tokens %d must be positive", c.MaxTokens))
	}
	errs = append(errs, c.validateUI()...)
	return errors.Join(errs...)
}

// validRenders, validThinking and validDiffStyles gate the matching ui.* enums.
var validRenders = map[string]bool{"inline": true, "altscreen": true}
var validThinking = map[string]bool{"hidden": true, "shown": true}
var validDiffStyles = map[string]bool{"text": true, "background": true}

// legalSlots are ui.layout.slots' legal entries; transcript, input and status are each
// required exactly once, header is optional.
var legalSlots = map[string]bool{"header": true, "transcript": true, "input": true, "status": true}
var requiredSlots = []string{"transcript", "input", "status"}

// builtinStatusItems are ui.status.items' seven built-in keys; anything else must be a
// non-empty "<plugin>:<key>" pair.
var builtinStatusItems = map[string]bool{
	"vim_mode": true, "model": true, "permission_mode": true,
	"context": true, "cost": true, "workspace": true, "turn": true,
}

func (c *Config) validateUI() []error {
	var errs []error
	if !validRenders[c.UI.Render] {
		errs = append(errs, fmt.Errorf("config: ui.render %q is not inline or altscreen", c.UI.Render))
	}
	if !validThinking[c.UI.Transcript.Thinking] {
		errs = append(errs, fmt.Errorf("config: ui.transcript.thinking %q is not hidden or shown", c.UI.Transcript.Thinking))
	}
	if !validDiffStyles[c.UI.Diff.Style] {
		errs = append(errs, fmt.Errorf("config: ui.diff.style %q is not text or background", c.UI.Diff.Style))
	}
	if c.UI.Transcript.ToolPreviewLines < 0 {
		errs = append(errs, fmt.Errorf("config: ui.transcript.tool_preview_lines %d must be zero or positive", c.UI.Transcript.ToolPreviewLines))
	}
	if c.UI.Transcript.BlockGap < 0 {
		errs = append(errs, fmt.Errorf("config: ui.transcript.block_gap %d must be zero or positive", c.UI.Transcript.BlockGap))
	}
	if c.UI.Notices.Max < 0 {
		errs = append(errs, fmt.Errorf("config: ui.notices.max %d must be zero or positive", c.UI.Notices.Max))
	}
	seenSlots := map[string]bool{}
	for _, s := range c.UI.Layout.Slots {
		if !legalSlots[s] {
			errs = append(errs, fmt.Errorf("config: ui.layout.slots %q is not header, transcript, input or status", s))
			continue
		}
		if seenSlots[s] {
			errs = append(errs, fmt.Errorf("config: ui.layout.slots repeats %q", s))
			continue
		}
		seenSlots[s] = true
	}
	for _, req := range requiredSlots {
		if !seenSlots[req] {
			errs = append(errs, fmt.Errorf("config: ui.layout.slots is missing %q", req))
		}
	}
	for _, item := range c.UI.Status.Items {
		if builtinStatusItems[item] {
			continue
		}
		plugin, key, ok := strings.Cut(item, ":")
		if !ok || plugin == "" || key == "" {
			errs = append(errs, fmt.Errorf("config: ui.status.items %q is not a built-in item or plugin:key", item))
		}
	}
	return errs
}
