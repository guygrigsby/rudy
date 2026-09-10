package openaichat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/provider/httpx"
	"github.com/guygrigsby/rudy/internal/session"
)

const maxModelsBody = 32 << 20

type modelsResponse struct {
	Data []wireModel `json:"data"`
}

// wireModel is the /v1/models entry with the extension fields aperture adds:
// display_name, context_window_tokens, max_output_tokens and pricing. Plain
// OpenAI servers send only id and owned_by, which leaves the rest zero.
type wireModel struct {
	ID                  string                     `json:"id"`
	DisplayName         string                     `json:"display_name"`
	ContextWindowTokens int64                      `json:"context_window_tokens"`
	MaxOutputTokens     int64                      `json:"max_output_tokens"`
	Pricing             map[string]json.RawMessage `json:"pricing"`
	SupportedParameters []string                   `json:"supported_parameters"`
	Architecture        struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
	// Metadata is aperture's own: a proxy names the upstream that actually serves each
	// model here, which is the only thing that tells one apart from the next when every
	// id looks like the proxy's.
	Metadata struct {
		Provider struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"provider"`
	} `json:"metadata"`
}

func (c *Client) ListModels(ctx context.Context) ([]provider.Model, error) {
	hreq, err := c.newRequest(ctx, http.MethodGet, "/models", nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.opts.HTTP.Do(ctx, hreq, ulid.ULID{})
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, c.statusError(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBody))
	if err != nil {
		return nil, &provider.Error{Class: session.ErrTransport, Message: err.Error(), Attempts: httpx.AttemptsOf(resp)}
	}
	var mr modelsResponse
	if err := json.Unmarshal(body, &mr); err != nil {
		return nil, &provider.Error{Class: session.ErrProvider, Status: resp.StatusCode, Message: "malformed models list: " + err.Error(), Body: body, Attempts: httpx.AttemptsOf(resp)}
	}
	out := make([]provider.Model, 0, len(mr.Data))
	for _, m := range mr.Data {
		out = append(out, c.toModel(m))
	}
	slices.SortFunc(out, func(a, b provider.Model) int {
		return strings.Compare(a.Ref.Model, b.Ref.Model)
	})
	return out, nil
}

func (c *Client) toModel(m wireModel) provider.Model {
	name := m.DisplayName
	if name == "" {
		name = m.ID
	}
	return provider.Model{
		Ref:           session.ModelRef{Provider: c.opts.Name, Model: m.ID},
		DisplayName:   name,
		ContextWindow: m.ContextWindowTokens,
		MaxOutput:     m.MaxOutputTokens,
		Pricing: provider.Pricing{
			Input:      priceString(m.Pricing["input"]),
			Output:     priceString(m.Pricing["output"]),
			CacheRead:  priceString(m.Pricing["input_cache_read"]),
			CacheWrite: priceString(m.Pricing["input_cache_write"]),
		},
		Capabilities: provider.Capabilities{
			Tools:     true,
			Reasoning: reasoningHeuristic(m),
			Vision:    slices.Contains(m.Architecture.InputModalities, "image"),
		},
		Upstream: upstream(m),
	}
}

// upstream is who the endpoint says actually serves this model: its name when it has one,
// its id otherwise, and nothing at all when the endpoint says nothing.
func upstream(m wireModel) string {
	if n := strings.TrimSpace(m.Metadata.Provider.Name); n != "" {
		return n
	}
	return strings.TrimSpace(m.Metadata.Provider.ID)
}

// priceString keeps a price verbatim whether the provider sent it as a JSON
// string or a bare number. Never converted through float.
func priceString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// reasoningHeuristic is pass 1: /v1/models carries no capability flags on most
// providers, so the id and supported_parameters stand in. Listed as open in the
// design spec.
func reasoningHeuristic(m wireModel) bool {
	id := strings.ToLower(m.ID)
	for _, w := range []string{"reason", "deepseek", "kimi"} {
		if strings.Contains(id, w) {
			return true
		}
	}
	return slices.Contains(m.SupportedParameters, "reasoning")
}
