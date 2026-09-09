package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/tool"
)

// Manifest is one plugin.toml: what to run and what it calls itself. Dir is filled on read
// and is where a relative command is looked for first.
type Manifest struct {
	Name            string            `toml:"name"`
	Version         string            `toml:"version"`
	ProtocolVersion int               `toml:"protocol_version"`
	Command         string            `toml:"command"`
	Args            []string          `toml:"args"`
	Env             map[string]string `toml:"env"`
	Description     string            `toml:"description"`
	Dir             string            `toml:"-"` // the directory holding plugin.toml
}

// ManifestFile is the name every plugin directory carries.
const ManifestFile = "plugin.toml"

// manifestNameRe is the whole character set a manifest name may use: no ".", "/" or
// whitespace, so a name can never carry a path segment. rudy plugin install turns a
// manifest's name straight into a directory name (Root/plugins/<name>), so this is not
// merely cosmetic: a name that fails this cannot walk the checkout outside Root/plugins.
var manifestNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ValidateManifestName is the one rule every plugin name has to hold to, wherever it arrives
// from: non-empty, matching manifestNameRe, and equal to its own filepath.Base (implied by
// the regexp, which already excludes "/" and ".", but cheap and it says directly what the
// check is for). ReadManifest runs it on a freshly parsed plugin.toml; pluginstore runs it
// again on a bare name an operator typed (rudy plugin uninstall/enable/disable/update
// <name>) before that name ever reaches a lock lookup or a Root/plugins/<name> path, since a
// name is not trustworthy just because ReadManifest once accepted it for some other
// checkout.
func ValidateManifestName(name string) error {
	if name == "" {
		return fmt.Errorf("name is empty")
	}
	if !manifestNameRe.MatchString(name) || name != filepath.Base(name) {
		return fmt.Errorf("name %q must match %s", name, manifestNameRe.String())
	}
	return nil
}

// ReadManifest reads dir/plugin.toml. A manifest with no name or no command is refused: both
// are what the server needs before it can run anything. Both rudy's own Discover, which also
// checks the name against the directory it was found in, and rudy plugin Install, whose
// stage directory's name is a random temp name and never checked against the manifest, go
// through this to validate the name.
func ReadManifest(dir string) (Manifest, error) {
	path := filepath.Join(dir, ManifestFile)
	b, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := toml.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("plugin: %s: %w", path, err)
	}
	if err := ValidateManifestName(m.Name); err != nil {
		return Manifest{}, fmt.Errorf("plugin: %s: %w", path, err)
	}
	if m.Command == "" {
		return Manifest{}, fmt.Errorf("plugin: %s: command is empty", path)
	}
	m.Dir = dir
	return m, nil
}

// Discover reads <root>/plugins/<name>/plugin.toml under each root in order; the first
// manifest for a name wins, so the caller decides what shadows what by the order it passes
// (wire passes the data root first, so an installed plugin wins over a copy dropped in the
// config root or in a workspace). A
// directory with no manifest is not a plugin and is skipped silently; a manifest whose name
// differs from its directory is skipped with an error in the returned list, since the
// directory is the identity the lock file and the install path use.
func Discover(roots []string) ([]Manifest, []error) {
	var out []Manifest
	var errs []error
	seen := map[string]bool{}
	for _, root := range roots {
		if root == "" {
			continue
		}
		dir := filepath.Join(root, "plugins")
		ents, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("plugin: %s: %w", dir, err))
			continue
		}
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			pdir := filepath.Join(dir, e.Name())
			m, err := ReadManifest(pdir)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if m.Name != e.Name() {
				errs = append(errs, fmt.Errorf("plugin: %s: manifest name %q does not match its directory", pdir, m.Name))
				continue
			}
			if seen[m.Name] {
				continue
			}
			seen[m.Name] = true
			out = append(out, m)
		}
	}
	return out, errs
}

