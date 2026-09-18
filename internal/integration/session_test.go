// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openOneSession runs a turn so there is a real session on disk, and returns its directory.
func openOneSession(t *testing.T, h *home) string {
	t.Helper()
	if r := h.run(t, 60*time.Second, "-p", "hello"); r.code != 0 {
		t.Fatalf("a plain turn should work before anything is corrupted:\n%s", r.out())
	}
	dir := filepath.Join(h.root, "data", "rudy", "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no session on disk under %s: %v", dir, err)
	}
	return filepath.Join(dir, entries[0].Name())
}

// TestCorruptSessionLog takes a session rudy wrote and damages it the way a full disk, a
// killed process or a careless editor would. Reading it back must refuse or recover, never
// panic and never hang.
func TestCorruptSessionLog(t *testing.T) {
	cases := []struct {
		name   string
		damage func(t *testing.T, path string)
	}{
		{"truncated mid line", func(t *testing.T, p string) {
			b := read(t, p)
			write(t, p, b[:len(b)-len(b)/3])
		}},
		{"a garbage line in the middle", func(t *testing.T, p string) {
			lines := strings.Split(strings.TrimRight(string(read(t, p)), "\n"), "\n")
			if len(lines) < 2 {
				t.Skip("need more than one entry to corrupt the middle")
			}
			lines[len(lines)/2] = "{not json at all"
			write(t, p, []byte(strings.Join(lines, "\n")+"\n"))
		}},
		{"nul bytes through the whole file", func(t *testing.T, p string) {
			b := read(t, p)
			for i := range b {
				if i%17 == 0 {
					b[i] = 0
				}
			}
			write(t, p, b)
		}},
		{"an entry with a type nobody registered", func(t *testing.T, p string) {
			write(t, p, append(read(t, p), []byte(`{"id":"01JBQ0000000000000000000","type":"abduction","at":"2026-09-17T00:00:00Z"}`+"\n")...))
		}},
		{"an entry whose id is not a ulid", func(t *testing.T, p string) {
			write(t, p, append(read(t, p), []byte(`{"id":"not-a-ulid","type":"user_message","at":"2026-09-17T00:00:00Z"}`+"\n")...))
		}},
		{"an entry from the future, out of order", func(t *testing.T, p string) {
			write(t, p, append(read(t, p), []byte(`{"id":"00000000000000000000000000","type":"user_message","at":"1999-01-01T00:00:00Z"}`+"\n")...))
		}},
		{"a line of ten megabytes", func(t *testing.T, p string) {
			write(t, p, append(read(t, p), []byte(`{"id":"01JBQ0000000000000000001","type":"user_message","text":"`+strings.Repeat("x", 10<<20)+`"}`+"\n")...))
		}},
		{"the file emptied", func(t *testing.T, p string) {
			write(t, p, nil)
		}},
		{"the file replaced by a directory", func(t *testing.T, p string) {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(p, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"the file unreadable", func(t *testing.T, p string) {
			if err := os.Chmod(p, 0o000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHome(t)
			h.withProvider(t)
			dir := openOneSession(t, h)
			id := filepath.Base(dir)
			c.damage(t, filepath.Join(dir, "entries.jsonl"))

			// Listing walks every session and must survive one that is wrecked.
			list := h.run(t, 30*time.Second, "sessions", "list")
			assertNoPanic(t, list.out())

			// Resuming it is the read that has to hold: recover it or refuse it by name.
			r := h.run(t, 60*time.Second, "-p", "--resume", id, "and now")
			assertNoPanic(t, r.out())
			if r.code != 0 && strings.TrimSpace(r.out()) == "" {
				t.Errorf("resuming a session with %s failed silently, exit %d", c.name, r.code)
			}
		})
	}
}

// TestSessionDirectoryItself damages the directory rather than the log: the shapes a
// half-finished write, a crash or a manual rm leaves behind.
func TestSessionDirectoryItself(t *testing.T) {
	t.Run("a session directory with nothing in it", func(t *testing.T) {
		h := newHome(t)
		h.withProvider(t)
		dir := openOneSession(t, h)
		if err := os.Remove(filepath.Join(dir, "entries.jsonl")); err != nil {
			t.Fatal(err)
		}
		list := h.run(t, 30*time.Second, "sessions", "list")
		assertNoPanic(t, list.out())
		r := h.run(t, 60*time.Second, "-p", "--resume", filepath.Base(dir), "and now")
		assertNoPanic(t, r.out())
		if r.code == 0 {
			t.Errorf("resumed a session with no entries\n%s", r.out())
		}
	})

	t.Run("a directory that is not a session id", func(t *testing.T) {
		h := newHome(t)
		h.withProvider(t)
		_ = openOneSession(t, h)
		junk := filepath.Join(h.root, "data", "rudy", "sessions", "../../../etc")
		_ = junk // the store names its own directories; this only checks the walk
		if err := os.MkdirAll(filepath.Join(h.root, "data", "rudy", "sessions", "not-a-ulid"), 0o700); err != nil {
			t.Fatal(err)
		}
		r := h.run(t, 30*time.Second, "sessions", "list")
		assertNoPanic(t, r.out())
		if r.code != 0 {
			t.Errorf("a stray directory among the sessions stopped the listing\n%s", r.out())
		}
	})

	t.Run("resuming an id that was never a session", func(t *testing.T) {
		h := newHome(t)
		h.withProvider(t)
		for _, id := range []string{"01JBQ0000000000000000000", "not-a-ulid", "", "../../etc/passwd", strings.Repeat("9", 300)} {
			r := h.run(t, 30*time.Second, "-p", "--resume", id, "hi")
			assertNoPanic(t, r.out())
			if r.code == 0 {
				t.Errorf("resumed %q, which is not a session\n%s", id, r.out())
			}
		}
	})
}

// TestSessionsListOnGarbage: the listing reads every session's head, so one unreadable
// session must not take the command down.
func TestSessionsListOnGarbage(t *testing.T) {
	h := newHome(t)
	h.withProvider(t)
	good := openOneSession(t, h)
	sessions := filepath.Dir(good)
	bad := filepath.Join(sessions, "01JBQ0000000000000000002")
	if err := os.MkdirAll(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(bad, "entries.jsonl"), []byte("{\x00\x01 not json\n\n\n"))
	r := h.run(t, 30*time.Second, "sessions", "list")
	assertNoPanic(t, r.out())
	if !strings.Contains(r.out(), filepath.Base(good)) && r.code == 0 {
		t.Errorf("the good session vanished from a listing that survived the bad one:\n%s", r.out())
	}
}

func read(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func write(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// jsonLines is the entries file parsed, for a test that wants to assert on what recovery
// left behind rather than on the process's exit code.
func jsonLines(t *testing.T, p string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(string(read(t, p)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}
