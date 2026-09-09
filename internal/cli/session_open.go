package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/server"
	"github.com/guygrigsby/rudy/internal/session"
)

// serveInMemory starts b's server on an in-memory pipe and greets it as client. It is the
// embed branch of dial: a client with no daemon to attach to is its own server, over the
// same protocol and the same Serve loop the socket carries, because a command that skipped
// the wire would be a second, untested path into the kernel.
//
// The returned function unwinds the pair and must be called even when the caller is about
// to fail: closing the client is what ends the Serve loop, and that loop is what detaches
// the session and closes it, releasing the store's flock, so nothing this process started
// is still holding the session when the next command opens it.
func serveInMemory(b *Built, name string, asker bool) (*protocol.Client, func(), error) {
	srvCtx, cancelSrv := context.WithCancel(context.Background())
	clientConn, serverConn := protocol.Pipe()
	served := make(chan struct{})
	go func() { defer close(served); _ = b.Server.Serve(srvCtx, serverConn) }()
	client := protocol.NewClient(clientConn)
	closeConn := func() {
		_ = client.Close()
		<-served
		cancelSrv()
	}
	// A background context and no timeout: the server is in this process, on a pipe with no
	// backlog to sit in, and a hello it does not answer is a deadlock a deadline would only
	// rename. attach bounds its own, where the server is somebody else.
	if _, err := greet(context.Background(), client, name, b.Version, asker); err != nil {
		closeConn()
		return nil, nil, err
	}
	return client, closeConn, nil
}

// greet is the first request on every connection this binary opens, in-memory or over a
// socket. ctx is never the caller's: the hello has to be answered even when the caller's
// context is already the one an interrupt will cancel, so what a caller passes is a deadline
// of its own or nothing at all.
func greet(ctx context.Context, client *protocol.Client, name, version string, asker bool) (protocol.ClientHelloResult, error) {
	var hello protocol.ClientHelloResult
	err := client.Call(ctx, protocol.MethodClientHello,
		protocol.ClientHelloParams{Client: name, Version: version, Asker: asker}, &hello)
	if err != nil {
		return hello, fmt.Errorf("hello: %w", err)
	}
	return hello, nil
}

// openOrResume opens a new session or resumes one named by --resume or --continue and applies
// the --model, --mode and --thinking flags to it. source is how the caller named the session
// it is asking for ("--resume", "sessions resume"), so a bad id is reported against the thing
// the operator actually typed.
//
// Everything it needs beyond the connection comes off d, which answers from this process's
// store and registry when there is one and over the protocol when the server is another
// process: an attached client resolves the same session and the same model as an embedded
// one, from the same values, without a store or a registry of its own.
func openOrResume(ctx context.Context, d *dialed, o printOptions, cwd, source string) (protocol.SessionInfo, int, error) {
	var info protocol.SessionInfo
	id := o.Resume
	if o.Continue && id == "" {
		list, err := d.sessions(ctx)
		if err != nil {
			return info, 2, err
		}
		newest, err := newestIn(list, cwd)
		if err != nil {
			return info, 2, err
		}
		id = newest
	}
	if id == "" {
		err := d.Client.Call(ctx, protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: cwd, Model: o.Model, Mode: o.Mode, Thinking: o.Thinking}, &info)
		if err != nil {
			return info, 1, fmt.Errorf("open session: %w", err)
		}
		return info, 0, nil
	}
	if _, err := ulid.Parse(id); err != nil {
		return info, 2, fmt.Errorf("%s %q is not a session id", source, id)
	}
	if err := d.Client.Call(ctx, protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: id}, &info); err != nil {
		if hint := heldElsewhere(id, err); hint != nil {
			return info, 1, hint
		}
		return info, 1, fmt.Errorf("resume %s: %w", id, err)
	}
	// The set_* methods answer server.EntryIDResult, not SessionInfo, so info is updated here
	// from the same values the server accepted.
	var set server.EntryIDResult
	if o.Model != "" {
		if err := d.Client.Call(ctx, protocol.MethodSessionSetModel, protocol.SessionSetModelParams{SessionID: id, Model: o.Model}, &set); err != nil {
			return info, 1, err
		}
		m, err := d.resolve(ctx, o.Model)
		if err != nil {
			return info, 1, err
		}
		info.Model = m.Ref
	}
	if o.Mode != "" {
		if err := d.Client.Call(ctx, protocol.MethodSessionSetMode, protocol.SessionSetModeParams{SessionID: id, Mode: session.Mode(o.Mode)}, &set); err != nil {
			return info, 1, err
		}
		info.Mode = session.Mode(o.Mode)
	}
	if o.Thinking != "" {
		if err := d.Client.Call(ctx, protocol.MethodSessionSetThinking, protocol.SessionSetThinkingParams{SessionID: id, Thinking: session.ThinkingLevel(o.Thinking)}, &set); err != nil {
			return info, 1, err
		}
		info.Thinking = session.ThinkingLevel(o.Thinking)
	}
	return info, 0, nil
}

// heldElsewhere turns a session another rudy has open into the one thing an operator can act
// on: the socket that process is serving. A lock they cannot take is not the news; the way to
// reach the session through the server already holding it is. It answers nil for anything
// else, an unavailable carrying no socket included, since there is then nothing to attach to
// and the caller's own message is the better one.
func heldElsewhere(id string, err error) error {
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Code != protocol.CodeUnavailable {
		return nil
	}
	var data struct {
		Socket string `json:"socket"`
	}
	if !protocol.ErrorData(pe, &data) || data.Socket == "" {
		return nil
	}
	return fmt.Errorf("session %s is held by another process; attach with --socket %s", id, data.Socket)
}

// newestIn returns the newest session opened on cwd, skipping child sessions: a subagent's
// session sits on the same workspace and is newer than the session that opened it, so
// --continue after any agent tool call would otherwise resume the subagent instead of the
// user's own session. list is newest first, from the store or from session.list; both answer
// the same summaries, so the rule lives on the summaries rather than on either source.
func newestIn(list []session.Summary, cwd string) (string, error) {
	want, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		want = cwd
	}
	for _, s := range list {
		root, err := filepath.EvalSymlinks(s.Workspace.Root)
		if err != nil {
			root = s.Workspace.Root
		}
		if root == want && s.ParentSessionID == "" {
			return s.ID.String(), nil
		}
	}
	return "", fmt.Errorf("no session for %s; drop --continue to start one", cwd)
}