// SpawnServices is what a spawned plugin needs from the rest of the process: the server's
// plugin serve loop, the harness version and workspace roots for plugin.init, and the way to
// withdraw everything the plugin registered when its process dies.
type SpawnServices struct {
	ServePlugin func(ctx context.Context, conn protocol.Conn, reg Registrar) error
	Version     string
	Workspaces  []string
	Fail        func(name, reason string)
	// CommandTimeout bounds one command.invoke. A slash command runs on the calling
	// connection's serve loop, so a child that never answers would park that connection for
	// good; the budget is the same hook_timeout_ms a hook handler gets. Zero means
	// DefaultHookTimeout.
	CommandTimeout time.Duration
}

// Registrar is what the server calls when a spawned plugin sends plugin.register_* or
// plugin.set_status, and where it hands the plugin's notifications. Spawned implements it
// against the Host it received in Init, so a spawned plugin's registrations land in exactly
// the registry a linked plugin's do.
//
// plugin.append_note is not here: the server appends a note under the name on the
// connection, which is stronger than any name the Registrar could assert.
type Registrar interface {
	Name() string
	RegisterTool(name, description string, schema json.RawMessage, safety tool.Safety) error
	RegisterCommand(name, description string) error
	RegisterHook(point HookPoint, priority int) error
	RegisterProvider(name, wire string) error
	SetStatus(key string, content []Span)
	SetWidget(key string, slot WidgetSlot, content []Span) error
	// Deliver hands over a notification the plugin sent (tool.progress, provider.delta).
	Deliver(method string, params json.RawMessage)
}

// Register applies one plugin.register_* or plugin.set_status request to reg. It is the
// mapping from wire params to the Registrar, in one place: the server's dispatch decides
// whether the caller may make the assertion and this decides what the assertion says.
func Register(reg Registrar, method string, params json.RawMessage) (any, error) {
	ok := struct{}{}
	switch method {
	case protocol.MethodPluginRegisterTool:
		var p protocol.PluginRegisterToolParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("%w: %s", protocol.ErrInvalidArgument, err)
		}
		return ok, reg.RegisterTool(p.Name, p.Description, p.InputSchema, p.Safety)
	case protocol.MethodPluginRegisterCommand:
		var p protocol.PluginRegisterCommandParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("%w: %s", protocol.ErrInvalidArgument, err)
		}
		return ok, reg.RegisterCommand(p.Name, p.Description)
	case protocol.MethodPluginRegisterHook:
		var p protocol.PluginRegisterHookParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("%w: %s", protocol.ErrInvalidArgument, err)
		}
		return ok, reg.RegisterHook(HookPoint(p.Point), p.Priority)
	case protocol.MethodPluginRegisterProvider:
		var p protocol.PluginRegisterProviderParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("%w: %s", protocol.ErrInvalidArgument, err)
		}
		return ok, reg.RegisterProvider(p.Name, p.Wire)
	case protocol.MethodPluginRegisterWidget:
		var p protocol.PluginRegisterWidgetParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("%w: %s", protocol.ErrInvalidArgument, err)
		}
		return ok, reg.SetWidget(p.Key, p.Slot, p.Content)
	case protocol.MethodPluginSetStatus:
		var p protocol.PluginSetStatusParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("%w: %s", protocol.ErrInvalidArgument, err)
		}
		reg.SetStatus(p.Key, p.Content)
		return ok, nil
	}
	return nil, fmt.Errorf("%w: %s is not a registration", protocol.ErrInvalidArgument, method)
}

// The two things a Status can have come from.
const (
	OriginLinked  = "linked"
	OriginSpawned = "spawned"
)

