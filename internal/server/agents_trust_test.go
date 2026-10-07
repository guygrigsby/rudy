// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guygrigsby/rudy/internal/protocol"
	"github.com/guygrigsby/rudy/internal/session"
	"github.com/guygrigsby/rudy/internal/trust"
)

// workspaceWithDefaultAgent is a repository that ships .rudy/agents/default.md whose body is
// INJECTED, the file a cloned repo uses to take over the root session (ADR 0050).
func workspaceWithDefaultAgent(t *testing.T) (root, file string) {
	t.Helper()
	root = t.TempDir()
	dir := filepath.Join(root, ".rudy", "agents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file = filepath.Join(dir, "default.md")
	body := "---\ndescription: from the repo\n---\nINJECTED\n"
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, file
}

// openAndRead opens a root session in root and returns the system prompt the server sent the
// provider for its first turn, with every notice sent to the opening connection.
func openAndRead(t *testing.T, ts *testServer, root string) (system string, notices []string) {
	t.Helper()
	cl := dialAs(t, ts.srv, false)
	var info protocol.SessionInfo
	if err := cl.Call(context.Background(), protocol.MethodSessionOpen, protocol.SessionOpenParams{Cwd: root}, &info); err != nil {
		t.Fatalf("open: %v", err)
	}
	var sub protocol.SessionSubmitResult
	if err := cl.Call(context.Background(), protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: info.SessionID, Content: []session.Block{session.TextBlock("status")}, Source: session.SourceTyped,
	}, &sub); err != nil {
		t.Fatalf("submit: %v", err)
	}
	for _, n := range drain(t, cl, completedOn(info.SessionID)) {
		if n.Method != protocol.NotifyNotice {
			continue
		}
		var p protocol.NoticeParams
		_ = json.Unmarshal(n.Params, &p)
		notices = append(notices, p.Text)
	}
	reqs := ts.prov.requestsFor(info.SessionID)
	if len(reqs) == 0 {
		t.Fatal("no provider request recorded")
	}
	return reqs[0].System, notices
}

func hasNotice(notices []string, sub string) bool {
	for _, n := range notices {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

func TestUntrustedWorkspaceAgentIsSkipped(t *testing.T) {
	ts, _ := newTestServerTrusting(t, nil)
	root, _ := workspaceWithDefaultAgent(t)
	system, notices := openAndRead(t, ts, root)
	if strings.Contains(system, "INJECTED") {
		t.Fatalf("an untrusted workspace's default agent became the prompt: %q", system)
	}
	if !hasNotice(notices, "not trusted") {
		t.Fatalf("no not-trusted notice, got %q", notices)
	}
}

func TestTrustedWorkspaceAgentApplies(t *testing.T) {
	ts, _ := newTestServerTrusting(t, trustAll)
	root, _ := workspaceWithDefaultAgent(t)
	system, notices := openAndRead(t, ts, root)
	if !strings.Contains(system, "INJECTED") {
		t.Fatalf("a trusted workspace's default agent did not apply: %q", system)
	}
	if hasNotice(notices, "not trusted") {
		t.Fatalf("trusted workspace got a not-trusted notice: %q", notices)
	}
}

func TestChangedWorkspaceAgentAfterTrustIsSkipped(t *testing.T) {
	store := trust.NewStore(t.TempDir())
	trusted := func(root string) bool {
		d, _ := trust.Declarations(root)
		ok, _ := store.IsTrusted(root, d)
		return ok
	}
	ts, _ := newTestServerTrusting(t, trusted)
	root, file := workspaceWithDefaultAgent(t)
	decls, _ := trust.Declarations(root)
	if err := store.Trust(root, decls, time.Now()); err != nil {
		t.Fatal(err)
	}
	if system, _ := openAndRead(t, ts, root); !strings.Contains(system, "INJECTED") {
		t.Fatalf("the agent as trusted did not apply: %q", system)
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("x"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	system, notices := openAndRead(t, ts, root)
	if strings.Contains(system, "INJECTED") {
		t.Fatalf("a workspace agent changed after trust still applied: %q", system)
	}
	if !hasNotice(notices, "not trusted") {
		t.Fatalf("no not-trusted notice, got %q", notices)
	}
}
