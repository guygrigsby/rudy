// SPDX-License-Identifier: AGPL-3.0-or-later

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
	webplugin "github.com/guygrigsby/rudy/internal/plugins/web"
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
	// Prompt is the system prompt template this build read, empty for the built-in one.
	// `rudy prompt show` renders it without opening a session.
	Prompt string
}

// BuildOptions tunes wiring. Zero values mean the real environment.
type BuildOptions struct {
	Version   string
	Overrides map[string]any      // config keys that win over file and env, dotted ("default.model")
	Plugins   []plugin.Plugin     // nil means BuiltinPlugins
	Env       func(string) string // nil means os.Getenv
	Home      string              // "" means os.UserHomeDir
	Stderr    io.Writer           // nil means os.Stderr
	// Trust is how this caller asks whether the workspace's own plugins may run. Nil is a
	// caller that cannot ask, and no answer means no (ADR 0025).
	Trust          TrustAsker
	Socket         string        // "" means paths.Socket(); the socket a locked session tells a client to attach through
	RefreshTimeout time.Duration // <= 0 means 20 seconds; bounds the startup registry refresh
}

// defaultRefreshTimeout bounds the startup registry refresh when BuildOptions.RefreshTimeout
// is unset, so a stalled provider cannot hold a command silent forever.
const defaultRefreshTimeout = 20 * time.Second

// promptTemplate is the system prompt template a session renders: the file prompt.file
// names, else system.md under the config directory, else "" for the built-in one.
//
// A named file that cannot be read is an error the caller turns into a notice: the operator
// asked for that file by name and should hear that it is missing. A system.md that is not
// there is not an error, since nobody named it.
func promptTemplate(paths config.Paths, cfg *config.Config) (string, error) {
	named := config.ExpandHome(cfg.Prompt.File, paths.Home)
	path := named
	if path == "" {
		path = filepath.Join(paths.Config, "system.md")
	}
	body, err := os.ReadFile(path)
	switch {
	case err == nil:
		return string(body), nil
	case named != "":
		return "", fmt.Errorf("prompt: %s: %w; using the built-in prompt", named, err)
	}
	return "", nil
}

// shutdownBudget bounds the unwind of a half-built server, which has no running turn and no
// client, so it only has to close the sessions it never opened.
const shutdownBudget = 2 * time.Second

// buildFunc is what commands call to wire a server; tests substitute fakes through it. The
// options are the wiring a command varies: where its notices go, and which socket the server
// says a locked session is being served on. A fake builder honors those two and supplies the
// rest itself, so a test's socket still reaches the server it built.
type buildFunc func(ctx context.Context, o BuildOptions) (*Built, error)

// notices is the one place the harness's own prefix lives. Every line rudy writes about
// itself rather than about a session goes through this: "rudy: <text>" on the writer the
// command was given, so a plugin's failure and a daemon's accept error read the same in a
// log and neither can drift from the other.
func notices(w io.Writer) func(string) {
	return func(text string) { _, _ = fmt.Fprintln(w, "rudy:", text) }
}

// stderrOf is where a caller's diagnostics go: the writer it named, or this process's stderr
// when it named none. Every path that prints a notice without a Built to read it off goes
// through here, so a nil Stderr is one decision rather than one per caller.
func stderrOf(o BuildOptions) io.Writer {
	if o.Stderr == nil {
		return os.Stderr
	}
	return o.Stderr
}

