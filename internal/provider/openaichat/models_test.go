// SPDX-License-Identifier: AGPL-3.0-or-later

package openaichat

import (
	"context"
	"net/http"
	"testing"
)

func TestListModelsFromFixture(t *testing.T) {
	c, last := serve(t, 200, "application/json", fixture(t, "models.json"))
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if last.URL.Path != "/v1/models" || last.Method != http.MethodGet {
		t.Fatalf("request went to %s %s", last.Method, last.URL.Path)
	}
	if len(models) != 5 {
		t.Fatalf("want 5 models, got %d", len(models))
	}
	wantOrder := []string{
		"anthropic/claude-fable-5",
		"cline-pass/deepseek-v4-flash",
		"cline-pass/kimi-k3",
		"deepseek-v4-pro",
		"gpt-5.6-sol",
	}
	for i, id := range wantOrder {
		if models[i].Ref.Model != id || models[i].Ref.Provider != "aperture" {
			t.Fatalf("model %d = %+v, want %s", i, models[i].Ref, id)
		}
	}

	ds := models[1]
	if ds.DisplayName != "DeepSeek V4 Flash" {
		t.Fatalf("display name = %q", ds.DisplayName)
	}
	if ds.ContextWindow != 1000000 || ds.MaxOutput != 384000 {
		t.Fatalf("window/max = %d/%d", ds.ContextWindow, ds.MaxOutput)
	}
	if ds.Pricing.Input != "0.00000014" || ds.Pricing.Output != "0.00000028" || ds.Pricing.CacheRead != "0.00000000" || ds.Pricing.CacheWrite != "" {
		t.Fatalf("pricing = %+v", ds.Pricing)
	}
	if !ds.Capabilities.Tools || !ds.Capabilities.Reasoning || ds.Capabilities.Vision {
		t.Fatalf("capabilities = %+v", ds.Capabilities)
	}

	gpt := models[4]
	if gpt.Pricing.CacheWrite != "0.00000500" {
		t.Fatalf("gpt cache write = %q", gpt.Pricing.CacheWrite)
	}
	if gpt.Capabilities.Reasoning {
		t.Fatalf("gpt-5.6-sol should not be flagged reasoning by the pass-1 heuristic")
	}
}

func TestListModelsFallsBackToIDForDisplayName(t *testing.T) {
	c, _ := serve(t, 200, "application/json", []byte(`{"object":"list","data":[{"id":"local-model","object":"model"}]}`))
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := models[0]
	if m.DisplayName != "local-model" || m.ContextWindow != 0 || m.Pricing.Input != "" || !m.Capabilities.Tools {
		t.Fatalf("got %+v", m)
	}
}

func TestListModelsNumericPricingKeptVerbatim(t *testing.T) {
	c, _ := serve(t, 200, "application/json", []byte(`{"data":[{"id":"m","pricing":{"input":0.000001,"output":"0.000002"}}]}`))
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if models[0].Pricing.Input != "0.000001" || models[0].Pricing.Output != "0.000002" {
		t.Fatalf("pricing = %+v", models[0].Pricing)
	}
}

func TestListModelsReasoningFromSupportedParameters(t *testing.T) {
	c, _ := serve(t, 200, "application/json", []byte(`{"data":[{"id":"x","supported_parameters":["reasoning"],"architecture":{"input_modalities":["text","image"]}}]}`))
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !models[0].Capabilities.Reasoning || !models[0].Capabilities.Vision {
		t.Fatalf("capabilities = %+v", models[0].Capabilities)
	}
}

func TestListModelsErrorStatus(t *testing.T) {
	c, _ := serve(t, 503, "application/json", []byte(`{"error":"warming up"}`))
	_, err := c.ListModels(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
}

// TestUpstreamNamesWhoActuallyServesTheModel: an aperture fronts OpenRouter, ClinePass and
// OpenAI, and every id looks like the proxy's own until the metadata is read.
//
// Per-model `provider` metadata is the first word on who serves an id: one route can front
// several vendors (this endpoint's "default" fronts ClinePass, OpenAI and trig), so the
// route-wide label is only the fallback for a model that says nothing about itself. An
// older shape put the vendor in `id` with no per-model name at all; reading it as the
// answer called three deepseek ids on "default" DeepSeek while the ClinePass models beside
// them served them, and a person picking one to stay off their ClinePass quota spent it.
func TestUpstreamNamesWhoActuallyServesTheModel(t *testing.T) {
	const body = `{"data":[
	 {"id":"anthropic/claude-fable-5","display_name":"Claude Fable 5","context_window_tokens":1000000,
	  "metadata":{"provider":{"id":"openrouter","name":"OpenRouter","upstream":"openrouter"}}},
	 {"id":"cline-pass/kimi-k3","display_name":"Kimi K3","context_window_tokens":1048576,
	  "metadata":{"provider":{"id":"ClinePass","name":"","upstream":"default"}}},
	 {"id":"gpt-5.5","metadata":{"provider":{"id":"openai-sub","name":"OpenAI (Subscription)","upstream":"default"}}},
	 {"id":"qwen-27b","metadata":{"provider":{"id":"trig","name":"trig","upstream":"default"}}},
	 {"id":"spooled","metadata":{"provider":{"id":"aperture","name":"Aperture","upstream":"spool"}}},
	 {"id":"unrouted","metadata":{"provider":{"id":"Groq","name":""}}},
	 {"id":"gpt-6","owned_by":"openai"}
	]}`
	c, _ := serve(t, 200, "application/json", []byte(body))
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"anthropic/claude-fable-5": "OpenRouter",
		// No per-model name, so the per-model id: what serves it, not what made it.
		"cline-pass/kimi-k3": "ClinePass",
		// The same route, a different per-model name: the route is not one upstream.
		"gpt-5.5":  "OpenAI (Subscription)",
		"qwen-27b": "trig",
		"spooled":  "Aperture",
		// A route nothing on it names is named by what the endpoint did say about it.
		"unrouted": "Groq",
		// A plain OpenAI server says nothing, and nothing is what the model carries.
		"gpt-6": "",
	}
	for _, m := range models {
		w, ok := want[m.Ref.Model]
		if !ok {
			t.Errorf("unexpected model %q", m.Ref.Model)
			continue
		}
		if m.Upstream != w {
			t.Errorf("%s upstream = %q, want %q", m.Ref.Model, m.Upstream, w)
		}
		delete(want, m.Ref.Model)
	}
	if len(want) != 0 {
		t.Errorf("models never listed: %v", want)
	}
}
