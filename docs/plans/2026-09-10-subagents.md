# Subagents wave implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Tool calls run concurrently, a parent watches the subagents it dispatched, and a plugin can ship an agent definition.

**Architecture:** The turn runner stops being a serial loop with one cancel, one interrupt flag and one state. A tool call becomes addressable by its `tool_use` id, which is what concurrency needs for per-call cancellation and what a client needs to render several calls at once. On top of that: the Gate coalesces asks so concurrent calls to one tool put one question to the operator, a session's tool set becomes a narrowing chain no caller can widen, a child's notifications reach its parent's subscribers without granting authority over the child, and the plugin registry gains agent definitions the way it already has providers.

**Tech Stack:** Go, `internal/turn` (runner), `internal/server` (sessions, fan-out), `internal/plugin` (registry, spawned plugins), `internal/tui` (Bubble Tea v2, teatest goldens).

**Spec:** [docs/adr/0028-the-subagents-wave.md](../adr/0028-the-subagents-wave.md), with the normative rows in [docs/specs/rudy-contracts.md](../specs/rudy-contracts.md) pass 5. Both are written and committed; the plan implements them and does not revise them. A task that finds the contract wrong stops and says so rather than coding around it.

## Global Constraints

- `for range n`, never a three-clause count loop.
- No vendor or wire types outside the two codecs and the provider plugins. `make vendor-types` is part of `check`.
- Commits: terse, verb-first, no em or en dashes, no Oxford commas, no attribution trailers. Prefix by area: `turn:`, `server:`, `plugin:`, `tui:`, `session:`, `docs:`.
- `make test` runs the whole suite, then the server package again under `RUDY_TEST_TRANSPORT=socket`. Every server test must pass over both transports.
- `make check` is the gate: `build`, `test`, `lint`, `fmt-check`, `vendor-types`, `config-example`.
- Every render choice is a config field with a default; a new key means a default, a catalogue entry in `internal/config/docs.go`, a contracts row and a regenerated `examples/config.toml`, and the `internal/config` guards fail until all four agree.
- The kernel has no private path to its own features. Anything the subagents plugin needs, it gets through the plugin interface an external plugin would use.
- Tool inputs and thinking signatures are raw bytes end to end. Never re-marshal them.
- charm.land v2 modules and provider SDKs are pinned exactly; a bump is gated on the teatest goldens.
- Every test that exercises concurrency runs under `-race`, and the plan says so per task. A concurrency test that has never run under `-race` has not been run.

## Beads

The epic is `rudy-ufq`. Two bugs this wave closes are filed separately and referenced by the tasks that fix them: `rudy-ef4` (a child's tool view is not bounded by its parent's) and `rudy-ulb` (the flaky cancel test). `rudy-0lz` tracks the real fix in memory-go and is **not** closed by this wave; Task 2 makes rudy correct regardless of it.

Claim each task's issue with `bd update <id> --claim` before starting it and close it in the same commit that lands the work.

---

## File Structure

**`internal/turn/runner.go`** — the scheduler. Today it holds `cancel`, `interrupt` and `state` as one each; it gains a per-`tool_use`-id inflight map and dispatches the calls of one assistant message concurrently. This is the largest single change in the wave and the one every other task assumes.

**`internal/turn/inflight.go`** (new) — the keyed set of running calls and its locking, split out so the runner file does not grow another hundred lines of bookkeeping and so the set can be tested on its own.

**`internal/session/gate.go`** (or wherever `Gate.Evaluate` lives) — ask coalescing: a call whose matcher and scope match an outstanding ask joins it rather than raising a second question.

**`internal/plugins/memory/tools.go`** — the memory plugin serializes its own invocations, because a tool that is not reentrant guards itself.

**`internal/server/server.go`** — `applyAgent` intersects the child's tool list with its parent's; `session.open` accepts a `tools` narrowing; `resolveAgent` merges the registry's agent definitions after the two disk roots.

**`internal/server/session_live.go`** — the downward fan-out walk: a child's observer notifications also reach the parent's non-plugin subscribers.

**`internal/plugin/registry.go`** — `RegisterAgent`, mirroring `RegisterProvider`: map and order, `stageRegister`, a commit branch, a withdraw in `Fail`, an `AgentDefs()` getter.

**`internal/plugin/plugin.go`** — `Host.RegisterAgent` on the interface.

**`internal/plugin/spawned.go`** — `Registrar.RegisterAgent` and the `Register` dispatch branch, so a spawned plugin contributes a definition over stdio.

**`internal/protocol/methods.go`** — `MethodPluginRegisterAgent` and its params; `tool.state` as a notification; `SessionOpenParams.Tools`.

**`internal/plugins/subagents/plugin.go`** — the `agent` tool's input gains `tools`; its description stops enumerating agents; a `session_opened` hook supplies the roster instead.

**`internal/tui/transcript/row.go`, `transcript.go`** — rows gain nesting so a child's work renders under the `agent` call that opened it.

**`internal/tui/app/model.go`** — stops dropping notifications whose session id is not the one being rendered, and routes them to the owning tool row.

---

## Task order and why

Tasks 1 and 2 land before concurrency because they are the two things concurrency would otherwise break, and both are testable on their own: the Gate by calling `Evaluate` concurrently in a test, the memory plugin by invoking it concurrently. Task 3 is the scheduler and closes `rudy-ulb`. Tasks 4 and 5 are the tool-list chain, which closes `rudy-ef4`. Tasks 6 and 7 are visibility, server then client. Tasks 8 through 10 are agents from plugins, ending with the roster hook that makes plugin-contributed definitions actually reachable by the model.

| task | deliverable | beads |
|---|---|---|
| 1 | The Gate coalesces concurrent asks onto one question | `rudy-ufq` |
| 2 | The memory plugin serializes its own invocations | `rudy-ufq` |
| 3 | The tool scheduler: concurrent dispatch, per-call cancel, broadcast interrupt, `tool.state` | `rudy-ufq`, closes `rudy-ulb` |
| 4 | A child's tool set is bounded by its parent's | closes `rudy-ef4` |
| 5 | `session.open` and the `agent` tool take a `tools` narrowing; the default definition drops the memory writes | `rudy-ufq` |
| 6 | A child's notifications reach its parent's subscribers | `rudy-ufq` |
| 7 | The TUI renders a live subagent under the call that opened it | `rudy-ufq` |
| 8 | `Host.RegisterAgent` and the registry | `rudy-ufq` |
| 9 | `plugin.register_agent` for spawned plugins | `rudy-ufq` |
| 10 | The agent roster moves to a `session_opened` hook | `rudy-ufq` |

---

## Task 1: Concurrent calls matching one question ask once

**Files:**
- Modify: `internal/server/session_live.go:67` (the `liveSession` fields), `:255` (`forget`), `:578` (`liveAsker.Ask`)
- Test: `internal/server/session_live_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks; this is the first.
- Produces: `liveAsker.Ask` may be called from several goroutines at once. Two calls whose `turn.Question.Matcher` is equal raise one `permission.requested` and share its answer. Task 3 relies on this being true before it lets calls overlap.

**Why here and not in the Gate:** `gate.Gate` holds only the dangerous list and `Evaluate` is a pure function of its `Input` (`internal/gate/gate.go:47`). It has no session to coalesce against. `liveAsker` already keys pending questions by `tool_use` id in `ls.pending` and already holds `ls.mu`, so the joining is the only new part.

**Semantics to implement:** an answer binds exactly the calls waiting on that question at the moment it is answered. A call arriving after a question resolves asks again, unless the answer was `session` scope, in which case the allowance now exists and `Evaluate` returns allow without asking at all.

- [ ] **Step 1: Write the failing test**

Add to `internal/server/session_live_test.go`:

```go
func TestConcurrentAsksOnOneMatcherAskOnce(t *testing.T) {
	ls, asker := newAskTestSession(t)

	var prompts atomic.Int32
	release := make(chan struct{})
	ls.subscribeAsker(t, func(req protocol.PermissionRequested) {
		prompts.Add(1)
		<-release
		ls.answer(t, req.ToolUseID, session.Allow, session.ScopeOnce)
	})

	q := turn.Question{Tool: "bash", Matcher: session.Matcher{Tool: "bash", Prefix: "git status"}}
	var wg sync.WaitGroup
	answers := make([]turn.Answer, 3)
	errs := make([]error, 3)
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			one := q
			one.ToolUseID = fmt.Sprintf("tu%d", i)
			answers[i], errs[i] = asker.Ask(context.Background(), one)
		}()
	}
	// Let all three reach the asker before any answer lands.
	waitFor(t, func() bool { return ls.waitingAsks() == 3 })
	close(release)
	wg.Wait()

	if got := prompts.Load(); got != 1 {
		t.Fatalf("the operator was asked %d times, want 1", got)
	}
	for i := range 3 {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if answers[i].Decision != session.Allow {
			t.Fatalf("call %d decision = %q, want allow", i, answers[i].Decision)
		}
	}
}

