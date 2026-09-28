// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeLinkIsAtomicPrivateAndExact(t *testing.T) {
	root := t.TempDir()
	store := NewRuntimeLinkStore(root)
	id := NewID()
	want := RuntimeLink{Runtime: "codex", ThreadID: "thr_1"}
	if err := store.Write(id, want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("link = %+v, want %+v", got, want)
	}
	info, err := os.Stat(store.Path(id))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	matches, err := filepath.Glob(filepath.Join(root, id.String(), ".runtime-*.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files remain: %v", matches)
	}
}

func TestRuntimeLinkRejectsUnknownFieldsAndDuplicateThread(t *testing.T) {
	root := t.TempDir()
	store := NewRuntimeLinkStore(root)
	one, two := NewID(), NewID()
	link := RuntimeLink{Runtime: "codex", ThreadID: "thr_1"}
	if err := store.Write(one, link); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(two, link); !errors.Is(err, ErrRuntimeLinkConflict) {
		t.Fatalf("duplicate thread error = %v", err)
	}
	bad := NewID()
	if err := os.MkdirAll(filepath.Dir(store.Path(bad)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path(bad), []byte("runtime = \"codex\"\nthread_id = \"thr_2\"\nextra = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(bad); err == nil {
		t.Fatal("unknown TOML field accepted")
	}
}

func TestNativeForkDoesNotCopyRuntimeLink(t *testing.T) {
	store, parent := newSession(t)
	links := NewRuntimeLinkStore(store.Root())
	if err := links.Write(parent.ID(), RuntimeLink{Runtime: "codex", ThreadID: "thr_parent"}); err != nil {
		t.Fatal(err)
	}
	at := mustAppend(t, parent, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("one")}})
	child, err := parent.Fork(store, at.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Close() }()
	if _, err := links.Read(child.ID()); !errors.Is(err, ErrRuntimeLinkNotFound) {
		t.Fatalf("child runtime link = %v, want not found", err)
	}
}
