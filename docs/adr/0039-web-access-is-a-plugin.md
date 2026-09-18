# 39. Web access is a plugin: search and fetch

Status: Accepted. The registration rule is superseded by ADR 0040.

Date: 2026-09-16

Deciders: Guy Grigsby

## Context

Rudy cannot read the web. Its tools are the workspace: read, write, edit, glob, grep and bash.
A model asked about a library's current API, an error nobody has seen before or a page the
operator names has nothing to reach for, and the session ends with the operator pasting the
answer in by hand.

Two ways to close it. An MCP server is ten minutes of configuration and no rudy code, and it
puts a node or python runtime between rudy and the web, on every machine a kernel runs on,
including the box behind `--host` (ADR 0029). A built-in tool keeps rudy one binary and takes
the Gate, the config catalogue and the session log as they already are.

Search alone is half a capability: results are titles and snippets, and the page they name is
where the answer is.

## Decision

Web access ships as a plugin, `tools.web`, registered through the same interface every other
built-in tool uses. The kernel gains nothing.

It registers two tools.

`web_search` takes a query and returns bounded results, each a title, URL and snippet. It is
`safe`: the query reaches one of the endpoints the operator configured.

Its backends are a chain, asked in order: Brave first, then Exa. One vendor's quota, outage
or bad gateway should cost a turn a few hundred milliseconds rather than the capability, and
a search key is the kind of thing that expires on a Sunday. Each backend has its own key
reference in config and a backend without one is not in the chain. An empty result set is an
answer and ends the chain: the fallback does not get to overrule the first backend about what
the web holds.

`web_fetch` takes a URL and returns the page as text. It is `unsafe`: the URL is the model's,
so the host is the model's, and an unsafe class is what makes strict mode ask before a session
reaches an arbitrary third party. A session allowance covers the rest of the session once the
operator says yes.

Neither tool is registered when no key is configured. A tool the model can see and cannot use
is a turn spent discovering that.

Fetch is GET over http or https, with redirects followed to a fixed depth and re-checked at
each hop, no cookies, no credentials and no request body. The response is capped, HTML is
reduced to text in this process rather than handed to the model as markup, and the text is
capped again. The plugin sends a real User-Agent naming rudy and its version, honours
`Retry-After` in both its forms, and backs off on the transient statuses.

Fetch refuses a host that resolves to a loopback, private, link-local or unique-local address
unless `tools.web.allow_private_hosts` says otherwise. The refusal is at resolution and at
every redirect, because the model choosing the URL means the model choosing the address, and
the addresses worth reaching that way are this machine's own services and the cloud metadata
endpoint.

## Consequences

- Rudy can answer from the web without a second runtime, on the Mac and on the box alike.
- A page fetched is untrusted text placed in the model's context, and rudy has no fence around
  it: a page can carry instructions and the model will read them. That exposure belongs in the
  README's trust model alongside the rest.
- A search backend is one vendor's API behind an interface in rudy's own types. A third
  backend is another implementation, not another tool.
- Exa without a key, which is how pi's web extension works out of the box, means speaking MCP
  to their hosted server. Rudy already ships an MCP client, and `rudy mcp add` is the path
  for somebody else's hosted tools, so this plugin does not grow a second one.
- Brave rate limits and a plan's monthly quota now bound part of what a session can do. Rudy
  reports the refusal rather than retrying past it.
- An operator who wants a search MCP server instead may still have one; this decides what
  rudy ships, not what it permits.