func TestDifferentMatchersAskSeparately(t *testing.T) {
	ls, asker := newAskTestSession(t)
	var prompts atomic.Int32
	ls.subscribeAsker(t, func(req protocol.PermissionRequested) {
		prompts.Add(1)
		ls.answer(t, req.ToolUseID, session.Allow, session.ScopeOnce)
	})

	var wg sync.WaitGroup
	for i, prefix := range []string{"git status", "go build"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = asker.Ask(context.Background(), turn.Question{
				ToolUseID: fmt.Sprintf("tu%d", i), Tool: "bash",
				Matcher: session.Matcher{Tool: "bash", Prefix: prefix},
			})
		}()
	}
	wg.Wait()
	if got := prompts.Load(); got != 2 {
		t.Fatalf("prompts = %d, want 2: different matchers are different questions", got)
	}
}
```

`newAskTestSession`, `subscribeAsker`, `answer`, `waitingAsks` and `waitFor` are helpers this task adds alongside the tests; `waitingAsks` reads `len(ls.asking)` under `ls.mu`, and `waitFor` polls a predicate to a deadline and fails the test if it never holds.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race ./internal/server/ -run 'ConcurrentAsks|DifferentMatchers' -v`
Expected: FAIL. `TestConcurrentAsksOnOneMatcherAskOnce` reports three prompts where one was wanted, because every call raises its own question today.

- [ ] **Step 3: Add the coalescing state to `liveSession`**

In `internal/server/session_live.go`, beside `pending` at `:67`:

```go
	// asking is the questions currently in front of the operator, keyed by what they ask
	// rather than by which call asked. Tool calls run concurrently, so several calls can want
	// the same permission at once; they share one question and its answer instead of
	// prompting the operator once per call (ADR 0028).
	asking map[askKey]*standingAsk
```

And the two types, near the other `liveSession` helpers:

```go
// askKey is the question a call would put to the operator. Two calls with the same matcher
// ask the same thing, whatever their tool_use ids are.
type askKey struct{ tool, prefix string }

// standingAsk is one outstanding question and the answer the calls waiting on it will take.
// done is closed once ans and err are final; nothing reads them before that.
type standingAsk struct {
	done chan struct{}
	ans  turn.Answer
	err  error
}
```

Initialize `asking` wherever `pending` is initialized.

- [ ] **Step 4: Make `Ask` join an outstanding question**

Rewrite `liveAsker.Ask` (`session_live.go:578`) so the existing body becomes the path taken only by the call that raises the question:

```go
func (a *liveAsker) Ask(ctx context.Context, q turn.Question) (turn.Answer, error) {
	key := askKey{tool: q.Matcher.Tool, prefix: q.Matcher.Prefix}
	a.ls.mu.Lock()
	if st, ok := a.ls.asking[key]; ok {
		a.ls.mu.Unlock()
		select {
		case <-st.done:
			return st.ans, st.err
		case <-ctx.Done():
			// This call is going away; the question stands for whoever else is waiting.
			return turn.Answer{}, ctx.Err()
		}
	}
	st := &standingAsk{done: make(chan struct{})}
	a.ls.asking[key] = st
	a.ls.mu.Unlock()

	ans, err := a.ask(ctx, q)

	a.ls.mu.Lock()
	delete(a.ls.asking, key)
	a.ls.mu.Unlock()
	st.ans, st.err = ans, err
	close(st.done)
	return ans, err
}
```

Move the current body of `Ask` verbatim into a new unexported `func (a *liveAsker) ask(ctx context.Context, q turn.Question) (turn.Answer, error)`. It is unchanged: it still registers `ls.pending[q.ToolUseID]`, calls `ls.stand`, notifies the askers and selects on the answer, `abandon` and `ctx`.

- [ ] **Step 5: Correct the stale doc comment**

`Ask`'s comment at `session_live.go:574-577` says it runs on the Runner's own goroutine between steps. Task 3 makes that false and this task already does. Replace that sentence with:

```go
// Ask is called from every goroutine running a tool call in the turn, so it may be re-entered
// while an earlier question stands. Calls that ask the same thing share one question: the
// operator sees one prompt and every call waiting on it when it is answered takes that answer.
// A call that arrives after a question resolves asks again, unless the answer was session
// scope, in which case the allowance exists and the Gate never asks.
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -race ./internal/server/ -run 'ConcurrentAsks|DifferentMatchers' -v`
Expected: PASS, both.

- [ ] **Step 7: Run the package over both transports**

Run: `go test -race ./internal/server/ && RUDY_TEST_TRANSPORT=socket go test -race ./internal/server/`
Expected: PASS. The asker path is exercised over the socket too, which is why both runs matter.

- [ ] **Step 8: Commit**

```bash
git add internal/server/session_live.go internal/server/session_live_test.go
git commit -m "server: calls asking the same thing share one question"
```

---

## Task 2: The memory plugin serializes its own invocations

**Files:**
- Modify: `internal/plugins/memory/tools.go:68-95` (the plugin struct and `registerTools`), `:116` (`remember`)
- Test: `internal/plugins/memory/tools_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `memory_remember` is safe to invoke concurrently. Task 3 assumes every shipped tool is, because the scheduler makes no exceptions.

**Why:** `memory.Remember` (`../memory/memory-go/write.go:36`) reads the existing concept at line 52, builds a revision from that base and writes it back, with no lock. Two concurrent calls on one title both read the same base and the later write discards the earlier revision. ADR 0028 decision 3 puts the obligation on the tool that owns the state. `rudy-0lz` tracks the real fix in memory-go and is not closed here.

- [ ] **Step 1: Write the failing test**

```go
func TestConcurrentRememberKeepsEveryRevision(t *testing.T) {
	p := newTestMemPlugin(t)
	call := func(body string) tool.Call {
		in, err := json.Marshal(map[string]any{
			"type": "fact", "title": "Concurrent title", "description": "d", "body": body,
		})
		if err != nil {
			t.Fatal(err)
		}
		return tool.Call{ID: body, Name: "memory_remember", Input: in, SessionID: session.NewID()}
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

	got := p.revisionCount(t, "fact", "concurrent-title")
	if got != 3 {
		t.Fatalf("the concept carries %d revisions, want 3: a concurrent write was lost", got)
	}
}
```

`newTestMemPlugin` builds the plugin over a bundle rooted at `t.TempDir()`, and `revisionCount` reads the concept file back and counts its recorded revisions. Both are helpers this task adds.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -race ./internal/plugins/memory/ -run ConcurrentRemember -v`
Expected: FAIL, either on the revision count or with a `-race` report inside `memory.Remember`.

- [ ] **Step 3: Add the lock**

In `internal/plugins/memory/tools.go`, on the `memPlugin` struct:

```go
	// writes serializes memory_remember. memory.Remember reads a concept, revises it and
	// writes it back, so two at once lose a revision. Tool calls run concurrently (ADR 0028)
	// and a tool that is not reentrant guards itself rather than asking the scheduler to.
	// rudy-0lz fixes this properly in memory-go; this keeps rudy correct meanwhile.
	writes sync.Mutex
```

At the top of `remember` (`tools.go:116`), after the input is unmarshalled and before `p.scopeOf`:

```go
	p.writes.Lock()
	defer p.writes.Unlock()
```

Leave `recall` and `recallObservation` unlocked: they only read, and a read racing a write reads either the old concept or the new one, both of which are concepts that existed.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -race ./internal/plugins/memory/ -run ConcurrentRemember -v -count=5`
Expected: PASS on every run. `-count=5` because one green run of a concurrency test proves very little.

- [ ] **Step 5: Commit**

```bash
git add internal/plugins/memory/tools.go internal/plugins/memory/tools_test.go
git commit -m "memory: remember serializes itself, since it revises in place"
```

---

## Task 3: The tool scheduler

**Files:**
- Create: `internal/turn/inflight.go`, `internal/turn/inflight_test.go`
- Modify: `internal/turn/runner.go:126-141` (fields), `:201-206` (`Interrupt`), `:351-374` (the loop), `:426` (`runTool`), `:848-884` (state, cancel, interrupt)
- Modify: `internal/protocol/methods.go` (`NotifyToolState`, `ToolStateChanged`)
- Modify: `internal/turn/runner.go` Observer interface, and every implementation of it
- Test: `internal/turn/runner_test.go`, `internal/turn/inflight_test.go`

**Interfaces:**
- Consumes: Task 1's coalescing asker, Task 2's reentrant memory tool.
- Produces:
  - `type inflight struct` with `add(id string, cancel context.CancelFunc)`, `remove(id string)`, `cancelAll()`, `len() int`.
  - `Observer.ToolStateChanged(turnID, toolUseID, name string, state ToolState)` alongside the existing `StateChanged`.
  - `protocol.NotifyToolState = "tool.state"` and `protocol.ToolStateChanged{SessionID, TurnID, ToolUseID, Name, State string}`.
  - Tasks 6 and 7 consume the notification; nothing else consumes `inflight`.

**What breaks and must be handled:** `runner.go:129-132` documents that `Run`, `loop`, `stream` and `runTool` execute in sequence on one goroutine, and that this is why `system` and `turnUsage` need no lock. Concurrency ends that. `turnUsage` accumulation moves under `r.mu`; `system` is written once before any call runs and stays lock-free, with its comment corrected to say why.

This task closes `rudy-ulb`: that test flakes because `Cancel` races a child on the single `r.cancel` slot, which stops existing here.

- [ ] **Step 1: Write the failing scheduler tests**

```go
func TestToolCallsRunConcurrently(t *testing.T) {
	s := openTestSession(t, session.ModePermissive)
	entered := make(chan string, 2)
	release := make(chan struct{})
	slow := tool.Tool{
		Name: "slow", Description: "waits", Schema: json.RawMessage(`{"type":"object"}`), Safety: tool.Safe,
		Invoke: func(ctx context.Context, c tool.Call) (tool.Result, error) {
			entered <- c.ID
			<-release
			return tool.Result{Content: []session.Block{session.TextBlock("done " + c.ID)}}, nil
		},
	}
	p := &scripted{scripts: [][]provider.Part{
		append(append([]provider.Part{text("both")},
			append(toolCall("tu1", "slow", `{}`), toolCall("tu2", "slow", `{}`)...)...),
			usage(5, 5), stop(session.StopToolUse, "tool_calls")),
		{text("finished"), usage(6, 1), stop(session.StopEndTurn, "stop")},
	}}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{"slow": slow}, nil, rec)

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), userMsg(session.SourceTyped, "go")) }()

	// Both calls must be inside Invoke at once. Serial dispatch cannot satisfy this: the
	// second never starts until the first returns, and the first is blocked on release.
	first := <-entered
	second := <-entered
	if first == second {
		t.Fatalf("the same call entered twice: %q", first)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if got := rec.count(session.KindToolResult); got != 2 {
		t.Fatalf("tool results = %d, want 2", got)
	}
}