const (
	// initTimeout bounds the plugin.init handshake. A child that has not answered by then
	// is not going to.
	initTimeout = 10 * time.Second
	// cancelTimeout bounds the tool.cancel a cancelled invocation sends, which nobody waits
	// on: it is a courtesy to the child, not part of the caller's unwind.
	cancelTimeout = 5 * time.Second
	// syncTimeout bounds the wait for a completion's last deltas to be delivered.
	syncTimeout = 2 * time.Second
	// stopGrace is how long Close waits for a child to notice its stdin closed before it is
	// killed.
	stopGrace = time.Second
	// exitGrace is how long a lost connection waits for the process to be reaped before it
	// decides the connection is what went: a child's stdout closes as it exits, and its exit
	// status is the better reason of the two.
	exitGrace = time.Second
	// termGrace is how long a terminated child gets between SIGTERM and the kill.
	termGrace = 5 * time.Second
	// tailBytes is how much of a child's stderr is kept for the failure notice.
	tailBytes = 4096
	// deltaBuffer is how far a streaming provider may run ahead of the turn consuming it.
	deltaBuffer = 64
)

// Spawned is one plugin running as a child process, speaking the protocol on its stdin and
// stdout. Everything it registers goes through the same Host a linked plugin uses: the
// kernel has no second path.
type Spawned struct {
	m        Manifest
	services SpawnServices
	start    func(ctx context.Context) (protocol.Conn, *tail, func() error, error)

	host Host
	peer *protocol.Peer
	tl   *tail

	// ready is set when Init has returned: from then on the registry has staged what this
	// plugin registered and a later registration would never be committed.
	ready atomic.Bool
	// commit is closed by the registry once this plugin's stage has been committed. The
	// failure path waits on it: a child that dies between Init returning and the commit
	// would otherwise have its registrations withdrawn before they were installed.
	commit chan struct{}
	// exited is closed when the child process has been reaped.
	exited chan struct{}
	// closing is set by Close: an exit we asked for is not a failure.
	closing atomic.Bool

	procMu sync.Mutex
	proc   *os.Process

	mu     sync.Mutex
	deltas map[string]*deltaSub
}

// deltaSub is one in-flight provider.complete: the parts its plugin is streaming.
type deltaSub struct {
	parts chan provider.Part
	done  chan struct{}
}

var (
	_ Plugin                       = (*Spawned)(nil)
	_ Registrar                    = (*Spawned)(nil)
	_ io.Closer                    = (*Spawned)(nil)
	_ interface{ Origin() string } = (*Spawned)(nil)
)

// NewSpawned returns the plugin for one manifest. Nothing runs until Init.
func NewSpawned(m Manifest, s SpawnServices) *Spawned {
	sp := newSpawnedWith(m, s, nil)
	sp.start = sp.startProcess
	return sp
}

// newSpawnedWith is the test seam: start returns the child's Conn, its stderr tail and a
// wait function that returns when the child exits.
func newSpawnedWith(m Manifest, s SpawnServices, start func(ctx context.Context) (protocol.Conn, *tail, func() error, error)) *Spawned {
	return &Spawned{
		m:        m,
		services: s,
		start:    start,
		commit:   make(chan struct{}),
		exited:   make(chan struct{}),
		deltas:   map[string]*deltaSub{},
	}
}

func (s *Spawned) Name() string { return s.m.Name }

// Origin is what plugin.state reports for this plugin.
func (s *Spawned) Origin() string { return OriginSpawned }

