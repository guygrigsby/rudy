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
		Mode      string   `mapstructure:"mode"`
		Dangerous []string `mapstructure:"dangerous"`
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
	MaxTokens int `mapstructure:"max_tokens"`
}

// Defaults are the values in force when neither the file nor the environment sets a key.
// Keys with empty defaults exist so RUDY_* environment variables can set them.
func Defaults() map[string]any {
	return map[string]any{
		"default.provider": "",
		"default.model":    "",
		"default.thinking": "high",
		"agent":            "default",
		"hook_timeout_ms":  5000,
		"tool_timeout_ms":  600000,
		"permissions.mode": "strict",
		"permissions.dangerous": []string{
			"rm -rf", "rm -r", "git push --force", "git push -f", "git reset --hard",
			"git clean", "sudo", "chmod -R", "chown -R", "mkfs", "dd",
		},
		"sessions.dir":           "", // filled from paths.Data when still empty after Load
		"sessions.compact_at":    0.8,
		"skills.dirs":            []string{"~/.agents/skills", ".agents/skills"},
		"skills.migrate_from":    []string{"~/.claude/skills", "~/.pi/agent/skills"},
		"memory.dir":             "~/.agents/memory",
		"memory.enabled":         true,
		"memory.summary_model":   "",
		"mcp.connect_timeout_ms": 10000,
		"max_tokens":             8192,
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
	return errors.Join(errs...)
}
