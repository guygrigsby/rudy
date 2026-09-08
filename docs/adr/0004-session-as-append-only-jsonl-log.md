# ADR 0004: A session is an append-only JSONL log

- Status: Accepted
- Date: 2026-09-07
- Deciders: Guy Grigsby

## Context

The standing rule is Postgres for every datastore. A session transcript is
an append-only event log, byte-exact, resumed and forked by replay, and
nothing in v1 needs a relational query over it. Requiring a running
Postgres to open a chat is a cost with no query to justify it. The
exception was raised and approved on 2026-09-07.

pi, pi-go, Claude Code and Codex all store sessions as JSONL files and
all support resume and fork from them.

## Decision

One directory per session under `$XDG_DATA_HOME/rudy/sessions/<ulid>/`
holding one file, `entries.jsonl`. Nothing else is stored. Model, mode,
thinking level, title, workspace and usage are derived from the entries.
The closed set of entry kinds and every payload field are in the
[domain model](../specs/rudy-domain-model.md).

Rules the log enforces:

- Entries are immutable once appended. Corrections are new entries.
- An unsafe tool runs only after its `permission_decision` allow is
  appended and fsynced.
- On resume, an allow with no `tool_result` gets one with outcome `lost`,
  so a crash is recorded rather than left as a hole.
- Tool inputs and thinking signatures are stored as the raw bytes the
  provider sent. They are never re-marshaled, because providers verify
  signatures and the model sees its own input back.
- A fork is a new session whose first entry is `fork_point`. Entries
  before the fork point are read from the parent at load time, never
  copied. Deleting a session that has children is refused.

Postgres is not in v1. If provenance across sessions ever needs a query,
the entries are the source and a projection is added then.

## Consequences

- Sessions are greppable, diffable and syncable with the same tools as
  the memory bundle.
- Session listing reads the first line of each file. A cached index is
  added when that is measurably slow, not before.
- Fork by reference means byte fidelity and no duplication, at the cost
  of a delete guard.
- The schema version rides on the `session_opened` entry so old logs stay
  readable.

## Alternatives considered

- Postgres per the standing rule: no query needs it and it makes the
  harness depend on a running database.
- SQLite: the rule forbids it and it adds nothing over a file for an
  append-only log.
- Fork by copying entries: simpler delete, but duplicates bytes and
  invites divergence.
