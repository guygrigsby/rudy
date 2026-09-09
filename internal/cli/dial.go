package cli

import (
	"context"
	"errors"
	"time"

	"github.com/spf13/cobra"

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

// dialOptions is how a command was told to reach a server. Both empty is the default: probe
// the socket a daemon would be serving and start one in this process if nothing answers.
type dialOptions struct {
	// Socket is --socket: that server or nothing.
	Socket string
	// Embed is --embed: this process serves itself, whatever is answering the default socket.
	Embed bool
}

// registerDialFlags puts --socket and --embed on a command that opens a client. Every such
// command carries both, so where a session runs is a property of the invocation rather than
// of which verb the operator happened to type.
func registerDialFlags(cmd *cobra.Command, d *dialOptions) {
	f := cmd.Flags()
	f.StringVar(&d.Socket, "socket", "", "attach to the server on this unix socket instead of probing the default")
	f.BoolVar(&d.Embed, "embed", false, "serve in this process without probing for a running server")
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
}

// dial reaches a server the way the operator asked for, and starts one when they did not ask
// for anything and none is running. The order is fixed: an explicit --socket is that server
// or an error; --embed is this process without a probe; otherwise the default socket gets a
// short probe and a server that is not there is one this process becomes.
//
// It returns the process exit code alongside the error, 2 for a usage error, so a command
// can report a bad flag pair as one.
func dial(ctx context.Context, build buildFunc, o BuildOptions, d dialOptions, name string, asker bool) (*dialed, int, error) {
	if d.Socket != "" && d.Embed {
		return nil, 2, errors.New("--socket names a server to attach to and --embed says to be one; pass one or the other")
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
			// A socket that is there and will not talk is not the same as an absent one:
			// answering that by starting a second server over the same store would split the
			// sessions between two processes rather than report the failure.
			return nil, code, err
		}
	}
	return embed(ctx, build, o, name, asker)
}

// attach connects to a server this process did not start and greets it. Everything it opened
// is closed again on any failure, since a caller that got an error will never call the Close
// it did not receive.
func attach(o BuildOptions, socket string, timeout time.Duration, name string, asker bool) (*dialed, int, error) {
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
	hello, err := greet(client, name, Version(), asker)
	if err != nil {
		closeClient()
		return nil, 1, err
	}
	paths, cfg, err := localConfig(o)
	if err != nil {
		closeClient()
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
