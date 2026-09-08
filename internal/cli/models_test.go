package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/guygrigsby/rudy/internal/provider"
	"github.com/guygrigsby/rudy/internal/session"
)

func TestPer1M(t *testing.T) {
	cases := map[string]string{
		"0.00000014": "$0.14",
		"0.000001":   "$1.00",
		"0.00003":    "$30.00",
		"":           "-",
		"nonsense":   "-",
	}
	for in, want := range cases {
		if got := per1M(in); got != want {
			t.Errorf("per1M(%q) = %q want %q", in, got, want)
		}
	}
}

func TestRenderModels(t *testing.T) {
	models := []provider.Model{
		{Ref: session.ModelRef{Provider: "aperture", Model: "cline-pass/kimi-k3"}, DisplayName: "Kimi K3", ContextWindow: 262144, Pricing: provider.Pricing{Input: "0.0000006", Output: "0.0000025"}},
		{Ref: session.ModelRef{Provider: "mlx", Model: "qwen3-coder"}},
	}
	var out bytes.Buffer
	if err := renderModels(&out, models); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want header plus two rows, got %q", out.String())
	}
	if !strings.HasPrefix(lines[0], "PROVIDER") {
		t.Fatalf("header %q", lines[0])
	}
	if !strings.Contains(lines[1], "cline-pass/kimi-k3") || !strings.Contains(lines[1], "$0.60") || !strings.Contains(lines[1], "$2.50") || !strings.Contains(lines[1], "262144") {
		t.Fatalf("row %q", lines[1])
	}
	if !strings.Contains(lines[2], "qwen3-coder") || strings.Count(lines[2], "-") < 3 {
		t.Fatalf("row with unknowns %q", lines[2])
	}
}

func TestModelsCommand(t *testing.T) {
	fp := &fakeProvider{}
	root := newRoot("test", testBuilder(t, fp))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"models"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "fake") || !strings.Contains(out.String(), "Fake M") {
		t.Fatalf("output %q", out.String())
	}
	out.Reset()
	root.SetArgs([]string{"models", "--json"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute --json: %v", err)
	}
	var models []provider.Model
	if err := json.Unmarshal(out.Bytes(), &models); err != nil || len(models) != 1 {
		t.Fatalf("json output %q err %v", out.String(), err)
	}
}
