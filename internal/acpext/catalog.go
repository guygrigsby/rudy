// Package acpext defines Rudy-owned metadata and payloads for the negotiated
// _rudy ACP extension. Stable ACP wire types stay in the two adapter packages.
// Those adapters use the maintained github.com/coder/acp-go-sdk v0.13.5 module,
// selected under its Apache-2.0 license instead of copying the stable schema.
package acpext

import (
	"slices"
)

//go:generate go run ./cmd/cataloggen -contract ../../docs/specs/rudy-contracts.md -output catalog_gen.go

// Capability names one independently negotiated Rudy extension behavior.
type Capability string

// MethodName is the ACP method attached to a Rudy capability.
type MethodName string

// Entry is one row in the generated Rudy extension catalogue.
type Entry struct {
	Capability   string
	Method       string
	Notification bool
}

// All returns the extension catalogue sorted by capability.
func All() []Entry {
	return slices.Clone(catalogue[:])
}

// CapabilityNames returns every generated capability name in lexical order.
func CapabilityNames() []string {
	return slices.Clone(capabilityNames[:])
}

// MethodNames returns every generated method name in lexical order.
func MethodNames() []string {
	return slices.Clone(methodNames[:])
}

// ByCapability returns the catalogue row for name.
func ByCapability(name string) (Entry, bool) {
	i, ok := slices.BinarySearchFunc(catalogue[:], name, func(entry Entry, target string) int {
		if entry.Capability < target {
			return -1
		}
		if entry.Capability > target {
			return 1
		}
		return 0
	})
	if !ok {
		return Entry{}, false
	}
	return catalogue[i], true
}

// CanonicalCapabilities returns names sorted with duplicates removed.
func CanonicalCapabilities(names []string) []string {
	canonical := slices.Clone(names)
	slices.Sort(canonical)
	return slices.Compact(canonical)
}
