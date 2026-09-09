package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
	anthropicplugin "github.com/guygrigsby/rudy/internal/plugins/anthropic"
	"github.com/guygrigsby/rudy/internal/plugins/commands"
	"github.com/guygrigsby/rudy/internal/plugins/compactcmd"
	"github.com/guygrigsby/rudy/internal/plugins/initcmd"
	memoryplugin "github.com/guygrigsby/rudy/internal/plugins/memory"
	openaichatplugin "github.com/guygrigsby/rudy/internal/plugins/openaichat"
	skillsplugin "github.com/guygrigsby/rudy/internal/plugins/skills"
	"github.com/guygrigsby/rudy/internal/plugins/subagents"
	"github.com/guygrigsby/rudy/internal/plugins/tools/bash"
	"github.com/guygrigsby/rudy/internal/plugins/tools/edit"
	"github.com/guygrigsby/rudy/internal/plugins/tools/glob"
	"github.com/guygrigsby/rudy/internal/plugins/tools/grep"
	"github.com/guygrigsby/rudy/internal/plugins/tools/read"
	"github.com/guygrigsby/rudy/internal/plugins/tools/write"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/provider/httpx"
	"github.com/guygrigsby/rudy/internal/server"
	"github.com/guygrigsby/rudy/internal/session"
)

// Built is everything a command needs after wiring.
type Built struct {
	Version  string
	Paths    config.Paths
	Config   *config.Config
	Store    *session.Store
	Registry *provider.Registry
	Plugins  *plugin.Registry
	Server   *server.Server
}

// BuildOptions tunes wiring. Zero values mean the real environment.
type BuildOptions struct {
	Version        string
	Overrides      map[string]any      // config keys that win over file and env, dotted ("default.model")
	Plugins        []plugin.Plugin     // nil means BuiltinPlugins
	Env            func(string) string // nil means os.Getenv
	Home           string              // "" means os.UserHomeDir
	Stderr         io.Writer           // nil means os.Stderr
	RefreshTimeout time.Duration       // <= 0 means 20 seconds; bounds the startup registry refresh
}

// defaultRefreshTimeout bounds the startup registry refresh when BuildOptions.RefreshTimeout
// is unset, so a stalled provider cannot hold a command silent forever.
const defaultRefreshTimeout = 20 * time.Second

// shutdownBudget bounds the unwind of a half-built server, which has no running turn and no
// client, so it only has to close the sessions it never opened.
const shutdownBudget = 2 * time.Second

// buildFunc is what commands call to wire a server; tests substitute fakes through it.
type buildFunc func(ctx context.Context, stderr io.Writer) (*Built, error)

// Build wires config, store, plugins, registry, gate and server. It never writes config. A
// failure after the server exists shuts it back down: it holds a context, loaded plugins and
// their connections, and a caller that got an error will never call Shutdown itself.
func Build(ctx context.Context, o BuildOptions) (_ *Built, err error) {
	env := o.Env
	if env == nil {
		env = os.Getenv
	}
	home := o.Home
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("home directory: %w", err)
		}
		home = h
	}
	stderr := o.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	paths := config.XDG(env, home)
	cfg, err := config.Load(paths, o.Overrides)
	if err != nil {
		return nil, err
	}
	store, err := session.OpenStore(cfg.Sessions.Dir)
	if err != nil {
		return nil, err
	}
	httpc := httpx.New(o.Version)
	notice := func(text string) { _, _ = fmt.Fprintln(stderr, "rudy:", text) }
	plugins := plugin.NewRegistry(cfg.Plugins, notice)
	plugins.Disable(cfg.PluginsDisabled...)
	// The provider registry is built here rather than below because the memory plugin folds
	// through a model: its summarize closure captures the registry and resolves a provider at
	// call time, long after Load has filled it.
	registry := provider.NewRegistry(filepath.Join(paths.Cache, "registry.json"))
	set := o.Plugins
	if set == nil {
		set = BuiltinPlugins(cfg, paths, httpc, home, env, o.Version, summarizeWith(cfg, registry, notice))
	}
	if err := os.MkdirAll(paths.Cache, 0o700); err != nil {
		return nil, fmt.Errorf("cache dir: %w", err)
	}
	// The server is built before the plugins load, and takes its providers from them
	// afterwards: a plugin's Host reaches the server through the protocol (Connect, Note,
	// the status broadcasts), so the server has to exist by the time any Init runs.
	g := gate.New(cfg.Permissions.Dangerous)
	srv := server.New(server.Deps{
		Version:  o.Version,
		Config:   cfg,
		Store:    store,
		Registry: registry,
		Plugins:  plugins,
		Gate:     g,
		Hooks:    plugin.NewHookRunner(plugins, time.Duration(cfg.HookTimeoutMS)*time.Millisecond, notice),
	})
	built := &Built{Version: o.Version, Paths: paths, Config: cfg, Store: store, Registry: registry, Plugins: plugins, Server: srv}
	defer func() {
		if err != nil {
			shutCtx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
			defer cancel()
			_ = built.Close(shutCtx)
		}
	}()
	services := srv.PluginServices()
	// Withdrawing a provider has to reach the provider registry's own copy, not just the
	// plugin registry; SetProviders below installs the initial set the same way.
	services.ProvidersChanged = func(ps []provider.Provider) { registry.SetProviders(ps...) }
	plugins.SetServices(services)
	plugins.Load(ctx, set...)
	providers := plugins.Providers()
	registry.SetProviders(providers...)
	if err := registry.LoadSnapshot(); err != nil {
		return nil, err
	}
	refreshTimeout := o.RefreshTimeout
	if refreshTimeout <= 0 {
		refreshTimeout = defaultRefreshTimeout
	}
	hadSnapshot := len(registry.Models()) > 0
	if !hadSnapshot {
		names := make([]string, len(providers))
		for i, p := range providers {
			names[i] = p.Name()
		}
		_, _ = fmt.Fprintln(stderr, "refreshing model registry from "+strings.Join(names, ", "))
	}
	refreshCtx, cancel := context.WithTimeout(ctx, refreshTimeout)
	refreshErr := registry.Refresh(refreshCtx)
	cancel()
	// Refresh returns nil when there is nothing to refresh (no provider plugin loaded), so an
	// empty registry after a nil-error refresh is just as fatal as one after a failed refresh:
	// either way there is no model to open a session with and no snapshot to fall back on.
	if len(registry.Models()) == 0 {
		if refreshErr == nil {
			refreshErr = fmt.Errorf("no provider registered for default.provider %q; configure a [providers.*] table or a provider plugin", cfg.Default.Provider)
		}
		return nil, fmt.Errorf("model registry: default provider %q could not be reached within %s: %w", cfg.Default.Provider, refreshTimeout, refreshErr)
	}
	if refreshErr != nil {
		notice("registry refresh: " + refreshErr.Error())
	}
	return built, nil
}

