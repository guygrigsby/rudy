# 28. The subagents wave: a tool call is addressable, a child's work is visible, agents come from plugins

- Status: Accepted
- Date: 2026-09-10
- Deciders: Guy Grigsby

## Context

ADR 0012 decision 3 built subagents as far as one blocking call: the `agent`
tool opens a child session, submits a prompt, waits and returns the final text.
ADR 0025 then named subagents as one of the next two features. Three things were
missing, and an audit of the code showed the first two are the same missing
thing.

A subagent is invisible. Notification fan-out is a per-session subscriber list
(`liveSession.broadcastObsLocked`), and the only subscriber to a child session is
the connection the subagents plugin opened for it. The parent's client never
learns the child exists until the tool result lands. `tool.progress` is not the
answer waiting to be used: it is delivered to the plugin adapter and dropped
there, and nothing forwards it.

A subagent is also serial. The turn runner invokes one tool at a time, so a model
that asks for three agents gets them one after another. Delegation that cannot
fan out is delegation that mostly is not worth doing.

Both wants meet at the same place. The runner holds one `cancel`, one
single-consumer `interrupt` flag and one `RunningTool` state for the whole turn,
and `Observer.StateChanged` carries a turn id but no tool identity. Concurrency
needs per-call cancellation and an interrupt every call observes. A client that
renders a running subagent needs to know which call is running. Neither is
reachable without giving a tool call an identity in the turn's own state, and
once it has one, both follow.

The third want is separate. Agent definitions are read from two hardcoded roots
at session open. A plugin cannot contribute one, so `rudy install` cannot deliver
an agent, which is what ADR 0025 said installing a plugin would be for.

## Decision

1. **A running tool call is addressable.** The runner's single `cancel` slot,
   single-consumer interrupt flag and single `RunningTool` state are replaced by
   a set keyed by `tool_use` id. `Interrupt` fans out over the set rather than
   cancelling whichever call registered last. An interrupt is observed
   independently by every in-flight call instead of being consumed by the first
   one to look, so an interrupted turn cannot record its other calls as clean
   successes. `Observer.StateChanged` carries the `tool_use` id.

   This is one change serving two features, and it is the reason they are one
   wave rather than two.

2. **Every tool call in an assistant message dispatches concurrently.** No
   exceptions keyed on safety, and no opt-in field.

   Permission and reentrancy are orthogonal questions and tying them answers
   neither. `tool.Safe` states that the operator need not be asked. It says
   nothing about whether a second copy of the tool may run, and a rule that
   reads it as though it did would serialize two unrelated MCP reads while
   waving through two writes to one concept file. An opt-in field inverts the
   default instead: concurrency then requires every author to have thought about
   it, almost nothing declares it, and the feature is nominally present and
   practically absent. Fan-out is the whole reason to delegate.

   So the runner makes no judgement about which calls may overlap. A tool's
   `Invoke` may be called concurrently with itself and with any other tool, and
   that is part of the tool contract rather than a property the harness infers.

3. **A tool that is not reentrant guards itself.** The obligation sits with the
   code that owns the state, which is the only code that knows what the state
   is. `memory_remember` is the live case and the reason this is written down
   rather than assumed: `memory.Remember` reads a concept, revises it and writes
   it back with no lock, so two concurrent calls on one title both revise the
   same base and one revision is lost. The memory plugin serializes its own
   invocations; memory-go keeps its own defect.

   The two hazards this is often raised against are less than they look. Two
   edits to one file fail cleanly, because an edit matches on the text it
   expects and the second no longer finds it. Two shell commands that contend
   are contending because the model asked for that, and serializing them would
   hide it rather than fix it.

4. **Concurrent calls ask once for the same question, and a session allow
   settles the questions already parked.** Two mechanisms, because there are two
   different reasons a second prompt is redundant and neither covers the other.

   Identical calls share one question. The key is the matcher together with the
   call's input bytes, not the matcher alone. A matcher is coarse on purpose:
   every tool but `bash` reduces to its own name, and `bash` to its first two
   words. Keying on it alone would show the operator one call's input and bind a
   different call to the answer, so `once` would mean "every call parked under
   this tool name", including one whose arguments the operator never saw.
   Identical inputs cannot have that problem, and a coarse key is not worth the
   consent it forges.

   A session-scope allow resolves every parked question it covers, except a
   dangerous one. This is the case the wave itself creates: two calls read the
   allowances before either has recorded one, both park, and the operator
   answering "allow for the session" on the first leaves the second still asking
   for something that is now allowed.

   The exception is not a detail. ADR 0011 puts the dangerous set ahead of
   session allowances in every mode but off, so a dangerous command asks again
   even when a matching allow already stands. Settlement has to honour that
   ordering or it becomes a way to launder consent past it: two `rm -rf` calls
   on different paths park under different questions but one matcher, and
   settling the second from the first's allow grants exactly what ADR 0011
   exists to refuse. A question the Gate marked dangerous is never settled by an
   allowance; it waits for its own answer.

   The Gate decides what is dangerous, and the asker only reads the bit. The
   asker must not re-derive that judgement, because a second implementation of a
   precedence rule is a second chance to get it wrong, and this one is on the
   fail-open side.

   Both are enforced at the asker rather than in the verdict. `Gate.Evaluate` is
   a pure function of the input it is handed and holds no session, so it has
   nothing to coalesce against; the asker already keys every pending question by
   `tool_use` id and already holds the session lock.

   This is a permission bug that concurrency exposes, not a concurrency bug, and
   it is fixed on the permission path rather than by serializing the caller.

