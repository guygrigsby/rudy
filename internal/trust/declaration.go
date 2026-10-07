// SPDX-License-Identifier: AGPL-3.0-or-later

// Package trust owns the record of which workspaces the operator let run what they ship.
// A workspace's .rudy holds three kinds of declaration that run code or speak for rudy:
// plugins, MCP servers and agent definitions. One digest covers all three, so a clone that
// swaps any of them asks again (ADR 0050).
package trust

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/mcp"
)

// Kind is what sort of thing a workspace declares.
type Kind string

const (
	KindPlugin Kind = "plugin"
	KindMCP    Kind = "mcp"
	KindAgent  Kind = "agent"
)

// Declaration is one thing a workspace ships under .rudy that asks rudy to run code or to
// speak for it. Digest is hex sha256 of what it would do; Summary is one line for the
// question the operator answers and is not part of the digest.
type Declaration struct {
	Kind    Kind
	Name    string
	Digest  string
	Summary string
}

// String is "kind:name", the form the trust record lists.
func (d Declaration) String() string { return string(d.Kind) + ":" + d.Name }

// Digest is the workspace digest: sha256 over the sorted "kind\x00name\x00digest" lines
// joined by "\n". Order of decls does not matter.
func Digest(decls []Declaration) string {
	lines := make([]string, 0, len(decls))
	for _, d := range decls {
		lines = append(lines, strings.Join([]string{string(d.Kind), d.Name, d.Digest}, "\x00"))
	}
	sort.Strings(lines)
	return hashHex([]byte(strings.Join(lines, "\n")))
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// kvLines is a map as sorted "k=v" lines, values exactly as written.
func kvLines(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// PluginDeclaration covers everything a manifest decides about what runs: command, args,
// build, env and, when the command resolves to an executable inside the plugin's own
// directory, that file's bytes. The resolution mirrors plugin.Spawned: only a bare command
// name is looked for next to the manifest, so a checkout that swaps the binary changes the
// digest even though the manifest is byte-identical.
func PluginDeclaration(m plugin.Manifest) Declaration {
	lines := []string{m.Name, m.Version, m.Command, strings.Join(m.Args, " "), m.Build}
	lines = append(lines, kvLines(m.Env)...)
	lines = append(lines, executableLine(m))
	summary := "runs: " + m.Command + " " + strings.Join(m.Args, " ")
	if m.Build != "" {
		summary += "; build: " + m.Build
	}
	return Declaration{
		Kind:    KindPlugin,
		Name:    m.Name,
		Digest:  hashHex([]byte(strings.Join(lines, "\x00"))),
		Summary: summary,
	}
}

// executableLine is the digest line for the file a bare command resolves to in m.Dir.
func executableLine(m plugin.Manifest) string {
	if m.Dir == "" || filepath.IsAbs(m.Command) || filepath.Base(m.Command) != m.Command {
		return "exec:none"
	}
	local := filepath.Join(m.Dir, m.Command)
	fi, err := os.Stat(local)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0 {
		return "exec:none"
	}
	b, err := os.ReadFile(local)
	if err != nil {
		return "exec:unreadable"
	}
	return "exec:" + hashHex(b)
}

// MCPDeclaration covers every field of a server entry. Env and header values are digested
// as written: a cache:KEY reference stays the reference, so the digest never holds a secret
// and does not move when the secret behind it rotates.
func MCPDeclaration(c mcp.ServerConfig) Declaration {
	lines := []string{c.Name, string(c.Transport), c.Command, strings.Join(c.Args, " ")}
	lines = append(lines, kvLines(c.Env)...)
	lines = append(lines, c.URL)
	lines = append(lines, kvLines(c.Headers)...)
	summary := "stdio: " + c.Command + " " + strings.Join(c.Args, " ")
	if c.Transport == mcp.TransportHTTP {
		summary = "http: " + c.URL
	}
	return Declaration{
		Kind:    KindMCP,
		Name:    c.Name,
		Digest:  hashHex([]byte(strings.Join(lines, "\x00"))),
		Summary: summary,
	}
}

// AgentDeclaration is an agent definition by its bytes: it replaces the system prompt, so
// any edit is a change.
func AgentDeclaration(name string, body []byte) Declaration {
	return Declaration{Kind: KindAgent, Name: name, Digest: hashHex(body), Summary: "agent " + name}
}

// Declarations is everything root ships: manifests under root/.rudy, servers in
// root/.rudy/mcp.toml and every root/.rudy/agents/*.md as bytes. A missing mcp.toml is none;
// one that will not parse is one error and no MCP declarations. An agent file that will not
// parse is still a declaration, because its bytes are what changed. Sorted by kind then name.
func Declarations(root string) ([]Declaration, []error) {
	rudy := filepath.Join(root, ".rudy")
	var out []Declaration
	manifests, errs := plugin.Discover([]string{rudy})
	for _, m := range manifests {
		out = append(out, PluginDeclaration(m))
	}
	f, err := mcp.ReadFile(filepath.Join(rudy, "mcp.toml"))
	if err != nil {
		errs = append(errs, err)
	}
	for _, c := range f.Servers {
		out = append(out, MCPDeclaration(c))
	}
	agents, agentErrs := agentDeclarations(filepath.Join(rudy, "agents"))
	out = append(out, agents...)
	errs = append(errs, agentErrs...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out, errs
}

func agentDeclarations(dir string) ([]Declaration, []error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{fmt.Errorf("trust: %w", err)}
	}
	var out []Declaration
	var errs []error
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".md")
		if !ok || e.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			errs = append(errs, fmt.Errorf("trust: %w", err))
			continue
		}
		out = append(out, AgentDeclaration(name, body))
	}
	return out, errs
}
