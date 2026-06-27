package chat

import (
	"testing"

	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// cacheCapableModel is a Bedrock model id whose family (Anthropic Claude)
// supports CachePoint blocks, so cache-marker insertion tests exercise the
// marker logic rather than the model gate. See bedrockModelSupportsCaching.
const cacheCapableModel = "anthropic.claude-3-5-sonnet-20241022-v2:0"

func TestOpenAIMessagesToBedrockWithCache_NilStrategyNoMarker(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "system text"},
		{Role: "user", Content: "hi"},
	}
	system, _, err := openAIMessagesToBedrockWithCache(msgs, nil, cacheCapableModel)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if hasSystemCachePoint(system) {
		t.Error("nil strategy must not insert a CachePointBlock")
	}
}

func TestOpenAIMessagesToBedrockWithCache_OffStrategyNoMarker(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "system text"},
		{Role: "user", Content: "hi"},
	}
	system, _, err := openAIMessagesToBedrockWithCache(msgs, &CacheStrategy{Mode: CacheModeOff}, cacheCapableModel)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if hasSystemCachePoint(system) {
		t.Error("off strategy must not insert a CachePointBlock")
	}
}

func TestOpenAIMessagesToBedrockWithCache_AutoInsertsMarker(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "you are an assistant"},
		{Role: "user", Content: "hi"},
	}
	system, _, err := openAIMessagesToBedrockWithCache(msgs, &CacheStrategy{Mode: CacheModeAuto}, cacheCapableModel)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !hasSystemCachePoint(system) {
		t.Error("auto mode with a system message must insert a CachePointBlock")
	}
	// Marker must come AFTER the system text — otherwise the
	// cached prefix would be empty.
	if !cachePointIsLast(system) {
		t.Error("CachePointBlock must be the last entry so the entire system block is cached")
	}
}

func TestOpenAIMessagesToBedrockWithCache_PrefixRespectsFlag(t *testing.T) {
	// CachePrefix=false on system message → no marker even under prefix mode.
	msgs := []Message{
		{Role: "system", Content: "system text"},
		{Role: "user", Content: "hi"},
	}
	system, _, err := openAIMessagesToBedrockWithCache(msgs, &CacheStrategy{Mode: CacheModePrefix}, cacheCapableModel)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if hasSystemCachePoint(system) {
		t.Error("prefix mode without CachePrefix must not insert marker")
	}
	// Same input with the flag set → marker.
	msgs[0].CachePrefix = true
	system, _, err = openAIMessagesToBedrockWithCache(msgs, &CacheStrategy{Mode: CacheModePrefix}, cacheCapableModel)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !hasSystemCachePoint(system) {
		t.Error("prefix mode with CachePrefix on system message must insert marker")
	}
}

func TestOpenAIMessagesToBedrockWithCache_NoSystemNoMarker(t *testing.T) {
	// No system message → no marker even under auto.
	msgs := []Message{
		{Role: "user", Content: "hi"},
	}
	system, _, err := openAIMessagesToBedrockWithCache(msgs, &CacheStrategy{Mode: CacheModeAuto}, cacheCapableModel)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if hasSystemCachePoint(system) {
		t.Error("auto mode without a system message must not insert marker")
	}
}

// TestOpenAIMessagesToBedrockWithCache_UnsupportedModelNoMarker — regression for
// the 2026-06-13 incident: prompt_cache_mode=auto added a CachePoint to EVERY
// Bedrock-routed model, but only Claude/Nova accept them. Kimi/GLM (and the
// other marketplace OSS models routed kind:bedrock) rejected the request with
// PROVIDER_ERROR, breaking the headmatch issue-fix reviewer (exec ...e9ec, all
// 12 retries across kimi-k2.5 + glm-5). Auto + a system message + an
// unsupported model MUST NOT insert a marker.
func TestOpenAIMessagesToBedrockWithCache_UnsupportedModelNoMarker(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "you are a reviewer"},
		{Role: "user", Content: "review this"},
	}
	for _, model := range []string{"zai.glm-5", "moonshotai.kimi-k2.5", "qwen.qwen3-235b", "deepseek.r1", ""} {
		system, _, err := openAIMessagesToBedrockWithCache(msgs, &CacheStrategy{Mode: CacheModeAuto}, model)
		if err != nil {
			t.Fatalf("convert(%q): %v", model, err)
		}
		if hasSystemCachePoint(system) {
			t.Errorf("model %q does not support Bedrock cache points — auto mode must NOT insert a CachePointBlock (would PROVIDER_ERROR upstream)", model)
		}
	}
}

// TestBedrockModelSupportsCaching pins the conservative allowlist: only the
// Anthropic Claude and Amazon Nova families (incl. region-prefixed inference
// profiles) are caching-capable; everything else — especially the kind:bedrock
// OSS models — is excluded so an unrecognised id is a safe no-op, never a guess
// that re-triggers the provider error.
func TestBedrockModelSupportsCaching(t *testing.T) {
	cases := map[string]bool{
		"anthropic.claude-3-5-sonnet-20241022-v2:0":    true,
		"us.anthropic.claude-3-7-sonnet-20250219-v1:0": true,
		"eu.anthropic.claude-haiku-4-5":                true,
		"amazon.nova-pro-v1:0":                         true,
		"us.amazon.nova-lite-v1:0":                     true,
		"zai.glm-5":                                    false,
		"zai.glm-4.7-flash":                            false,
		"moonshotai.kimi-k2.5":                         false,
		"qwen.qwen3-235b":                              false,
		"deepseek.r1":                                  false,
		"nvidia.nemotron":                              false,
		"":                                             false,
	}
	for model, want := range cases {
		if got := bedrockModelSupportsCaching(model); got != want {
			t.Errorf("bedrockModelSupportsCaching(%q) = %v, want %v", model, got, want)
		}
	}
}

// hasSystemCachePoint scans for the cache-marker variant of the
// system content block.
func hasSystemCachePoint(blocks []bedrocktypes.SystemContentBlock) bool {
	for _, b := range blocks {
		if _, ok := b.(*bedrocktypes.SystemContentBlockMemberCachePoint); ok {
			return true
		}
	}
	return false
}

// cachePointIsLast reports whether the cache marker is the final
// entry in the system block array. Required for "cache the whole
// system block" semantics — anything after the marker is excluded.
func cachePointIsLast(blocks []bedrocktypes.SystemContentBlock) bool {
	if len(blocks) == 0 {
		return false
	}
	_, ok := blocks[len(blocks)-1].(*bedrocktypes.SystemContentBlockMemberCachePoint)
	return ok
}
