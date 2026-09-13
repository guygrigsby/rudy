package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/guygrigsby/rudy/internal/cli/hosts"
	"github.com/guygrigsby/rudy/internal/config"
	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

// probeTimeout bounds the dial at the default socket. A local connect is answered by the
// kernel the moment a listener is bound, so anything slower than this is not a server that
// happens to be busy; it is a path nothing is serving, and every client run would pay the
// wait before it started its own server.
const probeTimeout = 50 * time.Millisecond

// attachTimeout bounds the dial at a socket an operator named. There is no fallback behind
// it, so it is worth waiting out a loaded machine rather than reporting no server on a path
// that has one.
const attachTimeout = 2 * time.Second

// greetTimeout bounds the hello on every attached connection, which the connect timeout does
// not: rudy serve binds its socket before it builds, so a daemon still loading plugins and
// refreshing its registry holds a client in the listener's backlog with a connect that has
// already succeeded and a hello nobody is reading. Unbounded, a build that then fails leaves
// the client waiting on a process that is exiting. A variable so a test can shrink it: what
// happens when it expires is behavior, and two seconds of it is not a test.
var greetTimeout = 2 * time.Second

// dialOptions is how a command was told to reach a server. All empty is the default: probe
// the socket a daemon would be serving and start one in this process if nothing answers.
type dialOptions struct {
	// Socket is --socket: that server or nothing.
	Socket string
	// Embed is --embed: this process serves itself, whatever is answering the default socket.
	Embed bool
	// Host is --host: the kernel runs on that machine, reached by ssh, or the command fails.
	Host string
	// Cwd is --cwd: the workspace path on the host, for a cwd the home-relative rule cannot
	// place. Only meaningful with Host.
	Cwd string
	// NoSync is --no-sync: open on the placement as it is, moving nothing there first.
	NoSync bool
}

// registerDialFlags puts --socket, --embed and the three host flags on a command that opens
// a client. Every such command carries all of them, so where a session runs is a property of
// the invocation rather than of which verb the operator happened to type.
func registerDialFlags(cmd *cobra.Command, d *dialOptions) {
	f := cmd.Flags()
	f.StringVar(&d.Socket, "socket", "", "attach to the server on this unix socket instead of probing the default")
	f.BoolVar(&d.Embed, "embed", false, "serve in this process without probing for a running server")
	f.StringVar(&d.Host, "host", "", "run the kernel on this ssh host instead of here")
	f.StringVar(&d.Cwd, "cwd", "", "workspace path on the host (default: your cwd's path relative to your home, under the host's home)")
	f.BoolVar(&d.NoSync, "no-sync", false, "do not push or copy the working tree to the host before opening")
}

// dialed is a greeted connection and everything the client needs beside it. Built is nil
// when the connection is attached to a daemon: the store, the registry and the plugins are
// that process's, and the only thing this one owns is the socket. Paths and Config are
// resolved either way, since the theme, the key table and the ui.* settings are the local
// terminal's business and not the server's.
type dialed struct {
	Client  *protocol.Client
	Paths   config.Paths
	Config  *config.Config
	Version string // the server's, from its hello
	Built   *Built // nil when attached
	Close   func()
	// Host is the machine the kernel is running on, zero when it is this one. Everything that
	// differs about a remote connection hangs off this being set rather than off a flag the
	// caller still holds, so a command that was handed a dialed does not need the invocation.
	Host hosts.Host
	// Home is the server's home directory, from its hello. The placement is computed against
	// it; meaningless when Host is zero.
	Home string
	// Cwd is --cwd verbatim: the placement the operator named for a cwd the home-relative
	// rule cannot map.
	Cwd string
	// NoSync is --no-sync: the working tree is not moved to the placement before a session
	// opens on it.
	NoSync bool
	// copied is the sync step's answer: the session opened on a copy of a tree no git tracks
	// rather than on a checkout. A copy has no other way home, so the client brings the
	// placement back when the session closes. Unexported because nothing outside this package
	// sets it and no flag says it.
	copied bool
}

