package memory

import (
	"context"
	"encoding/json"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/tool"
)

// newTestMemPlugin builds a plugin over a bundle rooted at t.TempDir(), reusing the harness
// plugin_test.go already builds for the hook tests, with fixtureSession opened against
// fixtureProject so a remember call needs only a session id to resolve its scope.
func newTestMemPlugin(t *testing.T) *memPlugin {
	t.Helper()
	h := newHarness(t, nil)
	h.open(fixtureProject)
	return h.plug
}

// revisionCount is how many times title's concept was recorded in its directory's revision
// log (log.md's Creation/Update entries, one appended per call that reached AppendLog). The
// concept file itself carries no revision count to read back: Revise replaces title,
// description, body and tags wholesale on every call, and Sources, the one field Revise
// merges instead of replacing, never reaches the concept because the memory_remember schema
// does not expose it. The log is the one place a call that completed leaves a trace a later
// call cannot overwrite outright, which is what makes it the right thing to count here.
func (p *memPlugin) revisionCount(t *testing.T, projectID, title string) int {
	t.Helper()
	dir, err := p.b.Dir(projectID)
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	logRel := path.Join(dir.Rel, "log.md")
	text, ok := p.b.Read(logRel)
	if !ok {
		t.Fatalf("no log at %s", logRel)
	}
	return strings.Count(text, "["+title+"]")
}

// TestConcurrentRememberKeepsEveryRevision is the race memory.Remember cannot see: it reads
// the existing concept, revises it in memory and writes the result back, so two calls that
// read the same base before either writes discard one revision when the later write lands.
// ADR 0028 decision 3 makes the tool that owns the state responsible for guarding it, since
// task 3 makes tool calls run concurrently and the scheduler will not serialize this for it.
func TestConcurrentRememberKeepsEveryRevision(t *testing.T) {
	p := newTestMemPlugin(t)
	sid := ulid.MustParse(fixtureSession)
	call := func(body string) tool.Call {
		in, err := json.Marshal(map[string]any{
			"type": "Feedback", "title": "Concurrent title", "description": "d", "body": body,
		})
		if err != nil {
			t.Fatal(err)
		}
		return tool.Call{ID: body, Name: "memory_remember", Input: in, SessionID: sid}
	}

	var wg sync.WaitGroup
	for _, body := range []string{"first", "second", "third"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.remember(context.Background(), call(body)); err != nil {
				t.Errorf("remember %s: %v", body, err)
			}
		}()
	}
	wg.Wait()

	got := p.revisionCount(t, fixtureProject, "Concurrent title")
	if got != 3 {
		t.Fatalf("the concept's log carries %d revisions, want 3: a concurrent write was lost", got)
	}
}
