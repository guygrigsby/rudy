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
	"strconv"
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

// frame is the canonical encoding a declaration digest is taken over. Every field is
// length-prefixed, so no value can run into its neighbour: a string is "s<len>:<bytes>", a
// list is "l<count>:" and its strings, a map is "m<count>:" and its key and value strings
// in key order. Values are exactly as written.
type frame struct{ b []byte }

func (f *frame) str(s string) *frame {
	f.b = append(f.b, 's')
	f.b = strconv.AppendInt(f.b, int64(len(s)), 10)
	f.b = append(f.b, ':')
	f.b = append(f.b, s...)
	return f
}

func (f *frame) list(items []string) *frame {
	f.b = append(f.b, 'l')
	f.b = strconv.AppendInt(f.b, int64(len(items)), 10)
	f.b = append(f.b, ':')
	for _, it := range items {
		f.str(it)
	}
	return f
}

func (f *frame) kv(m map[string]string) *frame {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	f.b = append(f.b, 'm')
	f.b = strconv.AppendInt(f.b, int64(len(keys)), 10)
	f.b = append(f.b, ':')
	for _, k := range keys {
		f.str(k).str(m[k])
	}
	return f
}

func (f *frame) hash() string { return hashHex(f.b) }

// PluginDeclaration covers everything a manifest decides about what runs: command, args,
// build, env and, when the command resolves to an executable inside the plugin's own
// directory, that file's bytes. The resolution mirrors plugin.Spawned: a command that is not an
// absolute path is looked for next to the manifest, so a checkout that swaps the binary changes the
// digest even though the manifest is byte-identical.
func PluginDeclaration(m plugin.Manifest) Declaration {
	f := new(frame).str(m.Name).str(m.Version).str(m.Command).list(m.Args).str(m.Build).kv(m.Env).str(executableLine(m))
	summary := "runs: " + m.Command + " " + strings.Join(m.Args, " ")
	if m.Build != "" {
		summary += "; build: " + m.Build
	}
	return Declaration{
		Kind:    KindPlugin,
		Name:    m.Name,
		Digest:  f.hash(),
		Summary: summary,
	}
}

// executableLine is the digest line for the file the command resolves to in m.Dir.
func executableLine(m plugin.Manifest) string {
	local, ok := plugin.LocalExecutable(m)
	if !ok {
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
	f := new(frame).str(c.Name).str(string(c.Transport)).str(c.Command).list(c.Args).kv(c.Env).str(c.URL).kv(c.Headers)
	summary := "stdio: " + c.Command + " " + strings.Join(c.Args, " ")
	if c.Transport == mcp.TransportHTTP {
		summary = "http: " + c.URL
	}
	return Declaration{
		Kind:    KindMCP,
		Name:    c.Name,
		Digest:  f.hash(),
		Summary: summary,
	}
}

// AgentDeclaration is an agent definition by its bytes: it replaces the system prompt, so
// any edit is a change.
func AgentDeclaration(name string, body []byte) Declaration {
	return Declaration{Kind: KindAgent, Name: name, Digest: new(frame).str(string(body)).hash(), Summary: "agent " + name}
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
