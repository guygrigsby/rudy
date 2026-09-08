package provider

import (
	"math/big"

	"github.com/guygrigsby/rudy/internal/session"
)

// Pricing is USD per token as decimal strings, verbatim from the provider. "" is unknown.
type Pricing struct {
	Input      string `json:"input"`
	Output     string `json:"output"`
	CacheRead  string `json:"cache_read"`
	CacheWrite string `json:"cache_write"`
}

// Cost prices a usage. It is known only when every price that a non-zero count needs
// is present. The result carries six decimals.
func (p Pricing) Cost(u session.Usage) (usd string, known bool) {
	terms := []struct {
		price string
		n     int64
	}{
		{p.Input, u.Input}, {p.Output, u.Output}, {p.CacheRead, u.CacheRead}, {p.CacheWrite, u.CacheWrite},
	}
	total := new(big.Rat)
	for _, t := range terms {
		if t.n == 0 {
			continue
		}
		r, ok := new(big.Rat).SetString(t.price)
		if !ok {
			return "", false
		}
		total.Add(total, r.Mul(r, new(big.Rat).SetInt64(t.n)))
	}
	return total.FloatString(6), true
}

// Capabilities are what a model can do, from discovery or enrichment.
type Capabilities struct {
	Tools     bool `json:"tools"`
	Vision    bool `json:"vision"`
	Reasoning bool `json:"reasoning"`
}

// Model is one id a provider serves.
type Model struct {
	Ref           session.ModelRef `json:"ref"`
	DisplayName   string           `json:"display_name"`
	ContextWindow int64            `json:"context_window"` // 0 unknown
	MaxOutput     int64            `json:"max_output"`     // 0 unknown
	Pricing       Pricing          `json:"pricing"`
	Capabilities  Capabilities     `json:"capabilities"`
}
