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
	Wire    string            `mapstructure:"wire"`     // "openai_chat"
	BaseURL string            `mapstructure:"base_url"` // ends with /v1
	Auth    string            `mapstructure:"auth"`     // "", "env:NAME", "cache:KEY"
	Headers map[string]string `mapstructure:"headers"`  // extra request headers, verbatim
}

// Config is config.toml after defaults, environment and overrides.
type Config struct {
	Default struct {
		Provider string `mapstructure:"provider"`
		Model    string `mapstructure:"model"`
		Thinking string `mapstructure:"thinking"`
	} `mapstructure:"default"`
	Permissions struct {
		Mode      string   `mapstructure:"mode"`
		Dangerous []string `mapstructure:"dangerous"`
	} `mapstructure:"permissions"`
	Providers map[string]ProviderConfig `mapstructure:"providers"`
	Plugins   map[string]map[string]any `mapstructure:"plugins"`
	MaxTokens int                       `mapstructure:"max_tokens"`
}

// Defaults are the values in force when neither the file nor the environment sets a key.
// Keys with empty defaults exist so RUDY_* environment variables can set them.
func Defaults() map[string]any {
	return map[string]any{
		"default.provider": "",
		"default.model":    "",
		"default.thinking": "high",
		"permissions.mode": "strict",
		"permissions.dangerous": []string{
			"rm -rf", "rm -r", "git push --force", "git push -f", "git reset --hard",
			"git clean", "sudo", "chmod -R", "chown -R", "mkfs", "dd",
		},
		"max_tokens": 8192,
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
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
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
		if p.Wire != "openai_chat" {
			errs = append(errs, fmt.Errorf("config: providers.%s.wire %q is not openai_chat", name, p.Wire))
		}
		if p.BaseURL == "" {
			errs = append(errs, fmt.Errorf("config: providers.%s.base_url is empty", name))
		}
	}
	if c.MaxTokens <= 0 {
		errs = append(errs, fmt.Errorf("config: max_tokens %d must be positive", c.MaxTokens))
	}
	return errors.Join(errs...)
}
