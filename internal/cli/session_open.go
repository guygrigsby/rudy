package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/server"
	"github.com/guygrigsby/rudy/internal/session"
)

// serveInMemory starts b's server on an in-memory pipe and greets it as client. Both
// clients this binary has, --print and the TUI, reach the server this way: there is one
// server implementation and one protocol, and a command that skipped the wire would be a
// second, untested path into the kernel.
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
	// A background context on purpose: the hello has to be answered even when the caller's
	// context is already the one an interrupt will cancel.
	var hello protocol.ClientHelloResult
	if err := client.Call(context.Background(), protocol.MethodClientHello,
		protocol.ClientHelloParams{Client: name, Version: b.Version, Asker: asker}, &hello); err != nil {
		closeConn()
		return nil, nil, fmt.Errorf("hello: %w", err)
	}
	return client, closeConn, nil
}

// openOrResume opens a new session or resumes one named by --resume or --continue and applies
// the --model, --mode and --thinking flags to it. source is how the caller named the session
// it is asking for ("--resume", "sessions resume"), so a bad id is reported against the thing
// the operator actually typed.
func openOrResume(ctx context.Context, client *protocol.Client, b *Built, o printOptions, cwd, source string) (protocol.SessionInfo, int, error) {
	var info protocol.SessionInfo
	id := o.Resume
	if o.Continue && id == "" {
		newest, err := newestFor(b.Store, cwd)
		if err != nil {
			return info, 2, err
		}
		id = newest
	}
	if id == "" {
		err := client.Call(ctx, protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: cwd, Model: o.Model, Mode: o.Mode, Thinking: o.Thinking}, &info)
		if err != nil {
			return info, 1, fmt.Errorf("open session: %w", err)
		}
		return info, 0, nil
	}
	if _, err := ulid.Parse(id); err != nil {
		return info, 2, fmt.Errorf("%s %q is not a session id", source, id)
	}
	if err := client.Call(ctx, protocol.MethodSessionResume, protocol.SessionResumeParams{SessionID: id}, &info); err != nil {
		return info, 1, fmt.Errorf("resume %s: %w", id, err)
	}
	// The set_* methods answer server.EntryIDResult, not SessionInfo, so info is updated here
	// from the same values the server accepted.
	var set server.EntryIDResult
	if o.Model != "" {
		if err := client.Call(ctx, protocol.MethodSessionSetModel, protocol.SessionSetModelParams{SessionID: id, Model: o.Model}, &set); err != nil {
			return info, 1, err
		}
		m, err := b.Registry.Resolve(o.Model)
		if err != nil {
			return info, 1, err
		}
		info.Model = m.Ref
	}
	if o.Mode != "" {
		if err := client.Call(ctx, protocol.MethodSessionSetMode, protocol.SessionSetModeParams{SessionID: id, Mode: session.Mode(o.Mode)}, &set); err != nil {
			return info, 1, err
		}
		info.Mode = session.Mode(o.Mode)
	}
	if o.Thinking != "" {
		if err := client.Call(ctx, protocol.MethodSessionSetThinking, protocol.SessionSetThinkingParams{SessionID: id, Thinking: session.ThinkingLevel(o.Thinking)}, &set); err != nil {
			return info, 1, err
		}
		info.Thinking = session.ThinkingLevel(o.Thinking)
	}
	return info, 0, nil
}

// newestFor returns the newest session opened on cwd, skipping child sessions: a subagent's
// session sits on the same workspace and is newer than the session that opened it, so
// --continue after any agent tool call would otherwise resume the subagent instead of the
// user's own session.
func newestFor(st *session.Store, cwd string) (string, error) {
	want, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		want = cwd
	}
	list, err := st.List()
	if err != nil {
		return "", err
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
