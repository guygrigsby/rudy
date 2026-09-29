// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	memory "github.com/aeryx-ai/memory/memory-go"

	"github.com/guygrigsby/rudy/internal/config"
)

// setupHome is a fresh install: an empty HOME, an env that resolves nothing, and the
// paths rudy would resolve from them.
func setupHome(t *testing.T) (string, func(string) string, config.Paths) {
	t.Helper()
	home := t.TempDir()
	env := func(string) string { return "" }
	return home, env, config.XDG(env, home)
}

func TestSetupNonInteractiveConfiguresEverything(t *testing.T) {
	home, env, paths := setupHome(t)
	var out bytes.Buffer
	err := runSetup(setupOptions{
		Env: env, Home: home,
		Provider: "anthropic", Model: "claude-sonnet-4-5", Auth: "env:ANTHROPIC_API_KEY",
	}, strings.NewReader(""), &out, &out)
	if err != nil {
		t.Fatalf("runSetup: %v", err)
	}
	cfg, err := config.Load(paths, nil)
	if err != nil {
		t.Fatalf("config no longer loads: %v", err)
	}
	p, ok := cfg.Providers["anthropic"]
	if !ok {
		t.Fatalf("providers %v, want anthropic", cfg.Providers)
	}
	if p.Wire != "anthropic_messages" || p.Auth != "env:ANTHROPIC_API_KEY" || p.BaseURL != "https://api.anthropic.com" {
		t.Errorf("provider %+v", p)
	}
	if cfg.Default.Provider != "anthropic" || cfg.Default.Model != "claude-sonnet-4-5" {
		t.Errorf("default %s:%s", cfg.Default.Provider, cfg.Default.Model)
	}
	if b := memory.New(memory.ResolveRoot(cfg.Memory.Dir, env)); !b.Exists() {
		t.Errorf("memory bundle at %s was not initialized", b.Root)
	}
	// The sync went first, so every catalogue key is there and the comments that name
	// them survived the table the wizard appended.
	body, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "[providers.anthropic]") {
		t.Errorf("config.toml has no [providers.anthropic] table:\n%s", body)
	}
}

func TestSetupCachesAPastedKey(t *testing.T) {
	home, env, paths := setupHome(t)
	var out bytes.Buffer
	err := runSetup(setupOptions{
		Env: env, Home: home,
		Provider: "anthropic", Model: "m", Auth: "sk-ant-pasted",
	}, strings.NewReader(""), &out, &out)
	if err != nil {
		t.Fatalf("runSetup: %v", err)
	}
	cfg, err := config.Load(paths, nil)
	if err != nil {
		t.Fatalf("config no longer loads: %v", err)
	}
	if got := cfg.Providers["anthropic"].Auth; got != "cache:ANTHROPIC_API_KEY" {
		t.Errorf("auth %q, want the cache reference, not the key", got)
	}
	secret, err := os.ReadFile(cfg.Secrets.File)
	if err != nil {
		t.Fatalf("secrets.file: %v", err)
	}
	if !strings.Contains(string(secret), "ANTHROPIC_API_KEY=sk-ant-pasted") {
		t.Errorf("secrets.file says %q", secret)
	}
	if strings.Contains(string(must(os.ReadFile(paths.ConfigFile()))), "sk-ant-pasted") {
		t.Errorf("config.toml carries the key itself")
	}
}

func TestSetupInteractivePresetWalkthrough(t *testing.T) {
	home, env, paths := setupHome(t)
	var out bytes.Buffer
	// The menu choice, an empty line for the default auth reference and an empty line
	// for the default model.
	in := strings.NewReader("2\n\n\n")
	err := runSetup(setupOptions{Env: env, Home: home}, in, &out, &out)
	if err != nil {
		t.Fatalf("runSetup: %v", err)
	}
	cfg, err := config.Load(paths, nil)
	if err != nil {
		t.Fatalf("config no longer loads: %v", err)
	}
	p, ok := cfg.Providers["openai"]
	if !ok {
		t.Fatalf("providers %v, want openai", cfg.Providers)
	}
	if p.Wire != "openai_chat" || p.BaseURL != "https://api.openai.com/v1" || p.Auth != "env:OPENAI_API_KEY" {
		t.Errorf("provider %+v", p)
	}
	if cfg.Default.Provider != "openai" || cfg.Default.Model != "gpt-5.2" {
		t.Errorf("default %s:%s", cfg.Default.Provider, cfg.Default.Model)
	}
}

