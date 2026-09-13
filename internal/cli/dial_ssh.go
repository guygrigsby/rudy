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
	cmd    *exec.Cmd
	stdin  io.Closer
	waited chan error // cmd.Wait's result, sent once
	once   sync.Once
}

// Close hangs up and waits. Shutting stdin is what the bridge reads as its client leaving,
// so ssh exits by itself with the status the remote line produced; only a process still
// there after the grace is killed, and a killed process reports a signal instead of the 111
// that says the box has no rudy.
func (p *sshProc) Close() error {
	p.once.Do(func() {
		_ = p.stdin.Close()
		select {
		case <-p.waited:
		case <-time.After(sshExitGrace):
			_ = p.cmd.Process.Kill()
			<-p.waited
		}
	})
	return nil
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
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, 1, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 1, err
	}
	var stderr limitedBuffer // the tail of ssh's stderr, for the error below
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, 1, fmt.Errorf("%s: %w", sshBin(), err)
	}
	proc := &sshProc{cmd: cmd, stdin: stdin, waited: make(chan error, 1)}
	go func() { proc.waited <- cmd.Wait() }()
	client := protocol.NewClient(protocol.NewStreamConn(stdout, stdin, proc))
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
		if text := stderr.String(); text != "" {
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

// stderrTailBytes is how much of ssh's stderr is kept. Enough for a host key warning and the
// reason underneath it, and bounded because the other end of that pipe is a machine that may
// decide to talk for as long as it likes.
const stderrTailBytes = 4 << 10

// limitedBuffer keeps the last stderrTailBytes written to it and drops the rest. ssh writes
// its diagnostics last, so the tail is the half that says what went wrong.
type limitedBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > stderrTailBytes {
		b.buf = b.buf[len(b.buf)-stderrTailBytes:]
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}
