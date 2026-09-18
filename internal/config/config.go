// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	// Items is the status line under the composer.
	Items []string `mapstructure:"items"`
	// AboveEditor is the line drawn over the composer, for the items worth seeing while
	// typing rather than after: the turn cell and whatever else belongs with it. Same
	// vocabulary as Items; an item named in both is drawn in both.
	AboveEditor []string `mapstructure:"above_editor"`
	// Host prefixes the workspace item with the host name when the session runs on a
	// machine reached by --host or remote.host. A render choice, so a config field.
	Host bool `mapstructure:"host"`
}

// NoticesConfig is the [ui.notices] table. Max is how many notice lines the client draws,
// newest first: a notice is chrome under the transcript and must never push the editor
// off the frame, so the count is a config field like every other render choice.
type NoticesConfig struct {
	Max int `mapstructure:"max"`
	// TTLMS is how long a notice stays on screen. A notice is chrome, not history: it
	// says something happened and then gets out of the way. Zero keeps every notice until
	// a newer one pushes it out, which is what the client did before it had a clock.
	TTLMS int `mapstructure:"ttl_ms"`
}

// SpinnerConfig is the [ui.spinner] table: the glyph the turn cell animates. Frames
// replace the preset's own when they are given, and IntervalMS its timing when positive.
type SpinnerConfig struct {
	Name       string   `mapstructure:"name"`
	Frames     []string `mapstructure:"frames"`
	IntervalMS int      `mapstructure:"interval_ms"`
}

// InputConfig is the [ui.input] table. Rules are the two lines that bracket the composer,
// the lower one carrying the context percentage (ADR 0017).
type InputConfig struct {
	Rules bool `mapstructure:"rules"`
}

// HeaderConfig is the [ui.header] table: the startup header the client draws once at the
// top of the transcript and lets the conversation scroll away (ADR 0016). Name empty
// resolves git's user.name and then the OS user.
type HeaderConfig struct {
	Show    bool `mapstructure:"show"`
	Animate bool `mapstructure:"animate"`
	// Frame draws the box around the header. False leaves its lines with no border, which
	// is what a terminal too narrow for one gets anyway.
	Frame bool `mapstructure:"frame"`
	// Greeting and Mark are the two halves of the left column above the facts: the time of
	// day with a name, and the cat.
	Greeting bool   `mapstructure:"greeting"`
	Mark     bool   `mapstructure:"mark"`
	Name     string `mapstructure:"name"`
	// Facts are the session's own, in the order they draw: model, thinking, mode,
	// workspace. An empty list draws none.
	Facts    []string `mapstructure:"facts"`
	Tips     int      `mapstructure:"tips"`
	Updates  int      `mapstructure:"updates"`
	MaxWidth int      `mapstructure:"max_width"`
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
	Header     HeaderConfig      `mapstructure:"header"`
	Input      InputConfig       `mapstructure:"input"`
	Spinner    SpinnerConfig     `mapstructure:"spinner"`
	Theme      map[string]string `mapstructure:"theme"`
	// Icons holds ui.icons.set plus per-name overrides, merged the same way Theme is.
	Icons map[string]string `mapstructure:"icons"`
	// Mouse is what the client asks the terminal to report: "off" leaves the mouse to the
	// terminal, so a drag selects text to copy as it does anywhere else; "click" reports
	// clicks and the wheel, which is what expands a tool row and scrolls the transcript;
	// "all" reports movement too, which nothing here uses yet.
	//
	// Off is the default: copying an error out of the transcript is a daily thing, and a
	// client that takes the mouse to expand a row nobody clicks has taken the more useful
	// half. With reporting on, a terminal's own selection is one modifier away, and which
	// one is the terminal's business: Shift in xterm, Ghostty, WezTerm and most others,
	// Option in Terminal.app and iTerm2.
	Mouse string `mapstructure:"mouse"`
	// Cats draws a random cat face in the status line's cat cell, one for the life of the
	// client. False leaves the cell empty wherever ui.status.items placed it.
	Cats bool `mapstructure:"cats"`
}

