# 25. How a plugin is published, installed and trusted

- Status: Accepted
- Date: 2026-09-10
- Deciders: Guy Grigsby

## Context

Subagents and LSP are the next two features, and both arrive as plugins. Before
inviting anyone to publish one, three things had to be settled: where a plugin
comes from, how a cloned one becomes runnable, and who decided it may run at all.

The last question was a hole. `spawnedPlugins` discovers manifests under the
workspace's own `.rudy` alongside the operator's roots and spawns them at boot,
so cloning a repository and running `rudy` in it executed whatever that
repository declared, with the operator's permissions, before a session existed.
The Gate could not help: it governs the model's tool calls, not what the harness
spawns for itself.

pi's answer to the first question is that there is no registry of its own: npm is
the registry, git is the fallback, and a package is a bundle of resources. Its
answer to the third is that a project is trusted once, by a person, before its
packages load.

## Decision

1. **Sources are a vocabulary, and git is not the only one.** `rudy install`
   takes `go:module/path@version`, installed the way `go install` does;
   `git:host/user/repo@ref`; a raw https URL; and a local path. Each records what
   it resolved to in the lock, so an install is reproducible and an update is a
   diff rather than a surprise.

   There is no rudy package server. A registry is a thing somebody runs forever;
   the Go module proxy and git already exist and already answer the question.

2. **`rudy install` is the top-level spelling.** It is what everybody types, and
   it is the one verb this CLI adds to its exceptions beyond `serve` (ADR 0022),
   with that reason recorded in the shape test. `rudy plugins install` remains
   the noun-then-verb form and does the same thing.

3. **A manifest may name a `build` command.** It runs once in the checkout after
   staging and before the manifest is accepted, so a Go plugin can be cloned and
   built rather than shipped as a binary or left to `go run`. It executes
   arbitrary code by construction. That is what installing from a source means,
   and the install path says so rather than implying otherwise.

4. **A workspace's own plugins load only after that workspace has been trusted,
   once.** The decision is remembered per workspace root. A client that can ask,
   asks; one that cannot (`--print`, a pipe, no terminal) does not load them and
   says why, which is the same rule the Gate keeps for an unsafe tool with no
   asker: no answer means no.

   Trust is per root and per source: a workspace whose `.rudy` changes after
   being trusted is trusted for what it was, so the record carries what was
   agreed to and a change asks again.

## Consequences

- The install path runs code from a source the operator named, twice over: the
  build command and then the plugin itself. Both are the point, and the warning
  is part of the command rather than a line in a document nobody reads.
- A project that ships a plugin gets it loaded for a teammate only after that
  teammate agrees, which is a prompt they will see once per repository.
- `rudy -p` in an untrusted repository runs with the user's plugins only. That is
  a behaviour difference between headless and interactive, and it is the safe
  direction.

## Alternatives considered

- A rudy package registry: infrastructure to run, moderate and keep up, for a
  problem git and the Go proxy already solve.
- Prebuilt binaries only, no build step: pushes every plugin author into a
  release pipeline before their first user, and rudy would still be running
  their binary.
- Trusting a workspace implicitly because the operator opened it: opening a
  repository to read it is not agreeing to run it, and reading a repository is
  the first thing anybody does with one.