func TestInterruptStopsEveryRunningCall(t *testing.T) {
	s := openTestSession(t, session.ModePermissive)
	started := make(chan struct{}, 2)
	blocking := tool.Tool{
		Name: "block", Description: "blocks", Schema: json.RawMessage(`{"type":"object"}`), Safety: tool.Safe,
		Invoke: func(ctx context.Context, c tool.Call) (tool.Result, error) {
			started <- struct{}{}
			<-ctx.Done()
			return tool.Result{}, ctx.Err()
		},
	}
	p := &scripted{scripts: [][]provider.Part{
		append(append([]provider.Part{text("two")},
			append(toolCall("tu1", "block", `{}`), toolCall("tu2", "block", `{}`)...)...),
			usage(5, 5), stop(session.StopToolUse, "tool_calls")),
	}}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{"block": blocking}, nil, rec)

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), userMsg(session.SourceTyped, "go")) }()
	<-started
	<-started
	r.Interrupt(session.InterruptCancel)
	<-done

	// Every call that was running records killed. None records success: an interrupt is
	// observed by each call, not consumed by whichever looked first.
	results := rec.payloads(session.KindToolResult)
	if len(results) != 2 {
		t.Fatalf("tool results = %d, want 2", len(results))
	}
	for _, p := range results {
		tr := p.(session.ToolResult)
		if tr.Outcome != session.OutcomeKilled {
			t.Fatalf("call %s outcome = %q, want killed", tr.ToolUseID, tr.Outcome)
		}
	}
}

func TestToolStateReportsEachCall(t *testing.T) {
	s := openTestSession(t, session.ModePermissive)
	p := &scripted{scripts: [][]provider.Part{
		append(append([]provider.Part{text("two")},
			append(toolCall("tu1", "echo", `{}`), toolCall("tu2", "echo", `{}`)...)...),
			usage(5, 5), stop(session.StopToolUse, "tool_calls")),
		{text("done"), usage(6, 1), stop(session.StopEndTurn, "stop")},
	}}
	rec := &recorder{}
	r := newRunner(t, s, p, toolSet{"echo": echoTool()}, nil, rec)
	if err := r.Run(context.Background(), userMsg(session.SourceTyped, "go")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"tu1", "tu2"} {
		if !rec.sawToolState(id, ToolRunning) || !rec.sawToolState(id, ToolDone) {
			t.Fatalf("call %s was not reported running and done: %+v", id, rec.toolStates)
		}
	}
}
```

`rec.count`, `rec.payloads`, `rec.sawToolState` and the `toolStates` field are added to `recorder` in `runner_test.go` in this task. They replace positional indexing for concurrent tests: with calls overlapping, `rec.entries[2]` is no longer a fixed entry.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race ./internal/turn/ -run 'ToolCallsRunConcurrently|InterruptStopsEvery|ToolStateReports' -v`
Expected: FAIL. `TestToolCallsRunConcurrently` blocks and times out, because the second call never starts while the first holds.

- [ ] **Step 3: Write `internal/turn/inflight.go`**

```go
package turn

import (
	"context"
	"sync"
)

// inflight is the set of tool calls currently running in a turn, keyed by tool_use id. It
// replaces the Runner's single cancel slot: calls of one assistant message run at once
// (ADR 0028), so cancelling the turn has to reach every one of them rather than whichever
// registered last.
type inflight struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	// cancelled is set once cancelAll has run, so a call registering afterwards is cancelled
	// immediately rather than running on past an interrupt that already happened.
	cancelled bool
}

func newInflight() *inflight { return &inflight{cancels: map[string]context.CancelFunc{}} }

// add registers a running call. It cancels immediately, and reports false, when the turn has
// already been cancelled.
func (f *inflight) add(id string, cancel context.CancelFunc) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelled {
		cancel()
		return false
	}
	f.cancels[id] = cancel
	return true
}

func (f *inflight) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.cancels, id)
}

// cancelAll cancels every running call and every call that registers later.
func (f *inflight) cancelAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = true
	for _, c := range f.cancels {
		c()
	}
}

func (f *inflight) len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cancels)
}
```

- [ ] **Step 4: Write `internal/turn/inflight_test.go`**

```go
func TestInflightCancelsEveryRegisteredCall(t *testing.T) {
	f := newInflight()
	var a, b atomic.Bool
	f.add("tu1", func() { a.Store(true) })
	f.add("tu2", func() { b.Store(true) })
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
}
```

Run: `go test -race ./internal/turn/ -run Inflight -v`
Expected: PASS.

- [ ] **Step 5: Add the `tool.state` protocol surface**

In `internal/protocol/methods.go`, beside `NotifyTurnState`:

```go
	NotifyToolState = "tool.state"
```

and with the other notification payloads:

```go
// ToolStateChanged reports where one tool call has got to. Tool calls of an assistant message
// run concurrently (ADR 0028), so turn.state cannot say which of several is waiting on the
// operator; this can. Not replayed: a finished call is its tool_result entry.
type ToolStateChanged struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	ToolUseID string `json:"tool_use_id"`
	Name      string `json:"name"`
	State     string `json:"state"`
}

// The states a tool call is reported in.
const (
	ToolStateRunning            = "running"
	ToolStateAwaitingPermission = "awaiting_permission"
	ToolStateDone               = "done"
)
```

- [ ] **Step 6: Extend the Observer and the Runner**

Add to the `Observer` interface in `internal/turn/runner.go`:

```go
	// ToolStateChanged reports one call's progress. Several calls run at once, so this carries
	// the tool_use id that StateChanged cannot.
	ToolStateChanged(turnID, toolUseID, name string, state ToolState)
```

with `type ToolState string` and the three constants mirroring the protocol ones. Implement it on `recorder` in `runner_test.go`, on the server's observer, and on any other implementation `go build ./...` turns up.

Replace the Runner's `cancel context.CancelFunc` field with `calls *inflight`, initialized in `NewRunner`. Then:

- `Interrupt` (`:201-206`) sets `r.interrupt` under `r.mu` as it does now, then calls `r.calls.cancelAll()` outside the lock, and also cancels the stream's own cancel func, which stays a single slot because there is only ever one stream.
- `setCancel` becomes `r.calls.add(tu.ID, cancel)` at the call site in `runTool`, with `r.calls.remove(tu.ID)` deferred.
- `setStateLocked` (`:854`) stops clearing `r.cancel`; that line goes, since the inflight set is emptied by each call removing itself.
- `takeInterrupt` (`:878`) becomes `observeInterrupt`, which reads `r.interrupt` under `r.mu` and **does not clear it**:

```go
// observeInterrupt reports the interrupt this turn is under, if any. It does not consume it:
// every call in flight has to see it, or the ones that did not would record success for a turn
// that was cut (ADR 0028). It is cleared when the turn rests, not when a call reads it.
func (r *Runner) observeInterrupt() session.Interrupt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.interrupt
}
```

Add `clearInterrupt()` and call it where the turn comes to rest, in `rest` and in `finishInterrupt`.

- [ ] **Step 7: Make the loop dispatch concurrently**

Replace the serial `for _, tu := range toolUses` at `runner.go:359-372` with:

```go
		// Every call of this message runs at once (ADR 0028). Malformed inputs are refused
		// first and in order, since they append without running anything and keeping them
		// ordered keeps the log readable.
		var runnable []session.Block
		for _, tu := range toolUses {
			if raw, bad := malformed[tu.ID]; bad {
				if err := r.refuse(ctx, r.classDeny(tu, "malformed input"), session.OutcomeError, "malformed tool input: "+raw); err != nil {
					return r.fail(session.ErrInternal, err)
				}
				continue
			}
			runnable = append(runnable, tu)
		}
		if len(runnable) > 0 {
			r.setState(RunningTool)
			var wg sync.WaitGroup
			outcomes := make([]toolOutcome, len(runnable))
			for i, tu := range runnable {
				wg.Add(1)
				go func() {
					defer wg.Done()
					done, err := r.runTool(ctx, tu)
					outcomes[i] = toolOutcome{done: done, err: err}
				}()
			}
			wg.Wait()
			// One slot per call, read in call order, so the turn ends on the same outcome
			// whatever order the calls finished in.
			for _, o := range outcomes {
				if o.err != nil {
					return o.err
				}
				if o.done {
					return nil
				}
			}
		}
```

