// SPDX-License-Identifier: AGPL-3.0-or-later

// Package web is the built-in web tools: search the web and read a page (ADR 0039).
package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
)

// errRefused is a URL the policy will not reach. Its text names the rule and the host, never
// the address the host resolved to: what the model gets back is what the operator would want
// in the transcript, and an internal address is a fact about the operator's network.
var errRefused = errors.New("refused")

// policy is what a fetch may reach. It is a value: the plugin builds one from config and
// every fetch reads it.
type policy struct {
	// allowPrivate lets an address that is loopback, private, link-local or unique-local
	// through. False is the default and the reason this type exists: the model chooses the
	// URL, so it chooses the address, and this machine's own services and the cloud
	// metadata endpoint are exactly what an untrusted URL would otherwise reach.
	allowPrivate bool
	// resolve is the lookup, injected so a test can decide what a name resolves to without
	// a DNS server. Nil uses the system resolver.
	resolve func(ctx context.Context, host string) ([]net.IP, error)
}

// check refuses a URL this policy will not reach: a scheme that is not http or https, a URL
// with credentials in it, a missing host, or a host that resolves to an address the policy
// excludes. Called for the URL the model gave and again for every redirect, because a
// redirect is the same choice made by somebody else.
func (p policy) check(ctx context.Context, u *url.URL) error {
	_, err := p.vet(ctx, u)
	return err
}

// vet is check that also returns the addresses it accepted, in resolver order. The fetch
// dials only these, so a name that answers differently the second time is never asked again
// (ADR 0053). With allowPrivate the classification is skipped and the addresses are still
// returned, so the dial stays pinned.
func (p policy) vet(ctx context.Context, u *url.URL) ([]net.IP, error) {
	switch u.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("%w: %s is not http or https", errRefused, u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: a URL with credentials in it", errRefused)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("%w: no host", errRefused)
	}
	ips, err := p.lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("%w: %s did not resolve", errRefused, host)
	}
	if p.allowPrivate {
		return ips, nil
	}
	own := ownAddrs()
	for _, ip := range ips {
		if !public(ip) || isOwn(own, ip) || isOwn(own, embeddedV4(ip)) {
			return nil, fmt.Errorf("%w: %s is not a public address", errRefused, host)
		}
	}
	return ips, nil
}

// isOwn reports whether ip is one of this machine's addresses. A nil ip is not.
func isOwn(own []net.IP, ip net.IP) bool {
	return ip != nil && slices.ContainsFunc(own, ip.Equal)
}

// ownAddrs is every address on this machine's interfaces. A listener bound to all interfaces
// answers on the machine's public address too, so that address reaches the same services
// loopback does. An error reads as none: the classification still applies.
func ownAddrs() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var ips []net.IP
	for _, a := range addrs {
		switch v := a.(type) {
		case *net.IPNet:
			ips = append(ips, v.IP)
		case *net.IPAddr:
			ips = append(ips, v.IP)
		}
	}
	return ips
}

func (p policy) lookup(ctx context.Context, host string) ([]net.IP, error) {
	// The injected resolver first, including for a literal address: a test standing up a
	// server on loopback needs to say what that address counts as, and production never
	// sets one.
	if p.resolve != nil {
		return p.resolve(ctx, host)
	}
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	if len(ips) == 0 {
		return nil, errors.New("no addresses")
	}
	return ips, nil
}

// public reports whether an address is one the internet routes to somebody else. Every other
// kind is refused by default: loopback and link-local reach this machine, private and
// unique-local reach the operator's network, and the unspecified and multicast ranges reach
// whatever the stack makes of them.
func public(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	// Unique local addresses (fc00::/7) are IPv6's private range and net.IP.IsPrivate
	// covers them, but the IPv4-mapped form of a private v4 address does not answer to it.
	v4 := ip.To4()
	if v4 == nil {
		if e := embeddedV4(ip); e != nil {
			return public(e)
		}
		return true
	}
	switch {
	case v4[0] >= 240: // reserved, and the broadcast address
		return false
	case v4[0] == 0, v4[0] == 127, v4[0] == 10:
		return false
	case v4[0] == 169 && v4[1] == 254: // link local, the metadata endpoint's range
		return false
	case v4[0] == 172 && v4[1]&0xf0 == 16:
		return false
	case v4[0] == 192 && v4[1] == 168:
		return false
	case v4[0] == 100 && v4[1]&0xc0 == 64: // carrier grade NAT
		return false
	}
	return true
}

// embeddedV4 is the IPv4 address an IPv6 address carries inside it, or nil. NAT64, 6to4,
// Teredo and the IPv4-compatible form each route to the embedded address, so the IPv4 rules
// have to apply to it: the IPv6 switch alone would pass 64:ff9b::7f00:1 as global unicast.
func embeddedV4(ip net.IP) net.IP {
	b := ip.To16()
	if b == nil {
		return nil
	}
	tail := net.IP{b[12], b[13], b[14], b[15]}
	switch {
	case slices.Equal(b[:12], []byte{0, 0x64, 0xff, 0x9b, 0, 0, 0, 0, 0, 0, 0, 0}): // 64:ff9b::/96
		return tail
	case b[0] == 0 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b && b[4] == 0 && b[5] == 1: // 64:ff9b:1::/48
		return tail
	case b[0] == 0x20 && b[1] == 0x02: // 2002::/16, 6to4
		return net.IP{b[2], b[3], b[4], b[5]}
	case b[0] == 0x20 && b[1] == 0x01 && b[2] == 0 && b[3] == 0: // 2001::/32, Teredo
		return net.IP{tail[0] ^ 0xff, tail[1] ^ 0xff, tail[2] ^ 0xff, tail[3] ^ 0xff}
	case !slices.ContainsFunc(b[:12], func(x byte) bool { return x != 0 }) && !tail.IsUnspecified():
		return tail // ::/96
	}
	return nil
}

// hostOf is the host a message names, for the rate limiter's per-host key.
func hostOf(u *url.URL) string { return strings.ToLower(u.Hostname()) }
