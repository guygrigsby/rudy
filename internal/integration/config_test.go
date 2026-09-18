// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestHostileConfig points rudy at config files nobody would write. Each case is a valid
// config with one thing wrong in it, so the only reason to fail is that one thing: a case
// built on nothing would fail for want of a provider and prove the parser nothing.
//
// want is what the operator should get. refused means a non-zero exit naming the key, since
// a config rudy cannot understand is one it must not guess at. ignored means the contract
// says unknown keys are dropped, so the run has to succeed.
func TestHostileConfig(t *testing.T) {
	const (
		refused = "refused"
		ignored = "ignored"
	)
	cases := []struct {
		name     string
		fragment string
		want     string
		names    string // a word the refusal has to carry, so a right answer for a wrong reason fails
	}{
		{"not toml at all", "\x00\x01\x02 this is not toml \xff\xfe", refused, ""},
		{"a key with no table", "= 1\n", refused, ""},
		{"duplicate key", "[default]\nprovider=\"twice\"\n", refused, ""},
		{"max_tokens is a word", "max_tokens = \"many\"\n", refused, "max_tokens"},
		{"max_tokens overflows int64", "max_tokens = 99999999999999999999999\n", refused, "max_tokens"},
		{"max_tokens is negative", "max_tokens = -5\n", refused, "max_tokens"},
		{"mode is a number", "[permissions]\nmode = 7\n", refused, "mode"},
		{"mode is unknown", "[permissions]\nmode = \"whatever\"\n", refused, "mode"},
		{"log level is unknown", "[log]\nlevel = \"shout\"\n", refused, "level"},
		{"render is unknown", "[ui]\nrender = \"holograph\"\n", refused, "render"},
		{"remote host looks like a flag", "[remote]\nhost = \"--oops\"\n", refused, "host"},
		{"compact_at out of range", "[sessions]\ncompact_at = 12.5\n", refused, "compact_at"},
		{"thinking is unknown", "[default]\nthinking = \"hardest\"\n", refused, "thinking"},
		{"a key nobody registered", "wat = 1\n", ignored, ""},
		{"a table nobody registered", "[wat]\nx = 1\n", ignored, ""},
		{"deeply nested tables nobody registered", nestedTables(200), ignored, ""},
		{"a giant value on a key nobody registered", "wat = \"" + strings.Repeat("m", 1<<20) + "\"\n", ignored, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHome(t)
			p := h.withProvider(t)
			// The fragment goes first: a bare key written after a table header belongs to
			// that table, and a fragment appended to the sample config would be testing
			// providers.fake.max_tokens rather than max_tokens.
			h.writeConfig(t, c.fragment+"\n"+minimalConfig(p.URL()))
			r := h.run(t, 30*time.Second, "models", "list")
			assertNoPanic(t, r.out())
			switch c.want {
			case refused:
				if r.code == 0 {
					t.Errorf("exited 0 on %q\n%s", c.name, r.out())
				}
				if strings.TrimSpace(r.out()) == "" {
					t.Errorf("refused %q silently", c.name)
				}
				if c.names != "" && !strings.Contains(r.out(), c.names) {
					t.Errorf("the refusal of %q never says %q, so nobody can find it:\n%s", c.name, c.names, r.out())
				}
			case ignored:
				if r.code != 0 {
					t.Errorf("%q is an unknown key, which the contract drops, but rudy failed:\n%s", c.name, r.out())
				}
			}
		})
	}
}

// TestAnUnknownKeyBindingReachesTheOperator: [keys] is the client's table, and the closed
// set is checked when the client starts, not when config loads. So a dead binding is
// invisible to every other command, and the operator hears about it the next time they open
// the client rather than when they write it (rudy-djd).
func TestAnUnknownKeyBindingReachesTheOperator(t *testing.T) {
	h := newHome(t)
	p := h.withProvider(t)
	h.writeConfig(t, "[keys]\n\"app.not.a.thing\" = \"ctrl+z\"\n\n"+minimalConfig(p.URL()))

	// models list has no keys to bind, so it runs.
	if r := h.run(t, 30*time.Second, "models", "list"); r.code != 0 {
		t.Errorf("models list does not bind keys and should not care:\n%s", r.out())
	}

	// The client does, and refuses by name rather than starting with a binding that is not
	// there.
	r := h.run(t, 30*time.Second)
	assertNoPanic(t, r.out())
	if r.code == 0 {
		t.Errorf("the client started with an action id nobody registered\n%s", r.out())
	}
	if !strings.Contains(r.out(), "app.not.a.thing") {
		t.Errorf("the refusal never names the id the operator typed:\n%s", r.out())
	}
}

