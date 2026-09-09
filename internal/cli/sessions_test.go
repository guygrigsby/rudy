package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/session"
)

func openTestSession(t *testing.T, st *session.Store, root string) *session.Session {
	t.Helper()
	s, err := session.Open(st, session.SessionOpened{
		SchemaVersion: 1,
		RudyVersion:   "test",
		Workspace:     session.Workspace{Root: root, ProjectID: "local/" + filepath.Base(root)},
		Model:         session.ModelRef{Provider: "fake", Model: "m"},
		Thinking:      session.ThinkingOff,
		Mode:          session.ModeOff,
		Agent:         "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// openChildSession opens the session a subagent's tool call would: same workspace as its
// parent, newer than it, and a child. It is the trap --continue has to step over.
func openChildSession(t *testing.T, st *session.Store, root, parentID string) *session.Session {
	t.Helper()
	s, err := session.Open(st, session.SessionOpened{
		SchemaVersion:   1,
		RudyVersion:     "test",
		Workspace:       session.Workspace{Root: root, ProjectID: "local/" + filepath.Base(root)},
		Model:           session.ModelRef{Provider: "fake", Model: "m"},
		Thinking:        session.ThinkingOff,
		Mode:            session.ModeOff,
		Agent:           "default",
		ParentSessionID: parentID,
		ParentToolUseID: "agent_0_abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRenderSessionsNewestFirst(t *testing.T) {
	st, err := session.OpenStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	first := openTestSession(t, st, "/tmp/alpha")
	second := openTestSession(t, st, "/tmp/beta")
	list, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := renderSessions(&out, list); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "ID") {
		t.Fatalf("output %q", out.String())
	}
	if !strings.Contains(lines[1], second.ID().String()) || !strings.Contains(lines[1], "/tmp/beta") {
		t.Fatalf("first row should be the newest session: %q", lines[1])
	}
	if !strings.Contains(lines[2], first.ID().String()) || !strings.Contains(lines[2], "fake:m") {
		t.Fatalf("second row %q", lines[2])
	}
}

func TestSessionsListCommand(t *testing.T) {
	root := newRoot("test", testBuilder(t, &fakeProvider{}))
	st, err := session.OpenStore(filepath.Join(os.Getenv("XDG_DATA_HOME"), "rudy", "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	s := openTestSession(t, st, "/tmp/gamma")
	_ = s.Close()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"sessions", "list"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), s.ID().String()) || !strings.Contains(out.String(), "/tmp/gamma") {
		t.Fatalf("output %q", out.String())
	}
}