// Init starts the child, serves its requests and completes the plugin.init handshake. The
// child must send its registrations before it answers plugin.init: what it registers after
// that is refused, because the registry commits this plugin's stage when Init returns.
func (s *Spawned) Init(ctx context.Context, h Host) error {
	if s.services.ServePlugin == nil {
		return fmt.Errorf("plugin %s: no server to serve its connection", s.m.Name)
	}
	conn, tl, wait, err := s.start(ctx)
	if err != nil {
		return err
	}
	s.host = h
	s.tl = tl
	s.peer = protocol.NewPeer(conn)
	// The plugin's own connection to the server, serving exactly the requests a linked
	// plugin's Host.Connect client may make plus the registrations. Its return is how this
	// hears that the connection has ended, which is a way for a plugin to be gone that has
	// nothing to do with its process still being alive.
	lost := make(chan error, 1)
	go func() { lost <- s.services.ServePlugin(ctx, s.peer.Incoming(), s) }()

	ictx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	var res protocol.PluginInitResult
	err = s.peer.Client().Call(ictx, protocol.MethodPluginInit, protocol.PluginInitParams{
		Name:            s.m.Name,
		Version:         s.services.Version,
		ProtocolVersion: protocol.ProtocolVersion,
		Config:          h.Config(),
		WorkspaceRoots:  s.services.Workspaces,
	}, &res)
	if err != nil {
		return s.abandon(wait, fmt.Errorf("plugin.init: %w", err))
	}
	if res.ProtocolVersion != protocol.ProtocolVersion {
		return s.abandon(wait, fmt.Errorf("protocol version %d, want %d", res.ProtocolVersion, protocol.ProtocolVersion))
	}
	s.ready.Store(true)
	go s.watch(wait, lost)
	return nil
}

// committed is called by the registry once this plugin's registrations are installed. It
// releases the failure path, which must not withdraw registrations that are not there yet.
func (s *Spawned) committed() { close(s.commit) }

// abandon ends a child that failed its handshake and returns why, with whatever it managed
// to say on stderr.
func (s *Spawned) abandon(wait func() error, err error) error {
	s.closing.Store(true)
	s.stop(wait)
	if t := s.tl.String(); t != "" {
		return fmt.Errorf("%w; stderr: %s", err, t)
	}
	return err
}

// watch ends the plugin when either half of it ends: the process exits, or the connection to
// it is lost while it is still running (a child that closed its stdout, a transport that
// overflowed). Either way, once the plugin was committed, everything it registered is
// withdrawn and the reason is said once: the session goes on without it.
func (s *Spawned) watch(wait func() error, lost <-chan error) {
	exit := make(chan error, 1)
	go func() {
		err := wait()
		exit <- err
		close(s.exited)
	}()
	var reason string
	select {
	case err := <-exit:
		reason = "exited: " + exitReason(err)
	case err := <-lost:
		if s.closing.Load() || errors.Is(err, context.Canceled) {
			// Our own shutdown, not the plugin's: the server cancelled the serve loop.
			return
		}
		select {
		case perr := <-exit:
			reason = "exited: " + exitReason(perr)
		case <-time.After(exitGrace):
			// The connection is gone and the process is not. A child nobody can talk to is
			// not going to stop on its own.
			s.terminate()
			reason = "connection lost: " + lostReason(err)
		}
	}
	if s.closing.Load() {
		return
	}
	// The registry commits this plugin's stage when Init returns; withdrawing before that
	// would leave behind exactly what it was meant to remove.
	<-s.commit
	if s.closing.Load() {
		return
	}
	reason = fmt.Sprintf("%s; stderr: %s", reason, s.tl.String())
	if s.services.Fail != nil {
		s.services.Fail(s.m.Name, reason)
	}
	if s.host != nil {
		s.host.Notice(reason)
	}
	_ = s.peer.Close()
}

func exitReason(err error) string {
	if err != nil {
		return err.Error()
	}
	return "process ended"
}

func lostReason(err error) string {
	if err != nil {
		return err.Error()
	}
	return "end of input"
}

// terminate asks a child that has stopped talking to us to stop, and insists if it will not.
func (s *Spawned) terminate() {
	s.procMu.Lock()
	p := s.proc
	s.procMu.Unlock()
	if p == nil {
		return
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		_ = p.Kill()
		return
	}
	select {
	case <-s.exited:
	case <-time.After(termGrace):
		_ = p.Kill()
	}
}

// Close ends the child: its stdin is closed first, which is how a well-behaved plugin is
// told to exit, and it is killed if it does not. The watcher started by Init is what reaps
// it, so this waits on that.
func (s *Spawned) Close() error {
	s.closing.Store(true)
	s.shutdown(s.exited)
	return nil
}

