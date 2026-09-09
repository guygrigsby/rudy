package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/config"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestXDGDefaultsAndOverrides(t *testing.T) {
	p := config.XDG(envOf(nil), "/home/guy")
	if p.Config != "/home/guy/.config/rudy" {
		t.Errorf("config %q", p.Config)
	}
	if p.Data != "/home/guy/.local/share/rudy" {
		t.Errorf("data %q", p.Data)
	}
	if p.Cache != "/home/guy/.cache/rudy" {
		t.Errorf("cache %q", p.Cache)
	}
	if !strings.HasSuffix(p.Runtime, "/rudy") {
		t.Errorf("runtime %q", p.Runtime)
	}
	p = config.XDG(envOf(map[string]string{
		"XDG_CONFIG_HOME": "/x/cfg", "XDG_DATA_HOME": "/x/data",
		"XDG_RUNTIME_DIR": "/x/run", "XDG_CACHE_HOME": "/x/cache",
	}), "/home/guy")
	want := config.Paths{Config: "/x/cfg/rudy", Data: "/x/data/rudy", Runtime: "/x/run/rudy", Cache: "/x/cache/rudy", Home: "/home/guy"}
	if p != want {
		t.Errorf("got %+v want %+v", p, want)
	}
}

func TestLoadDefaultsWithoutFile(t *testing.T) {
	c, err := config.Load(config.Paths{Config: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Permissions.Mode != "strict" || c.Default.Thinking != "high" || c.MaxTokens != 8192 {
		t.Errorf("defaults: %+v", c)
	}
	if len(c.Permissions.Dangerous) == 0 || c.Permissions.Dangerous[0] != "rm -rf" {
		t.Errorf("dangerous: %v", c.Permissions.Dangerous)
	}
}

const sampleTOML = `
[default]
provider = "aperture"
model = "cline-pass/kimi-k3"

[permissions]
mode = "permissive"

[providers.aperture]
wire = "openai_chat"
base_url = "https://ai.guy.ts.net/v1"
auth = "env:APERTURE_TOKEN"
headers = { "X-Team" = "rudy" }

[plugins.memory]
dir = "/Users/guy/.agents/memory"
`

func writeConfig(t *testing.T, body string) config.Paths {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Paths{Config: dir}
}

func TestLoadParsesTOML(t *testing.T) {
	c, err := config.Load(writeConfig(t, sampleTOML), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Default.Provider != "aperture" || c.Default.Model != "cline-pass/kimi-k3" {
		t.Errorf("default: %+v", c.Default)
	}
	if c.Permissions.Mode != "permissive" {
		t.Errorf("mode %q", c.Permissions.Mode)
	}
	p := c.Providers["aperture"]
	if p.Wire != "openai_chat" || p.BaseURL != "https://ai.guy.ts.net/v1" || p.Auth != "env:APERTURE_TOKEN" || p.Headers["X-Team"] != "rudy" {
		t.Errorf("provider: %+v", p)
	}
	if c.Plugins["memory"]["dir"] != "/Users/guy/.agents/memory" {
		t.Errorf("plugins: %+v", c.Plugins)
	}
}

func TestLoadEnvOverridesFile(t *testing.T) {
	t.Setenv("RUDY_PERMISSIONS_MODE", "off")
	t.Setenv("RUDY_DEFAULT_MODEL", "cline-pass/glm-5.3")
	c, err := config.Load(writeConfig(t, sampleTOML), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Permissions.Mode != "off" {
		t.Errorf("mode %q", c.Permissions.Mode)
	}
	if c.Default.Model != "cline-pass/glm-5.3" {
		t.Errorf("model %q", c.Default.Model)
	}
}

func TestLoadOverridesWinOverEnvAndFile(t *testing.T) {
	t.Setenv("RUDY_PERMISSIONS_MODE", "off")
	c, err := config.Load(writeConfig(t, sampleTOML), map[string]any{"permissions.mode": "strict"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Permissions.Mode != "strict" {
		t.Errorf("mode %q", c.Permissions.Mode)
	}
}

func TestLoadRefusesInvalid(t *testing.T) {
	cases := map[string]string{
		"bad mode":         strings.Replace(sampleTOML, `mode = "permissive"`, `mode = "yolo"`, 1),
		"unknown provider": strings.Replace(sampleTOML, `provider = "aperture"`, `provider = "nope"`, 1),
		"bad wire":         strings.Replace(sampleTOML, `wire = "openai_chat"`, `wire = "grpc"`, 1),
		"no base url":      strings.Replace(sampleTOML, `base_url = "https://ai.guy.ts.net/v1"`, `base_url = ""`, 1),
	}
	for name, body := range cases {
		if _, err := config.Load(writeConfig(t, body), nil); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestLoadNeverWritesTheFile(t *testing.T) {
	paths := writeConfig(t, sampleTOML)
	file := filepath.Join(paths.Config, "config.toml")
	before, _ := os.Stat(file)
	if _, err := config.Load(paths, map[string]any{"permissions.mode": "off"}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(file)
	body, _ := os.ReadFile(file)
	if string(body) != sampleTOML || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("config file was rewritten")
	}
}

func TestResolveSecret(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "op-secrets.env")
	if err := os.WriteFile(cache, []byte("# comment\nexport OTHER=1\nAPERTURE_TOKEN=\"tok-123\"\nPLAIN=abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := envOf(map[string]string{"FROM_ENV": "env-value"})
	cases := []struct {
		ref, want string
		wantErr   bool
	}{
		{"", "", false},
		{"env:FROM_ENV", "env-value", false},
		{"env:MISSING", "", true},
		{"cache:APERTURE_TOKEN", "tok-123", false},
		{"cache:PLAIN", "abc", false},
		{"cache:OTHER", "1", false},
		{"cache:MISSING", "", true},
		{"vault:x", "", true},
	}
	for _, c := range cases {
		got, err := config.ResolveSecret(c.ref, env, cache)
		if (err != nil) != c.wantErr {
			t.Errorf("%q: err %v", c.ref, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %q want %q", c.ref, got, c.want)
		}
	}
}

func TestWaveDefaults(t *testing.T) {
	home := t.TempDir()
	paths := config.XDG(func(string) string { return "" }, home)
	c, err := config.Load(paths, map[string]any{"default.provider": "p", "default.model": "m"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent != "default" || c.HookTimeoutMS != 5000 || c.ToolTimeoutMS != 600000 || c.Sessions.CompactAt != 0.8 || c.MCP.ConnectTimeoutMS != 10000 {
		t.Errorf("defaults: %+v", c)
	}
	if c.Sessions.Dir != filepath.Join(paths.Data, "sessions") {
		t.Errorf("sessions.dir = %q", c.Sessions.Dir)
	}
	if c.Memory.Dir != filepath.Join(home, ".agents", "memory") || !c.Memory.Enabled {
		t.Errorf("memory: %+v", c.Memory)
	}
	want := []string{filepath.Join(home, ".agents", "skills"), ".agents/skills"}
	if !reflect.DeepEqual(c.Skills.Dirs, want) {
		t.Errorf("skills.dirs = %v", c.Skills.Dirs)
	}
	wantFrom := []string{filepath.Join(home, ".claude", "skills"), filepath.Join(home, ".pi", "agent", "skills")}
	if !reflect.DeepEqual(c.Skills.MigrateFrom, wantFrom) {
		t.Errorf("skills.migrate_from = %v", c.Skills.MigrateFrom)
	}
}

func TestPluginsDisabledAndTables(t *testing.T) {
	home := t.TempDir()
	paths := config.XDG(func(string) string { return "" }, home)
	if err := os.MkdirAll(paths.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Config, "config.toml"), []byte(`
[default]
provider = "p"
model = "m"
[providers.p]
wire = "openai_chat"
base_url = "http://x/v1"
dialect = "clinepass"
[providers.a]
wire = "anthropic_messages"
base_url = "http://y"
[plugins]
disabled = ["mcp"]
[plugins.memory]
verbose = true
[memory]
enabled = false
summary_model = "p:small"
[memory.fold]
observe_after_tokens = 10
`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(paths, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.PluginsDisabled, []string{"mcp"}) {
		t.Errorf("disabled = %v", c.PluginsDisabled)
	}
	if _, ok := c.Plugins["disabled"]; ok {
		t.Error("disabled leaked into the plugin tables")
	}
	if c.Plugins["memory"]["verbose"] != true {
		t.Errorf("plugins.memory = %v", c.Plugins["memory"])
	}
	if c.Providers["p"].Dialect != "clinepass" || c.Providers["a"].Wire != "anthropic_messages" {
		t.Errorf("providers = %+v", c.Providers)
	}
	if c.Memory.Enabled || c.Memory.SummaryModel != "p:small" || c.Memory.Fold["observe_after_tokens"] != 10 {
		t.Errorf("memory = %+v", c.Memory)
	}
}

func TestWaveValidation(t *testing.T) {
	home := t.TempDir()
	paths := config.XDG(func(string) string { return "" }, home)
	base := map[string]any{"default.provider": "p", "default.model": "m", "providers.p.wire": "openai_chat", "providers.p.base_url": "http://x/v1"}
	for name, over := range map[string]map[string]any{
		"bad wire":        {"providers.p.wire": "grpc"},
		"bad dialect":     {"providers.p.dialect": "openrouter"},
		"dialect on anth": {"providers.p.wire": "anthropic_messages", "providers.p.dialect": "clinepass"},
		"compact_at 0":    {"sessions.compact_at": 0.0},
		"compact_at 1.5":  {"sessions.compact_at": 1.5},
		"hook timeout":    {"hook_timeout_ms": 0},
		"tool timeout":    {"tool_timeout_ms": -1},
	} {
		t.Run(name, func(t *testing.T) {
			m := map[string]any{}
			for k, v := range base {
				m[k] = v
			}
			for k, v := range over {
				m[k] = v
			}
			if _, err := config.Load(paths, m); err == nil {
				t.Error("want error")
			}
		})
	}
}
