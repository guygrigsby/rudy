# 53. web_fetch dials the address it vetted

- Status: Accepted
- Date: 2026-10-07
- Deciders: Guy Grigsby

Amends ADR 0039.

## Context

ADR 0039 refuses loopback, private and link-local addresses at every redirect
so a URL cannot reach this machine's own services. The check resolves the
host and classifies the addresses; the dial then resolves the host again. An
authoritative server that answers public on the first query and `127.0.0.1`
on the second passes the check and reaches loopback, the LAN and the cloud
metadata endpoint. The policy also admits every global unicast address, so
the machine's own public address reaches any listener bound to all
interfaces, and the NAT64, 6to4, Teredo and IPv4-compatible prefixes carry an
embedded private address past the IPv4 switch (`rudy-75j`).

## Decision

1. The policy returns the addresses it vetted, and the fetch dials only
   those. The dialer never resolves a name. The TLS server name stays the
   hostname. After connecting, the peer address is checked against the vetted
   set, so a dial that lands elsewhere is refused rather than read.
2. An address on one of this machine's own interfaces is refused.
3. An IPv6 address that embeds an IPv4 address (`64:ff9b::/96`,
   `64:ff9b:1::/48`, `2002::/16`, `2001::/32`, `::/96`) is classified by the
   address it embeds. `240.0.0.0/4` and the broadcast address are refused.

## Consequences

- A host whose name resolves to several addresses is dialed at each in turn
  until one connects, the way the stack would have, but only among the vetted
  ones.
- `web.allow_private_hosts` still turns the whole policy off, for a test
  server on loopback.
- The README sentence "a URL cannot reach this machine's own services" is
  true again against an attacker with a domain.