// stop is Close for a child the registry never took, whose exit nobody is watching: it reaps
// on its own goroutine and waits on that instead.
func (s *Spawned) stop(wait func() error) {
	done := make(chan struct{})
	go func() { defer close(done); _ = wait() }()
	s.shutdown(done)
}

// shutdown closes the child's stdin and waits for exited, killing it if it takes longer than
// the grace. The second wait is bounded too: the process has been killed, and a wait that
// still has not returned is not something a shutdown is going to stand still for.
func (s *Spawned) shutdown(exited <-chan struct{}) {
	if s.peer != nil {
		_ = s.peer.Close()
	}
	select {
	case <-exited:
		return
	case <-time.After(stopGrace):
	}
	s.kill()
	select {
	case <-exited:
	case <-time.After(stopGrace):
	}
}

func (s *Spawned) kill() {
	s.procMu.Lock()
	p := s.proc
	s.procMu.Unlock()
	if p != nil {
		_ = p.Kill()
	}
}

// startProcess runs the manifest's command. The child's lifetime is the plugin's, not the
// load context's, so it is started on a background context and ended by Close.
func (s *Spawned) startProcess(context.Context) (protocol.Conn, *tail, func() error, error) {
	cmd := exec.CommandContext(context.Background(), s.command(), s.m.Args...)
	cmd.Dir = s.m.Dir
	cmd.Env = append(os.Environ(), envPairs(s.m.Env)...)
	// Explicit pipes rather than StdinPipe and StdoutPipe: those are closed by Wait, which
	// runs concurrently with the reader here, and a pipe closed under a read turns a plain
	// end of input into an error that says nothing about why the plugin went away.
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("plugin %s: %w", s.m.Name, err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = closeAll(inR, inW)
		return nil, nil, nil, fmt.Errorf("plugin %s: %w", s.m.Name, err)
	}
	cmd.Stdin = inR
	cmd.Stdout = outW
	tl := newTail(tailBytes)
	cmd.Stderr = tl
	if err := cmd.Start(); err != nil {
		_ = closeAll(inR, inW, outR, outW)
		return nil, nil, nil, fmt.Errorf("plugin %s: %s: %w", s.m.Name, s.command(), err)
	}
	// The child holds its own ends now.
	_ = closeAll(inR, outW)
	s.procMu.Lock()
	s.proc = cmd.Process
	s.procMu.Unlock()
	conn := protocol.NewStreamConn(outR, inW, closerFunc(func() error { return closeAll(inW, outR) }))
	return conn, tl, cmd.Wait, nil
}

// command is the manifest's command, resolved against the plugin's own directory first: a
// plugin that ships its binary next to its manifest names it plainly.
func (s *Spawned) command() string {
	if s.m.Dir == "" || filepath.IsAbs(s.m.Command) || filepath.Base(s.m.Command) != s.m.Command {
		return s.m.Command
	}
	local := filepath.Join(s.m.Dir, s.m.Command)
	if fi, err := os.Stat(local); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
		return local
	}
	return s.m.Command
}