// TestConfigPathsThatAreNotFiles: a config, a session dir or a log that is a directory, a
// device or a symlink loop. The loop is the one that can hang rather than fail.
func TestConfigPathsThatAreNotFiles(t *testing.T) {
	t.Run("config.toml is a directory", func(t *testing.T) {
		h := newHome(t)
		if err := os.RemoveAll(h.config); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(h.config, 0o700); err != nil {
			t.Fatal(err)
		}
		r := h.run(t, 30*time.Second, "models", "list")
		assertNoPanic(t, r.out())
		if r.code == 0 {
			t.Errorf("a config.toml that is a directory exited 0\n%s", r.out())
		}
	})

	t.Run("config.toml is a symlink loop", func(t *testing.T) {
		h := newHome(t)
		_ = os.Remove(h.config)
		if err := os.Symlink(h.config, h.config); err != nil {
			t.Skipf("symlink loop not creatable here: %v", err)
		}
		r := h.run(t, 30*time.Second, "models", "list")
		assertNoPanic(t, r.out())
		if r.code == 0 {
			t.Errorf("a config.toml that points at itself exited 0\n%s", r.out())
		}
	})

	t.Run("config.toml is a fifo nobody writes", func(t *testing.T) {
		h := newHome(t)
		_ = os.Remove(h.config)
		if err := syscall.Mkfifo(h.config, 0o600); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
		// Nothing ever writes the other end. Reading it blocks forever, so this is the test
		// for whether a read of the config file can hold the process open.
		r := h.run(t, 20*time.Second, "config", "path")
		assertNoPanic(t, r.out())
	})

	t.Run("sessions dir is a file", func(t *testing.T) {
		h := newHome(t)
		p := h.withProvider(t)
		_ = p
		blocker := filepath.Join(h.root, "not-a-dir")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(h.config)
		if err != nil {
			t.Fatal(err)
		}
		h.writeConfig(t, string(body)+"\n[sessions]\ndir = \""+blocker+"\"\n")
		r := h.run(t, 30*time.Second, "sessions", "list")
		assertNoPanic(t, r.out())
		if r.code == 0 {
			t.Errorf("a sessions dir that is a file exited 0\n%s", r.out())
		}
	})

	t.Run("log file is a directory", func(t *testing.T) {
		h := newHome(t)
		p := h.withProvider(t)
		_ = p
		dir := filepath.Join(h.root, "log-dir")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(h.config)
		if err != nil {
			t.Fatal(err)
		}
		h.writeConfig(t, string(body)+"\n[log]\nfile = \""+dir+"\"\n")
		// Documented as a notice, not a failure: the trail goes to stderr instead.
		r := h.run(t, 30*time.Second, "models", "list")
		assertNoPanic(t, r.out())
		if r.code != 0 {
			t.Errorf("a log file that cannot be opened is a notice, not a failure\n%s", r.out())
		}
	})
}

// minimalConfig is a working config and nothing else: one provider, one model, no table a
// test fragment might want to open for itself.
func minimalConfig(url string) string {
	return "[default]\nprovider = \"fake\"\nmodel = \"m1\"\n\n[providers.fake]\nwire = \"openai_chat\"\nbase_url = \"" + url + "/v1\"\n"
}

// nestedTables is n distinct deep tables, none of them a key rudy knows. Repeating one table
// would be a TOML duplicate, which is a different refusal than the one under test.
func nestedTables(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "[a.b.c.d.e.f.g.h.i.j%d]\nx = 1\n", i)
	}
	return b.String()
}
