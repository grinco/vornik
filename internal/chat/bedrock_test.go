package chat

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// TestNewBedrockProvider_RequiresRegion — defensive: an empty region
// is a misconfigured deployment, fail-fast at construction time
// rather than blow up on the first Converse call (which produces a
// confusing AWS-side error about endpoint resolution).
func TestNewBedrockProvider_RequiresRegion(t *testing.T) {
	_, err := NewBedrockProvider(context.Background(), "", "anthropic.claude-3-haiku-20240307-v1:0")
	if err == nil {
		t.Fatal("expected error for empty region")
	}
}

// TestBedrockProvider_ModelAccessor — Model() returns whatever was
// passed at construction; remains stable across SetMetrics calls.
func TestBedrockProvider_ModelAccessor(t *testing.T) {
	p, err := NewBedrockProvider(context.Background(), "us-east-1", "anthropic.claude-3-haiku-20240307-v1:0")
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if got := p.Model(); got != "anthropic.claude-3-haiku-20240307-v1:0" {
		t.Errorf("Model(): got %q", got)
	}
	p.SetMetrics(nil)
	if got := p.Model(); got != "anthropic.claude-3-haiku-20240307-v1:0" {
		t.Errorf("Model() after SetMetrics: got %q", got)
	}
}

// TestBedrockProvider_WithModel — per-request model override returns
// a new provider pinned to the new model, leaving the original
// untouched. The router uses this hot-path pattern; if WithModel
// mutated the original, two concurrent requests would race.
func TestBedrockProvider_WithModel(t *testing.T) {
	p, err := NewBedrockProvider(context.Background(), "us-east-1", "anthropic.claude-3-haiku-20240307-v1:0")
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	override := p.WithModel("zai.glm-5")
	if override.Model() != "zai.glm-5" {
		t.Errorf("override Model(): got %q", override.Model())
	}
	if p.Model() != "anthropic.claude-3-haiku-20240307-v1:0" {
		t.Errorf("original mutated: got %q", p.Model())
	}
}

// TestBedrockProvider_Complete_RejectsEmptyModel — the WithModel
// override is supposed to be the only path on a default-empty
// provider. Without one, Complete must fail with a recognisable
// sentinel rather than send a malformed request.
func TestBedrockProvider_Complete_RejectsEmptyModel(t *testing.T) {
	p, err := NewBedrockProvider(context.Background(), "us-east-1", "")
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = p.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != ErrEmptyModel {
		t.Errorf("expected ErrEmptyModel, got %v", err)
	}
}

// TestBedrockProvider_Complete_RejectsEmptyMessages — caller bug
// guard so the SDK's 400 doesn't bubble up as an opaque AWS error.
func TestBedrockProvider_Complete_RejectsEmptyMessages(t *testing.T) {
	p, err := NewBedrockProvider(context.Background(), "us-east-1", "anthropic.claude-3-haiku-20240307-v1:0")
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = p.Complete(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil messages")
	}
}

// TestBedrockProvider_OptionsApplied — smoke test the option
// closures actually mutate the constructed provider. Catches a
// future refactor that drops a field.
func TestBedrockProvider_OptionsApplied(t *testing.T) {
	models := []ModelInfo{{ID: "test-model"}}
	p, err := NewBedrockProvider(context.Background(), "us-east-1", "default-model",
		WithBedrockMaxTokens(2048),
		WithBedrockTimeout(45*time.Second),
		WithBedrockLogger(zerolog.Nop()),
		WithBedrockStaticModelList(models),
	)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if p.maxTokens != 2048 {
		t.Errorf("maxTokens: got %d", p.maxTokens)
	}
	if p.timeout != 45*time.Second {
		t.Errorf("timeout: got %v", p.timeout)
	}
	if len(p.staticModelList) != 1 {
		t.Errorf("staticModelList: got %d entries", len(p.staticModelList))
	}
}

// TestBedrockProvider_ListModels_StaticPath — when a static list is
// configured, ListModels stamps Provider="bedrock" + a default
// Source so the router's aggregation finds it consistent with other
// sub-providers. Without the stamping the model picker shows
// "Provider: " (blank) for bedrock entries.
func TestBedrockProvider_ListModels_StaticPath(t *testing.T) {
	models := []ModelInfo{
		{ID: "anthropic.claude-3-haiku-20240307-v1:0", OwnedBy: "anthropic"},
		{ID: "zai.glm-5", OwnedBy: "z.ai"},
	}
	p, err := NewBedrockProvider(context.Background(), "us-east-1", "",
		WithBedrockStaticModelList(models),
	)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	got, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 models, got %d", len(got))
	}
	for _, m := range got {
		if m.Provider != "bedrock" {
			t.Errorf("%s: provider not stamped: %q", m.ID, m.Provider)
		}
		if m.Source == "" {
			t.Errorf("%s: source not stamped", m.ID)
		}
	}
}

// TestSortInt32 — defensive test for the inline insertion sort used
// by the streaming reader to order tool calls by ContentBlockIndex.
// Catches a future refactor that would, e.g., assume go's "sort"
// package is already imported.
func TestSortInt32(t *testing.T) {
	cases := []struct {
		in   []int32
		want []int32
	}{
		{[]int32{}, []int32{}},
		{[]int32{1}, []int32{1}},
		{[]int32{2, 1}, []int32{1, 2}},
		{[]int32{3, 1, 2}, []int32{1, 2, 3}},
		{[]int32{5, 5, 5}, []int32{5, 5, 5}},
		{[]int32{0, -1, 7, 3}, []int32{-1, 0, 3, 7}},
	}
	for _, tc := range cases {
		a := append([]int32(nil), tc.in...)
		sortInt32(a)
		if len(a) != len(tc.want) {
			t.Errorf("len: in %v got %v want %v", tc.in, a, tc.want)
			continue
		}
		for i := range a {
			if a[i] != tc.want[i] {
				t.Errorf("in %v got %v want %v", tc.in, a, tc.want)
				break
			}
		}
	}
}

// TestBedrockProvider_ListModels_EmptyWithoutStatic — the design
// choice: without a static list, ListModels is a no-op rather than
// hitting Bedrock's heavy ListFoundationModels API. Documents the
// expectation so a future operator who wires static expects the
// nil-empty contrast.
func TestBedrockProvider_ListModels_EmptyWithoutStatic(t *testing.T) {
	p, err := NewBedrockProvider(context.Background(), "us-east-1", "default-model")
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	got, err := p.ListModels(context.Background())
	if err != nil {
		t.Errorf("ListModels: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected nil/empty list, got %d entries", len(got))
	}
}
