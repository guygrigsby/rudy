# 20. /scoped-models: the cycle is a set the client keeps

- Status: Accepted
- Date: 2026-09-09
- Deciders: Guy Grigsby

## Context

`app.model.cycleForward` and `cycleBackward` step through the whole registry.
Against an endpoint that serves thirty models, cycling is useless: the two or
three worth switching between are scattered among ones nobody would pick by
accident. pi solves it with `/scoped-models`, which enables and disables models
for its Ctrl+P cycling, and the owner asked for the same.

## Decision

1. **The scope is the client's, not the session's.** Which models one person
   cycles between is a preference about their keyboard, not a fact about the
   conversation, and it belongs in no session log. So `/scoped-models` is a
   client-local command beside `/exit` and `/quit` (ADR 0015 decision 3), and the
   scope lives in the client for the life of the process. A session switch keeps
   it; a restart forgets it.

   It is deliberately not persisted yet. A file would be a small thing to write
   and a decision about where a client's preferences live, which nothing else in
   the client has needed. When a second preference wants persisting, they can
   arrive together.

2. **The picker takes a set.** The model picker gains a multi-select mode: space
   or tab toggles the row under the cursor, `[x]` and `[ ]` say what is in, and
   Enter confirms the set as it stands. No new action ids, so ADR 0013
   decision 5's closed set stands: space is not bound to anything, and a model id
   never needs one in a filter.

   The picker opens on the scope already set, so a person sees what they chose
   last rather than an empty list, and an argument (`/scoped-models kimi`) is the
   filter it opens under.

3. **An empty set means the whole registry.** That is how a person undoes a
   scope, and it is what a client opens on. The client says which of the two it
   is in a notice, because nothing on screen carries the scope.

4. **The registry is the authority on what exists.** A scoped model the registry
   has stopped carrying is skipped when the cycle is read, not when it was
   chosen, so a refresh can never leave ctrl+p pointing at something that no
   longer answers.

## Consequences

- `cycleModel` walks a list of refs rather than the registry, and every scope
  question is answered in one place, `cycle()`.
- The scope is invisible except in the notice and in the picker. A status item
  for it was considered and left out: the line is already six cells wide.
- A person who scopes to one model still has ctrl+p bound to something that does
  nothing new. That is what asking for one model means.

## Alternatives considered

- A server-side scope on the session: it would put a keyboard preference in the
  log and hand it to every other client attached to the same session.
- A `[ui.models] scope = [...]` config field: config is read and never written by
  the harness, so a command could not change it, and a scope a person cannot
  change from inside the client is not the thing that was asked for.
- Filtering the model picker instead of scoping the cycle: the picker already
  filters, and the ask was about the cycle.
