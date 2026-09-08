# ADR 0001: Build rudy instead of adopting an existing harness

- Status: Accepted
- Date: 2026-09-07
- Deciders: Guy Grigsby

## Context

rudy replaces pi as the daily coding harness. pi covers about 75% of the
need and fails on the rest in ways its extension API cannot fix: tool-row
folding and message spacing required monkey-patching core classes, mouse
reporting is absent, footer and status have no ownership model, the config
dir ignores XDG and the harness rewrites `settings.json` on version bumps.
pi is also Node, and every harness surveyed that is not Node is either
Rust or Go.

Before building, the field was surveyed on 2026-09-07 across Go, Rust, C,
C++, Zig, Swift, Nim, OCaml and Haskell. The wants: a TUI whose render
layer we own, headless and semi-autonomous operation, an extension model
covering skills, hooks, subagents and MCP, live multi-provider model
discovery, and Go so the code is hackable.

| harness | lang | license | why it lost |
|---|---|---|---|
| crush | Go, Bubble Tea | FSL | best Go TUI, but no custom subagents, hooks preliminary, ACP issues unanswered for a year |
| pi-go (dimetron) | Go, Bubble Tea v2 on Google ADK | MIT | every checkbox on paper; fails on render ownership and registry, see below |
| terva | Go, hand-rolled TUI | MIT | fullest feature list, but 2 stars, a fork, development on a private Forgejo |
| Codewhale | Rust | MIT | 40.9k stars, complete checklist, but Rust |
| jcode | Rust | MIT | 19.3k stars, fastest boot, but Rust and reuses `~/.claude` OAuth credentials, the policy ban vector |
| VT Code | Rust | MIT/Apache | ACP, MCP, hooks, skills, subagents, but Rust and 833 stars |
| codex | Rust | Apache | best ratatui TUI, but OpenAI-shaped with no Anthropic path |
| agentty | C++26 | MIT | 604 stars, 13MB static binary, headless only via ACP |

Zig, Swift, Nim, OCaml and Haskell produced nothing past toys. Dead: the
Go opencode, sketch, mods, plandex, kwaak.

pi-go was the closest and was read at code level, not README level. It
compiles clean with `CGO_ENABLED=0` (the ONNX embedder sits behind a build
tag) and its provider and session packages are good reference material.
It fails the actual bar:

- The whole screen is one hardcoded `View()` in a 2,753-line file. Tool
  folding is a single global bool toggled by a keypress, spacing is a
  constant, the theme is embedded with 13 roles and never read from disk.
- Switching `/model` or `/theme` rewrites `~/.pi-go/config.json`. No XDG.
- The model registry is embedded llm-prices snapshots plus prefix tables.
  Live `/v1/models` only refreshes or validates. Proxy models ride an
  escape hatch marked Custom and never populate the picker.
- Extensions are two shell hook events. Slash commands are a fixed array.
  No extension can add a command, tool, widget or system-prompt mutation.
- No permission gate. The ACP permission handler is an auto-approve
  constant; bash is unrestricted inside an `os.Root`.
- Google ADK owns the loop, the model interface, the tool interface and
  session state across 22 packages. The TUI imports 19 internal packages.
  Lifting the TUI without ADK is a rewrite.
- One maintainer with 1,082 of 1,100 commits, 646 in the last 30 days,
  318k lines of Go in under six months.

Claude subscription login, the one feature every candidate fails on, is
not a want. Every provider rudy uses is reached through the aperture proxy
(OpenAI-compatible plus anthropic-messages) or a local endpoint, and
Anthropic's February 2026 policy makes third-party OAuth a ban vector
regardless.

## Decision

Build rudy in Go. The build is justified by render ownership and the
extension contract, not by the loop. Nothing that is library-grade gets
rebuilt:

- provider wire codecs on `anthropic-sdk-go` and the OpenAI-compatible
  shape (ADR 0010)
- MCP via `modelcontextprotocol/go-sdk`
- memory via a Go SDK owned by the memory project (ADR 0008)
- ACP later via a community Go SDK at the edge, never as the internal seam

What rudy builds: the loop and message model, the session log, the gate,
the TUI, the plugin contract, skill and hook loading, headless mode.

## Consequences

- Full control of every render decision and of the plugin surface, which
  is the reason the project exists.
- One more harness to maintain. Mitigated by keeping the kernel small and
  putting every feature behind the same plugin interface (ADR 0005).
- pi-go's `internal/provider` and `internal/session` are read as prior
  art when the equivalent packages are written.

## Alternatives considered

- Fork pi-go: two frameworks (ADK and Bubble Tea) to fight for the parts
  we care about; the jess audit already concluded an ACL over a framework
  is not worth it for one maintainer.
- Adopt crush: FSL license, no subagents, render layer not ours.
- Adopt a Rust harness: not Go, and the top two carry the OAuth risk.
- Keep pi and patch harder: the patches are the problem.
