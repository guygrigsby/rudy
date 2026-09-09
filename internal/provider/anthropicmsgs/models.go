package anthropicmsgs

import (
	"context"
	"slices"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

// ListModels reads /v1/models. The API carries no prices, so pricing stays unknown and the
// registry shows a model without a cost. Every model the Messages API serves takes tools,
// images and extended thinking, so the capabilities are not a guess.
func (c *Client) ListModels(ctx context.Context) ([]provider.Model, error) {
	sdk := c.sdk(&doer{c: c.opts.HTTP, idle: c.idle}, nil)
	pager := sdk.Models.ListAutoPaging(ctx, anthropic.ModelListParams{})
	var out []provider.Model
	for pager.Next() {
		m := pager.Current()
		name := m.DisplayName
		if name == "" {
			name = m.ID
		}
		out = append(out, provider.Model{
			Ref:           session.ModelRef{Provider: c.opts.Name, Model: m.ID},
			DisplayName:   name,
			ContextWindow: m.MaxInputTokens,
			MaxOutput:     m.MaxTokens,
			Capabilities:  provider.Capabilities{Tools: true, Vision: true, Reasoning: true},
		})
	}
	if err := pager.Err(); err != nil {
		return nil, classify(ctx, err)
	}
	slices.SortFunc(out, func(a, b provider.Model) int {
		return strings.Compare(a.Ref.Model, b.Ref.Model)
	})
	return out, nil
}