// themeDefaults are the design's ui.theme role values
// (docs/specs/2026-09-07-rudy-design.md, [ui.theme]): the fourteen color roles plus code.
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
	// Teal, where a tool row used to be muted: what the agent did is not chrome, and the
	// transcript reads better when the doing is one colour and the frame another.
	"tool":     "#73daca",
	"success":  "#9ece6a",
	"error":    "#f7768e",
	"warning":  "#e0af68",
	"diff_add": "success",
	"diff_del": "error",
	"shell":    "warning",
	// The status line is pink where it used to be muted, and the spinner is the hot one:
	// the only cell on that line that moves is the only one that is news.
	"status":  "#b48ead",
	"spinner": "#ff69b4",
	"code":    "chroma:tokyonight-night",
}

// ThemeDefaults is the design's ui.theme role values, a fresh copy each call.
func ThemeDefaults() map[string]string {
	out := make(map[string]string, len(themeDefaults))
	for k, v := range themeDefaults {
		out[k] = v
	}
	return out
}

// PromptConfig is the [prompt] table: the system prompt an operator writes instead of the
// built-in one. File is a path; empty means system.md under the config directory when that
// exists, and the built-in template when it does not.
type PromptConfig struct {
	File string `mapstructure:"file"`
}

// MemoryConfig is the [memory] table.
type MemoryConfig struct {
	Dir          string         `mapstructure:"dir"`
	Enabled      bool           `mapstructure:"enabled"`
	SummaryModel string         `mapstructure:"summary_model"`
	Fold         map[string]int `mapstructure:"fold"`
}