func envPairs(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func closeAll(cs ...io.Closer) error {
	var errs []error
	for _, c := range cs {
		if err := c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// refuseLate is what a registration arriving after Init returned comes back as. Status and
// widget writes are exempt: both are live by contract, and neither adds a capability.
func (s *Spawned) refuseLate(kind, name string) error {
	if !s.ready.Load() {
		return nil
	}
	return fmt.Errorf("%w: plugin %s registered %s %q after plugin.init returned", session.ErrInvariant, s.m.Name, kind, name)
}

func (s *Spawned) RegisterTool(name, description string, schema json.RawMessage, safety tool.Safety) error {
	if err := s.refuseLate("tool", name); err != nil {
		return err
	}
	if safety == "" {
		safety = tool.Unsafe
	}
	return s.host.RegisterTool(tool.Tool{
		Name:        name,
		Description: description,
		Schema:      schema,
		Safety:      safety,
		Invoke:      s.invoke(name),
	})
}

func (s *Spawned) RegisterCommand(name, description string) error {
	if err := s.refuseLate("command", name); err != nil {
		return err
	}
	return s.host.RegisterCommand(Command{Name: name, Description: description, Run: s.runCommand(name)})
}

func (s *Spawned) RegisterHook(point HookPoint, priority int) error {
	if err := s.refuseLate("hook", string(point)); err != nil {
		return err
	}
	if !point.Valid() {
		return fmt.Errorf("%w: unknown hook point %q", protocol.ErrInvalidArgument, string(point))
	}
	return s.host.RegisterHook(HookHandler{Point: point, Priority: priority, Handle: s.fireHook(point)})
}

// RegisterProvider takes a wire: custom provider, which the server drives by calling
// provider.complete on the plugin. A codec wire from a spawned plugin is refused: it would
// need the endpoint and credentials config only a linked provider plugin reads.
func (s *Spawned) RegisterProvider(name, wire string) error {
	if err := s.refuseLate("provider", name); err != nil {
		return err
	}
	switch wire {
	case protocol.WireCustom:
		return s.host.RegisterProvider(&remoteProvider{name: name, sp: s})
	case protocol.WireOpenAIChat, protocol.WireAnthropicMessages:
		return fmt.Errorf("%w: wire %q is for a linked provider plugin; a spawned plugin registers wire custom", protocol.ErrInvalidArgument, wire)
	}
	return fmt.Errorf("%w: unknown provider wire %q", protocol.ErrInvalidArgument, wire)
}

func (s *Spawned) SetStatus(key string, content []Span) { s.host.SetStatus(key, content) }

func (s *Spawned) SetWidget(key string, slot WidgetSlot, content []Span) error {
	return s.host.SetWidget(key, slot, content)
}

// Deliver routes a notification the plugin sent. A delta for a completion nobody is waiting
// on is dropped: the turn that asked for it has already ended.
func (s *Spawned) Deliver(method string, params json.RawMessage) {
	switch method {
	case protocol.NotifyProviderDelta:
		var d protocol.ProviderDelta
		if err := json.Unmarshal(params, &d); err != nil {
			return
		}
		s.mu.Lock()
		sub := s.deltas[d.RequestID]
		s.mu.Unlock()
		if sub == nil {
			return
		}
		select {
		case sub.parts <- d.Part:
		case <-sub.done:
		}
	case protocol.NotifyToolProgress:
		// Nothing renders a running tool's progress in this plan. A plugin that sends it is
		// not doing anything wrong, so it is dropped here rather than refused.
	}
}

// invoke is the tool body for a tool the child registered: one tool.invoke call. A context
// that ends before the answer sends tool.cancel, so the child stops working on a result
// nobody will read.
func (s *Spawned) invoke(name string) func(ctx context.Context, call tool.Call) (tool.Result, error) {
	return func(ctx context.Context, call tool.Call) (tool.Result, error) {
		var res protocol.ToolInvokeResult
		err := s.peer.Client().Call(ctx, protocol.MethodToolInvoke, protocol.ToolInvokeParams{
			SessionID: call.SessionID.String(),
			ToolUseID: call.ID,
			Name:      name,
			Input:     call.Input,
			Workspace: call.Workspace,
			TimeoutMS: timeoutMS(ctx),
		}, &res)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				s.cancelTool(call.ID)
				return tool.Result{}, ctxErr
			}
			return tool.Result{}, err
		}
		return tool.Result{Content: res.Content, IsError: res.IsError}, nil
	}
}

// timeoutMS is how long the caller is prepared to wait, so the child can give up when the
// harness would have. The turn's tool timeout is a deadline on this context; no deadline is
// no timeout, which the contract spells zero.
func timeoutMS(ctx context.Context) int64 {
	dl, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	ms := time.Until(dl).Milliseconds()
	if ms < 0 {
		return 0
	}
	return ms
}

// cancelTool tells the child to stop. It runs on its own goroutine with its own deadline:
// the caller is already unwinding an interrupted turn and has nothing to wait for.
func (s *Spawned) cancelTool(toolUseID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), cancelTimeout)
		defer cancel()
		_ = s.peer.Client().Call(ctx, protocol.MethodToolCancel, protocol.ToolCancelParams{ToolUseID: toolUseID}, nil)
	}()
}

