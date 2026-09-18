# 40. web_fetch needs no search key

Status: Accepted. Supersedes ADR 0039's registration rule.

Date: 2026-09-17

Deciders: Guy Grigsby

## Context

ADR 0039 registered `web_search` and `web_fetch` together and said neither is registered
when no key is configured, because "a tool the model can see and cannot use is a turn spent
discovering that".

That reasoning is about search. A search tool with no backend has nowhere to send the query,
and a model that finds out by trying has wasted the turn. `web_fetch` is not in that
position: it takes a URL and reads it. It asks Brave nothing and Exa nothing, and it works
exactly as well with no key configured anywhere.

So the rule cost the capability it was protecting. An operator with no search key could not
have the model read a page they pasted into the conversation, and the notice told them to go
and get a search key to enable a tool that does not use one. The integration suite found it
sideways: the web battery reported `unknown tool web_fetch` on CI while passing on a laptop
whose environment happened to carry a real key.

## Decision

The two tools register independently.

`web_fetch` is always registered. It needs no backend, no key and no configuration, and its
own refusals (the address policy, the size caps, the redirect re-checks) are what bound it.

`web_search` is registered only when a backend has a key, which is ADR 0039's rule kept where
its reasoning holds.

The startup notice names only what is missing. With no key it says search is off and which
keys turn it on, rather than claiming fetch is off too.

## Consequences

A model always has a way to read a URL it was given, which is the half of web access that
needs no account anywhere.

`web_fetch` is `unsafe` and that does not change: strict mode asks before a session reaches a
third party, and the address policy still refuses loopback, private, link-local and
unique-local hosts at resolution and at every redirect. What changes is only which tools exist,
not what they are allowed to do.

An operator who configured no web keys now has a tool they did not have before. That is the
point, and it is also the surprise: a session that could not reach the network can now fetch a
URL the model chose, subject to the Gate. The permission classes are what stand there, as they
do for bash.