// WebConfig is what the web tools may reach and how much they may bring back (ADR 0039).
type WebConfig struct {
	// BraveAPIKey and ExaAPIKey are where each search backend's key comes from, in the form
	// a provider key takes: "env:NAME" or "cache:NAME" for the 1Password cache. Brave is
	// asked first and Exa is the fallback, so a Brave quota or outage does not take search
	// with it (ADR 0039). A backend whose ref is empty or does not resolve is not in the
	// chain; an empty chain registers neither tool, because a model that can see a tool it
	// cannot use spends a turn finding out.
	BraveAPIKey string `mapstructure:"brave_api_key"`
	ExaAPIKey   string `mapstructure:"exa_api_key"`
	// MaxResults is how many search results one call returns.
	MaxResults int `mapstructure:"max_results"`
	// FetchMaxBytes is the most one fetch reads from a response before it stops reading.
	FetchMaxBytes int `mapstructure:"fetch_max_bytes"`
	// AllowPrivateHosts lets a fetch reach an address that is loopback, private, link-local
	// or unique-local. False refuses them, at resolution and at every redirect: the model
	// chooses the URL, so it chooses the address, and the ones worth reaching that way are
	// this machine's own services and the cloud metadata endpoint (ADR 0039).
	AllowPrivateHosts bool `mapstructure:"allow_private_hosts"`
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
	Prompt    PromptConfig              `mapstructure:"prompt"`
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
	Web WebConfig `mapstructure:"web"`
	Log struct {
		Level string `mapstructure:"level"`
		File  string `mapstructure:"file"`
	} `mapstructure:"log"`
	Secrets struct {
		// File is the env-format file a cache:KEY reference reads a secret from. Empty is
		// filled at load with the platform's own, the way Log.File is.
		File string `mapstructure:"file"`
	} `mapstructure:"secrets"`
	Remote struct {
		// Host is the ssh destination that runs the kernel when --host is not given. Empty
		// means the kernel runs here.
		Host string `mapstructure:"host"`
		// Source is the rudy checkout on the host, which rudy hosts install builds from. A
		// path on the host, kept exactly as it was written: a leading ~ is expanded by the
		// host's shell, since the home it names is the host's.
		Source string `mapstructure:"source"`
	} `mapstructure:"remote"`
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
		"web.brave_api_key":                "env:BRAVE_API_KEY",
		"web.exa_api_key":                  "env:EXA_API_KEY",
		"web.max_results":                  5,
		"web.fetch_max_bytes":              2000000,
		"web.allow_private_hosts":          false,
		"log.level":                        "info",
		"log.file":                         "", // filled from paths.Cache when still empty after Load
		"secrets.file":                     "", // filled by defaultSecretsFile when still empty after Load
		"remote.host":                      "",
		"remote.source":                    "~/projects/rudy",
		"max_tokens":                       0, // 0 means the model's own max_output (provider.Model.OutputBudget)
		"prompt.file":                      "",
		"ui.render":                        "altscreen",
		"ui.header.show":                   true,
		"ui.header.animate":                true,
		"ui.header.frame":                  true,
		"ui.header.greeting":               true,
		"ui.header.mark":                   true,
		"ui.header.facts":                  []string{"model", "thinking", "workspace"},
		"ui.header.name":                   "",
		"ui.header.tips":                   2,
		"ui.header.updates":                3,
		"ui.header.max_width":              120,
		"ui.input.rules":                   true,
		"ui.spinner.name":                  "arc",
		"ui.spinner.frames":                []string{},
		"ui.spinner.interval_ms":           0,
		"ui.cats":                          true,
		"ui.mouse":                         "off",
		"ui.vim":                           true,
		"ui.layout.slots":                  []string{"transcript", "input", "status"},
		"ui.transcript.tool_collapsed":     true,
		"ui.transcript.tool_preview_lines": 2,
		"ui.transcript.thinking":           "hidden",
		"ui.transcript.user_prefix":        "›",
		"ui.transcript.block_gap":          1,
		"ui.diff.style":                    "text",
		"ui.status.items":                  []string{"vim_mode", "model", "permission_mode", "cost", "cwd", "workspace", "cat"},
		"ui.status.above_editor":           []string{"turn"},
		"ui.status.host":                   true,
		"ui.notices.max":                   3,
		"ui.notices.ttl_ms":                8000,
		"plugins.disabled":                 []string{},
		// The two tables merged by hand below still take their defaults from here, so a
		// default lives in one place whatever shape its table has.
		"ui.icons.set":  "nerd",
		"ui.theme.name": "default",
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
	file := paths.ConfigFile()
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
	c.UI.Theme["name"] = Defaults()["ui.theme.name"].(string)
	for k, val := range v.GetStringMapString("ui.theme") {
		c.UI.Theme[k] = val
	}
	for _, key := range v.AllKeys() {
		if role, ok := strings.CutPrefix(key, "ui.theme."); ok {
			c.UI.Theme[role] = v.GetString(key)
		}
	}
	// ui.icons is the same shape: the set's name plus per-icon overrides, merged over the
	// default set by hand for the same reason ui.theme is.
	c.UI.Icons = map[string]string{"set": Defaults()["ui.icons.set"].(string)}
	for k, val := range v.GetStringMapString("ui.icons") {
		c.UI.Icons[k] = val
	}
	// A caller that set one key by its dotted name rather than as a table, which is what
	// an override map does, is not in the map view above: viper answers a nested table
	// from the file and the defaults, not from Set. Walk the keys it knows instead.
	for _, key := range v.AllKeys() {
		if name, ok := strings.CutPrefix(key, "ui.icons."); ok {
			c.UI.Icons[name] = v.GetString(key)
		}
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
	if c.Log.File == "" {
		c.Log.File = filepath.Join(paths.Cache, "rudy.log")
	}
	c.Log.File = ExpandHome(c.Log.File, paths.Home)
	if c.Secrets.File == "" {
		c.Secrets.File = defaultSecretsFile(paths)
	}
	c.Secrets.File = ExpandHome(c.Secrets.File, paths.Home)
	// remote.source is deliberately not expanded: it is a directory on the host, and the home
	// a ~ in it means is the host's, not this machine's. The client sends it as written and
	// the host's own shell expands it (ADR 0029).
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

// defaultSecretsFile is the file a cache:KEY reference reads when secrets.file is empty.
// macOS has one already: op-refresh-secrets writes the 1Password cache under
// ~/Library/Caches, and reading it is why cache: exists. Nothing writes that path on any
// other platform, so there the default is rudy's own file under the cache directory, which
// is a file the operator writes rather than one this binary keeps up to date.
func defaultSecretsFile(paths Paths) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(paths.Home, "Library", "Caches", "op-secrets.env")
	}
	return filepath.Join(paths.Cache, "secrets.env")
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
	if c.Web.MaxResults < 1 || c.Web.MaxResults > 20 {
		errs = append(errs, fmt.Errorf("config: web.max_results %d must be 1 through 20", c.Web.MaxResults))
	}
	if c.Web.FetchMaxBytes < 1024 {
		errs = append(errs, fmt.Errorf("config: web.fetch_max_bytes %d must be at least 1024", c.Web.FetchMaxBytes))
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
	if c.MaxTokens < 0 {
		errs = append(errs, fmt.Errorf("config: max_tokens %d must be positive, or zero for the model's own maximum", c.MaxTokens))
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("config: log.level %q must be debug, info, warn or error", c.Log.Level))
	}
	if strings.HasPrefix(c.Remote.Host, "-") {
		errs = append(errs, fmt.Errorf("config: remote.host %q begins with -, which ssh would read as an option; name a host alias or user@host", c.Remote.Host))
	}
	errs = append(errs, c.validateUI()...)
	return errors.Join(errs...)
}

