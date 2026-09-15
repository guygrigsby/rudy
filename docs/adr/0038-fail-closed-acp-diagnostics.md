# 38. Fail closed on ACP diagnostics

Status: Accepted

## Context

`rudy bridge` can show local log detail because its stream is Rudy's private transport. `rudy acp`
uses stdout for ACP and stderr crosses the SSH trust boundary. Falling back to stderr when the box
log cannot open or later fails could expose provider bodies, credentials, prompts, tool input or
captured daemon stderr to the remote caller.

## Decision

Select an ACP-specific 0600 box log sink before reading an ACP frame. If it cannot open, emit only a
fixed diagnostic with a bounded correlation id and exit. Never install stderr as the record sink.

Wrap the opened sink so a later write failure signals the adapter supervisor. Close the ACP stream
and internal daemon connection and emit only the same fixed diagnostic. Retain or discard captured
daemon stderr locally, but never forward it through ACP stderr. Keep ordinary operator-visible
stderr fallback for non-ACP commands.

## Consequences

- Diagnostic failure cannot turn into a secret-bearing remote channel.
- ACP may terminate when durable diagnostic logging is unavailable.
- Correlation still lets the operator find local evidence when the sink remains usable.
- Bridge compatibility behavior remains unchanged until bridge retirement.
