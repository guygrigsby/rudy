// SPDX-License-Identifier: AGPL-3.0-or-later

package trust_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/mcp"
	"github.com/guygrigsby/rudy/internal/trust"
)

// TestPluginDigestCoversEnvAndExecutable: a plugin whose binary is swapped in the checkout
// or whose environment gains a variable is not the plugin that was agreed to.
func TestPluginDigestCoversEnvAndExecutable(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "plugin")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho a\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := plugin.Manifest{Name: "p", Version: "1", Command: "plugin", Dir: dir}
	base := trust.PluginDeclaration(m).Digest
	if again := trust.PluginDeclaration(m).Digest; again != base {
		t.Fatalf("same manifest, different digest: %s %s", base, again)
	}
	withEnv := m
	withEnv.Env = map[string]string{"LD_PRELOAD": "/tmp/x.so"}
	if trust.PluginDeclaration(withEnv).Digest == base {
		t.Error("adding an env entry must change the digest")
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho b\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if trust.PluginDeclaration(m).Digest == base {
		t.Error("changing the executable's bytes must change the digest")
	}
}

func TestPluginSummary(t *testing.T) {
	d := trust.PluginDeclaration(plugin.Manifest{Name: "p", Command: "run", Args: []string{"a", "b"}, Build: "make"})
	if d.Summary != "runs: run a b; build: make" || d.String() != "plugin:p" {
		t.Errorf("%q %q", d.Summary, d.String())
	}
}

// TestMCPDigestKeepsReferencesUnresolved: the digest is over the reference as written, so a
// cache:KEY entry does not change when the secret behind it rotates.
func TestMCPDigestKeepsReferencesUnresolved(t *testing.T) {
	a := mcp.ServerConfig{Name: "s", Transport: mcp.TransportStdio, Command: "srv", Args: []string{"-x"}, Env: map[string]string{"X": "cache:A"}}
	b := a
	b.Env = map[string]string{"X": "cache:B"}
	da, db := trust.MCPDeclaration(a), trust.MCPDeclaration(b)
	if da.Digest == db.Digest {
		t.Fatal("different references must differ")
	}
	lines := []string{"s", "stdio", "srv", "-x", "X=cache:A", ""}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\x00")))
	if want := hex.EncodeToString(sum[:]); da.Digest != want {
		t.Errorf("digest %s, want %s over the documented lines", da.Digest, want)
	}
	if da.Summary != "stdio: srv -x" {
		t.Errorf("summary %q", da.Summary)
	}
	h := trust.MCPDeclaration(mcp.ServerConfig{Name: "h", Transport: mcp.TransportHTTP, URL: "https://x"})
	if h.Summary != "http: https://x" {
		t.Errorf("summary %q", h.Summary)
	}
}

func TestAgentDeclaration(t *testing.T) {
	d := trust.AgentDeclaration("default", []byte("you are root"))
	sum := sha256.Sum256([]byte("you are root"))
	if d.Digest != hex.EncodeToString(sum[:]) || d.Summary != "agent default" || d.String() != "agent:default" {
		t.Errorf("%+v", d)
	}
}

func TestDeclarationsReadsAllThreeKinds(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, ".rudy", rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if decls, errs := trust.Declarations(root); len(decls) != 0 || len(errs) != 0 {
		t.Fatalf("empty root: %v %v", decls, errs)
	}
	write("plugins/p/plugin.toml", "name = \"p\"\nversion = \"1\"\nprotocol_version = 1\ncommand = \"run\"\n")
	write("mcp.toml", "[servers.s]\ntransport = \"stdio\"\ncommand = \"srv\"\n")
	write("agents/default.md", "---\nnot: [valid\n---\nbody")
	write("agents/notes.txt", "ignored")
	decls, errs := trust.Declarations(root)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	var got []string
	for _, d := range decls {
		got = append(got, d.String())
	}
	if want := "agent:default mcp:s plugin:p"; strings.Join(got, " ") != want {
		t.Errorf("got %v want %s", got, want)
	}

	write("mcp.toml", "this is = = not toml")
	decls, errs = trust.Declarations(root)
	if len(errs) != 1 {
		t.Fatalf("want one error, got %v", errs)
	}
	for _, d := range decls {
		if d.Kind == trust.KindMCP {
			t.Errorf("unparseable mcp.toml must yield no MCP declaration: %v", d)
		}
	}
}
