package chat

import "testing"

// The OpenAI-compat path parsed only `cache_read_tokens` — the Bedrock/Anthropic
// spelling. OpenAI and vLLM report `prompt_tokens_details.cached_tokens`, so a
// self-hosted server's cache reporting was dropped and the field sat at 0
// forever, making "no caching" indistinguishable from "not measured".
//
// This matters because the 2026-08 arm measured 71.5:1 prompt-to-completion —
// prompt is 98.6% of everything processed, so whether any of it came from cache
// is the single most consequential unobserved quantity in the deployment.
func TestNormalizeCachedTokens(t *testing.T) {
	t.Run("openai-compatible spelling is adopted", func(t *testing.T) {
		var r ChatResponse
		r.Usage.PromptTokens = 3613
		r.Usage.PromptTokensDetails.CachedTokens = 3584
		normalizeCachedTokens(&r)
		if r.Usage.CacheReadTokens != 3584 {
			t.Errorf("CacheReadTokens = %d, want 3584", r.Usage.CacheReadTokens)
		}
	})

	t.Run("a provider-set value is never overwritten", func(t *testing.T) {
		// Bedrock and Anthropic populate CacheReadTokens directly and must win;
		// clobbering them with an absent OpenAI field would zero real data.
		var r ChatResponse
		r.Usage.CacheReadTokens = 900
		r.Usage.PromptTokensDetails.CachedTokens = 0
		normalizeCachedTokens(&r)
		if r.Usage.CacheReadTokens != 900 {
			t.Errorf("CacheReadTokens = %d, want the provider's 900", r.Usage.CacheReadTokens)
		}
	})

	t.Run("absent everywhere stays zero", func(t *testing.T) {
		// The self-hosted endpoint measured on 2026-08-16 does not populate
		// prompt_tokens_details at all, even though prefix caching is active
		// (~0.09s repeat vs ~0.26s unique). Zero here means "not reported",
		// which is exactly what it should mean.
		var r ChatResponse
		normalizeCachedTokens(&r)
		if r.Usage.CacheReadTokens != 0 {
			t.Errorf("CacheReadTokens = %d, want 0", r.Usage.CacheReadTokens)
		}
	})

	t.Run("nil is safe", func(*testing.T) { normalizeCachedTokens(nil) })
}
