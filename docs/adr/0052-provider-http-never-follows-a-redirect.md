# 52. Provider HTTP never follows a redirect

- Status: Accepted
- Date: 2026-10-07
- Deciders: Guy Grigsby

## Context

The shared provider HTTP client sets no `CheckRedirect`, so a 3xx from a
`base_url` is followed up to ten times. Go drops `Authorization` and
`Cookie` on a hop to another domain and keeps every other header: the
Anthropic codec's `x-api-key`, every `providers.<name>.headers` value and
every `before_request` hook header travel to whatever host the redirect
names, and a 307 or 308 replays the body, which is the system prompt and the
conversation. A proxy that answers an expired session with a 302 to a login
page is enough (`rudy-mf6`). The MCP plugin and the web plugin already refuse
to follow.

## Decision

The provider client does not follow redirects. A 3xx response is a transport
failure whose message names the status and the host the `Location` points at,
so the operator can set `base_url` to the endpoint that answers. The same
client serves the models listing, so an absolute `models_path` on another host
receives the chat endpoint's credentials by the operator's choice and is
documented as doing so.

## Consequences

- An endpoint behind a redirecting front door needs its final URL in config.
  That was already the only configuration that worked without leaking.
- No credential header leaves for a host the operator did not name.