with `type toolOutcome struct { done bool; err error }` beside it. Each goroutine writes its own index, so the slice needs no lock.

`runTool` keeps its signature. Inside it: `r.setState(RunningTool)` at `:533` goes, since the loop sets it once for the group; `r.setCancel(cancel)` at `:544` becomes `r.calls.add(tu.ID, cancel)`; and it reports its call's progress with `r.cfg.Observer.ToolStateChanged` on entry, when it starts awaiting permission, and on the way out.

- [ ] **Step 8: Put `turnUsage` under the lock**

`runner.go:129-132` says `system` and `turnUsage` are lock-free because one goroutine touches them. That is now false for `turnUsage`, which every finishing call adds to. Guard every read and write of it with `r.mu`, and correct the comment:

```go
	// system is written once before the turn's first request and read by every goroutine
	// afterwards, so it needs no lock. turnUsage is accumulated by each tool call as it
	// finishes and so is guarded by mu: calls run concurrently (ADR 0028).
```

- [ ] **Step 9: Repair the tests concurrency invalidates**

`runner_test.go` asserts positionally on `rec.entries[N]` and on exact `rec.states` sequences. Both are order-dependent and several tests now have no fixed order. For each failing test: if it uses one tool call, it still has a fixed order and can stay as it is; if it uses more than one, convert it to the kind-count and payload-lookup helpers from Step 1. Do not weaken an assertion to make it pass. A test that genuinely wants one call at a time should script one call.

Run: `go test -race ./internal/turn/ -v`
Expected: PASS.

- [ ] **Step 10: Verify `rudy-ulb` is fixed**

Run: `go test -race ./internal/plugins/subagents/ -run TestCancelInterruptsTheChildAndWaitsForIt -count=20`
Expected: PASS on all 20. This test flaked because `Cancel` raced the child on the one cancel slot; the slot is gone.

If it still flakes, stop and treat it as a real bug in this task rather than a pre-existing one: the cancel path is what this task rewrote.

- [ ] **Step 11: Full check**

Run: `make check`
Expected: PASS.

- [ ] **Step 12: Commit**

```bash
git add internal/turn internal/protocol internal/server
git commit -m "turn: tool calls of one message run at once"
bd close rudy-ulb --reason "the single cancel slot the race needed no longer exists"
```

---

## Task 4: A child's tool set is bounded by its parent's

**Files:**
- Modify: `internal/server/server.go:1187-1203` (`applyAgent`)
- Test: `internal/server/server_test.go`

**Interfaces:**
- Consumes: nothing from Tasks 1 to 3.
- Produces: `applyAgent` intersects with the parent's effective tool list. Task 5 adds the caller's narrowing to the same chain.

**Why:** `applyAgent` builds the child's view with `plugin.NewToolView(s.d.Plugins, def.Tools, deny)`, which starts from the whole registry. A parent restricted to `[read, agent]` can open a child holding `bash`. Removal is the only way to restrict an agent, so delegation must not route around it. Closes `rudy-ef4`.

- [ ] **Step 1: Write the failing test**

```go
func TestChildToolsCannotExceedItsParent(t *testing.T) {
	srv, cn := newTestServer(t)
	writeAgentDef(t, srv, "narrow", "reads only", []string{"read", "agent"})
	writeAgentDef(t, srv, "wide", "everything", nil) // absent tools means every tool

	parent := openSession(t, cn, sessionOpenParams{Agent: "narrow"})
	child := openChildSession(t, cn, parent, "wide")

	got := toolNames(t, srv, child)
	for _, banned := range []string{"bash", "edit", "write"} {
		if slices.Contains(got, banned) {
			t.Fatalf("the child holds %q, which its parent does not: %v", banned, got)
		}
	}
	if !slices.Contains(got, "read") {
		t.Fatalf("the child lost read, which both it and its parent hold: %v", got)
	}
	if slices.Contains(got, "agent") {
		t.Fatalf("a child still sees the agent tool: %v", got)
	}
}
```

`writeAgentDef`, `openChildSession` and `toolNames` are helpers this task adds; `openChildSession` opens with `parent` set, the way the subagents plugin does.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -race ./internal/server/ -run ChildToolsCannotExceed -v`
Expected: FAIL, reporting the child holds `bash`.

- [ ] **Step 3: Intersect in `applyAgent`**

```go
// applyAgent stamps a definition onto a session that is not yet shared: its tool view, its
// system prompt and its step limit. A child never sees the agent tool, even when its
// definition lists it, which is what keeps subagent depth at one: with no tool to call, a child
// cannot open a grandchild.
//
// A child's list is also intersected with its parent's. Restricting an agent is done by
// removing tools from its list, so delegation must not hand out what the parent does not hold,
// or the restriction means nothing (ADR 0028, rudy-ef4).
func (s *Server) applyAgent(ls *liveSession, def agentdef.Definition) {
	var deny []string
	allow := def.Tools
	if ls.parent != nil || openedAsChild(ls.entries) {
		deny = []string{"agent"}
		allow = intersectTools(allow, parentToolNames(ls))
	}
	if allow != nil || deny != nil {
		ls.tools = plugin.NewToolView(s.d.Plugins, allow, deny)
	}
	ls.system = def.Prompt
	ls.maxSteps = def.MaxTurns
}

// intersectTools narrows want by have. A nil want means every tool, so the result is have; a
// nil have means the parent is unrestricted, so the result is want. Only ever removes.
func intersectTools(want, have []string) []string {
	switch {
	case have == nil:
		return want
	case want == nil:
		return slices.Clone(have)
	}
	out := make([]string, 0, min(len(want), len(have)))
	for _, n := range want {
		if slices.Contains(have, n) {
			out = append(out, n)
		}
	}
	return out
}

// parentToolNames is the parent's effective list, or nil when the parent holds every tool. It
// reads the live parent rather than the log: a child is opened while its parent is live, and
// the parent's own view is already the intersection of everything above it.
func parentToolNames(ls *liveSession) []string {
	if ls.parent == nil || ls.parent.tools == nil {
		return nil
	}
	all := ls.parent.tools.Tools()
	out := make([]string, 0, len(all))
	for _, t := range all {
		out = append(out, t.Name)
	}
	return out
}
```

Note the empty-list case: an explicit empty `tools` in a definition means no tools, and `intersectTools` returns an empty non-nil slice for it, which `NewToolView` already reads as "permit nothing". Do not collapse empty to nil.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -race ./internal/server/ -run ChildToolsCannotExceed -v`
Expected: PASS.

- [ ] **Step 5: Test the resumed child**

A child resumed after its parent is gone has no live parent. `applyAgentFromLog` runs then, and `ls.parent` is nil, so the intersection is skipped and the child gets its definition's list. Add a test that documents this deliberately:

```go
func TestResumedChildKeepsItsOwnList(t *testing.T) {
	// A child resumed long after its parent is gone has no parent to intersect with. It keeps
	// its own definition's list, which is what it ran under, and cannot gain tools by being
	// resumed: the definition is the same file it was opened with.
}
```

Fill the body in the style of the other resume tests in the package.

- [ ] **Step 6: Both transports, then commit**

Run: `go test -race ./internal/server/ && RUDY_TEST_TRANSPORT=socket go test -race ./internal/server/`

```bash
git add internal/server/server.go internal/server/server_test.go
git commit -m "server: a child holds no tool its parent lacks"
bd close rudy-ef4
```

---

## Task 5: The caller narrows per call, and the default agent stops writing memory

**Files:**
- Modify: `internal/protocol/methods.go` (`SessionOpenParams.Tools`)
- Modify: `internal/server/server.go` (the `session.open` handler, threading `Tools` into `applyAgent`)
- Modify: `internal/plugins/subagents/plugin.go:27` (schema), `:66` (`invoke`)
- Create: `examples/agents/default.md`
- Test: `internal/server/server_test.go`, `internal/plugins/subagents/plugin_test.go`

**Interfaces:**
- Consumes: Task 4's `intersectTools`.
- Produces: `session.open` accepts `tools?: [string]`; the `agent` tool's input accepts `tools?: [string]`.

- [ ] **Step 1: Write the failing tests**

```go
func TestSessionOpenToolsOnlyNarrows(t *testing.T) {
	srv, cn := newTestServer(t)
	writeAgentDef(t, srv, "wide", "everything", nil)

	// Asking for more than the definition grants gains nothing: the set is an intersection.
	s := openSession(t, cn, sessionOpenParams{Agent: "wide", Tools: []string{"read", "nonexistent"}})
	got := toolNames(t, srv, s)
	if slices.Contains(got, "nonexistent") {
		t.Fatalf("a name no tool answers to was admitted: %v", got)
	}
	if slices.Contains(got, "bash") {
		t.Fatalf("tools did not narrow: %v", got)
	}
	if !slices.Contains(got, "read") {
		t.Fatalf("read was dropped: %v", got)
	}
}

func TestAgentToolPassesItsNarrowing(t *testing.T) {
	// The agent tool's input carries tools through to session.open, so the orchestrating model
	// decides what this delegation needs.
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/server/ ./internal/plugins/subagents/ -run 'ToolsOnlyNarrows|PassesItsNarrowing' -v`
Expected: FAIL; `Tools` is not a field yet, so this will not compile until Step 3.

- [ ] **Step 3: Add `Tools` to `SessionOpenParams`**

