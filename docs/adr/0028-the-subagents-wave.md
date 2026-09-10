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

2. **A call dispatches concurrently only if its tool is safe and declares
   itself concurrent.** Two conditions, because they answer two different
   questions, and neither answers the other's.

   Safe, because of the gate. It reads a session's allowances when a call begins
   and appends the allow when the asker answers, so two concurrent calls to one
   unsafe tool would both prompt and "allow for the session" would stop meaning
   what it says. Keeping unsafe calls serial closes that window rather than
   patching it. It also settles the contended-workspace hazard by construction:
   `bash`, `edit` and `write` are unsafe, so two of them never overlap and
   neither the git index nor a same-file write is ever contended.

   Declared, because safety is a statement about permission and says nothing
   about side effects. `memory_remember` is the proof: it is safe, since
   recording a memory needs no operator's consent, and it read-modify-writes a
   concept file on disk, so two of them at once would race. Inferring that a
   tool tolerates a second copy of itself from the fact that it needs no
   permission is a guess about code the harness did not write, and a plugin
   author writing a safe tool has no reason to suspect reentrancy was required
   of them. So `tool.Tool` gains a field, it defaults to serial, and a tool that
   wants concurrency says so.

   `read`, `grep`, `glob` and `agent` declare it. `memory_remember` does not.
   The default preserves today's behaviour for every tool that does not think
   about the question, including every tool already written.

   The cost is named rather than hidden: MCP tools are all unsafe, so two
   independent MCP reads serialize when nothing required it.

3. **A child's notifications reach its parent's subscribers.** The child's
   fan-out also delivers to the parent's non-plugin subscribers, tagged with the
   child's session id, which every payload already carries. It is the mirror of
   the walk that already exists in the other direction: a child with no asker of
   its own borrows its parent's. Depth is one, so the walk does not recurse.

   Not a `session.watch` method. Method authority is gated on whether a
   connection is subscribed to a session, so a client that subscribed to a child
   in order to watch it would also be entitled to submit to it and interrupt it.
   Watching is not owning, and the routing rule keeps them apart without
   inventing a second tier of subscription to hold them apart.

4. **A plugin may contribute an agent definition.** `Host.RegisterAgent` follows
   the shape `RegisterProvider` already established for a resource that is
   registered rather than called, and `resolveAgent` merges the registry's
   definitions into the roots it reads. A definition is static data, so a spawned
   plugin contributes one over stdio with no callback, under the same rule as
   every other registration: it must arrive before the plugin's init returns.

   Disk beats plugins. The order is the user's config, then the workspace's
   `.rudy/agents`, then whatever plugins registered, so an operator can always
   override what an installed plugin shipped, and installing a plugin cannot
   break a session that already worked.

5. **The `agent` tool stops enumerating agents in its description.** The
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
- Fan-out under the session lock includes an fsync on an allow decision, so
  concurrent safe calls contend on it. Safe calls do not append permission
  decisions, so the contention is bounded by what unsafe calls were doing
  anyway.
- `tool.Tool` gains a field, so the tool contract changes and the spawned
  plugin registration carries one more value. It defaults to the old behaviour,
  so a plugin written before this wave keeps working and keeps its meaning.
- `rudy-ulb`, a flaky cancel test in the subagents plugin, is a race in the
  cancel path this wave rewrites. It is fixed here rather than separately.

## Alternatives considered

- Concurrency in the `agent` tool instead of the runner: one call taking a list
  of tasks and opening N children itself. It needs no runner change, but it is a
  private path to a capability the kernel otherwise denies every other tool, and
  the kernel has no private paths. It also leaves the interrupt and cancel bugs
  in place, since they are only invisible while one call runs at a time.
- Unbounded concurrency with a configured cap: honest about what the model
  asked for, but it leaves the contended-workspace and double-ask problems to be
  solved separately and explicitly, and a cap is a number nobody knows how to
  choose.
- Inferring concurrency from `tool.Safe` alone, with no declared field: it needs
  no new vocabulary and it decides the workspace hazards correctly, which is why
  it was the first shape of decision 2. It is wrong because safe means the
  operator need not be asked, and a tool can be both safe and stateful.
  `memory_remember` already is, so the rule would have shipped a race on the day
  it landed rather than a latent one.
- Namespacing plugin-contributed agents as `<plugin>:<agent>`: collisions become
  impossible, at the cost of changing every name the model types and the tool's
  input vocabulary, to solve a collision that ordering already answers.
