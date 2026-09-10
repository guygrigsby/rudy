package provider_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

type fakeProvider struct {
	name   string
	models []provider.Model
	err    error
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Complete(context.Context, provider.Request, func(provider.Part) error) error {
	return errors.New("not implemented")
}
func (f *fakeProvider) ListModels(context.Context) ([]provider.Model, error) {
	return f.models, f.err
}

func model(prov, id string) provider.Model {
	return provider.Model{Ref: session.ModelRef{Provider: prov, Model: id}, ContextWindow: 128000}
}

func TestRegistryRefreshKeepsOldModelsWhenAProviderFails(t *testing.T) {
	a := &fakeProvider{name: "aperture", models: []provider.Model{model("aperture", "cline-pass/kimi-k3"), model("aperture", "gpt-5.6-sol")}}
	b := &fakeProvider{name: "mlx", models: []provider.Model{model("mlx", "qwen3-coder")}}
	snap := filepath.Join(t.TempDir(), "registry.json")
	r := provider.NewRegistry(snap, a, b)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(r.Models()); got != 3 {
		t.Fatalf("want 3 models, got %d", got)
	}
	b.err = errors.New("connection refused")
	b.models = nil
	err := r.Refresh(context.Background())
	if err == nil || !errors.Is(err, b.err) {
		t.Fatalf("want joined error containing the provider failure, got %v", err)
	}
	if got := len(r.Models()); got != 3 {
		t.Fatalf("failing provider must keep its previous models, got %d", got)
	}
}

func TestRegistrySnapshotRoundTrip(t *testing.T) {
	a := &fakeProvider{name: "aperture", models: []provider.Model{{
		Ref:           session.ModelRef{Provider: "aperture", Model: "cline-pass/deepseek-v4-flash"},
		DisplayName:   "DeepSeek V4 Flash",
		ContextWindow: 1000000,
		MaxOutput:     384000,
		Pricing:       provider.Pricing{Input: "0.00000014", Output: "0.00000028", CacheRead: "0.00000000"},
		Capabilities:  provider.Capabilities{Tools: true, Reasoning: true},
	}}}
	snap := filepath.Join(t.TempDir(), "registry.json")
	r := provider.NewRegistry(snap, a)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(snap); err != nil {
		t.Fatalf("snapshot not written: %v", err)
	}
	r2 := provider.NewRegistry(snap, a)
	if err := r2.LoadSnapshot(); err != nil {
		t.Fatal(err)
	}
	got := r2.Models()
	if len(got) != 1 || got[0] != a.models[0] {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	r3 := provider.NewRegistry(filepath.Join(t.TempDir(), "missing.json"), a)
	if err := r3.LoadSnapshot(); err != nil {
		t.Fatalf("missing snapshot must not be an error: %v", err)
	}
}

func TestRegistryResolve(t *testing.T) {
	a := &fakeProvider{name: "aperture", models: []provider.Model{model("aperture", "deepseek-v4-flash"), model("aperture", "gpt-5.6-sol")}}
	b := &fakeProvider{name: "direct", models: []provider.Model{model("direct", "deepseek-v4-flash")}}
	r := provider.NewRegistry(filepath.Join(t.TempDir(), "r.json"), a, b)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m, err := r.Resolve("aperture:deepseek-v4-flash"); err != nil || m.Ref.Provider != "aperture" {
		t.Errorf("qualified: %+v %v", m, err)
	}
	if m, err := r.Resolve("gpt-5.6-sol"); err != nil || m.Ref.Provider != "aperture" {
		t.Errorf("unique bare id: %+v %v", m, err)
	}
	if _, err := r.Resolve("deepseek-v4-flash"); !errors.Is(err, provider.ErrAmbiguous) {
		t.Errorf("ambiguous bare id: %v", err)
	}
	if _, err := r.Resolve("nope"); !errors.Is(err, provider.ErrUnknownModel) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := r.Resolve("aperture:nope"); !errors.Is(err, provider.ErrUnknownModel) {
		t.Errorf("unknown qualified: %v", err)
	}
	if p, ok := r.Provider("direct"); !ok || p.Name() != "direct" {
		t.Errorf("provider lookup")
	}
}

// TestOneRefIsOneModel is the snapshot invariant (rudy-aol): an endpoint that fronts
// several serves some ids from more than one of them and lists each pair, but the pair
// (provider, id) is what everything downstream addresses a model by. Two entries the ref
// cannot tell apart are one model, and the surviving one names every upstream it came
// from, so filtering by either finds it.
//
// The stuck cycle is what this was: sorting puts the two side by side, so ctrl+p stepped
// from the first to the second, set the model it was already on, and never moved.
func TestOneRefIsOneModel(t *testing.T) {
	dup := func(id, up string) provider.Model {
		m := model("aperture", id)
		m.Upstream = up
		return m
	}
	a := &fakeProvider{name: "aperture", models: []provider.Model{
		dup("moonshotai/kimi-k3", "OpenRouter"),
		dup("anthropic/claude-opus-5", "OpenRouter"),
		dup("moonshotai/kimi-k3", "Aperture"),
		dup("moonshotai/kimi-k3", "OpenRouter"),
	}}
	r := provider.NewRegistry(filepath.Join(t.TempDir(), "registry.json"), a)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	models := r.Models()
	if len(models) != 2 {
		t.Fatalf("one entry per ref, got %d: %+v", len(models), models)
	}
	seen := map[session.ModelRef]bool{}
	for _, m := range models {
		if seen[m.Ref] {
			t.Errorf("%s listed twice", m.Ref)
		}
		seen[m.Ref] = true
	}
	kimi := models[1]
	if kimi.Ref.Model != "moonshotai/kimi-k3" {
		t.Fatalf("sorted by id: %+v", models)
	}
	if kimi.Upstream != "OpenRouter, Aperture" {
		t.Errorf("the survivor names both upstreams once each, got %q", kimi.Upstream)
	}
}