// validRenders, validThinking and validDiffStyles gate the matching ui.* enums.
var validRenders = map[string]bool{"inline": true, "altscreen": true}

// validMouse are ui.mouse's three values.
var validMouse = map[string]bool{"off": true, "click": true, "all": true}
var validThinking = map[string]bool{"hidden": true, "shown": true}
var validDiffStyles = map[string]bool{"text": true, "background": true}

// legalSlots are ui.layout.slots' legal entries; transcript, input and status are each
// required exactly once, header is optional.
var legalSlots = map[string]bool{"header": true, "transcript": true, "input": true, "status": true}
var requiredSlots = []string{"transcript", "input", "status"}

// legalHeaderFacts are ui.header.facts' entries: the session's own facts the box may
// carry, each one the status line spells the same way.
var legalHeaderFacts = map[string]bool{
	"model": true, "thinking": true, "mode": true, "workspace": true,
}

// builtinStatusItems are ui.status.items' built-in keys; anything else must be a
// non-empty "<plugin>:<key>" pair.
var builtinStatusItems = map[string]bool{
	"vim_mode": true, "model": true, "permission_mode": true,
	"context": true, "cost": true, "cwd": true, "workspace": true, "turn": true, "cat": true,
}

func (c *Config) validateUI() []error {
	var errs []error
	if !validMouse[c.UI.Mouse] {
		errs = append(errs, fmt.Errorf("config: ui.mouse %q is not off, click or all", c.UI.Mouse))
	}
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
	if c.UI.Spinner.IntervalMS < 0 {
		errs = append(errs, fmt.Errorf("config: ui.spinner.interval_ms %d must be zero or positive", c.UI.Spinner.IntervalMS))
	}
	if c.UI.Notices.TTLMS < 0 {
		errs = append(errs, fmt.Errorf("config: ui.notices.ttl_ms %d must be zero or positive", c.UI.Notices.TTLMS))
	}
	if c.UI.Notices.Max < 0 {
		errs = append(errs, fmt.Errorf("config: ui.notices.max %d must be zero or positive", c.UI.Notices.Max))
	}
	for _, f := range c.UI.Header.Facts {
		if !legalHeaderFacts[f] {
			errs = append(errs, fmt.Errorf("config: ui.header.facts %q is not model, thinking, mode or workspace", f))
		}
	}
	if c.UI.Header.Tips < 0 {
		errs = append(errs, fmt.Errorf("config: ui.header.tips %d must be zero or positive", c.UI.Header.Tips))
	}
	if c.UI.Header.Updates < 0 {
		errs = append(errs, fmt.Errorf("config: ui.header.updates %d must be zero or positive", c.UI.Header.Updates))
	}
	if c.UI.Header.MaxWidth <= 0 {
		errs = append(errs, fmt.Errorf("config: ui.header.max_width %d must be positive", c.UI.Header.MaxWidth))
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
	errs = append(errs, validStatusItems("ui.status.items", c.UI.Status.Items)...)
	errs = append(errs, validStatusItems("ui.status.above_editor", c.UI.Status.AboveEditor)...)
	return errs
}

// validStatusItems checks one status list: every entry is a built-in item or a plugin's
// "<plugin>:<key>". Both lists take the same vocabulary, so an item can be drawn over the
// composer, under it, or in both places.
func validStatusItems(key string, items []string) []error {
	var errs []error
	for _, item := range items {
		if builtinStatusItems[item] {
			continue
		}
		plugin, k, ok := strings.Cut(item, ":")
		if !ok || plugin == "" || k == "" {
			errs = append(errs, fmt.Errorf("config: %s %q is not a built-in item or plugin:key", key, item))
		}
	}
	return errs
}