// fireHook is one hook.fire round trip: the payload marshaled, the answer unmarshaled into
// the point's own result type. A point that returns nothing, or an empty result, is a pass.
func (s *Spawned) fireHook(point HookPoint) func(ctx context.Context, call HookCall) (any, error) {
	return func(ctx context.Context, call HookCall) (any, error) {
		payload, err := json.Marshal(call.Payload)
		if err != nil {
			return nil, err
		}
		var out protocol.HookFireResult
		err = s.peer.Client().Call(ctx, protocol.MethodHookFire, protocol.HookFireParams{
			Point:     string(point),
			SessionID: call.SessionID,
			TurnID:    call.TurnID,
			Payload:   payload,
		}, &out)
		if err != nil {
			return nil, err
		}
		res := ResultFor(point)
		if res == nil || len(out.Result) == 0 || string(out.Result) == "null" {
			return nil, nil
		}
		if err := json.Unmarshal(out.Result, res); err != nil {
			return nil, err
		}
		return res, nil
	}
}

// runCommand is one command.invoke: a prompt the server submits, a notice it shows, or
// nothing at all.
func (s *Spawned) runCommand(name string) func(ctx context.Context, call CommandCall) (Action, error) {
	return func(ctx context.Context, call CommandCall) (Action, error) {
		// A slash command runs on the calling connection's serve loop, so this budget is
		// what keeps a child that never answers from parking that connection for good.
		ctx, cancel := context.WithTimeout(ctx, s.commandTimeout())
		defer cancel()
		var out protocol.CommandInvokeResult
		err := s.peer.Client().Call(ctx, protocol.MethodCommandInvoke, protocol.CommandInvokeParams{
			SessionID: call.SessionID.String(),
			Name:      name,
			Args:      call.Args,
		}, &out)
		if errors.Is(err, context.DeadlineExceeded) {
			timedOut := fmt.Errorf("/%s: no answer within %s", name, s.commandTimeout())
			if s.host != nil {
				s.host.Notice(timedOut.Error())
			}
			return nil, timedOut
		}
		if err != nil {
			return nil, err
		}
		switch {
		case out.Prompt != "":
			return SubmitPrompt{Text: out.Prompt}, nil
		case out.Notice != "":
			return Notice{Text: out.Notice}, nil
		}
		return NoAction{}, nil
	}
}

func (s *Spawned) commandTimeout() time.Duration {
	if s.services.CommandTimeout > 0 {
		return s.services.CommandTimeout
	}
	return DefaultHookTimeout
}

func (s *Spawned) subscribe(requestID string) *deltaSub {
	sub := &deltaSub{parts: make(chan provider.Part, deltaBuffer), done: make(chan struct{})}
	s.mu.Lock()
	s.deltas[requestID] = sub
	s.mu.Unlock()
	return sub
}

func (s *Spawned) unsubscribe(requestID string) {
	s.mu.Lock()
	sub := s.deltas[requestID]
	delete(s.deltas, requestID)
	s.mu.Unlock()
	if sub != nil {
		close(sub.done)
	}
}

// tail keeps the last max bytes written to it, which is what a failed plugin's notice
// carries: the end of a child's stderr is where it says why it died.
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newTail(max int) *tail { return &tail{max: max} }

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
