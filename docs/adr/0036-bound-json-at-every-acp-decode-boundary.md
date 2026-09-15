# 36. Bound JSON at every ACP decode boundary

Status: Accepted

## Context

An 8 MiB frame limit bounds encoded bytes but does not bound the object graph produced by JSON
decode. Duplicate keys can also let a routing classifier and a later typed decoder select different
values. Rudy carries raw tool input inside encoded Entry fragments, so applying one graph limit to
both a nested input and its enclosing Entry would reject an otherwise valid boundary input.

Rudy also produces Registry, command, config, status and widget arrays. A byte-only producer bound
could emit an ACP frame that a conforming Rudy peer must reject under its structural limits.

## Decision

Use one configurable streaming strict-JSON scanner before semantic decode. The outer ACP profile
allows depth 64, 65,536 structural units, 16,384 elements in one array and 4,096 members in one
object. It also requires valid UTF-8 and recursively unique decoded object keys. Both adapters scan
every incoming ACP frame before SDK decode and every complete outgoing frame before enqueue.

Use the outer profile as the nested tool-input profile too, with the added requirement that the
root is one object. Use a separate decoded-event profile for reassembled public Entry and Part JSON:
depth 72, 131,072 structural units and the same per-container and key limits. The extra envelope
headroom keeps one legal maximum-structure tool input representable. The public event projector
checks that profile while streaming and emits a bounded oversized reference on byte or structural
overflow.

Preflight predictable mutation responses and process updates as complete worst-case ACP envelopes
before commit. Registry and command aggregate mutation also preflights their complete result
envelopes. Ordinary query construction that cannot produce a conforming frame returns fixed
internal failure without exposing the rejected value.

Keep ACP types out of Registry by giving its internal catalogue a depth-56 and 60,000-unit budget
in addition to 4 MiB. Cross-package fixtures prove that exact ACP conversion stays within the outer
profile after adding its fixed envelope. Registry commits no candidate that crosses that budget.

## Consequences

- Encoded byte limits and decoded graph limits protect different resources and both apply.
- Duplicate keys cannot select different routing and domain values.
- Rudy does not emit a frame its own peer is required to close on.
- Structurally large durable events remain observable through bounded references.
- The shared scanner needs profiles instead of one hard-coded ceiling.
