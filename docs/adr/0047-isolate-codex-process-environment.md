# 47. Isolate the Codex process environment

Status: Accepted. Extends ADR 0045.

Date: 2026-09-26

Deciders: Guy Grigsby

## Context

Rudy may read provider keys and service credentials from its environment. Codex
App Server owns an agent loop whose tools inherit its process environment by
default. Passing Rudy's complete environment into App Server therefore gives
repository instructions and model-generated commands access to credentials
unrelated to Codex.

Codex still needs paths for its executable, account store, shell, temporary
files, locale and TLS certificate discovery. Tests also need explicit process
overrides to drive the fake App Server.

## Decision

The operational environment allowlist is `CODEX_HOME`, `HOME`, `PATH`, `SHELL`,
`TMPDIR`, `TMP`, `TEMP`, `LANG`, `LC_ALL`, `LC_CTYPE`, `SSL_CERT_FILE` and
`SSL_CERT_DIR`. Start both `codex --version` and `codex app-server` with only
that allowlist. Include a variable only when it exists in Rudy's environment.

Apply explicit `Command.Env` entries after the inherited allowlist. An explicit
entry replaces an inherited value with the same name and otherwise adds the
entry. This is deliberate delegation by the process owner, not ambient
inheritance.

## Consequences

Codex account storage, command discovery, temporary files, locale and custom CA
paths keep working. Ambient Rudy credentials, provider configuration and agent
sockets do not cross the process boundary.

An installation that needs another environment variable must pass it as an
explicit process override. Broad inheritance and name-pattern filtering were
rejected because new credential names would fail open.
