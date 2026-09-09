package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/compactcmd"
	"github.com/guygrigsby/rudy/internal/plugins/initcmd"
	openaichatplugin "github.com/guygrigsby/rudy/internal/plugins/openaichat"
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
	set := o.Plugins
	if set == nil {
		set = BuiltinPlugins(cfg, httpc, home, env)
	}
	if err := os.MkdirAll(paths.Cache, 0o700); err != nil {
		return nil, fmt.Errorf("cache dir: %w", err)
	}
	// The server is built before the plugins load, and takes its providers from them
	// afterwards: a plugin's Host reaches the server through the protocol (Connect, Note,
	// the status broadcasts), so the server has to exist by the time any Init runs.
	registry := provider.NewRegistry(filepath.Join(paths.Cache, "registry.json"))
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
	defer func() {
		if err != nil {
			shutCtx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
			defer cancel()
			_ = srv.Shutdown(shutCtx)
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
	return &Built{
		Version:  o.Version,
		Paths:    paths,
		Config:   cfg,
		Store:    store,
		Registry: registry,
		Plugins:  plugins,
		Server:   srv,
	}, nil
}

// storeFromEnv opens the session store at its XDG data path without wiring the rest of the
// server; commands that only read sessions (sessions list) use this instead of Build so they
// pay for no plugin load or registry refresh.
func storeFromEnv(env func(string) string, home string) (*session.Store, error) {
	paths := config.XDG(env, home)
	return session.OpenStore(filepath.Join(paths.Data, "sessions"))
}

// BuiltinPlugins is the linked-in set: the six tools, /init, /compact and the openai_chat
// providers.
func BuiltinPlugins(cfg *config.Config, httpc *httpx.Client, home string, env func(string) string) []plugin.Plugin {
	cache := filepath.Join(home, "Library", "Caches", "op-secrets.env")
	resolve := func(ref string) (string, error) { return config.ResolveSecret(ref, env, cache) }
	return append(BuiltinTools(), initcmd.New(), compactcmd.New(), openaichatplugin.New(cfg.Providers, httpc, resolve))
}

// BuiltinTools is the six tool plugins alone, for tests that supply their own provider.
func BuiltinTools() []plugin.Plugin {
	return []plugin.Plugin{read.New(), write.New(), edit.New(), bash.New(), grep.New(), glob.New()}
}