// place is the cwd a session opens on: the placement on the host when this connection is
// remote, the local cwd otherwise. openOrResume calls it so no caller has to know which.
func (d *dialed) place(localCwd string) (string, error) {
	if d.Host.IsZero() {
		return localCwd, nil
	}
	return hosts.Place(localCwd, d.Paths.Home, d.Home, d.Cwd)
}

// dial reaches a server the way the operator asked for, and starts one when they did not ask
// for anything and none is running. The order is fixed: a host is that machine over ssh or an
// error, never a local server; an explicit --socket is that server or an error; --embed is
// this process without a probe; otherwise the default socket gets a short probe and a server
// that is not there is one this process becomes.
//
// It returns the process exit code alongside the error, 2 for a usage error, so a command
// can report a bad flag pair as one.
func dial(ctx context.Context, build buildFunc, o BuildOptions, d dialOptions, name string, asker bool) (*dialed, int, error) {
	// The config's remote.host is read here rather than in the caller so every command that
	// dials gets the same answer. localConfig is what attach reads anyway, and a config this
	// process cannot parse is reported by the branch that loads it for real rather than
	// turned into "no host" here. It is a default for an invocation that named no transport:
	// --socket and --embed each answer where the kernel runs, so an operator who typed one
	// has answered the question remote.host was answering, and only --host itself contradicts
	// them.
	host := d.Host
	if host == "" && d.Socket == "" && !d.Embed {
		if _, cfg, err := localConfig(o); err == nil {
			host = cfg.Remote.Host
		}
	}
	switch {
	case d.Socket != "" && d.Embed:
		return nil, 2, errors.New("--socket names a server to attach to and --embed says to be one; pass one or the other")
	case d.Host != "" && (d.Socket != "" || d.Embed):
		return nil, 2, errors.New("--host runs the kernel on another machine; it cannot be combined with --socket or --embed")
	case (d.Cwd != "" || d.NoSync) && host == "":
		return nil, 2, errors.New("--cwd and --no-sync only mean something with --host")
	}
	if host != "" {
		h, err := hosts.ParseHost(host)
		if err != nil {
			return nil, 2, err
		}
		return dialSSH(o, h, d, name, asker)
	}
	if d.Socket != "" {
		return attach(o, d.Socket, attachTimeout, name, asker)
	}
	if !d.Embed {
		socket, err := serveSocket("", o)
		if err != nil {
			return nil, 1, err
		}
		attached, code, err := attach(o, socket, probeTimeout, name, asker)
		switch {
		case err == nil:
			return attached, 0, nil
		case !errors.Is(err, protocol.ErrNoServer):
			// A socket that answered and then would not talk is not the same as an absent one:
			// a daemon that died between the connect and the hello, a listener that is not
			// rudy, a peer the transport refused. Answering any of those by starting a second
			// server over the same store would split the sessions between two processes rather
			// than report the failure.
			return nil, code, err
		}
	}
	return embed(ctx, build, o, name, asker)
}

