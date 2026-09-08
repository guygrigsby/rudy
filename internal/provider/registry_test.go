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
