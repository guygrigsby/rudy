# ADR 0008: Memory is integrated through a Go SDK

- Status: Accepted, implemented 2026-09-08 as memory-go in the memory repository
- Date: 2026-09-07
- Deciders: Guy Grigsby

## Context

Shared agent memory is an OKF bundle at `~/.agents/memory`, git-synced,
written by pi, Claude Code and the `memory` CLI. The reference
implementation is Node, 1,288 lines of source, with a domain model, a
context map, a design spec and ADR 0001 already in its repo. rudy will not
shell out to it.

## Decision

A Go SDK, `memory-go`, lives with the memory project, not in rudy, because
memory's premise is one bundle every harness reads the same way and the
SDK is harness-agnostic. rudy's memory plugin imports it.

The SDK ports `context`, `remember`, `recall`, `project-id`, `sync` and
`fold` from the memory specs. Two constraints:

- Byte compatibility. Both implementations write the same `index.md`,
  `log.md` and concept files in one git-synced bundle, so index
  regeneration and concept rendering must be byte-identical or the two
  fight in git. Golden tests against the Node outputs are the gate.
- Fold calls a model. In the SDK that is a port the caller supplies.
  rudy's plugin satisfies it with the Provider context, so memory carries
  no LLM client of its own.

This is a second project with its own spec and plan, sequenced before
rudy's memory plugin.

## Consequences

- No Node runtime in rudy's process tree.
- A second codebase to keep in step with the Node one. The goldens make
  drift visible in CI.
- Any Go harness or tool gets the same memory contract for free.

## Alternatives considered

- Shell out to the `memory` CLI: fast and already JSON, but a subprocess
  and a Node dependency on every session start.
- Port into rudy: the SDK would be trapped in one harness.
- Reimplement the format loosely: two writers with different bytes in one
  git repo.
