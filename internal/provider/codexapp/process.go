// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

const (
	diagnosticBytes = 4 << 10
	stopGrace       = 3 * time.Second
)

var versionPattern = regexp.MustCompile(`^codex-cli (\d+)\.(\d+)\.(\d+)$`)

var codexEnvironmentKeys = [...]string{
	"CODEX_HOME",
	"HOME",
	"PATH",
	"SHELL",
	"TMPDIR",
	"TMP",
	"TEMP",
	"LANG",
	"LC_ALL",
	"LC_CTYPE",
	"SSL_CERT_FILE",
	"SSL_CERT_DIR",
}

type Command struct {
	Path      string
	Env       []string
	CodexHome string
}

type appProcess struct {
	cmd      *exec.Cmd
	peer     *peer
	stderr   *diagnosticTail
	waitDone chan error
}

func startProcess(ctx context.Context, command Command, onRequest inboundHandler, onNotification notificationHandler) (*appProcess, error) {
	if err := prepareCodexHome(command.CodexHome, command.Env); err != nil {
		return nil, err
	}
	if command.CodexHome != "" {
		resolved, err := filepath.EvalSymlinks(command.CodexHome)
		if err != nil {
			return nil, fmt.Errorf("resolve isolated codex home: %w", err)
		}
		command.CodexHome = resolved
	}
	path := command.Path
	if path == "" {
		path = "codex"
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return nil, fmt.Errorf("find codex: %w", err)
	}
	env := processEnvironment(command.Env, command.CodexHome)
	if err := checkVersion(ctx, resolved, env); err != nil {
		return nil, err
	}

	cmd := exec.Command(resolved, "app-server", "--stdio")
	cmd.Env = env
	if command.CodexHome != "" {
		cmd.Dir = command.CodexHome
	}
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

func prepareCodexHome(home string, overrides []string) error {
	if home == "" {
		return nil
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("create isolated codex home: %w", err)
	}
	info, err := os.Lstat(home)
	if err != nil {
		return fmt.Errorf("inspect isolated codex home: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("isolated codex home is not a directory")
	}
	if err := os.Chmod(home, 0o700); err != nil {
		return fmt.Errorf("protect isolated codex home: %w", err)
	}
	expected, err := isolatedCodexConfig(Command{CodexHome: home, Env: overrides})
	if err != nil {
		return err
	}
	configPath := filepath.Join(home, "config.toml")
	configInfo, err := os.Lstat(configPath)
	if errors.Is(err, os.ErrNotExist) {
		config, createErr := os.OpenFile(configPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			return fmt.Errorf("create isolated codex config: %w", createErr)
		}
		if _, writeErr := config.Write(expected); writeErr != nil {
			_ = config.Close()
			return fmt.Errorf("write isolated codex config: %w", writeErr)
		}
		if syncErr := config.Sync(); syncErr != nil {
			_ = config.Close()
			return fmt.Errorf("sync isolated codex config: %w", syncErr)
		}
		if closeErr := config.Close(); closeErr != nil {
			return fmt.Errorf("close isolated codex config: %w", closeErr)
		}
		configInfo, err = os.Lstat(configPath)
	}
	if err != nil {
		return fmt.Errorf("inspect isolated codex config: %w", err)
	}
	if !configInfo.Mode().IsRegular() || configInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("isolated codex home contains an unexpected authority file \"config.toml\"")
	}
	body, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read isolated codex config: %w", err)
	}
	if !bytes.Equal(body, expected) {
		return errors.New("isolated codex home contains an unexpected authority file \"config.toml\"")
	}
	if err := os.Chmod(configPath, 0o600); err != nil {
		return fmt.Errorf("protect isolated codex config: %w", err)
	}
	if _, err := os.Lstat(filepath.Join(home, "rules")); err == nil {
		return errors.New("isolated codex home contains an unexpected authority file \"rules\"")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect isolated codex home authority file \"rules\": %w", err)
	}
	return nil
}

func isolatedCodexConfig(command Command) ([]byte, error) {
	type profile struct {
		Description string            `toml:"description"`
		Filesystem  map[string]string `toml:"filesystem"`
		Network     struct {
			Enabled bool `toml:"enabled"`
		} `toml:"network"`
	}
	filesystem := map[string]string{
		":minimal":       "read",
		":project_roots": "read",
	}
	for _, path := range protectedCodexPaths(command) {
		filesystem[path] = "deny"
	}
	config := struct {
		Permissions map[string]profile `toml:"permissions"`
	}{Permissions: map[string]profile{
		"rudy_strict": {
			Description: "Rudy strict mode: workspace read-only with Codex account homes denied",
			Filesystem:  filesystem,
		},
	}}
	body, err := toml.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("encode isolated codex config: %w", err)
	}
	return body, nil
}

func protectedCodexPaths(command Command) []string {
	paths := []string{command.CodexHome}
	if external := effectiveEnvironmentValue("CODEX_HOME", command.Env); external != "" {
		paths = append(paths, external)
	}
	if home := effectiveEnvironmentValue("HOME", command.Env); home != "" {
		paths = append(paths, filepath.Join(home, ".codex"))
	}
	seen := make(map[string]struct{}, len(paths)*2)
	protected := make([]string, 0, len(paths)*2)
	for _, path := range paths {
		if path == "" {
			continue
		}
		for _, candidate := range []string{filepath.Clean(path), resolvedPath(path)} {
			if _, ok := seen[candidate]; ok {
				continue
			}
			seen[candidate] = struct{}{}
			protected = append(protected, candidate)
		}
	}
	return protected
}

func effectiveEnvironmentValue(name string, overrides []string) string {
	value := os.Getenv(name)
	for _, entry := range overrides {
		key, candidate, ok := strings.Cut(entry, "=")
		if ok && key == name {
			value = candidate
		}
	}
	return value
}

func checkVersion(ctx context.Context, path string, env []string) error {
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = env
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
	reviewed := [3]int{0, 155, 1}
	if version != reviewed {
		return fmt.Errorf("rudy requires codex-cli 0.155.1, found %d.%d.%d", version[0], version[1], version[2])
	}
	return nil
}

func processEnvironment(overrides []string, codexHome string) []string {
	env := make([]string, 0, len(codexEnvironmentKeys)+len(overrides))
	positions := make(map[string]int, len(codexEnvironmentKeys)+len(overrides))
	for _, name := range codexEnvironmentKeys {
		if value, ok := os.LookupEnv(name); ok {
			positions[name] = len(env)
			env = append(env, name+"="+value)
		}
	}
	for _, entry := range overrides {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			env = append(env, entry)
			continue
		}
		if position, ok := positions[name]; ok {
			env[position] = entry
			continue
		}
		positions[name] = len(env)
		env = append(env, entry)
	}
	if codexHome != "" {
		entry := "CODEX_HOME=" + codexHome
		if position, ok := positions["CODEX_HOME"]; ok {
			env[position] = entry
		} else {
			env = append(env, entry)
		}
	}
	return env
}

func captureDiagnostics(reader io.ReadCloser, tail *diagnosticTail) {
	defer func() { _ = reader.Close() }()
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