```go
	// Tools narrows the session's tool set to these names. It only ever removes: a name the
	// agent definition or the parent did not hold is dropped rather than refused, since the
	// set is an intersection and asking for less than you are owed is not an error (ADR 0028).
	Tools []string `json:"tools,omitempty"`
```

Thread it into `applyAgent` as a third term in the chain: `allow = intersectTools(allow, params.Tools)` before the parent intersection. Order does not matter mathematically; do it first so the parent bound is applied last and is visibly the outer limit.

- [ ] **Step 4: Add `tools` to the `agent` tool**

In `internal/plugins/subagents/plugin.go`, extend the schema at `:27`:

```go
const schema = `{"type":"object","properties":{"agent":{"type":"string","description":"An agent definition name from agents/<name>.md"},"prompt":{"type":"string","description":"The task for the subagent"},"tools":{"type":"array","items":{"type":"string"},"description":"Narrow the subagent to these tools. Only removes: naming a tool the definition or this session does not hold does not grant it."}},"required":["agent","prompt"],"additionalProperties":false}`
```

Add `Tools []string `json:"tools"`` to the anonymous struct in `invoke` (`:67`) and pass it in `SessionOpenParams`.

- [ ] **Step 5: Ship a default agent definition that does not write memory**

Create `examples/agents/default.md`:

```markdown
---
description: The default subagent: gathers, reasons and reports back
tools: [read, grep, glob, bash, edit, write]
---

You are a subagent. You were given one task by the session that called you, and
your answer is the only thing that reaches it: your transcript is not read.

Say what you found, what you changed if anything, and what you could not
determine. If the task was a question, answer it. If you could not finish, say
where you stopped and why, rather than reporting a partial result as a whole one.

Do not record anything durable. What is worth keeping is the caller's decision,
not yours, and it cannot make that decision about a conclusion it never saw.
```

The `tools` list omits `memory_remember`, `memory_recall` and `memory_recall_observation`. It is a ceiling, not a grant: a session still only gets these intersected with what its parent holds.

- [ ] **Step 6: Run the tests, then the whole suite**

Run: `go test -race ./internal/server/ ./internal/plugins/subagents/ -v`, then `make check`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/protocol internal/server internal/plugins/subagents examples/agents
git commit -m "session: a caller narrows the tool set it opens with"
```

---

## Task 6: A child's notifications reach its parent's subscribers

**Files:**
- Modify: `internal/server/session_live.go:316` (`broadcastObsLocked`), and the `liveSession` fan-out helpers around `:193-209`
- Test: `internal/server/session_live_test.go`

**Interfaces:**
- Consumes: Task 3's `tool.state`, which is one of the notifications that now walks.
- Produces: a client subscribed to a parent receives the child's `entry.appended`, `stream.delta`, `turn.state` and `tool.state`, each carrying the child's `session_id`. Task 7 consumes exactly this.

**Why this shape:** `broadcastObsLocked` sends to `ls.conns`, which only `subscribeLocked` populates, and the sole subscriber to a child is the connection the subagents plugin opened. The mirror already exists going the other way: `askers()` (`:201-209`) walks `ls.parent` up one link when a child has no asker of its own, and the lock discipline for a parent walk is documented at `:193-195`. Not a `session.watch` method: `ownSession` (`server.go:428`) gates submit and interrupt on subscription, so subscribing a client to a child in order to watch it would also let it drive the child.

- [ ] **Step 1: Write the failing test**

```go
func TestAParentsClientSeesItsChildsWork(t *testing.T) {
	srv, cn := newTestServer(t)
	parent := openSession(t, cn, sessionOpenParams{})
	watcher := attach(t, srv, parent) // a second, non-plugin connection on the parent

	child := openChildSession(t, cn, parent, "default")
	appendAssistantText(t, srv, child, "what the subagent found")

	n := waitForNotification(t, watcher, protocol.NotifyEntryAppended, func(p protocol.EntryAppended) bool {
		return p.SessionID == child
	})
	if n.SessionID != child {
		t.Fatalf("session id = %q, want the child's %q", n.SessionID, child)
	}
	if n.SessionID == parent {
		t.Fatal("the child's entry arrived tagged as the parent's own")
	}
}

func TestAParentsClientCannotDriveTheChild(t *testing.T) {
	srv, cn := newTestServer(t)
	parent := openSession(t, cn, sessionOpenParams{})
	watcher := attach(t, srv, parent)
	child := openChildSession(t, cn, parent, "default")

	// Seeing a session is not owning it: the watcher is not subscribed to the child.
	err := call(t, watcher, protocol.MethodSessionSubmit, protocol.SessionSubmitParams{
		SessionID: child, Content: []session.Block{session.TextBlock("do as I say")}, Source: session.SourceTyped,
	})
	if err == nil {
		t.Fatal("a client that only watches a child was allowed to submit to it")
	}
}

