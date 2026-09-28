# 49. Isolate the Codex Account Home

Status: accepted

Supersedes: ADR 0048

## Context

Codex loads account state, project trust, exec-policy rules, MCP servers and app
approval modes from its home and effective project configuration. A persisted
exec-policy allow can bypass the local sandbox. An app or MCP tool configured
for automatic approval can perform a remote mutation without an App Server
approval request. Read-only sandboxing alone does not close either path.

Rudy needs Codex account persistence to use a ChatGPT subscription but must not
inherit authority granted to another Codex client.

## Decision

Force the built-in App Server's `CODEX_HOME` to
`$XDG_DATA_HOME/rudy/codex`. Ignore ambient and explicit `CODEX_HOME` values for
that runtime. Codex owns account persistence inside this directory; Rudy never
reads or copies credential files. Start the App Server process in that directory
and pass each thread's workspace explicitly.

Rudy creates and byte-verifies an exact `config.toml` defining permission
profile `rudy_strict`. The profile reads minimal system paths and project roots,
denies the dedicated and ambient Codex account homes and disables network.
Select it at thread start, resume, fork and
every turn. Use approval policy `on-request`, route review to `user`, enable
`features.exec_permission_approvals` and `features.request_permissions_tool`
and disable `features.apps`, `features.plugins` and
`features.remote_plugin`.

Reject permissive and off mode before starting a process or mutating a thread.
Reject any `rules` path and any unexpected `config.toml`. Reject filesystem
permission grants at or below either account home after resolving existing
symlinks. Decline all file-change approvals because App Server 0.155.1 does not
expose their concrete paths; a validated permission request can preapprove a
safe patch without emitting that opaque request.

Require exactly Codex CLI 0.155.1. These permission fields and merge semantics
are experimental, so a newer version is unreviewed rather than implicitly
trusted. Treat system and managed policy outside the dedicated home as higher
operator or organization authority. Workspace project configuration remains
untrusted because the isolated home carries no inherited project trust.

## Consequences

`/login` authenticates Rudy's Codex runtime separately from other Codex clients.
Existing Codex CLI login state and approvals are not imported. Strict mode
cannot inherit an exec-policy sandbox bypass or an auto-approved remote tool.
Deleting Rudy's dedicated Codex home signs this runtime out without changing
another Codex installation. Upgrading Codex requires a security review, fixture
refresh and a passing installed-binary isolation test before changing the pin.
