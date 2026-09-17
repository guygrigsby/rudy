// SPDX-License-Identifier: AGPL-3.0-or-later

package provider_test

import (
	"testing"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

func TestPricingCost(t *testing.T) {
	aperture := provider.Pricing{Input: "0.00000014", Output: "0.00000028", CacheRead: "0.00000000"}
	cases := []struct {
		name  string
		p     provider.Pricing
		u     session.Usage
		want  string
		known bool
	}{
		{"input only", aperture, session.Usage{Input: 1200}, "0.000168", true},
		{"input and output", aperture, session.Usage{Input: 1200, Output: 80}, "0.000190", true},
		{"cache read priced at zero", aperture, session.Usage{Input: 100, CacheRead: 5000}, "0.000014", true},
		{"zero usage is free", aperture, session.Usage{}, "0.000000", true},
		{"cache write unknown but used", aperture, session.Usage{Input: 1, CacheWrite: 1}, "", false},
		{"no prices at all", provider.Pricing{}, session.Usage{Input: 1}, "", false},
		{"no prices no usage", provider.Pricing{}, session.Usage{}, "0.000000", true},
	}
	for _, c := range cases {
		got, known := c.p.Cost(c.u)
		if known != c.known || got != c.want {
			t.Errorf("%s: got %q %v want %q %v", c.name, got, known, c.want, c.known)
		}
	}
}

func TestErrorString(t *testing.T) {
	e := &provider.Error{Class: session.ErrProvider, Status: 500, Message: "empty response content"}
	if e.Error() != "provider: provider 500 empty response content" {
		t.Fatalf("got %q", e.Error())
	}
}

// TestOutputBudget pins what one request may produce. The registry already knows each
// model's ceiling, so a flat config value was both too low for a model that serves 128000
// and too high for one that serves 64000; the first truncated a large write, the second was
// refused by the endpoint.
func TestOutputBudget(t *testing.T) {
	known := provider.Model{MaxOutput: 64000}
	unknown := provider.Model{}
	cases := []struct {
		name       string
		model      provider.Model
		configured int
		want       int
	}{
		{"unset takes the model's own", known, 0, 64000},
		{"a value under the ceiling is the operator's", known, 8192, 8192},
		{"a value over it is clamped rather than refused", known, 1000000, 64000},
		{"negative is treated as unset", known, -1, 64000},
		{"unset with no ceiling known falls back", unknown, 0, 8192},
		{"a value with no ceiling known is sent as asked", unknown, 32000, 32000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.model.OutputBudget(c.configured); got != c.want {
				t.Errorf("OutputBudget(%d) = %d, want %d", c.configured, got, c.want)
			}
		})
	}
}