func TestThePluginDoesNotReceiveItsOwnChildsEcho(t *testing.T) {
	// The connection the subagents plugin opened is the child's own subscriber and is already
	// reading these notifications. It must not also receive them as a parent's subscriber.
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/server/ -run 'ParentsClientSees|ParentsClientCannotDrive|DoesNotReceiveItsOwn' -v`
Expected: FAIL on the first; the watcher never receives the child's entry.

- [ ] **Step 3: Walk down to the parent in the fan-out**

In `internal/server/session_live.go`, extend `broadcastObsLocked` so that after sending to its own subscribers it also sends to its parent's, and add the helper that collects them:

```go
// watchers is the parent's subscribers, which also see this session's work so a client can
// watch the subagents it dispatched (ADR 0028). The mirror of askers, which walks the same
// link upward when a child has no asker of its own.
//
// The plugin connection that opened this child is excluded: it is the one waiting on the tool
// call and has no use for its own echo. Receiving these confers nothing, since authority over a
// session follows subscription and a parent's client is not subscribed to the child.
//
// Caller holds obsMu. The parent's obsMu is taken only after this session's is released, the
// ordering fixed at the top of this file; depth is one, so the walk does not recurse.
func (ls *liveSession) watchers() []*conn {
	if ls.parent == nil {
		return nil
	}
	ls.parent.obsMu.RLock()
	defer ls.parent.obsMu.RUnlock()
	out := make([]*conn, 0, len(ls.parent.conns))
	for _, c := range ls.parent.conns {
		if c.class == callerPlugin {
			continue
		}
		out = append(out, c)
	}
	return out
}
```

Follow the file's existing lock ordering exactly. If `broadcastObsLocked` is called with `obsMu` held, collect the watchers into a slice, release, then notify, the way the file already handles the asker walk. Do not hold two sessions' `obsMu` at once.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/server/ -run 'ParentsClientSees|ParentsClientCannotDrive|DoesNotReceiveItsOwn' -v -count=5`
Expected: PASS on every run.

- [ ] **Step 5: Track in-flight tool states so a client attaching mid-turn sees them**

The `tool.state` contracts row says every call in flight is sent to a client attaching mid-turn, after the replay and before the resume response. Task 3 added the notification but nothing records which calls are currently in flight, so there is nothing to send. This step owns that row.

On `liveSession`, keep the current state per `tool_use` id: set it when a `tool.state` of `running` or `awaiting_permission` is emitted, and drop the entry on `done`. Send the surviving set to a connection attaching while a turn is active, in the same place the current `turn.state` is sent, before the resume response. A finished call is not replayed: it is its `tool_result` entry and the client reads it from the log.

```go
func TestAClientAttachingMidTurnSeesCallsInFlight(t *testing.T) {
	// Two calls running, one awaiting permission. A client attaching now must receive a
	// tool.state for each before the resume response, or it renders a turn with no visible
	// tool activity until something finishes.
}
```

Fill the body in the style of the package's other attach tests. Assert on the notifications the attaching connection receives, and assert that a call which already completed is NOT among them.

- [ ] **Step 6: Confirm no replay to the parent on attach**

The contract says a child's entries are not replayed to a parent's client on attach: the parent's own log carries the `agent` tool result, and a finished child is read by resuming it. Confirm the attach path does not walk down, and add a test asserting a client attaching to a parent with a finished child receives no entries for that child.

- [ ] **Step 7: Both transports, then commit**

Run: `go test -race ./internal/server/ && RUDY_TEST_TRANSPORT=socket go test -race ./internal/server/`

```bash
git add internal/server/session_live.go internal/server/session_live_test.go
git commit -m "server: a parent's client sees the work it delegated"
```

---

## Task 7: The TUI renders a live subagent

**Files:**
- Modify: `internal/tui/transcript/row.go:38-66` (the `Row` struct)
- Modify: `internal/tui/transcript/transcript.go:105` (`Apply`), `:163` (`toolRow`), `:269` (`Delta`), `:343-372` (`layout`)
- Modify: `internal/tui/app/model.go:418-437` (the drop), `:722-740` (`sessionScoped`)
- Test: `internal/tui/transcript/transcript_test.go`, `internal/tui/app/golden_test.go`

**Interfaces:**
- Consumes: Task 6's parent-routed notifications and Task 3's `tool.state`.
- Produces: nothing later tasks depend on.

**The problem in the current model:** rows are a flat ordered slice with no session field (`row.go:38-66`), keyed by entry id or bare `tool_use` id. `Apply` and `Delta` are keyed purely by turn and `tool_use` id; no session id reaches either. `model.go:436` drops any notification whose session id is not the one on screen. So three things change: rows learn which session they came from, the drop learns to keep a child's, and `layout` indents a child's rows under the `agent` row that opened it.

- [ ] **Step 1: Write the failing transcript test**

```go
func TestChildRowsNestUnderTheAgentCall(t *testing.T) {
	tr := newTestTranscript(t)
	// The parent's agent call.
	tr.Apply(assistantEntry(t, session.ToolUseBlock("tu1", "agent", json.RawMessage(`{"agent":"explorer","prompt":"look"}`))))
	// The child's own work, arriving tagged with the child's session id.
	tr.ApplyFrom("child-session-id", "tu1", assistantEntry(t, session.TextBlock("I looked")))

	rows := tr.Rows()
	agentRow := rowByKey(t, rows, "tu1")
	childRow := rowAfter(t, rows, agentRow)
	if childRow.ParentToolUseID != "tu1" {
		t.Fatalf("child row parent = %q, want tu1", childRow.ParentToolUseID)
	}
	if childRow.SessionID != "child-session-id" {
		t.Fatalf("child row session = %q", childRow.SessionID)
	}

	lines := tr.Layout()
	childLine := lineForRow(t, lines, childRow)
	parentLine := lineForRow(t, lines, agentRow)
	if indentOf(childLine) <= indentOf(parentLine) {
		t.Fatalf("the child is not indented under its call: parent=%d child=%d",
			indentOf(parentLine), indentOf(childLine))
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/tui/transcript/ -run ChildRowsNest -v`
Expected: FAIL to compile: `Row` has no `ParentToolUseID` or `SessionID`, and there is no `ApplyFrom`.

- [ ] **Step 3: Give a row its origin**

Add to `Row` in `row.go`:

```go
	// SessionID is the session the row's entry came from, empty for the session being
	// rendered. A subagent's rows arrive tagged with the child's id (ADR 0028) and render
	// under the agent call that opened it.
	SessionID string
	// ParentToolUseID is the agent call this row's session was opened by, empty unless
	// SessionID is set. It is what layout indents under, and what Commit removes with the
	// call when the turn is dropped.
	ParentToolUseID string
```

Add `ApplyFrom(sessionID, parentToolUseID string, e session.Entry) []string` to `Transcript`: it is `Apply` with the two fields stamped on every row it creates, inserted immediately after the last row belonging to that `parentToolUseID` so a child's rows stay grouped under their call. Keep row keys unique by prefixing a child's key with its session id.

- [ ] **Step 4: Indent a child's rows**

In `layout` (`transcript.go:343-372`), pass an indent to `Render` for a row whose `ParentToolUseID` is set. The primitive exists: `line` (`render.go:373`) already takes an indent, and `previewIndent = 4` (`render.go:46`) is what a tool row's preview already uses. Use the same value so a subagent's output lines up with the other detail under a tool call.

- [ ] **Step 5: Stop dropping a child's notifications**

In `model.go`, the drop at `:436` returns nil for any other session's notification. Keep the drop, but let a child through when the model knows the child belongs to a call it is rendering:

```go
		case sid != "" && sid != m.session.SessionID:
			// Another session's. A subagent this session dispatched is rendered under the
			// agent call that opened it (ADR 0028); anything else is genuinely not ours.
			call, ok := m.tr.AgentCallFor(sid)
			if !ok {
				return nil
			}
			return m.childNotification(sid, call, n)
		}
```

`Transcript.AgentCallFor(sessionID) (toolUseID string, ok bool)` maps a child session to the call that opened it. The model learns the mapping from the child's first notification: `session_opened` carries `parent_tool_use_id` (`server.go:1146`), so record it when that entry arrives and look it up afterwards.

`childNotification` routes `entry.appended` to `ApplyFrom`, `stream.delta` to a child-tagged `Delta`, and `tool.state` and `turn.state` to the agent row's spinner state rather than the parent's own turn state. The parent's turn is not the child's turn, and letting a child's `turn.state` set the parent's would end the parent's spinner when the child finished.

Add `protocol.NotifyToolState` to `sessionScoped` (`:722-740`) so it passes through the same gate.

- [ ] **Step 6: Run the transcript tests**

Run: `go test -race ./internal/tui/transcript/ -v`
Expected: PASS.

- [ ] **Step 7: Add a golden**

```go
func TestGoldenSubagentRunning(t *testing.T) {
	golden(t, "subagent_running", screen(t, nil, func(tm *teatest.TestModel, sid string) {
		child := session.NewID().String()
		turn := session.NewID().String()
		tm.Send(appendedMsg(t, sid, session.AssistantMessage{
			Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopToolUse,
			Content: []session.Block{session.ToolUseBlock("t9", "agent",
				json.RawMessage(`{"agent":"explorer","prompt":"find the caller"}`))},
			Usage: goldenUsage,
		}))
		tm.Send(notify(t, protocol.NotifyToolState, protocol.ToolStateChanged{
			SessionID: sid, TurnID: turn, ToolUseID: "t9", Name: "agent", State: protocol.ToolStateRunning,
		}))
		tm.Send(appendedMsg(t, child, session.SessionOpened{
			Workspace: testWorkspace, Model: testRef, Mode: session.ModeStrict,
			Thinking: session.ThinkingHigh, Agent: "explorer",
			ParentSessionID: sid, ParentToolUseID: "t9",
		}))
		tm.Send(appendedMsg(t, child, session.AssistantMessage{
			Model: testRef, Thinking: session.ThinkingHigh, StopReason: session.StopEndTurn,
			Content: []session.Block{session.TextBlock("Found it in internal/cli/wire.go")},
			Usage:   goldenUsage,
		}))
	}))
}
```

- [ ] **Step 8: Generate and read the golden**

Run: `go test ./internal/tui/app/ -run GoldenSubagentRunning -update`
Then open `internal/tui/app/testdata/subagent_running.golden` and read it. A golden is only worth having if someone looked at it once: confirm the child's line is indented under the agent call, the child's text is present, and nothing else moved.

Run: `go test ./internal/tui/app/`
Expected: PASS with no other golden changed. If another golden moved, that is a regression in this task, not a golden to bless.

- [ ] **Step 9: Commit**

```bash
git add internal/tui examples
git commit -m "tui: a subagent's work renders under the call that opened it"
```

---

## Task 8: `Host.RegisterAgent` and the registry

**Files:**
- Modify: `internal/plugin/plugin.go:117` (the `Host` interface)
- Modify: `internal/plugin/registry.go:44-45` (fields), `:64` (init), `:248-251` (commit), `:364` (`Fail`), `:505-518` (getter), `:539-540` (host fields), `:554` (`newHost`), `:568-573` (beside `RegisterProvider`)
- Modify: `internal/server/server.go:1176-1185` (`resolveAgent`)
- Test: `internal/plugin/registry_test.go`, `internal/server/server_test.go`

**Interfaces:**
- Consumes: nothing from Tasks 1 to 7.
- Produces:
  - `Host.RegisterAgent(d agentdef.Definition) error`
  - `Registry.AgentDefs() map[string]agentdef.Definition`
  - `Host.AgentDefs() map[string]agentdef.Definition`, the read accessor, beside the existing `Tools()`, `Commands()` and `Statuses()` (`plugin.go:136-138`)
  - Task 9 mirrors the write side over stdio and its test reads `AgentDefs`; Task 10 reads it for the roster. Both need the accessor, so it lands here with the state it reads rather than in Task 10.

**Template:** `RegisterProvider` is the established shape for a resource that is registered rather than called. Copy it exactly: a map and order slice on `Registry` and on `host`, a `stageRegister` call, a commit branch, a `withdraw` in `Fail`, and a getter. One difference: `agentdef.Definition` is a plain struct with no `Name()` method, so `stageRegister` takes `d.Name` directly the way `RegisterTool` takes `t.Name`.

- [ ] **Step 1: Write the failing test**

Following `TestDuplicateToolKeepsFirstAndNotices` (`registry_test.go:87-114`) exactly:

```go
func TestRegisterAgentKeepsFirstAndNotices(t *testing.T) {
	r, notices := newTestRegistry(nil)
	var second error
	r.Load(context.Background(),
		fakePlugin{"first", func(_ context.Context, h Host) error {
			return h.RegisterAgent(agentdef.Definition{Name: "explorer", Description: "reads"})
		}},
		fakePlugin{"second", func(_ context.Context, h Host) error {
			second = h.RegisterAgent(agentdef.Definition{Name: "explorer", Description: "impostor"})
			return nil
		}},
	)
	if !errors.Is(second, ErrDuplicate) {
		t.Fatalf("second registration error = %v", second)
	}
	defs := r.AgentDefs()
	if len(defs) != 1 || defs["explorer"].Description != "reads" {
		t.Fatalf("agents = %+v", defs)
	}
	if len(*notices) != 1 || (*notices)[0] != "plugin second: agent explorer already registered by first" {
		t.Fatalf("notices = %v", *notices)
	}
	st := r.Statuses()
	if st[1].State != StateReady {
		t.Fatalf("second should still be ready: %+v", st[1])
	}
}

func TestFailedPluginWithdrawsItsAgents(t *testing.T) {
	r, _ := newTestRegistry(nil)
	r.Load(context.Background(), fakePlugin{"one", func(_ context.Context, h Host) error {
		return h.RegisterAgent(agentdef.Definition{Name: "explorer", Description: "reads"})
	}})
	if len(r.AgentDefs()) != 1 {
		t.Fatalf("agents = %+v", r.AgentDefs())
	}
	r.Fail("one", errors.New("died"))
	if len(r.AgentDefs()) != 0 {
		t.Fatalf("a failed plugin's agents are still registered: %+v", r.AgentDefs())
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race ./internal/plugin/ -run 'RegisterAgent|WithdrawsItsAgents' -v`
Expected: FAIL to compile: no `RegisterAgent`, no `AgentDefs`.

- [ ] **Step 3: Add the registry state**

`Registry` fields beside `providers` at `:44-45`:

```go
	agents     map[string]owned[agentdef.Definition]
	agentOrder []string
```

Initialize in `NewRegistry` at `:64`: `agents: map[string]owned[agentdef.Definition]{},`

`host` fields at `:539-540`:

```go
	agents     map[string]agentdef.Definition
	agentOrder []string
```

Initialize in `newHost` at `:554`: `agents: map[string]agentdef.Definition{},`

Commit branch beside the provider one at `:248-251`:

```go
	for _, n := range h.agentOrder {
		r.agents[n] = owned[agentdef.Definition]{owner: h.name, value: h.agents[n]}
		r.agentOrder = append(r.agentOrder, n)
	}
```

Withdraw in `Fail` beside `:364`: `r.agentOrder = withdraw(r.agents, r.agentOrder, name)`

Agents need no change callback: `resolveAgent` re-reads per session and never caches, so a withdrawn definition is gone by the next session open.

- [ ] **Step 4: Add the Host method and the getter**

On the `Host` interface at `plugin.go:117`, after `RegisterProvider`:

```go
	// RegisterAgent contributes an agent definition, the same thing an agents/<name>.md file
	// carries. A definition is static data, so it needs no callback. An operator's file of the
	// same name wins: resolveAgent reads the disk roots first (ADR 0028).
	RegisterAgent(d agentdef.Definition) error
```

On `host`, beside `RegisterProvider` at `:568-573`:

```go
func (h *host) RegisterAgent(d agentdef.Definition) error {
	if d.Description == "" {
		return fmt.Errorf("plugin %s: agent %s: description is required", h.name, d.Name)
	}
	if d.Thinking != "" && !d.Thinking.Valid() {
		return fmt.Errorf("plugin %s: agent %s: thinking %q is not off, low, medium or high", h.name, d.Name, d.Thinking)
	}
	return stageRegister(h, "agent", d.Name, d, h.r.agents, h.agents, &h.agentOrder)
}
```

The getter, beside `Providers` at `:505-518`:

```go
// AgentDefs is every agent definition plugins have registered, by name. A map rather than an
// ordered slice because resolveAgent merges it with the definitions read from disk, where the
// name is the key and precedence has already decided the winner.
func (r *Registry) AgentDefs() map[string]agentdef.Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]agentdef.Definition, len(r.agentOrder))
	for _, n := range r.agentOrder {
		out[n] = r.agents[n].value
	}
	return out
}
```

- [ ] **Step 5: Run to verify they pass**

Run: `go test -race ./internal/plugin/ -run 'RegisterAgent|WithdrawsItsAgents' -v`
Expected: PASS.

- [ ] **Step 6: Merge into `resolveAgent`, disk first**

```go
// resolveAgent reads the definitions visible to a session in ws and resolves one by name,
// telling cn about any file that would not parse. The user's root, then the workspace's, then
// whatever plugins registered, first name winning: an operator's file always beats a plugin's
// registration, so installing a plugin cannot take a name that is already in use (ADR 0028).
// Read per session rather than cached, so a definition edited between two sessions takes effect
// on the second without a restart. ok is false only for a name nothing defines; "" and
// "default" always resolve.
func (s *Server) resolveAgent(cn *conn, ws session.Workspace, name string) (agentdef.Definition, bool) {
	defs, errs := agentdef.Load([]string{
		filepath.Join(s.d.Config.ConfigDir, "agents"),
		filepath.Join(ws.Root, ".rudy", "agents"),
	})
	for _, e := range errs {
		cn.notify(protocol.NotifyNotice, protocol.NoticeParams{Level: "warn", Text: e.Error()})
	}
	for n, d := range s.d.Plugins.AgentDefs() {
		if _, seen := defs[n]; !seen {
			defs[n] = d
		}
	}
	return agentdef.Resolve(defs, name)
}
```

- [ ] **Step 7: Test the precedence**

```go
func TestADiskDefinitionBeatsAPluginsOfTheSameName(t *testing.T) {
	srv, cn := newTestServerWithPlugins(t, fakeAgentPlugin{"explorer", "from the plugin"})
	writeAgentDef(t, srv, "explorer", "from disk", []string{"read"})
	s := openSession(t, cn, sessionOpenParams{Agent: "explorer"})
	if got := agentDescriptionOf(t, srv, s); got != "from disk" {
		t.Fatalf("description = %q, want the operator's file to win", got)
	}
}

func TestAPluginDefinitionResolvesWhenNoFileClaimsTheName(t *testing.T) {
	srv, cn := newTestServerWithPlugins(t, fakeAgentPlugin{"reviewer", "from the plugin"})
	s := openSession(t, cn, sessionOpenParams{Agent: "reviewer"})
	if got := agentDescriptionOf(t, srv, s); got != "from the plugin" {
		t.Fatalf("description = %q, want the plugin's", got)
	}
}
```

Run: `go test -race ./internal/server/ -run 'BeatsAPlugins|ResolvesWhenNoFile' -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/plugin internal/server
git commit -m "plugin: a plugin may contribute an agent definition"
```

---

## Task 9: `plugin.register_agent` over stdio

**Files:**
- Modify: `internal/protocol/methods.go:38-48` (constants), `:361-373` (beside `PluginRegisterProviderParams`)
- Modify: `internal/plugin/spawned.go:162-179` (`Registrar`), `:181-217` (`Register`), `:582-612` (beside `RegisterTool`)
- Modify: the server's plugin-request dispatch, wherever `MethodPluginRegisterProvider` is handled
- Test: `internal/plugin/spawned_test.go`

**Interfaces:**
- Consumes: Task 8's `Host.RegisterAgent`.
- Produces: a spawned plugin contributes a definition with no callback, refused after `plugin.init` returns like every other registration.

- [ ] **Step 1: Write the failing test**

```go
func TestSpawnedPluginRegistersAnAgent(t *testing.T) {
	reg, host := newSpawnedTestHost(t)
	raw := json.RawMessage(`{"name":"reviewer","description":"reviews a diff","prompt":"You review.","tools":["read","grep"],"thinking":"high","max_turns":12}`)
	if _, err := Register(reg, protocol.MethodPluginRegisterAgent, raw); err != nil {
		t.Fatal(err)
	}
	defs := host.AgentDefs()
	got, ok := defs["reviewer"]
	if !ok {
		t.Fatalf("agents = %+v", defs)
	}
	if got.Description != "reviews a diff" || got.Prompt != "You review." || got.MaxTurns != 12 {
		t.Fatalf("definition = %+v", got)
	}
	if !slices.Equal(got.Tools, []string{"read", "grep"}) {
		t.Fatalf("tools = %v", got.Tools)
	}
	if got.Thinking != session.ThinkingHigh {
		t.Fatalf("thinking = %q", got.Thinking)
	}
}

func TestSpawnedAgentAfterInitIsRefused(t *testing.T) {
	s := readySpawned(t) // ready.Load() is true
	err := s.RegisterAgent("late", "too late", "", nil, "", "", 0)
	if !errors.Is(err, session.ErrInvariant) {
		t.Fatalf("error = %v, want ErrInvariant: the agent set a session opened with never changes under it", err)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race ./internal/plugin/ -run Spawned.*Agent -v`
Expected: FAIL to compile.

- [ ] **Step 3: Add the protocol method and params**

In `methods.go:38-48`:

```go
	MethodPluginRegisterAgent = "plugin.register_agent"
```

Beside `PluginRegisterProviderParams` at `:361-373`:

```go
// PluginRegisterAgentParams carries the same fields agents/<name>.md does. Tools is a pointer
// so an absent key (every tool) stays distinguishable from an explicit empty list (no tool),
// exactly as the file's frontmatter does.
type PluginRegisterAgentParams struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Prompt      string    `json:"prompt"`
	Tools       *[]string `json:"tools,omitempty"`
	Model       string    `json:"model,omitempty"`
	Thinking    string    `json:"thinking,omitempty"`
	MaxTurns    int       `json:"max_turns,omitempty"`
}
```

- [ ] **Step 4: Add it to `Registrar` and `Register`**

On the `Registrar` interface (`spawned.go:162-179`), after `RegisterProvider`:

```go
	RegisterAgent(name, description, prompt string, tools *[]string, model, thinking string, maxTurns int) error
```

In `Register` (`:181-217`), a case mirroring the provider one:

```go
	case protocol.MethodPluginRegisterAgent:
		var p protocol.PluginRegisterAgentParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("%w: %s", protocol.ErrInvalidArgument, err)
		}
		return ok, reg.RegisterAgent(p.Name, p.Description, p.Prompt, p.Tools, p.Model, p.Thinking, p.MaxTurns)
```

On `Spawned`, beside `RegisterCommand` at `:582-612` (the closer template, since neither needs an invoke closure):

```go
func (s *Spawned) RegisterAgent(name, description, prompt string, tools *[]string, model, thinking string, maxTurns int) error {
	if err := s.refuseLate("agent", name); err != nil {
		return err
	}
	level := session.ThinkingLevel(thinking)
	if thinking != "" && !level.Valid() {
		return fmt.Errorf("%w: plugin %s: agent %s: thinking %q is not off, low, medium or high", protocol.ErrInvalidArgument, s.m.Name, name, thinking)
	}
	if maxTurns < 0 {
		return fmt.Errorf("%w: plugin %s: agent %s: max_turns %d must be zero or positive", protocol.ErrInvalidArgument, s.m.Name, name, maxTurns)
	}
	d := agentdef.Definition{
		Name: name, Description: description, Prompt: prompt,
		Model: model, Thinking: level, MaxTurns: maxTurns,
	}
	if tools != nil {
		d.Tools = slices.Clone(*tools)
	}
	return s.host.RegisterAgent(d)
}
```

- [ ] **Step 5: Allow the method in the server's dispatch**

Find where the server routes `MethodPluginRegisterProvider` from a plugin connection and add `MethodPluginRegisterAgent` alongside it, under the same caller-class check: a plugin may register only under its own name.

- [ ] **Step 6: Run, then the whole suite**

Run: `go test -race ./internal/plugin/ -run Spawned.*Agent -v`, then `make check`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/protocol internal/plugin internal/server
git commit -m "plugin: a spawned plugin registers an agent over stdio"
```

---

## Task 10: The roster moves to a `session_opened` hook

**Files:**
- Modify: `internal/plugins/subagents/plugin.go:41-46` (`Init`), `:48-64` (`describe`)
- Test: `internal/plugins/subagents/plugin_test.go`

**Interfaces:**
- Consumes: Task 8's `Registry.AgentDefs`, Task 5's `tools` input.
- Produces: nothing later depends on. This is the last task.

**Why:** the description is built once in `Init` from `configDir/agents` alone, so it omits a workspace's `.rudy/agents/` today, papered over by the prose "A workspace may add more". Plugin-contributed definitions make it worse: `Registry.Load` (`registry.go:95-125`) inits and commits one plugin at a time in slice order, and subagents sits seventh in `wire.go:359`, so any agent-contributing plugin after it is invisible to its `Init`. There is no escape hatch, because `stageRegister` refuses a duplicate, so the tool cannot be re-registered with a fresh description.

The skills plugin already solves the same problem the same way: it registers a `session_opened` hook returning `SessionOpenedResult{Context: text}` (`skills/plugin.go:34-48`). The hook fires per session, after the workspace is known and every plugin has registered.

- [ ] **Step 1: Write the failing test**

```go
func TestTheRosterComesFromTheSessionNotTheProcess(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, filepath.Join(dir, "agents"), "explorer", "reads the codebase")
	ws := t.TempDir()
	writeDef(t, filepath.Join(ws, ".rudy", "agents"), "reviewer", "reviews a diff")

	p := New(dir).(*agentPlugin)
	h := newTestHost(t, map[string]agentdef.Definition{
		"migrator": {Name: "migrator", Description: "writes migrations"},
	})
	if err := p.Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}

	res := h.fireSessionOpened(t, ws)
	for _, want := range []string{"explorer", "reviewer", "migrator"} {
		if !strings.Contains(res.Context, want) {
			t.Fatalf("the roster omits %q: %q", want, res.Context)
		}
	}

	tool, ok := h.Tool("agent")
	if !ok {
		t.Fatal("no agent tool")
	}
	if strings.Contains(tool.Description, "explorer") {
		t.Fatal("the description still enumerates agents, so it is stale the moment a workspace differs")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -race ./internal/plugins/subagents/ -run RosterComesFromTheSession -v`
Expected: FAIL: the description still enumerates, and no hook is registered.

- [ ] **Step 3: Make the description static and register the hook**

```go
func (p *agentPlugin) Init(_ context.Context, h plugin.Host) error {
	p.host = h
	desc := "Delegate a task to a subagent running in its own session and return its final answer. " +
		"The agents available to this session, and what each is for, are listed in the system prompt. " +
		"Pass tools to narrow what the subagent may use; it only ever removes."
	if err := h.RegisterTool(tool.Tool{
		Name: "agent", Description: desc, Schema: json.RawMessage(schema),
		Safety: tool.Safe, Invoke: p.invoke,
	}); err != nil {
		return err
	}
	// The roster is a session fact, not a process fact: it depends on the workspace and on
	// plugins that may register after this one. A description is written once and cannot be
	// revised, so it says where to look rather than trying to hold the answer (ADR 0028).
	return h.RegisterHook(plugin.HookHandler{
		Point:  plugin.HookSessionOpened,
		Handle: p.roster,
	})
}

// roster lists the definitions visible to one session, in name order so a session's system
// prompt is stable between runs.
func (p *agentPlugin) roster(_ context.Context, call plugin.HookCall) (any, error) {
	payload, ok := call.Payload.(*plugin.SessionOpenedPayload)
	if !ok || payload == nil {
		return nil, nil
	}
	defs, errs := agentdef.Load([]string{
		filepath.Join(p.configDir, "agents"),
		filepath.Join(payload.Workspace.Root, ".rudy", "agents"),
	})
	for _, e := range errs {
		p.host.Notice(e.Error())
	}
	for n, d := range p.host.AgentDefs() {
		if _, seen := defs[n]; !seen {
			defs[n] = d
		}
	}
	if len(defs) == 0 {
		return nil, nil
	}
	return &plugin.SessionOpenedResult{
		Context: "Agents you can delegate to with the agent tool: " + describe(defs) + ".",
	}, nil
}
```

`describe` (`:48-64`) is unchanged: it already sorts by name and renders `name (description)`.

This is the shape `skills/plugin.go:34-48` uses: `Host.RegisterHook` takes one `plugin.HookHandler{Point, Handle}` (`plugin.go:118`), `call.Payload` type-asserts to `*plugin.SessionOpenedPayload` (`hooks.go:64`), and the return is `*plugin.SessionOpenedResult` (`hooks.go:78`) whose `Context` is appended to the session's system prompt. Returning `nil, nil` means the hook has nothing to add, which is what the skills plugin does for an empty list.

`Host.AgentDefs()` already exists: Task 8 adds it beside `Registry.AgentDefs()`, because Task 9's test reads it too. This task only consumes it.

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race ./internal/plugins/subagents/ -v`
Expected: PASS.

- [ ] **Step 5: Full check and both transports**

Run: `make check`
Expected: PASS.

- [ ] **Step 6: Commit and close the epic**

```bash
git add internal/plugins/subagents internal/plugin
git commit -m "plugin: the agent roster is a session fact, not a process fact"
bd close rudy-ufq
```

---

## Self-review

**Spec coverage.** ADR 0028's eight decisions against the tasks: decision 1 (a tool call is addressable) is Task 3; decision 2 (every call dispatches concurrently) is Task 3; decision 3 (a non-reentrant tool guards itself) is Task 2; decision 4 (concurrent calls matching one question ask once) is Task 1; decision 5 (the tool list is one narrowing chain a caller may only shrink) is Tasks 4 and 5; decision 6 (a child's notifications reach its parent's subscribers) is Tasks 6 and 7; decision 7 (a plugin may contribute an agent definition) is Tasks 8 and 9; decision 8 (the description stops enumerating) is Task 10.

Contracts pass 5 rows against the tasks: `tool.state` is Task 3; the parent-routing paragraph is Task 6; `SessionOpenParams.Tools` is Task 5; `plugin.register_agent` is Task 9; the Tool scheduler service row is Task 3; the Gate service row's coalescing sentence is Task 1; the four new invariant rows are Tasks 4, 1 and 3.

**Not in this plan, deliberately.** `rudy-0lz` (memory-go's own fix) stays open: Task 2 makes rudy correct without it, and memory-go is a different repository whose change is a push there and a `go get` here. `rudy-2er` (the subagent model ladder from registry prices) is untouched; a definition's model or inheritance still decides. Nesting beyond depth one is not attempted: ADR 0012's rule stands and ADR 0028 does not revisit it.

**Type consistency.** `intersectTools(want, have []string) []string` is defined in Task 4 and used in Task 5. `inflight` with `add`/`remove`/`cancelAll`/`len` is defined in Task 3 and used nowhere else. `Observer.ToolStateChanged(turnID, toolUseID, name string, state ToolState)` is defined in Task 3 and consumed by Tasks 6 and 7. `Registry.AgentDefs() map[string]agentdef.Definition` is defined in Task 8 and used in Tasks 8, 9 and 10. `protocol.ToolStateChanged` and the three `ToolState*` constants are defined in Task 3 and used in Tasks 6 and 7. `Row.SessionID` and `Row.ParentToolUseID` are defined in Task 7 and used only there.

**Known soft spots, flagged rather than hidden.** Task 3 Step 9 says to repair the tests concurrency invalidates without listing them, because which tests break depends on how the runner is restructured; the instruction is to convert order-dependent assertions rather than weaken them, and a reviewer should check that is what happened. Task 6 Step 3 says to follow the file's existing lock ordering rather than prescribing it, because the ordering rule is documented in the file and restating it here risks contradicting it. Task 7's helper names in the transcript tests (`rowByKey`, `rowAfter`, `lineForRow`, `indentOf`) do not exist yet and are written as part of that task.
