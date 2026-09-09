package session

import (
	"errors"
	"testing"
)

func TestStoreListNewestFirstWithForks(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := Open(st, opened())
	if err != nil {
		t.Fatal(err)
	}
	at := mustAppend(t, a, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("x")}})
	child, err := a.Fork(st, at.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("List = %d sessions, want 2", len(got))
	}
	if got[0].ID != child.ID() || !got[0].Forked {
		t.Fatalf("newest first should be the fork: %+v", got[0])
	}
	if got[0].Workspace.Root != "/w" || got[0].Model.Model != "cline-pass/kimi-k3" {
		t.Fatalf("fork summary must resolve workspace and model through its parent: %+v", got[0])
	}
	if got[1].ID != a.ID() || got[1].Forked {
		t.Fatalf("second should be the root: %+v", got[1])
	}
	if got[0].ParentSessionID != "" || got[1].ParentSessionID != "" {
		t.Fatalf("neither session has a parent: %+v %+v", got[0], got[1])
	}
}

// TestStoreListForkOfAChildIsStillAChild: a fork inherits its parent's session_opened, so it
// inherits the parent session that entry names. A summary that dropped it would report a
// subagent's forked session as a root, and rudy --continue picks the newest root.
func TestStoreListForkOfAChildIsStillAChild(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	o := opened()
	o.ParentSessionID, o.ParentToolUseID = NewID().String(), "tu_agent"
	child, err := Open(st, o)
	if err != nil {
		t.Fatal(err)
	}
	at := mustAppend(t, child, UserMessage{Source: SourceTyped, Content: []Block{TextBlock("x")}})
	fork, err := child.Fork(st, at.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fork.Close(); err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != fork.ID() {
		t.Fatalf("List = %+v", got)
	}
	if got[0].ParentSessionID != o.ParentSessionID {
		t.Errorf("fork of a child parent_session_id = %q, want %q", got[0].ParentSessionID, o.ParentSessionID)
	}
}

func TestStoreLock(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := NewID()
	unlock, err := st.Lock(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Lock(id); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Lock = %v, want ErrLocked", err)
	}
	unlock()
	unlock2, err := st.Lock(id)
	if err != nil {
		t.Fatalf("Lock after unlock: %v", err)
	}
	unlock2()
}

func TestSessionHoldsLockUntilClose(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(st, opened())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(st, s.ID()); !errors.Is(err, ErrLocked) {
		t.Fatalf("Load while open = %v, want ErrLocked", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	re, err := Load(st, s.ID())
	if err != nil {
		t.Fatalf("Load after Close: %v", err)
	}
	_ = re.Close()
}
