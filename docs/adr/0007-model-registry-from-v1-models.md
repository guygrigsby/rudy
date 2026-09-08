# ADR 0007: The model registry comes from `/v1/models`

- Status: Accepted
- Date: 2026-09-07
- Deciders: Guy Grigsby

## Context

Hardcoded model lists rot. pi-go embeds llm-prices snapshots and routes by
name prefix, so a proxy model rides an escape hatch and never reaches the
picker. The standing rule from the pi extensions is that model selection
is data-driven from the live registry, never from a list in config.

The aperture proxy's `/v1/models` was probed on 2026-09-07. It returns 64
models and, beyond the standard `id` and `owned_by`, carries
`display_name`, `context_window_tokens`, `max_output_tokens`,
`supported_endpoints`, `pricing` with per-token `input`,
`input_cache_read` and `output`, and `metadata.provider.id`.

## Decision

Config names providers and one default model. It never lists models.

The registry is discovered per provider from its `/v1/models` (or the
Anthropic equivalent) and cached on disk as a snapshot with the fetch
time. Refresh is event-driven: on session open, on picker open and on a
model-not-found error. There is no timer.

Field mapping reads the standard fields, then the extension fields above
when present. Providers whose listing is bare, such as direct OpenAI or
Ollama, are enriched from catwalk's embedded data keyed by model id.
Enrichment never adds a model that the provider did not list.

Model and registry objects are in the
[domain model](../specs/rudy-domain-model.md).

## Consequences

- Adding a model to the proxy makes it appear in rudy without an edit.
- Prices and context windows for proxy models come from the proxy, so
  cost and compaction thresholds are right without a catalog update.
- A provider that is down at session open serves its last snapshot with
  the fetch time visible.
- Capabilities for a proxy model the enrichment does not match are an
  open question in the context map.

## Alternatives considered

- Embedded catalog with prefix routing, pi-go's model: the thing being
  ruled out.
- catwalk's hosted service as the registry: a network dependency on a
  third party for models the proxy already describes.
- Periodic refresh: a clock tick that fires when nothing changed.
