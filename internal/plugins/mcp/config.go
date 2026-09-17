// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mcp is the plugin that turns every server in mcp.toml into tools. One plugin
// serves every configured server: a server is a source of tools, not a plugin of its own,
// so nothing about it reaches the kernel except the tools it registers.
package mcp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/guygrigsby/rudy/internal/workspace"
)

// Transport is how the client reaches a server. The set is closed; mcp.toml naming anything
// else is refused rather than guessed at.
type Transport string

const (
	TransportStdio Transport = "stdio"
	TransportHTTP  Transport = "http"
)

// The two scopes an entry can come from. A project entry replaces a user entry of the same
// name, which is what lets a repository pin the server its own tooling needs.
const (
	ScopeUser    = "user"
	ScopeProject = "project"
)

// ServerConfig is one [servers.<name>] table. Name is the table key, not a field of it, so
// it is filled on read and never written. Values in Env and Headers may be secret references
// resolved like providers.<name>.auth ("env:NAME" or "cache:KEY") or plain literals, which
// are used as written.
type ServerConfig struct {
	Name      string            `toml:"-"`
	Transport Transport         `toml:"transport"`
	Command   string            `toml:"command,omitempty"`
	Args      []string          `toml:"args,omitempty"`
	Env       map[string]string `toml:"env,omitempty"`
	URL       string            `toml:"url,omitempty"`
	Headers   map[string]string `toml:"headers,omitempty"`
}

// File is one mcp.toml. The harness never writes config.toml; this file is the one rudy mcp
// owns, and it is written whole.
type File struct {
	Servers map[string]ServerConfig `toml:"servers"`
}

// ReadFile parses path. A missing file is an empty File and no error: nothing has been added
// yet, which is not a failure.
func ReadFile(path string) (File, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return File{Servers: map[string]ServerConfig{}}, nil
	}
	if err != nil {
		return File{}, fmt.Errorf("mcp: %w", err)
	}
	var f File
	if err := toml.Unmarshal(b, &f); err != nil {
		return File{}, fmt.Errorf("mcp: %s: %w", path, err)
	}
	if f.Servers == nil {
		f.Servers = map[string]ServerConfig{}
	}
	for name, c := range f.Servers {
		c.Name = name
		f.Servers[name] = c
	}
	return f, nil
}

// WriteFile replaces path with f, through a temp file in the same directory and a rename, so
// a reader sees either the old file or the new one.
func WriteFile(path string, f File) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mcp: %w", err)
	}
	b, err := toml.Marshal(f)
	if err != nil {
		return fmt.Errorf("mcp: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".mcp-*.toml")
	if err != nil {
		return fmt.Errorf("mcp: %w", err)
	}
	discard := func(err error) error {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("mcp: %s: %w", path, err)
	}
	// 0600: the values are secret references, and a reference names where a secret lives.
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return discard(err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return discard(err)
	}
	// Renaming over a file whose bytes are still only in the page cache is a file that can
	// come back empty after a crash, and this one is the operator's own record.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return discard(err)
	}
	if err := tmp.Close(); err != nil {
		return discard(err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return discard(err)
	}
	return nil
}

// Paths are the two files the plugin reads and rudy mcp writes: the user scope next to
// config.toml, and the project scope in the workspace cwd sits in. The project path is ""
// when cwd is empty or has no workspace, which means the user scope is all there is.
func Paths(configDir, cwd string) (user, project string) {
	user = filepath.Join(configDir, "mcp.toml")
	if cwd == "" {
		return user, ""
	}
	ws, err := workspace.Detect(cwd)
	if err != nil {
		return user, ""
	}
	return user, filepath.Join(ws.Root, ".rudy", "mcp.toml")
}

// Merged reads both files and returns every configured server with the scope it came from,
// user entries first by name and then the project-only ones by name. A project entry
// replaces the user entry of the same name in place, so the order a caller renders is
// stable across the replacement.
func Merged(userPath, projectPath string) ([]ServerConfig, []string, error) {
	user, err := ReadFile(userPath)
	if err != nil {
		return nil, nil, err
	}
	project := File{Servers: map[string]ServerConfig{}}
	if projectPath != "" {
		if project, err = ReadFile(projectPath); err != nil {
			return nil, nil, err
		}
	}
	names := sortedNames(user.Servers)
	for _, n := range sortedNames(project.Servers) {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	configs := make([]ServerConfig, 0, len(names))
	scopes := make([]string, 0, len(names))
	for _, n := range names {
		if c, ok := project.Servers[n]; ok {
			configs = append(configs, c)
			scopes = append(scopes, ScopeProject)
			continue
		}
		configs = append(configs, user.Servers[n])
		scopes = append(scopes, ScopeUser)
	}
	return configs, scopes, nil
}

func sortedNames(servers map[string]ServerConfig) []string {
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// nameRe is the character set a tool name may use. Every provider validates the tool names a
// request carries, so a server whose name is not in it makes every request fail, not just its
// own calls.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// validName refuses a server or tool name that cannot be half of mcp__<server>__<tool>: one
// outside the tool-name character set, or one carrying "__" itself, which would leave
// mcp__a__b__c ambiguous between server a's b__c and server a__b's c.
func validName(kind, name string) error {
	switch {
	case name == "":
		return fmt.Errorf("mcp: %s name is empty", kind)
	case strings.Contains(name, "__"):
		return fmt.Errorf("mcp: %s name %q contains __, which would make its tool name ambiguous", kind, name)
	case !nameRe.MatchString(name):
		return fmt.Errorf("mcp: %s name %q is not letters, digits, dashes and underscores", kind, name)
	}
	return nil
}

// Validate refuses an entry carrying anything but the fields of its transport. A field that
// belongs to the other transport is a mistake worth naming at write time, not one to
// discover as a server that silently ignores its environment.
func (c ServerConfig) Validate() error {
	if err := validName("server", c.Name); err != nil {
		return err
	}
	switch c.Transport {
	case TransportStdio:
		return errors.Join(
			required(c.Name, "command", c.Command != ""),
			refused(c.Name, "url", c.URL != "", TransportStdio),
			refused(c.Name, "headers", len(c.Headers) > 0, TransportStdio),
		)
	case TransportHTTP:
		return errors.Join(
			required(c.Name, "url", c.URL != ""),
			refused(c.Name, "command", c.Command != "", TransportHTTP),
			refused(c.Name, "args", len(c.Args) > 0, TransportHTTP),
			refused(c.Name, "env", len(c.Env) > 0, TransportHTTP),
		)
	default:
		return fmt.Errorf("mcp: server %s: transport %q is not stdio or http", c.Name, c.Transport)
	}
}

func required(name, field string, present bool) error {
	if present {
		return nil
	}
	return fmt.Errorf("mcp: server %s: %s is required", name, field)
}

func refused(name, field string, present bool, t Transport) error {
	if !present {
		return nil
	}
	return fmt.Errorf("mcp: server %s: %s is not a field of transport %s", name, field, t)
}
