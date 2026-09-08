# ADR 0005: Plugins, linked in and spawned, under one interface

- Status: Accepted
- Date: 2026-09-07
- Deciders: Guy Grigsby

## Context

In pi everything is an extension and the core is small. Claude Code is a
large binary with extension points at the edges. The preference is pi's
shape. Go constrains it: the `plugin` package is cgo-only and effectively
dead, so code cannot be loaded at runtime.

The needs an extension must meet, taken from the pi extensions actually in
use: mutate the system prompt per turn, read a live model registry with
prices, intercept provider requests and headers, add slash commands and
tools, own a status item or widget slot, render display-only entries, and
never refuse to boot over a duplicate tool name.

## Decision

Two tiers, one interface:

1. Linked-in plugins. Every built-in is a Go package that registers
   through the plugin API and nothing else. The kernel has no private path
   to its own features. The built-in bash tool can only do what an
   external plugin could do.
2. Spawned plugins. A subprocess speaking the protocol of ADR 0002 over
   stdio, any language. It receives the notifications a client receives
   plus the `plugin.register_*` requests. MCP servers are the degenerate
   case that only registers tools.

A linked-in plugin implements the same Go interface the wire protocol
serves. The protocol is a transport over that interface. A plugin does not
know whether it was linked or spawned.

The kernel is what a plugin needs to exist: session log, turn loop, gate,
protocol server, plugin registry, provider port, config, workspace.
Everything else starts as a plugin:

- tools: read, write, edit, bash, grep, glob, each its own package
- slash commands, including `/model`, `/help`, `/fork`
- provider adapters: anthropic messages, openai chat, clinepass (ADR 0010)
- skills loader, hooks runner, subagents, compaction, MCP bridge, memory
  adapter (ADR 0008), the `~/.agents` discovery
- later: LSP, sandbox, the ACP adapter for editors

The TUI and the headless printer are clients, not plugins. A client
renders and asks. A plugin extends.

A duplicate tool name is refused with a notice and the plugin still loads.
Plugin, capability and hook point objects are in the
[domain model](../specs/rudy-domain-model.md).

## Consequences

- The plugin API is exercised from day one by every built-in, so it
  cannot rot the way jess's library surface did.
- Spawned plugins carry subprocess overhead. Acceptable for v1.
- WASM via wazero, pure Go and sandboxed, is a third tier that can be
  added without changing the interface. Deferred until subprocess
  overhead is measured to matter.

## Alternatives considered

- Go `plugin` package: cgo-only, unusable on macOS in practice.
- Lua or JS embedding: another language nobody asked for.
- Shell hooks only, pi-go's model: cannot add a command, a tool or a
  widget.
- WASM now: adds a toolchain requirement before it is needed.
