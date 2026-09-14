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
// its PATH. dialSSH answers it by installing one and dialing again.
var errNoRudyOnHost = errors.New("rudy is not on the host's PATH over ssh")

// hostInstallBudget bounds the build on the box. A make install is a go build, which on a
// cold module cache and a box that is also compiling something else is minutes rather than
// seconds; the bound is here so a box that has stopped talking mid-build does not hold a
// client forever, not to hurry the compiler.
var hostInstallBudget = 15 * time.Minute

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

// dialSSH reaches the kernel on host, installing rudy there when the box has none.
//
// Exit 111 is the only thing that starts an install, and it starts exactly one: the box said
// it has no rudy, which is a fact and not a guess, and the answer to it is the same whether
// the box is new or somebody moved the binary. A daemon already serving an older version is
// not this: it is a process holding sessions, and ADR 0029 makes that a notice rather than a
// restart, which dialSSHOnce prints after the hello.
//
// The retry is once. A second 111 means the install ran and put rudy somewhere the remote
// line's PATH does not look, or built nothing at all, and dialing again would just be slower
// about saying so; the build's own output is what answers that, so it comes back with the
// error.
func dialSSH(o BuildOptions, host hosts.Host, d dialOptions, name string, asker bool) (*dialed, int, error) {
	dialed, code, err := dialSSHOnce(o, host, d, name, asker)
	if !errors.Is(err, errNoRudyOnHost) {
		return dialed, code, err
	}
	build, ierr := installOnHost(o, host)
	if ierr != nil {
		// Still errNoRudyOnHost: the box has no rudy whether the install could not start or
		// could not finish, and a caller telling that apart from a refused connection is the
		// reason the sentinel exists.
		return nil, 1, fmt.Errorf("%w on %s: %w", errNoRudyOnHost, host, ierr)
	}
	dialed, code, err = dialSSHOnce(o, host, d, name, asker)
	if errors.Is(err, errNoRudyOnHost) {
		return nil, 1, fmt.Errorf("installed rudy on %s and the remote line still cannot find it on $HOME/.local/bin, $HOME/bin or $HOME/go/bin; the install said:\n%s", host, build)
	}
	return dialed, code, err
}

// installOnHost builds this binary's revision on the box and says so. It returns what the
// build printed, for the caller that has to report a box which still has no rudy afterwards.
//
// A version that names no commit stops here rather than at the box: nothing can be checked
// out from "dev" or from a dirty tree, and the operator needs to hear that about the binary in
// their hand rather than watch a build fail on a machine they are not looking at.
func installOnHost(o BuildOptions, host hosts.Host) (string, error) {
	_, cfg, err := localConfig(o)
	if err != nil {
		return "", err
	}
	rev, err := hosts.Revision(Version())
	if err != nil {
		return "", err
	}
	stderr := stderrOf(o)
	// A context of this call's own, for the reason the greet has one: an interrupt that
	// arrived before the client got going must not report a cancelled build.
	ctx, done := context.WithTimeout(context.Background(), hostInstallBudget)
	defer done()
	tail := protocol.NewTail(protocol.TailBytes)
	_, _ = fmt.Fprintf(stderr, "rudy: %s has no rudy; building %s there from %s\n", host, rev, cfg.Remote.Source)
	if err := hosts.Install(ctx, hosts.SSHRunner(host), cfg.Remote.Source, rev, io.MultiWriter(stderr, tail)); err != nil {
		return tail.String(), err
	}
	_, _ = fmt.Fprintf(stderr, "rudy: installed rudy %s on %s\n", rev, host)
	return tail.String(), nil
}

// dialSSHOnce runs the remote line on host through ssh and greets the bridge. Exit 111 before
// the hello is errNoRudyOnHost; any other exit surfaces ssh's stderr, which is the only
// thing that says why.
func dialSSHOnce(o BuildOptions, host hosts.Host, d dialOptions, name string, asker bool) (*dialed, int, error) {
	paths, cfg, err := localConfig(o)
	if err != nil {
		return nil, 1, err
	}
	// The host after --, and the remote line as one argument: ssh joins its command words
	// with spaces and hands them to the box's shell, so a line that is already one word is
	// the line the shell runs.
	cmd := exec.Command(hosts.SSHBin(), "--", host.String(), hosts.RemoteLine())
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
			return nil, 1, fmt.Errorf("%w on %s", errNoRudyOnHost, host)
		}
		if text := strings.TrimSpace(stderr.String()); text != "" {
			return nil, 1, fmt.Errorf("ssh %s: %s", host, text)
		}
		return nil, 1, fmt.Errorf("ssh %s: %w", host, err)
	}
	// A daemon of another version is a notice and never a restart: it may be holding somebody
	// else's sessions, and a turn survives its client leaving precisely so that this client is
	// not the one that decides. The operator gets the two versions and the command that swaps
	// them when they know nothing is live.
	if hello.Version != Version() {
		_, _ = fmt.Fprintf(stderrOf(o), "rudy: host runs rudy %s, this is %s; rudy hosts install %s --force restarts it at this version\n", hello.Version, Version(), host)
	}
	slog.Info("host: dial", "host", host.String(), "home", hello.Home)
	return &dialed{
		Client:     client,
		Paths:      paths,
		Config:     cfg,
		Version:    hello.Version,
		InstanceID: hello.InstanceID,
		Close:      func() { _ = client.Close() },
		Host:       host,
		Home:       hello.Home,
		Cwd:        d.Cwd,
		NoSync:     d.NoSync,
	}, 0, nil
}
