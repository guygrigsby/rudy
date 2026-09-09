package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/tool"
)

// toolPrefix is how a server's tool is named to the model: the plugin, the server and the
// tool, so two servers offering "search" are two distinct tools and neither can shadow a
// built-in.
const toolPrefix = "mcp__"

type mcpPlugin struct {
	userPath       string
	projectPath    string // "" when the process has no workspace
	connectTimeout time.Duration
	resolve        func(string) (string, error)
	version        string

	mu      sync.Mutex
	servers []*server
}

// New builds the plugin over the user and project mcp.toml files. The servers are per
// process, not per session: plugins load once at boot, so the project scope is the workspace
// the process started in. connectTimeout bounds each server's handshake, resolve turns a
// secret reference into its value and version is what rudy announces itself as.
func New(userPath, projectPath string, connectTimeout time.Duration, resolve func(string) (string, error), version string) plugin.Plugin {
	return &mcpPlugin{
		userPath:       userPath,
		projectPath:    projectPath,
		connectTimeout: connectTimeout,
		resolve:        resolve,
		version:        version,
	}
}

func (p *mcpPlugin) Name() string { return "mcp" }

// Init connects every configured server and registers its tools. A server that cannot be
// reached costs a notice and the rest still load: one unreachable server is not a reason to
// have no tools at all. An unreadable mcp.toml is different, and fails the plugin.
func (p *mcpPlugin) Init(ctx context.Context, h plugin.Host) error {
	configs, _, err := Merged(p.userPath, p.projectPath)
	if err != nil {
		return err
	}
	if len(configs) == 0 {
		return nil
	}
	// Every server connects at once: a slow one costs its own timeout, not everyone else's
	// as well, and boot waits for the slowest rather than the sum.
	connected := make([]*server, len(configs))
	failures := make([]error, len(configs))
	var wg sync.WaitGroup
	for i, cfg := range configs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			connected[i], failures[i] = p.connect(ctx, cfg)
		}()
	}
	wg.Wait()
	// Registration is sequential and in Merged order, so which server answered first never
	// changes the tool list a model sees.
	ready := 0
	var live []*server
	for i, cfg := range configs {
		if failures[i] != nil {
			h.Notice(fmt.Sprintf("server %s: %v", cfg.Name, failures[i]))
			continue
		}
		live = append(live, connected[i])
		p.register(h, cfg, connected[i])
		ready++
	}
	p.mu.Lock()
	p.servers = append(p.servers, live...)
	p.mu.Unlock()
	h.SetStatus("servers", []plugin.Span{{Text: fmt.Sprintf("mcp %d/%d", ready, len(configs)), Role: "muted"}})
	return nil
}

// connect validates one entry and dials it under its own share of the connect timeout.
func (p *mcpPlugin) connect(ctx context.Context, cfg ServerConfig) (*server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	connectCtx, cancel := context.WithTimeout(ctx, p.connectTimeout)
	defer cancel()
	return connect(connectCtx, cfg, p.resolve, p.version)
}

// register hands the model everything one server advertised. A tool that cannot be
// registered, whether its name is unusable or already taken, is a notice and the server's
// other tools still register.
func (p *mcpPlugin) register(h plugin.Host, cfg ServerConfig, srv *server) {
	for _, t := range srv.tools {
		name := t.Name
		fail := func(err error) { h.Notice(fmt.Sprintf("server %s: tool %s: %v", cfg.Name, name, err)) }
		if err := validName("tool", name); err != nil {
			fail(err)
			continue
		}
		// The schema is the server's own, handed to the model as it described it. It
		// arrives already decoded, so this marshal is canonical rather than byte exact;
		// nothing durable is keyed on it.
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			fail(err)
			continue
		}
		if err := h.RegisterTool(tool.Tool{
			Name:        toolPrefix + cfg.Name + "__" + name,
			Description: t.Description,
			Schema:      schema,
			// Every MCP tool is unsafe: rudy cannot see what the server does, so the
			// gate decides before it runs.
			Safety: tool.Unsafe,
			Invoke: func(ctx context.Context, call tool.Call) (tool.Result, error) {
				return srv.call(ctx, name, call.Input)
			},
		}); err != nil {
			fail(err)
		}
	}
}

// Close ends every server session, which is what stops a stdio server's child process. The
// registry calls it on the way out; every session is closed even when an earlier one failed.
func (p *mcpPlugin) Close() error {
	p.mu.Lock()
	servers := p.servers
	p.servers = nil
	p.mu.Unlock()
	var errs []error
	for _, s := range servers {
		if err := s.close(); err != nil {
			errs = append(errs, fmt.Errorf("mcp: server %s: %w", s.cfg.Name, err))
		}
	}
	return errors.Join(errs...)
}