func TestSetupCustomProviderAndNotYet(t *testing.T) {
	home, env, paths := setupHome(t)
	var out bytes.Buffer
	in := strings.NewReader("3\nbox\nhttps://box.example/v1\n\nqwen3\n")
	if err := runSetup(setupOptions{Env: env, Home: home}, in, &out, &out); err != nil {
		t.Fatalf("runSetup: %v", err)
	}
	cfg, err := config.Load(paths, nil)
	if err != nil {
		t.Fatalf("config no longer loads: %v", err)
	}
	p, ok := cfg.Providers["box"]
	if !ok {
		t.Fatalf("providers %v, want box", cfg.Providers)
	}
	if p.Wire != "openai_chat" || p.Auth != "" {
		t.Errorf("provider %+v, want an unauthenticated openai_chat endpoint", p)
	}
	if cfg.Default.Provider != "box" || cfg.Default.Model != "qwen3" {
		t.Errorf("default %s:%s", cfg.Default.Provider, cfg.Default.Model)
	}

	// Not yet is an answer too: the config is synced and nothing is written that a
	// validator would refuse.
	home2, env2, paths2 := setupHome(t)
	out.Reset()
	err = runSetup(setupOptions{Env: env2, Home: home2}, strings.NewReader("4\n"), &out, &out)
	if err != nil {
		t.Fatalf("runSetup: %v", err)
	}
	cfg2, err := config.Load(paths2, nil)
	if err != nil {
		t.Fatalf("config no longer loads: %v", err)
	}
	if len(cfg2.Providers) != 0 {
		t.Errorf("providers %v, want none after a not-yet", cfg2.Providers)
	}
	if _, err := os.Stat(paths2.ConfigFile()); err != nil {
		t.Errorf("the sync still writes config.toml: %v", err)
	}
}

func TestSetupLeavesAConfiguredFileAlone(t *testing.T) {
	home, env, paths := setupHome(t)
	var out bytes.Buffer
	o := setupOptions{Env: env, Home: home, Provider: "anthropic", Model: "m"}
	if err := runSetup(o, strings.NewReader(""), &out, &out); err != nil {
		t.Fatalf("first run: %v", err)
	}
	out.Reset()
	if err := runSetup(setupOptions{Env: env, Home: home}, strings.NewReader(""), &out, &out); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !strings.Contains(out.String(), "already configured") {
		t.Errorf("second run said %q, want it to see the provider and stop", out.String())
	}
	cfg, err := config.Load(paths, nil)
	if err != nil {
		t.Fatalf("config no longer loads: %v", err)
	}
	if cfg.Default.Provider != "anthropic" {
		t.Errorf("default.provider %q after a second run", cfg.Default.Provider)
	}
}

func TestSetTableValues(t *testing.T) {
	body := "[default]\n# e.g. \"aperture\"\nprovider = \"\"\nmodel = \"\"\n\n[permissions]\nmode = \"strict\"\n"
	got := setTableValues(body, "default", map[string]string{"provider": "anthropic", "model": "claude"}, []string{"provider", "model"})
	if !strings.Contains(got, `provider = "anthropic"`) || !strings.Contains(got, `model = "claude"`) {
		t.Errorf("got %q", got)
	}
	if !strings.Contains(got, "# e.g. \"aperture\"") || !strings.Contains(got, "[permissions]\nmode = \"strict\"") {
		t.Errorf("the file around the edit changed: %q", got)
	}

	// A missing table is appended whole, and a missing key is inserted under the header.
	got = setTableValues(body, "remote", map[string]string{"host": "box"}, []string{"host"})
	if !strings.HasSuffix(got, "[remote]\nhost = \"box\"\n") {
		t.Errorf("missing table: got %q", got)
	}
	got = setTableValues("[default]\nprovider = \"\"\n", "default", map[string]string{"provider": "p", "model": "m"}, []string{"provider", "model"})
	if !strings.Contains(got, "[default]\nmodel = \"m\"\nprovider = \"p\"\n") {
		t.Errorf("missing key: got %q", got)
	}
	// A commented-out assignment is not the key.
	got = setTableValues("# provider = \"x\"\n[default]\n# provider = \"x\"\n", "default", map[string]string{"provider": "p"}, []string{"provider"})
	if strings.Count(got, "# provider") != 2 || !strings.Contains(got, "[default]\nprovider = \"p\"\n") {
		t.Errorf("commented key: got %q", got)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
