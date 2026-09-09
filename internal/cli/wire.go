package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/gate"
	"github.com/guygrigsby/rudy/internal/plugin"
	anthropicplugin "github.com/guygrigsby/rudy/internal/plugins/anthropic"
	"github.com/guygrigsby/rudy/internal/plugins/clinepass"
	"github.com/guygrigsby/rudy/internal/plugins/commands"
	"github.com/guygrigsby/rudy/internal/plugins/compactcmd"
	"github.com/guygrigsby/rudy/internal/plugins/initcmd"
	mcpplugin "github.com/guygrigsby/rudy/internal/plugins/mcp"
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
	"github.com/guygrigsby/rudy/internal/pluginstore"
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
	// A caller that named its own plugin set means exactly that set: no discovery, no child
	// processes it did not ask for.
	discover := set == nil
	if discover {
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
		Socket:   paths.Socket(),
	})
	built := &Built{Version: o.Version, Paths: paths, Config: cfg, Store: store, Registry: registry, Plugins: plugins, Server: srv}
	defer func() {
		if err != nil {
			shutCtx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
			defer cancel()
			_ = built.Close(shutCtx)
		}
	}()
	if discover {
		set = append(set, spawnedPlugins(paths, plugins, srv, o.Version,
			time.Duration(cfg.HookTimeoutMS)*time.Millisecond, notice)...)
	}
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
	return errors.Join(b.Server.Shutdown(ctx), b.Plugins.Close(ctx))
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

// spawnedPlugins is every manifest the roots hold that the lock file has not disabled, in
// discovery order, after the built-ins: a spawned plugin never shadows a linked one, since
// the first registration of a name wins. The roots are the data root (where rudy plugin
// install puts them), the config root (where a user drops one by hand) and the workspace's
// own .rudy, which is how a repository ships a plugin with itself.
func spawnedPlugins(paths config.Paths, plugins *plugin.Registry, srv *server.Server, version string, commandTimeout time.Duration, notice func(string)) []plugin.Plugin {
	// No working directory is no project scope, never a reason to fail the build.
	cwd, _ := os.Getwd()
	roots := []string{paths.Data, paths.Config}
	if cwd != "" {
		roots = append(roots, filepath.Join(cwd, ".rudy"))
	}
	manifests, errs := discoverPlugins(roots, filepath.Join(paths.Data, pluginstore.LockFile))
	for _, err := range errs {
		notice(err.Error())
	}
	var workspaces []string
	if cwd != "" {
		workspaces = []string{cwd}
	}
	out := make([]plugin.Plugin, 0, len(manifests))
	for _, m := range manifests {
		out = append(out, plugin.NewSpawned(m, plugin.SpawnServices{
			ServePlugin:    srv.ServePlugin,
			Version:        version,
			Workspaces:     workspaces,
			Fail:           plugins.Fail,
			CommandTimeout: commandTimeout,
		}))
	}
	return out
}

// discoverPlugins is Discover minus what the lock file disables. A disabled plugin is not
// started at all, so it gets no status row: it was never asked to load. A lock that will not
// parse spawns nothing at all: it is the only record of what the user turned off, and running
// a plugin the user disabled is worse than running none until the file is fixed.
func discoverPlugins(roots []string, lockPath string) ([]plugin.Manifest, []error) {
	disabled, err := pluginstore.DisabledFromLock(lockPath)
	if err != nil {
		return nil, []error{fmt.Errorf("%s: %v; spawning no plugins until it is fixed", pluginstore.LockFile, err)}
	}
	manifests, errs := plugin.Discover(roots)
	return slices.DeleteFunc(manifests, func(m plugin.Manifest) bool {
		return slices.Contains(disabled, m.Name)
	}), errs
}

// BuiltinPlugins is the linked-in set: the six tools, the agent tool, /init, /compact,
// /skills, /memory, the kernel's own slash commands (/model, /help, /fork, /plugins), the
// MCP servers from mcp.toml, the openai_chat and anthropic_messages providers, and the
// clinepass dialect over openai_chat.
func BuiltinPlugins(cfg *config.Config, paths config.Paths, httpc *httpx.Client, home string, env func(string) string, version string, summarize memoryplugin.Summarize) []plugin.Plugin {
	cache := filepath.Join(home, "Library", "Caches", "op-secrets.env")
	resolve := func(ref string) (string, error) { return config.ResolveSecret(ref, env, cache) }
	return append(BuiltinTools(), subagents.New(paths.Config), initcmd.New(), compactcmd.New(), commands.New(),
		skillsplugin.New(cfg.Skills.Dirs), memoryplugin.New(cfg.Memory, cfg.Sessions.Dir, version, summarize),
		newMCPPlugin(cfg, paths, resolve, version),
		openaichatplugin.New(cfg.Providers, httpc, resolve),
		anthropicplugin.New(cfg.Providers, httpc, resolve),
		clinepass.New(cfg.Providers, httpc, resolve))
}

// newMCPPlugin is the mcp plugin over the user and project mcp.toml files. Plugins load once
// at boot, before any session, so the project scope is the workspace rudy was started in; no
// workspace there means the user scope is all there is, never a reason to fail the build.
func newMCPPlugin(cfg *config.Config, paths config.Paths, resolve func(string) (string, error), version string) plugin.Plugin {
	cwd, err := os.Getwd()
	if err != nil {
		// No working directory is no project scope, never a reason to fail the build.
		cwd = ""
	}
	user, project := mcpplugin.Paths(paths.Config, cwd)
	return mcpplugin.New(user, project, time.Duration(cfg.MCP.ConnectTimeoutMS)*time.Millisecond, resolve, version)
}

// summarizeWith is the one prompt the memory plugin runs through a model. Nothing is resolved
// at wire time: the provider registry is filled by plugin.Load, which happens after this
// closure is built, so both the model and its provider are looked up per call.
func summarizeWith(cfg *config.Config, registry *provider.Registry, notice func(string)) memoryplugin.Summarize {
	// A misconfigured summary_model is a configuration fact, not a per-fold event: saying it
	// once keeps a fold on every turn from filling the operator's terminal with one message.
	var once sync.Once
	warn := func(text string) { once.Do(func() { notice(text) }) }
	return func(ctx context.Context, sessionID string, prompt string) (string, error) {
		m, err := summaryModel(cfg, registry, warn)
		if err != nil {
			return "", err
		}
		// A session id that will not parse costs the header, not the fold: the summary is
		// still the right answer for the session that asked for it.
		sid, _ := ulid.Parse(sessionID)
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
			SessionID: sid,
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
