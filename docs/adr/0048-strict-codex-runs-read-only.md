# 48. Strict Codex Runs Read Only

Status: accepted

## Context

Codex App Server combines an approval policy with a sandbox policy. Mapping
Rudy strict mode to approval policy alone leaves workspace writes and network
access dependent on the operator's Codex configuration. Those effects could
bypass Rudy's fsynced approval record.

## Decision

Map Rudy strict mode to App Server approval policy `untrusted`. Send sandbox
mode `read-only` at `thread/start` and sandbox policy
`{type:"readOnly",networkAccess:false}` at every `turn/start`.

Map permissive mode to `on-request` and off mode to `never`. Leave their
sandbox policy to the operator's Codex configuration.

## Consequences

Strict-mode writes, network access and unsandboxed commands require an App
Server approval request that Rudy can deny or durably authorize. Operator
configuration cannot silently weaken strict mode. Strict sessions may prompt
more often than the same Codex configuration used directly.
