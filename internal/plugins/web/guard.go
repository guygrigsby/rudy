// Package web is the built-in web tools: search the web and read a page (ADR 0039).
package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
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
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("%w: %s is not http or https", errRefused, u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("%w: a URL with credentials in it", errRefused)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: no host", errRefused)
	}
	if p.allowPrivate {
		return nil
	}
	ips, err := p.lookup(ctx, host)
	if err != nil {
		return fmt.Errorf("%w: %s did not resolve", errRefused, host)
	}
	for _, ip := range ips {
		if !public(ip) {
			return fmt.Errorf("%w: %s is not a public address", errRefused, host)
		}
	}
	return nil
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
		return true
	}
	switch {
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

// hostOf is the host a message names, for the rate limiter's per-host key.
func hostOf(u *url.URL) string { return strings.ToLower(u.Hostname()) }
