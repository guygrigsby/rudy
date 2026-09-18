// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestADaemonKilledMidTurnLeavesAResumableSession: kill -9 while a turn is streaming, which
// is a laptop lid, an OOM killer or a power cut. The log is append-only and fsynced, so what
// was written stays written, and the session has to load again afterwards: Session.Load runs
// recovery (the contracts say so on session.resume).
func TestADaemonKilledMidTurnLeavesAResumableSession(t *testing.T) {
	h := newHome(t)
	p := h.withProvider(t)
	streaming := make(chan struct{})
	var once bool
	p.onCompletion(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"working\"}}]}\n\n"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		if !once {
			once = true
			close(streaming)
		}
		<-r.Context().Done()
	})

	d := startDaemon(t, h)
	// A client that opens a session and submits, then dies with the daemon.
	go func() { _, _ = h.runMaybeHanging(t, 60*time.Second, "-p", "start something") }()
	select {
	case <-streaming:
	case <-time.After(60 * time.Second):
		t.Fatal("the provider was never called, so nothing was streaming to interrupt")
	}
	if err := d.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_, _ = d.cmd.Process.Wait()

	// The session on disk is whatever the kill left. Listing and resuming it must work.
	list := h.run(t, 60*time.Second, "sessions", "list")
	assertNoPanic(t, list.out())
	if list.code != 0 {
		t.Fatalf("listing sessions after a kill: %s", list.out())
	}
	dir := filepath.Join(h.root, "data", "rudy", "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no session survived the kill: %v", err)
	}
	// A fresh provider for the resumed turn, since the old one is still holding its stream.
	p.onCompletion(nil)
	r := h.run(t, 60*time.Second, "-p", "--resume", entries[0].Name(), "carry on")
	assertNoPanic(t, r.out())
	if r.code != 0 {
		t.Errorf("a session killed mid turn does not resume: %s", r.out())
	}
	if lock := filepath.Join(dir, entries[0].Name(), "lock"); fileExists(lock) {
		t.Logf("the lock file is still there after a kill, which is fine if nothing honours it: %s", lock)
	}
}

// TestASessionDirectoryThatGoesReadOnly: the disk stops accepting writes mid session, which
// is a full volume or a revoked mount. The turn must fail rather than report an answer it
// could not record.
func TestASessionDirectoryThatGoesReadOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory anyway")
	}
	h := newHome(t)
	h.withProvider(t)
	dir := openOneSession(t, h)
	// The file, not the directory: a directory without write permission still lets an
	// append to a file that already exists, so chmodding the directory proved nothing.
	log := filepath.Join(dir, "entries.jsonl")
	if err := os.Chmod(log, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(log, 0o600) })

	r := h.run(t, 60*time.Second, "-p", "--resume", filepath.Base(dir), "and now")
	assertNoPanic(t, r.out())
	if r.code == 0 {
		t.Errorf("a turn on a session it cannot write reported success\n%s", r.out())
	}
	if strings.TrimSpace(r.out()) == "" {
		t.Errorf("a turn on a read-only session failed silently")
	}
}

// TestTwoHeadlessRunsOnOneSession: the store takes an exclusive flock per session, so the
// second must be told to attach through the socket rather than both writing the same log.
func TestTwoHeadlessRunsOnOneSession(t *testing.T) {
	h := newHome(t)
	p := h.withProvider(t)
	dir := openOneSession(t, h)
	id := filepath.Base(dir)

	hold := make(chan struct{})
	var release sync.Once
	letGo := func() { release.Do(func() { close(hold) }) }
	t.Cleanup(letGo)
	p.onCompletion(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		select {
		case <-hold:
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
	})

	first := make(chan result, 1)
	go func() {
		r, _ := h.runMaybeHanging(t, 60*time.Second, "-p", "--resume", id, "one")
		first <- r
	}()
	time.Sleep(2 * time.Second) // the first run is holding the session by now

	second := h.run(t, 60*time.Second, "-p", "--resume", id, "two")
	assertNoPanic(t, second.out())
	if second.code == 0 {
		t.Errorf("two processes ran turns on one session at once\n%s", second.out())
	}
	letGo()
	<-first
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
