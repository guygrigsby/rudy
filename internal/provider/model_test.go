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