// Build wires config, store, plugins, registry, gate and server. It never writes config. A
// failure after the server exists shuts it back down: it holds a context, loaded plugins and
// their connections, and a caller that got an error will never call Shutdown itself.
func Build(ctx context.Context, o BuildOptions) (_ *Built, err error) {
	env, home, err := envAndHome(o)
	if err != nil {
		return nil, err
	}
	stderr := stderrOf(o)
	paths := config.XDG(env, home)
	// A command that named a socket is serving on that one, so that is the socket a locked
	// session tells the next client to attach through; nobody naming one means the default.
	socket := o.Socket
	if socket == "" {
		socket = paths.Socket()
	}
	cfg, err := config.Load(paths, o.Overrides)
	if err != nil {
		return nil, err
	}
	store, err := session.OpenStore(cfg.Sessions.Dir)
	if err != nil {
		return nil, err
	}
	httpc := httpx.New(o.Version)
	// The cache dir holds the log, so it exists before the logger opens; the registry
	// snapshot below lands in the same place.
	if err := os.MkdirAll(paths.Cache, 0o700); err != nil {
		return nil, fmt.Errorf("cache dir: %w", err)
	}
	plain := notices(stderr)
	logger := openLog(cfg, o.Version, stderr, plain)
	// Every notice the operator sees is mirrored into the log: what the user saw before it
	// died is the first question anyone asks of a trail.
	notice := func(text string) {
		plain(text)
		logger.Warn(text)
	}
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
		set = BuiltinPlugins(cfg, paths, httpc, env, o.Version, summarizeWith(cfg, registry, notice))
	}
	// The server is built before the plugins load, and takes its providers from them
	// afterwards: a plugin's Host reaches the server through the protocol (Connect, Note,
	// the status broadcasts), so the server has to exist by the time any Init runs.
	g := gate.New(cfg.Permissions.Dangerous)
	// The prompt file is read once, here: a turn never waits on a disk read for it, and a
	// file that cannot be read is a notice rather than a failed boot (ADR 0024).
	prompt, err := promptTemplate(paths, cfg)
	if err != nil {
		notice(err.Error())
	}
	srv := server.New(server.Deps{
		Version:  o.Version,
		Config:   cfg,
		Store:    store,
		Registry: registry,
		Plugins:  plugins,
		Gate:     g,
		Hooks:    plugin.NewHookRunner(plugins, time.Duration(cfg.HookTimeoutMS)*time.Millisecond, notice),
		Socket:   socket,
		Prompt:   prompt,
		Home:     paths.Home,
	})
	built := &Built{Version: o.Version, Paths: paths, Config: cfg, Store: store, Registry: registry, Plugins: plugins, Server: srv, Prompt: prompt}
	defer func() {
		if err != nil {
			shutCtx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
			defer cancel()
			_ = built.Close(shutCtx)
		}
	}()
	if discover {
		set = append(set, spawnedPlugins(paths, plugins, srv, o.Version,
			time.Duration(cfg.HookTimeoutMS)*time.Millisecond, notice, o.Trust)...)
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
	// No provider and no snapshot is a fresh install, not an endpoint that went away:
	// nothing is dialed below, so a timeout would be a lie. Name the file a provider goes
	// in and the command that writes it, which is all a first run needs to hear.
	if len(providers) == 0 && !hadSnapshot {
		return nil, fmt.Errorf("no model provider is configured: %s has no [providers.*] table and no provider plugin is loaded. `rudy config sync` writes that file with every key and what it is for", paths.ConfigFile())
	}
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
func spawnedPlugins(paths config.Paths, plugins *plugin.Registry, srv *server.Server, version string, commandTimeout time.Duration, notice func(string), trust TrustAsker) []plugin.Plugin {
	// No working directory is no project scope, never a reason to fail the build.
	cwd, _ := os.Getwd()
	roots := []string{paths.Data, paths.Config}
	manifests, errs := discoverPlugins(roots, filepath.Join(paths.Data, pluginstore.LockFile))
	for _, err := range errs {
		notice(err.Error())
	}
	// The workspace's own plugins are separate, and load only if somebody agreed to this
	// workspace: a repository is read before it is run, and opening one is not agreeing to
	// execute what it declares (ADR 0025).
	if cwd != "" {
		own, ownErrs := discoverPlugins([]string{filepath.Join(cwd, ".rudy")}, filepath.Join(paths.Data, pluginstore.LockFile))
		for _, err := range ownErrs {
			notice(err.Error())
		}
		manifests = append(manifests, trustedOnly(paths, cwd, own, trust, notice)...)
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

// TrustAsker is how a caller asks the operator whether this workspace's own plugins may
// run. It answers true only when a person said so. Nil is a caller that cannot ask, which is
// every headless one: no answer means no, the rule the Gate keeps for an unsafe tool.
type TrustAsker func(root string, manifests []plugin.Manifest) (bool, error)

// trustedOnly is the workspace's own manifests when the operator has trusted this workspace
// as it stands, and nothing when they have not. A caller that cannot ask says why once,
// naming what it did not run, so a plugin that is missing is never a mystery.
func trustedOnly(paths config.Paths, cwd string, own []plugin.Manifest, trust TrustAsker, notice func(string)) []plugin.Manifest {
	if len(own) == 0 {
		return nil
	}
	store := pluginstore.New(paths.Data)
	trusted, err := store.IsTrusted(cwd, own)
	if err != nil {
		notice(err.Error())
		return nil
	}
	if trusted {
		return own
	}
	if trust == nil {
		notice(fmt.Sprintf("this workspace ships %d plugin(s) and is not trusted; run rudy plugins trust to allow them", len(own)))
		return nil
	}
	ok, err := trust(cwd, own)
	if err != nil {
		notice(err.Error())
		return nil
	}
	if !ok {
		notice("this workspace's plugins were not trusted; none of them were started")
		return nil
	}
	if err := store.Trust(cwd, own, time.Now()); err != nil {
		notice(err.Error())
	}
	return own
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
func BuiltinPlugins(cfg *config.Config, paths config.Paths, httpc *httpx.Client, env func(string) string, version string, summarize memoryplugin.Summarize) []plugin.Plugin {
	resolve := func(ref string) (string, error) { return config.ResolveSecret(ref, env, cfg.Secrets.File) }
	return append(BuiltinTools(), webplugin.New(cfg.Web, version, resolve),
		subagents.New(paths.Config), initcmd.New(), compactcmd.New(), commands.New(),
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
			Model:    m.Ref,
			Messages: []provider.Message{{Role: provider.RoleUser, Content: []session.Block{session.TextBlock(prompt)}}},
			Thinking: session.ThinkingOff,
			// The fold resolves its budget against the model it is folding through, not the
			// session's: max_tokens 0 means the model's own, and a request that names no
			// limit is refused outright on the Anthropic wire.
			MaxTokens: m.OutputBudget(cfg.MaxTokens),
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
