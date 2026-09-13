package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/guygrigsby/rudy/internal/cli/hosts"
	"github.com/guygrigsby/rudy/internal/protocol"
)

// sshGreetTimeout bounds the hello over ssh. Longer than a socket's because the bridge on
// the far side may be starting a daemon that is loading plugins.
var sshGreetTimeout = 30 * time.Second

// sshExitGrace is how long a closing connection waits for ssh to go of its own accord once
// its stdin is shut. That is the client hanging up, which the bridge answers by exiting, and
// waiting it out is what leaves an exit status to read; a kill is the fallback for a peer
// that does not take the hint and it costs the status, so it is never the first move.
var sshExitGrace = 5 * time.Second

// errNoRudyOnHost is the remote line's exit 111: the host answered ssh and has no rudy on
// its PATH. Task 7 answers it with an install; until then the message names the command.
var errNoRudyOnHost = errors.New("rudy is not on the host's PATH over ssh")

// sshBin is the ssh to run. RUDY_SSH exists for the tests, whose shim runs the remote line
// locally; it is not a config key because nothing but a test wants it.
func sshBin() string {
	if v := os.Getenv("RUDY_SSH"); v != "" {
		return v
	}
	return "ssh"
}

// sshProc is the Closer under an ssh connection: ending it is the client detaching on the
// box. The daemon on the far side is setsid'd and outlives the bridge, so nothing here ends
// a session; what ends is this machine's half of it.
type sshProc struct {
	cmd *exec.Cmd
	// stdin and stdout are this process's ends of ssh's stdio, owned here rather than by
	// cmd: see sshTransport.
	stdin  io.Closer
	stdout io.Closer
	// exited closes once cmd.Wait has returned, which is also when ProcessState is readable.
	exited chan struct{}
	once   sync.Once
}

// Close hangs up and waits. Shutting ssh's stdin is what the bridge reads as its client
// leaving, so ssh exits by itself carrying the status the remote line produced; only a
// process still there after the grace is killed, and a killed process reports a signal
// instead of the 111 that says the box has no rudy. The read end goes last, once nothing can
// write into it any more: closed while the reader is still draining, it would turn the box's
// last frames into "file already closed".
func (p *sshProc) Close() error {
	p.once.Do(func() {
		_ = p.stdin.Close()
		select {
		case <-p.exited:
		case <-time.After(sshExitGrace):
			_ = p.cmd.Process.Kill()
			<-p.exited
		}
		_ = p.stdout.Close()
	})
	return nil
}

// sshTransport starts cmd and returns a Conn over its stdio. The pipes are explicit rather
// than StdinPipe and StdoutPipe because those are closed by Wait, which runs concurrently
// with the reader here: a box whose daemon died writes its last frames and exits, and a read
// end closed under the reader loses them and reports "file already closed" in place of the
// reason. internal/plugin/spawned.go carries the same lesson for a spawned plugin. This
// process holds both parent ends and closes them in sshProc.Close, in that order.
func sshTransport(cmd *exec.Cmd) (protocol.Conn, *sshProc, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_, _ = inR.Close(), inW.Close()
		return nil, nil, err
	}
	cmd.Stdin, cmd.Stdout = inR, outW
	if err := cmd.Start(); err != nil {
		_, _ = inR.Close(), inW.Close()
		_, _ = outR.Close(), outW.Close()
		return nil, nil, fmt.Errorf("%s: %w", cmd.Path, err)
	}
	// The child holds its own ends now. Ours would otherwise keep the pipes open past its
	// exit, and a read end nothing can write to is the EOF the reader is waiting for.
	_, _ = inR.Close(), outW.Close()
	proc := &sshProc{cmd: cmd, stdin: inW, stdout: outR, exited: make(chan struct{})}
	go func() { defer close(proc.exited); _ = cmd.Wait() }()
	return protocol.NewStreamConn(outR, inW, proc), proc, nil
}

// dialSSH runs the remote line on host through ssh and greets the bridge. Exit 111 before
// the hello is errNoRudyOnHost; any other exit surfaces ssh's stderr, which is the only
// thing that says why.
func dialSSH(o BuildOptions, host hosts.Host, d dialOptions, name string, asker bool) (*dialed, int, error) {
	paths, cfg, err := localConfig(o)
	if err != nil {
		return nil, 1, err
	}
	// The host after --, and the remote line as one argument: ssh joins its command words
	// with spaces and hands them to the box's shell, so a line that is already one word is
	// the line the shell runs.
	cmd := exec.Command(sshBin(), "--", host.String(), hosts.RemoteLine())
	stderr := protocol.NewTail(protocol.TailBytes) // for the error below; ssh's diagnostics come last
	cmd.Stderr = stderr
	conn, _, err := sshTransport(cmd)
	if err != nil {
		return nil, 1, err
	}
	client := protocol.NewClient(conn)
	// A context of this call's own, never the caller's: an interrupt that arrived before the
	// client got going would otherwise report a cancelled dial where it means an interrupt,
	// which is the rule attach follows for the same reason.
	ctx, done := context.WithTimeout(context.Background(), sshGreetTimeout)
	defer done()
	hello, err := greet(ctx, client, name, Version(), asker)
	if err != nil {
		// Closing waits for ssh, so the exit status below is the one ssh already produced.
		_ = client.Close()
		if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == hosts.ExitNoRudy {
			return nil, 1, fmt.Errorf("%w on %s; run rudy hosts install %s", errNoRudyOnHost, host, host)
		}
		if text := strings.TrimSpace(stderr.String()); text != "" {
			return nil, 1, fmt.Errorf("ssh %s: %s", host, text)
		}
		return nil, 1, fmt.Errorf("ssh %s: %w", host, err)
	}
	slog.Info("host: dial", "host", host.String(), "home", hello.Home)
	return &dialed{
		Client:  client,
		Paths:   paths,
		Config:  cfg,
		Version: hello.Version,
		Close:   func() { _ = client.Close() },
		Host:    host,
		Home:    hello.Home,
		Cwd:     d.Cwd,
		NoSync:  d.NoSync,
	}, 0, nil
}
