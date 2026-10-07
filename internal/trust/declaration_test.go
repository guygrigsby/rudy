// SPDX-License-Identifier: AGPL-3.0-or-later

package trust_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	framed := "s1:s" + "s5:stdio" + "s3:srv" + "l1:s2:-x" + "m1:s1:Xs7:cache:A" + "s0:" + "m0:"
	sum := sha256.Sum256([]byte(framed))
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
	sum := sha256.Sum256([]byte("s12:you are root"))
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

func writeExec(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(path)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// TestPathCommandInsidePluginDirIsDigested: a command with a path separator runs from the
// plugin directory, so a clone that swaps that binary asks again.
func TestPathCommandInsidePluginDirIsDigested(t *testing.T) {
	for _, command := range []string{"./bin/srv", "bin/srv"} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin", "srv")
			writeExec(t, bin, "#!/bin/sh\necho a\n", 0o755)
			m := plugin.Manifest{Name: "p", Version: "1", Command: command, Dir: dir}
			base := trust.PluginDeclaration(m).Digest
			writeExec(t, bin, "#!/bin/sh\necho b\n", 0o755)
			if trust.PluginDeclaration(m).Digest == base {
				t.Error("changing the executable's bytes must change the digest")
			}
		})
	}
}

// TestNonExecutableAndMissingPathCommandsDigestAsNone: only an executable regular file is
// digested; for anything else the bytes on disk are not part of the digest.
func TestNonExecutableAndMissingPathCommandsDigestAsNone(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "bin", "plain")
	for _, command := range []string{"bin/plain", "bin/missing", "../x"} {
		m := plugin.Manifest{Name: "p", Command: command, Dir: dir}
		if _, ok := plugin.LocalExecutable(m); ok {
			t.Errorf("%s resolved", command)
		}
		writeExec(t, plain, "data", 0o644)
		a := trust.PluginDeclaration(m).Digest
		writeExec(t, plain, "other", 0o644)
		if b := trust.PluginDeclaration(m).Digest; a != b {
			t.Errorf("%s: digest moved with the bytes of a file that does not run", command)
		}
	}
	// exec:none is a different digest from an executable's.
	writeExec(t, plain, "data", 0o755)
	m := plugin.Manifest{Name: "p", Command: "bin/plain", Dir: dir}
	exe := trust.PluginDeclaration(m).Digest
	writeExec(t, plain, "data", 0o644)
	if trust.PluginDeclaration(m).Digest == exe {
		t.Error("an executable and the same file without the bit must differ")
	}
}

// TestUnreadableExecutableDigestsAsUnreadable: an executable the digest cannot read is
// stable, not an error and not none.
func TestUnreadableExecutableDigestsAsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin", "srv")
	writeExec(t, bin, "x", 0o111)
	m := plugin.Manifest{Name: "p", Command: "bin/srv", Dir: dir}
	unreadable := trust.PluginDeclaration(m).Digest
	writeExec(t, bin, "x", 0o755)
	if trust.PluginDeclaration(m).Digest == unreadable {
		t.Error("readable and unreadable must differ")
	}
	writeExec(t, bin, "y", 0o111)
	if trust.PluginDeclaration(m).Digest != unreadable {
		t.Error("an unreadable executable digests the same whatever its bytes")
	}
	writeExec(t, bin, "y", 0o644)
	if trust.PluginDeclaration(m).Digest == unreadable {
		t.Error("unreadable must differ from none")
	}
}

// TestMCPDigestFramingIsUnambiguous: a value cannot be moved between fields without
// changing the digest.
func TestMCPDigestFramingIsUnambiguous(t *testing.T) {
	a := mcp.ServerConfig{Name: "s", Transport: mcp.TransportStdio, Command: "srv",
		URL: "NODE_OPTIONS=--require ./x.js", Headers: map[string]string{"a": "b"}}
	b := mcp.ServerConfig{Name: "s", Transport: mcp.TransportStdio, Command: "srv",
		Env: map[string]string{"NODE_OPTIONS": "--require ./x.js"}, URL: "a=b"}
	if trust.MCPDeclaration(a).Digest == trust.MCPDeclaration(b).Digest {
		t.Error("env and url/headers collide")
	}
	c := mcp.ServerConfig{Name: "s", Transport: mcp.TransportStdio, Command: "srv", Args: []string{"a b"}}
	d := mcp.ServerConfig{Name: "s", Transport: mcp.TransportStdio, Command: "srv", Args: []string{"a", "b"}}
	if trust.MCPDeclaration(c).Digest == trust.MCPDeclaration(d).Digest {
		t.Error("args join ambiguously")
	}
}

// TestUnparseableMCPIsATypedError: callers decide on the project MCP scope by type, never by
// the text of the error.
func TestUnparseableMCPIsATypedError(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".rudy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mcp.toml")
	if err := os.WriteFile(path, []byte("not toml ["), 0o644); err != nil {
		t.Fatal(err)
	}
	_, errs := trust.Declarations(root)
	var re *trust.ReadError
	if len(errs) != 1 || !errors.As(errs[0], &re) || re.Path != path {
		t.Fatalf("errs = %v, want one *trust.ReadError for %s", errs, path)
	}
	if !strings.Contains(errs[0].Error(), "mcp.toml") {
		t.Errorf("message %q lost the path", errs[0])
	}
}
