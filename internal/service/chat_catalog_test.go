package service

import (
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/pricing"
)

func TestIsAnthropicModelID(t *testing.T) {
	cases := map[string]bool{
		"claude-opus-4-7":   true,
		"claude-sonnet-4-6": true,
		"claude-haiku-3":    true,
		// Bedrock-routed Claude IDs use the "anthropic." prefix and
		// belong to the Bedrock catalog — must NOT match here.
		"anthropic.claude-opus-4-1": false,
		"gpt-5.4-mini":              false,
		"google/gemini-2.5-pro":     false,
		"qwen.qwen3-32b-v1:0":       false,
		"":                          false,
	}
	for in, want := range cases {
		if got := isAnthropicModelID(in); got != want {
			t.Errorf("isAnthropicModelID(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestIsOpenAIModelID(t *testing.T) {
	cases := map[string]bool{
		"gpt-5":                 true,
		"gpt-5.4-mini":          true,
		"gpt-5-codex":           true,
		"o3-deep-research":      true,
		"o4-mini-deep-research": true,
		// Two-digit reasoning families would still match — guard the
		// shape's intent (digits then a dash).
		"o12-future-model": true,

		// Single-letter "o" without trailing digit-dash → not OpenAI.
		"ollama-something": false,
		"o-deep":           false,
		// Bedrock-routed openai.* IDs use a dot, not a dash.
		"openai.gpt-oss-120b-1:0": false,
		"claude-opus-4-7":         false,
		"google/gemini-2.5-pro":   false,
		"":                        false,
	}
	for in, want := range cases {
		if got := isOpenAIModelID(in); got != want {
			t.Errorf("isOpenAIModelID(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestIsVertexModelID(t *testing.T) {
	cases := map[string]bool{
		"google/gemini-2.5-pro":           true,
		"google/gemini-3.1-flash-preview": true,
		"google/gemma-4-26b-a4b-it-maas":  true,

		"gemini-2.5-pro":      false, // bare ID — Vertex rejects this
		"claude-opus-4-7":     false,
		"qwen.qwen3-32b-v1:0": false,
		"":                    false,

		// Regression: OpenRouter ":free" models carry the "google/"
		// publisher prefix but route to openrouter via the suffix
		// route, so the vertex catalog must NOT claim them (else they
		// show mis-attributed under vertex in `vornikctl models`).
		"google/gemma-4-26b-a4b-it:free": false,
		"google/gemma-4-31b-it:free":     false,
	}
	for in, want := range cases {
		if got := isVertexModelID(in); got != want {
			t.Errorf("isVertexModelID(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestIsBedrockModelID(t *testing.T) {
	cases := map[string]bool{
		"anthropic.claude-opus-4-1":     true,
		"openai.gpt-oss-120b-1:0":       true,
		"qwen.qwen3-coder-30b-a3b-v1:0": true,
		"minimax.minimax-m2":            true,
		"deepseek.v3.2":                 true,

		// Native vendor IDs are owned by their dedicated providers.
		"claude-opus-4-7":       false,
		"gpt-5.4-mini":          false,
		"o3-deep-research":      false,
		"google/gemini-2.5-pro": false,
		"":                      false,

		// Regression: ":free" models belong to openrouter. Now that
		// isVertexModelID excludes them, they must not fall through to
		// the bedrock fallback catalog either.
		"google/gemma-4-26b-a4b-it:free": false,
		"qwen/qwen3-coder:free":          false,
	}
	for in, want := range cases {
		if got := isBedrockModelID(in); got != want {
			t.Errorf("isBedrockModelID(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestChatModelCatalogFromPricing(t *testing.T) {
	// Materialise a tiny pricing.yaml so the helper exercises real
	// Table.IDs() behaviour rather than a hand-rolled stub.
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	body := []byte(`models:
  claude-opus-4-7: { input: 5.00, output: 25.00 }
  claude-haiku-3:  { input: 0.25, output: 1.25 }
  gpt-5.4-mini:    { input: 0.75, output: 4.50 }
  google/gemini-2.5-pro: { input: 1.25, output: 10.00 }
  qwen.qwen3-32b-v1:0:   { input: 0.15, output: 0.62 }
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write pricing.yaml: %v", err)
	}
	table, err := pricing.Load(path)
	if err != nil {
		t.Fatalf("load pricing: %v", err)
	}

	anthropic := chatModelCatalogFromPricing(table, isAnthropicModelID, "anthropic")
	openai := chatModelCatalogFromPricing(table, isOpenAIModelID, "openai")
	vertex := chatModelCatalogFromPricing(table, isVertexModelID, "google")
	bedrock := chatModelCatalogFromPricing(table, isBedrockModelID, "")

	if len(anthropic) != 2 {
		t.Errorf("anthropic catalog: got %d models, want 2 (%+v)", len(anthropic), anthropic)
	}
	if len(openai) != 1 {
		t.Errorf("openai catalog: got %d, want 1 (%+v)", len(openai), openai)
	}
	if len(vertex) != 1 {
		t.Errorf("vertex catalog: got %d, want 1 (%+v)", len(vertex), vertex)
	}
	if len(bedrock) != 1 {
		t.Errorf("bedrock catalog: got %d, want 1 (%+v)", len(bedrock), bedrock)
	}
	// OwnedBy stamped.
	for _, m := range anthropic {
		if m.OwnedBy != "anthropic" {
			t.Errorf("anthropic OwnedBy: got %q, want %q", m.OwnedBy, "anthropic")
		}
		if m.Source != "pricing" {
			t.Errorf("anthropic Source: got %q, want %q", m.Source, "pricing")
		}
	}

	// Nil receiver → nil result, no panic.
	if got := chatModelCatalogFromPricing(nil, isAnthropicModelID, "anthropic"); got != nil {
		t.Errorf("nil table: got %v, want nil", got)
	}

	// Empty match → nil (so callers know not to pass an empty catalog
	// through WithXModelCatalog, which would shadow any later fallback).
	if got := chatModelCatalogFromPricing(table, func(string) bool { return false }, "x"); got != nil {
		t.Errorf("empty match: got %v, want nil", got)
	}
}
