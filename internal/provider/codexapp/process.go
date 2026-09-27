// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	diagnosticBytes = 4 << 10
	stopGrace       = 3 * time.Second
)

var versionPattern = regexp.MustCompile(`^codex-cli (\d+)\.(\d+)\.(\d+)`)

type Command struct {
	Path string
	Env  []string
}

type appProcess struct {
	cmd      *exec.Cmd
	peer     *peer
	stderr   *diagnosticTail
	waitDone chan error
}

func startProcess(ctx context.Context, command Command, onRequest inboundHandler, onNotification notificationHandler) (*appProcess, error) {
	path := command.Path
	if path == "" {
		path = "codex"
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return nil, fmt.Errorf("find codex: %w", err)
	}
	if err := checkVersion(ctx, resolved, command.Env); err != nil {
		return nil, err
	}

	cmd := exec.Command(resolved, "app-server", "--stdio")
	cmd.Env = append(os.Environ(), command.Env...)
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("codex app server stdin: %w", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = closeAll(inR, inW)
		return nil, fmt.Errorf("codex app server stdout: %w", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = closeAll(inR, inW, outR, outW)
		return nil, fmt.Errorf("codex app server stderr: %w", err)
	}
	cmd.Stdin = inR
	cmd.Stdout = outW
	cmd.Stderr = errW
	if err := cmd.Start(); err != nil {
		_ = closeAll(inR, inW, outR, outW, errR, errW)
		return nil, fmt.Errorf("start codex app server: %w", err)
	}
	_ = closeAll(inR, outW, errW)

	tail := newDiagnosticTail(diagnosticBytes)
	go captureDiagnostics(errR, tail)
	p := newPeer(outR, inW, closerFunc(func() error { return closeAll(inW, outR, errR) }), onRequest, onNotification)
	proc := &appProcess{cmd: cmd, peer: p, stderr: tail, waitDone: make(chan error, 1)}
	go func() {
		err := cmd.Wait()
		if err != nil {
			p.fail(fmt.Errorf("codex app server exited: %w: %s", err, strings.TrimSpace(tail.String())))
		} else {
			p.fail(io.EOF)
		}
		proc.waitDone <- err
	}()
	return proc, nil
}

func checkVersion(ctx context.Context, path string, env []string) error {
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("read codex version: %w", err)
	}
	match := versionPattern.FindStringSubmatch(strings.TrimSpace(string(out)))
	if match == nil {
		return fmt.Errorf("read codex version: unexpected output %q", strings.TrimSpace(string(out)))
	}
	version := [3]int{}
	for i := range version {
		version[i], err = strconv.Atoi(match[i+1])
		if err != nil {
			return fmt.Errorf("read codex version: %w", err)
		}
	}
	minimum := [3]int{0, 155, 1}
	if lessVersion(version, minimum) {
		return fmt.Errorf("rudy requires codex-cli >= 0.155.1, found %d.%d.%d", version[0], version[1], version[2])
	}
	return nil
}

func lessVersion(got, minimum [3]int) bool {
	for i := range got {
		if got[i] != minimum[i] {
			return got[i] < minimum[i]
		}
	}
	return false
}

func captureDiagnostics(reader io.ReadCloser, tail *diagnosticTail) {
	defer reader.Close()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxWireBytes)
	for scanner.Scan() {
		tail.add(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		tail.add("stderr capture: " + err.Error())
	}
}

func (p *appProcess) close() error {
	closeErr := p.peer.Close()
	select {
	case err := <-p.waitDone:
		return errors.Join(closeErr, err)
	case <-time.After(stopGrace):
	}
	killErr := p.cmd.Process.Kill()
	select {
	case err := <-p.waitDone:
		return errors.Join(closeErr, killErr, err)
	case <-time.After(stopGrace):
		return errors.Join(closeErr, killErr, errors.New("codex app server did not exit after kill"))
	}
}

func closeAll(closers ...io.Closer) error {
	var errs []error
	for _, closer := range closers {
		if err := closer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }
