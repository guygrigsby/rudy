package turn

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestInflightCancelsEveryRegisteredCall(t *testing.T) {
	f := newInflight()
	var a, b atomic.Bool
	f.add("tu1", func() { a.Store(true) })
	f.add("tu2", func() { b.Store(true) })
	if f.len() != 2 {
		t.Fatalf("len = %d, want 2", f.len())
	}
	f.cancelAll()
	if !a.Load() || !b.Load() {
		t.Fatalf("cancelAll reached a=%v b=%v, want both", a.Load(), b.Load())
	}
}

func TestInflightCancelsALateArrival(t *testing.T) {
	f := newInflight()
	f.cancelAll()
	var late atomic.Bool
	if f.add("tu1", func() { late.Store(true) }) {
		t.Fatal("add reported the call registered after the turn was cancelled")
	}
	if !late.Load() {
		t.Fatal("a call registering after cancelAll was not cancelled")
	}
	if f.len() != 0 {
		t.Fatalf("a cancelled call was kept: len = %d", f.len())
	}
}

func TestInflightRemoveLeavesTheRest(t *testing.T) {
	f := newInflight()
	var a, b atomic.Bool
	f.add("tu1", func() { a.Store(true) })
	f.add("tu2", func() { b.Store(true) })
	f.remove("tu1")
	f.cancelAll()
	if a.Load() {
		t.Fatal("a call that finished was cancelled anyway")
	}
	if !b.Load() {
		t.Fatal("cancelAll missed the call still running")
	}
}

// TestInflightResetOpensTheSetAgain covers the turn after an interrupted one: cancelAll
// latches so a call registering late is still cancelled, and without reset the next turn's
// calls would all be cancelled before they ran.
func TestInflightResetOpensTheSetAgain(t *testing.T) {
	f := newInflight()
	f.add("tu1", func() {})
	f.cancelAll()
	f.reset()
	var late atomic.Bool
	if !f.add("tu2", func() { late.Store(true) }) {
		t.Fatal("add refused a call after reset")
	}
	if late.Load() {
		t.Fatal("a call registered after reset was cancelled by the previous turn")
	}
	if f.len() != 1 {
		t.Fatalf("len = %d, want 1", f.len())
	}
}

// TestInflightIsSafeUnderConcurrentUse is the shape the runner uses it in: calls registering
// and finishing while an interrupt fans out over the set.
func TestInflightIsSafeUnderConcurrentUse(t *testing.T) {
	f := newInflight()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			id := string(rune('a' + i))
			f.add(id, func() {})
			f.len()
			f.remove(id)
		})
	}
	wg.Go(f.cancelAll)
	wg.Wait()
}
