# 50. Workspace trust covers every declaration a workspace makes

- Status: Accepted
- Date: 2026-10-07
- Deciders: Guy Grigsby

Amends ADR 0025 decision 4.

## Context

ADR 0025 closed one hole: a workspace's own plugins under `.rudy/plugins` load
only after the operator trusts the workspace. The security audit of 2026-10-07
(`rudy-a6t`, `rudy-md0`, `rudy-hz09`) found the same hole open three more ways.

- `<workspace>/.rudy/mcp.toml` is merged over the user scope by name and every
  entry connects at plugin load, before any session and in every entry point
  including `rudy -p`. A stdio entry runs the command it names with the full
  environment, and its `env` and `headers` values resolve `cache:KEY`, so a
  cloned repository can run code as the operator and hand it any line of the
  1Password cache, with no question asked.
- `<workspace>/.rudy/agents/default.md` is resolved as the root session's agent
  whenever the operator has no `default.md` of their own, which nothing ships.
  Its body replaces the system prompt; its `model`, `thinking`, `tools` and
  `max_turns` take effect. A plugin may register the same name.
- The trust digest hashes a manifest's name, version, command, args and build.
  A plugin that ships its binary beside the manifest, or sets `env`, changes
  what runs without changing the digest, so "a change asks again" was not
  true of the two things that most matter.

## Decision

1. **A workspace declaration is anything under the workspace that makes rudy
   run code or puts text in front of the model as its own.** Three kinds today:
   a plugin manifest, an MCP server entry, an agent definition. The trust
   record covers the set of them, and the question the operator answers lists
   all three kinds.

2. **Each declaration carries a digest of what it would do.** A plugin: name,
   version, command, args, build, the `env` table, and the bytes of the
   executable when the command resolves to a file inside the plugin's own
   directory. An MCP server: name, transport, command, args, `env`, `url` and
   `headers` as written (references, never resolved values). An agent
   definition: the bytes of its file. The workspace digest is the hash of the
   sorted declaration digests. Any change asks again.

3. **The gate is applied where each kind loads.** Plugins and the project
   `mcp.toml` load at boot, in the directory rudy started in, so the client
   asks there as ADR 0025 already has it, and an untrusted workspace gets the
   user scope only. Agent definitions load per session in that session's
   workspace, which may not be the daemon's, so the server reads the trust
   record at `session.open` and loads `.rudy/agents` only for a trusted root,
   with a notice naming `rudy plugins trust` otherwise. The server never asks:
   no asker means no, as before.

4. **A plugin may not register an agent named `default`.** The default agent
   is the root session's prompt, and an operator who has not written one has
   not delegated it. `plugin.register_agent` with that name is
   `invalid_argument`.

5. **`rudy plugins trust` keeps its spelling** and trusts every declaration the
   workspace makes. The record lists them by kind and name so the file reads
   as what was agreed to.

## Consequences

- Everyone who trusted a workspace before this change is asked once more,
  because the digest covers more than it did.
- A daemon serving a workspace it was not started in loads that workspace's
  agents only after `rudy plugins trust` has been run there.
- `rudy -p` in an untrusted repository runs with the user's plugins, the
  user's MCP servers and the user's agents only, which is what ADR 0025 said
  and the code did not do.
- `AGENTS.md` and `.agents/skills` are still read without trust (`rudy-367i`);
  they put text in front of the model but run nothing, and whether they belong
  under the same record is left to that issue.
- The contracts row for `<workspace>/.rudy/config.toml` described an override
  nothing reads. It is struck: a project config would be a declaration under
  this record, and until one is designed there is none.

## Alternatives considered

- A separate yes per kind: three prompts for one repository, and a record in
  three parts to reason about. One question, one record.
- Trusting MCP servers implicitly because `mcp.toml` is "configuration": it
  names a command to run with the operator's environment, which is what a
  plugin manifest does.
- Hashing the whole `.rudy` tree: asks again on a README edit, and still
  misses an executable the manifest names outside the directory.