// Close unwinds what Build wired, in the order that keeps it safe: the server first, so
// every session is detached and closed and no hook can fire again, then the plugins, whose
// background work (a memory fold, a bundle commit) has to be waited on before the process
// exits or a half-written concept is what the next run finds. Both halves always run; the
// errors are joined.
func (b *Built) Close(ctx context.Context) error {
	return errors.Join(b.Server.Shutdown(ctx), b.Plugins.Close())
}

// storeFromEnv opens the session store without wiring the rest of the server; commands that
// only read sessions (sessions list) use this instead of Build so they pay for no plugin load
// or registry refresh. It goes through config.Load rather than straight to the XDG data path
// so a configured sessions.dir is the one directory every command means by "sessions".
func storeFromEnv(env func(string) string, home string) (*session.Store, error) {
	paths := config.XDG(env, home)
	cfg, err := config.Load(paths, nil)
	if err != nil {
		return nil, err
	}
	return session.OpenStore(cfg.Sessions.Dir)
}

// BuiltinPlugins is the linked-in set: the six tools, the agent tool, /init, /compact,
// /skills, /memory, the kernel's own slash commands (/model, /help, /fork, /plugins) and the
// openai_chat and anthropic_messages providers.
func BuiltinPlugins(cfg *config.Config, paths config.Paths, httpc *httpx.Client, home string, env func(string) string, version string, summarize memoryplugin.Summarize) []plugin.Plugin {
	cache := filepath.Join(home, "Library", "Caches", "op-secrets.env")
	resolve := func(ref string) (string, error) { return config.ResolveSecret(ref, env, cache) }
	return append(BuiltinTools(), subagents.New(paths.Config), initcmd.New(), compactcmd.New(), commands.New(),
		skillsplugin.New(cfg.Skills.Dirs), memoryplugin.New(cfg.Memory, cfg.Sessions.Dir, version, summarize),
		openaichatplugin.New(cfg.Providers, httpc, resolve),
		anthropicplugin.New(cfg.Providers, httpc, resolve))
}

// summarizeWith is the one prompt the memory plugin runs through a model. Nothing is resolved
// at wire time: the provider registry is filled by plugin.Load, which happens after this
// closure is built, so both the model and its provider are looked up per call.
func summarizeWith(cfg *config.Config, registry *provider.Registry, notice func(string)) memoryplugin.Summarize {
	// A misconfigured summary_model is a configuration fact, not a per-fold event: saying it
	// once keeps a fold on every turn from filling the operator's terminal with one message.
	var once sync.Once
	warn := func(text string) { once.Do(func() { notice(text) }) }
	return func(ctx context.Context, prompt string) (string, error) {
		m, err := summaryModel(cfg, registry, warn)
		if err != nil {
			return "", err
		}
		prov, ok := registry.Provider(m.Ref.Provider)
		if !ok {
			return "", fmt.Errorf("memory: no provider %q for summary model %s", m.Ref.Provider, m.Ref)
		}
		var b strings.Builder
		err = prov.Complete(ctx, provider.Request{
			Model:     m.Ref,
			Messages:  []provider.Message{{Role: provider.RoleUser, Content: []session.Block{session.TextBlock(prompt)}}},
			Thinking:  session.ThinkingOff,
			MaxTokens: cfg.MaxTokens,
		}, func(part provider.Part) error {
			if part.Type == provider.PartTextDelta {
				b.WriteString(part.Text)
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		return b.String(), nil
	}
}

// summaryModel is memory.summary_model when it resolves and the session default otherwise: a
// configured model that has gone out of the registry costs a notice, not a failed fold.
func summaryModel(cfg *config.Config, registry *provider.Registry, warn func(string)) (provider.Model, error) {
	if spec := cfg.Memory.SummaryModel; spec != "" {
		m, err := registry.Resolve(spec)
		if err == nil {
			return m, nil
		}
		warn(fmt.Sprintf("memory: summary_model %s: %v; folding with %s:%s instead", spec, err, cfg.Default.Provider, cfg.Default.Model))
	}
	return registry.Resolve(cfg.Default.Provider + ":" + cfg.Default.Model)
}

// BuiltinTools is the six tool plugins alone, for tests that supply their own provider.
func BuiltinTools() []plugin.Plugin {
	return []plugin.Plugin{read.New(), write.New(), edit.New(), bash.New(), grep.New(), glob.New()}
}