5. **Every agent runs under a tool list, and a caller may only narrow it.**
   This is one mechanism, not a rule about subagents. Root or child, an agent
   has a list of the tools it may call. Restricting an agent means removing
   tools from that list, so nothing else in the system has to know why a tool is
   missing.

   Three things set it, each able only to remove:

   The definition sets the agent's own list, as it does today.

   The caller narrows it per call. The `agent` tool's input takes an optional
   list, so the orchestrating model decides what this particular delegation
   needs rather than being stuck with whatever the definition allows. It is a
   judgement the orchestrator is well placed to make and the definition's author
   is not: the author does not know what the task will be.

   The parent bounds it. A child's list is intersected with its parent's, so a
   parent cannot hand out a tool it does not itself hold. This does not work
   today. `applyAgent` builds the child's view from the whole registry, so a
   parent restricted to reading, holding `agent`, can open a child that writes.
   Removal is the only way to restrict an agent, so delegation must not be a way
   around removal, or restricting an agent means nothing.

   The shipped default definition drops the memory writes, which is the
   mechanism above being used rather than a special case in it. A subagent
   gathers and reports; the caller decides what is worth keeping. A child that
   records its own conclusions commits the parent to them without the parent
   ever seeing them, from a session whose transcript nobody reads. A definition
   that wants the memory tools can name them.

6. **A child's notifications reach its parent's subscribers.** The child's
   fan-out also delivers to the parent's non-plugin subscribers, tagged with the
   child's session id, which every payload already carries. It is the mirror of
   the walk that already exists in the other direction: a child with no asker of
   its own borrows its parent's. Depth is one, so the walk does not recurse.

   Not a `session.watch` method. Method authority is gated on whether a
   connection is subscribed to a session, so a client that subscribed to a child
   in order to watch it would also be entitled to submit to it and interrupt it.
   Watching is not owning, and the routing rule keeps them apart without
   inventing a second tier of subscription to hold them apart.

7. **A plugin may contribute an agent definition.** `Host.RegisterAgent` follows
   the shape `RegisterProvider` already established for a resource that is
   registered rather than called, and `resolveAgent` merges the registry's
   definitions into the roots it reads. A definition is static data, so a spawned
   plugin contributes one over stdio with no callback, under the same rule as
   every other registration: it must arrive before the plugin's init returns.

   Disk beats plugins. The order is the user's config, then the workspace's
   `.rudy/agents`, then whatever plugins registered, so an operator can always
   override what an installed plugin shipped, and installing a plugin cannot
   break a session that already worked.

8. **The `agent` tool stops enumerating agents in its description.** The
   description is built once when the plugin initializes, from the user's config
   directory alone, which is why it omits a workspace's own definitions today and
   would omit every plugin-contributed one tomorrow: plugins initialize in order,
   and a plugin registering an agent after the subagents plugin cannot be seen by
   a description already written. A registration cannot be revised afterwards.

   Instead the roster is injected as session context from a `session_opened`
   hook, the way the skills plugin already injects skills. The hook fires per
   session, after the workspace is known and every plugin has registered, so the
   roster is accurate for the session it is describing rather than for the
   machine the harness started on.

## Consequences

- A turn's log can interleave the results of calls that overlapped. Entry
  appends are already serialized and ids are minted under that lock, so ordering
  stays monotonic, and both codecs key a tool result to its call by id rather
  than by position.
- `liveAsker` documents itself as running on the runner's goroutine between
  steps. That sentence stops being true and is rewritten. Its behaviour does
  not change: it already keys every pending ask by `tool_use` id, so multiple
  outstanding asks were structurally supported before this wave needed them.
- The parent's client receives notifications for sessions it did not open. A
  client that assumes every notification belongs to the session it is rendering
  will render a subagent's output as the parent's own; the session id in the
  payload is what distinguishes them, and it was always there.
- Appending an allow decision fsyncs under the session lock, so concurrent
  calls contend on it. The tools that append one are the tools that asked, so
  the contention is bounded by how often the operator is being prompted.
- Reentrancy becomes a stated obligation on every tool, including every tool
  already written and every tool a plugin author writes next. The contract says
  so explicitly rather than leaving it to be discovered, and `memory_remember`
  is the worked example of getting it wrong.
- A turn's wall-clock cost stops being the sum of its tool calls, so a timeout
  that was tuned against serial execution is now loose rather than tight. None
  is tightened here.
- `rudy-ulb`, a flaky cancel test in the subagents plugin, is a race in the
  cancel path this wave rewrites. It is fixed here rather than separately.

## Alternatives considered

- Concurrency in the `agent` tool instead of the runner: one call taking a list
  of tasks and opening N children itself. It needs no runner change, but it is a
  private path to a capability the kernel otherwise denies every other tool, and
  the kernel has no private paths. It also leaves the interrupt and cancel bugs
  in place, since they are only invisible while one call runs at a time.
- Safe tools concurrent, unsafe tools serial: it settles the contended-workspace
  and double-ask problems without writing any new code, which is why it was the
  first shape of this decision. It is wrong twice over. Permission and
  reentrancy are orthogonal, so the rule serializes two unrelated MCP reads
  while waving through two writes to one concept file, and it buys its
  correctness by giving up most of the concurrency it was introduced to get.
- The same rule plus an opt-in field on `tool.Tool`, so a safe tool declares
  itself reentrant: it closes the `memory_remember` hole, but it makes serial
  the default that every author has to escape, and a feature nobody opts into is
  a feature nobody has. Reentrancy belongs in the contract every tool is held
  to, not in a field most of them will leave unset.
- A configured concurrency cap: a number nobody knows how to choose, defended by
  the fear that the model will ask for too much at once. If it does, that is
  visible and fixable where it happened.
- Namespacing plugin-contributed agents as `<plugin>:<agent>`: collisions become
  impossible, at the cost of changing every name the model types and the tool's
  input vocabulary, to solve a collision that ordering already answers.