// attach connects to a server this process did not start and greets it. Everything it opened
// is closed again on any failure, since a caller that got an error will never call the Close
// it did not receive.
func attach(o BuildOptions, socket string, timeout time.Duration, name string, asker bool) (*dialed, int, error) {
	// The local config comes first, before anything is connected: a config.toml that will not
	// parse is this process's problem and no daemon needs to hear about it. Loading it after
	// the hello would greet a server, take a connection off its accept loop and drop it again,
	// all to report an error that was true before the client started.
	paths, cfg, err := localConfig(o)
	if err != nil {
		return nil, 1, err
	}
	// Who owns the path comes before anything is sent down it. The server checks its peer's
	// uid; this is the same boundary from the other side, and without it a socket another
	// local user got to first (linux, XDG_RUNTIME_DIR unset, the default under a /tmp anyone
	// can write) takes this client's prompts, tool calls and permission answers into that
	// user's process. A refusal is final on both paths, explicit and default: embedding
	// instead would be a second server over the same store, which is the failure the deaf
	// socket above is already refused for.
	if err := protocol.CheckSocketOwner(socket); err != nil {
		return nil, 1, err
	}
	// The connect is bounded by its own timeout and by nothing else, not by the caller's
	// context: an interrupt that arrived before the client got going would otherwise turn
	// "nothing is serving that path" into a dial failure, and the run would report a cancelled
	// connect where it means to report an interrupt. Two seconds is the whole exposure.
	ctx, done := context.WithTimeout(context.Background(), timeout)
	defer done()
	conn, err := protocol.DialUnix(ctx, socket, timeout)
	if err != nil {
		// DialUnix names the path in every error it returns.
		return nil, 1, err
	}
	client := protocol.NewClient(conn)
	closeClient := func() { _ = client.Close() }
	greetCtx, greeted := context.WithTimeout(context.Background(), greetTimeout)
	defer greeted()
	hello, err := greet(greetCtx, client, name, Version(), asker)
	if err != nil {
		closeClient()
		if errors.Is(err, context.DeadlineExceeded) {
			// Not ErrNoServer: something is holding that path, and a second server over the
			// same store would split the sessions between the two. Retrying is the answer when
			// the daemon was still building, --embed when it is never going to answer.
			return nil, 1, fmt.Errorf("a server at %s answered but did not greet within %s; retry, or pass --embed", socket, greetTimeout)
		}
		return nil, 1, err
	}
	return &dialed{Client: client, Paths: paths, Config: cfg, Version: hello.Version, Close: closeClient}, 0, nil
}

// embed wires a server in this process and connects to it over the in-memory pipe. Its Close
// is the whole unwind: the client, then the serve loop, then everything Build wired, on the
// budget a client run gets.
func embed(ctx context.Context, build buildFunc, o BuildOptions, name string, asker bool) (*dialed, int, error) {
	b, err := build(ctx, o)
	if err != nil {
		return nil, 1, err
	}
	shut := func() {
		shutdownCtx, done := context.WithTimeout(context.Background(), clientShutdownBudget)
		defer done()
		_ = b.Close(shutdownCtx)
	}
	client, closeConn, err := serveInMemory(b, name, asker)
	if err != nil {
		shut()
		return nil, 1, err
	}
	closeAll := func() {
		// The connection first: closing it is what ends the Serve loop, and that loop is what
		// detaches the session and closes it, releasing the store's flock.
		closeConn()
		shut()
	}
	return &dialed{Client: client, Paths: b.Paths, Config: b.Config, Version: b.Version, Built: b, Close: closeAll}, 0, nil
}

// localConfig is the config and the paths a client resolves for itself, the same two steps
// Build starts with. An attached client has no Built to read them off, and the theme, the key
// table and the ui.* settings belong to the terminal in front of the operator rather than to
// whichever process is running the turn.
func localConfig(o BuildOptions) (config.Paths, *config.Config, error) {
	env, home, err := envAndHome(o)
	if err != nil {
		return config.Paths{}, nil, err
	}
	paths := config.XDG(env, home)
	cfg, err := config.Load(paths, o.Overrides)
	if err != nil {
		return config.Paths{}, nil, err
	}
	return paths, cfg, nil
}

// sessions is the session list, from the store when this process holds one and over the
// protocol when it does not. Store.List and session.list answer the same summaries in the
// same order, so --continue applies one rule to one shape either way.
func (d *dialed) sessions(ctx context.Context) ([]session.Summary, error) {
	if d.Built != nil {
		return d.Built.Store.List()
	}
	var res protocol.SessionListResult
	if err := d.Client.Call(ctx, protocol.MethodSessionList, nil, &res); err != nil {
		return nil, err
	}
	return res.Sessions, nil
}

// resolve finds a model by the spec an operator typed, against this process's registry when
// it has one and against the server's listing when it does not. provider.ResolveIn is the
// rule Registry.Resolve applies, so "provider:id" and a unique bare id mean the same thing
// attached and embedded.
func (d *dialed) resolve(ctx context.Context, spec string) (provider.Model, error) {
	if d.Built != nil {
		return d.Built.Registry.Resolve(spec)
	}
	var res protocol.RegistryListResult
	if err := d.Client.Call(ctx, protocol.MethodRegistryList, nil, &res); err != nil {
		return provider.Model{}, err
	}
	return provider.ResolveIn(res.Models, spec)
}
