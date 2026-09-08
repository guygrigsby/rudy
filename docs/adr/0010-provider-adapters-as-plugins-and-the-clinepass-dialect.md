# ADR 0010: Provider adapters are plugins, and the clinepass dialect

- Status: Accepted
- Date: 2026-09-07
- Deciders: Guy Grigsby

## Context

Every model rudy uses is reached over one of two wire protocols: Anthropic
messages, and OpenAI chat completions as served by the aperture proxy,
mlx and llama.cpp. The proxy's cline-pass route claims OpenAI
compatibility and does not strictly deliver it.

Probed on 2026-09-07 against `cline-pass/deepseek-v4-flash` and
`cline-pass/kimi-k3`:

- Non-streaming responses are wrapped: `{"data": {<chat.completion>}}`
  instead of the bare object. Streaming is standard SSE chunks.
- Errors arrive as `{"error": "<text>", "success": false}`.
- Reasoning arrives in `message.reasoning` and `reasoning_details` on
  non-streaming, and as `reasoning` and `reasoning_details` deltas when
  streaming, the OpenRouter shape.
- A `max_tokens` too small to finish reasoning yields HTTP 500 with
  `empty response content` rather than an empty completion.

## Decision

Two wire kinds exist as codecs in the Provider context:
`anthropic_messages` on `anthropic-sdk-go`, and `openai_chat` as a
shared codec lifted from the `llm` module's OpenAI wire shape and retyped
onto rudy's model. Provider adapters are plugins (ADR 0005) that bind a
codec to an endpoint, an auth reference and headers.

The clinepass plugin is the first proof that adapters are plugins. It
reuses the `openai_chat` codec and adds:

- unwrap the `data` envelope on non-streaming responses
- map the `error` and `success` envelope to a provider error
- map `reasoning` and `reasoning_details` to thinking blocks, both
  streaming and not
- surface the empty-content 500 as a provider error whose message names
  the token budget as the likely cause

Codec and adapter objects are in the
[domain model](../specs/rudy-domain-model.md).

## Consequences

- Adding a dialect is a plugin, not a branch in the codec.
- The probe responses are kept as fixtures so the plugin is tested
  offline.
- Only the two codec packages import an SDK. `grep anthropic.` and
  `grep openai.` return hits nowhere else.

## Alternatives considered

- A dialect enumeration inside the codec: every quirk becomes a branch in
  shared code.
- charm's fantasy: retagged five times in two weeks chasing SDK minors.
- Using `openai-go` for the compatible path: the `llm` module's shape is
  already hardened for streaming, idle timeout and cancellation.
