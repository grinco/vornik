package api

import (
	"context"
	"strings"
	"testing"
)

func TestIsPlaceholderKey(t *testing.T) {
	for _, p := range []string{"changeme", "CHANGEME", "your-api-key", "<your-key>", "replace_me", "REPLACE-WITH-KEY", "todo", ""} {
		if !isPlaceholderKey(p) {
			t.Errorf("%q should be a placeholder", p)
		}
	}
	// Real key with a placeholder substring must NOT be flagged (whole-value match).
	for _, k := range []string{"sk-prod-your-server-key-abc123", "sk-vornik-4521ab7127de.z_iZZ"} {
		if isPlaceholderKey(k) {
			t.Errorf("%q is a real key, not a placeholder", k)
		}
	}
}

func TestCheckAgentLLMAPIKey(t *testing.T) {
	base := func() *DoctorHandlers {
		return &DoctorHandlers{chatEndpoint: "https://api.z.ai/v1", chatAPIKey: "sk-real-1234567890abcdef"}
	}
	// Empty key + real upstream -> ERROR.
	h := base()
	h.chatAPIKey = ""
	if got := h.checkAgentLLMAPIKey(context.Background()); got.Status != "ERROR" {
		t.Fatalf("empty upstream key -> ERROR, got %q", got.Status)
	}
	// Empty key + loopback endpoint (may need no key) -> OK.
	h = base()
	h.chatAPIKey = ""
	h.chatEndpoint = "http://127.0.0.1:8080/v1"
	if got := h.checkAgentLLMAPIKey(context.Background()); got.Status != "OK" {
		t.Fatalf("empty key on loopback -> OK, got %q", got.Status)
	}
	// Placeholder -> ERROR.
	h = base()
	h.chatAPIKey = "changeme"
	if got := h.checkAgentLLMAPIKey(context.Background()); got.Status != "ERROR" {
		t.Fatalf("placeholder -> ERROR, got %q", got.Status)
	}
	// Present + probe 401 -> ERROR.
	h = base()
	h.probeFunc = func(context.Context, string, string) (int, error) { return 401, nil }
	if got := h.checkAgentLLMAPIKey(context.Background()); got.Status != "ERROR" {
		t.Fatalf("probe 401 -> ERROR, got %q", got.Status)
	}
	// Present + probe 200 -> OK.
	h = base()
	h.probeFunc = func(context.Context, string, string) (int, error) { return 200, nil }
	if got := h.checkAgentLLMAPIKey(context.Background()); got.Status != "OK" {
		t.Fatalf("probe 200 -> OK, got %q", got.Status)
	}
}

// TestCheckAgentLLMAPIKey_NativeAnthropicSkipsLiveProbe is a regression test
// for M-1 (final-review fix wave): liveProbe sends Authorization: Bearer to
// <endpoint>/models, which is correct for OpenAI-compatible upstreams
// (z.ai, ollama, openrouter) but WRONG for native Anthropic
// (api.anthropic.com uses x-api-key + /v1/models) — that mismatch would 401
// a perfectly valid key and produce a false ERROR on the shipped default. A
// native-Anthropic chatEndpoint must skip the live probe entirely (no
// probeFunc injected here — if the real liveProbe ran, it would attempt a
// network call in this test and either hang/fail or return a non-OK
// status; asserting OK with the skip-note is proof it never ran).
func TestCheckAgentLLMAPIKey_NativeAnthropicSkipsLiveProbe(t *testing.T) {
	h := &DoctorHandlers{chatEndpoint: "https://api.anthropic.com", chatAPIKey: "sk-ant-real1234567890"}
	got := h.checkAgentLLMAPIKey(context.Background())
	if got.Status != "OK" {
		t.Fatalf("native Anthropic endpoint -> OK (probe skipped), got %q (message: %q)", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "skipped live probe") {
		t.Fatalf("expected message to note the skipped live probe, got %q", got.Message)
	}
}

// TestCheckAgentLLMAPIKey_RouterProviderIsNotAFault pins the fix for a false
// positive found on the dev box the day this check shipped: a router-provider
// deployment has NO top-level chat.endpoint and NO top-level chat.api_key
// (credentials live per sub-provider — bedrock uses AWS SigV4, vertex uses
// GCP ADC, neither has an api_key), yet the check hard-ERRORed because
// url.Parse("") yields an empty host that fell through endpointNeedsKey's
// "needs a key" default.
func TestCheckAgentLLMAPIKey_RouterProviderIsNotAFault(t *testing.T) {
	h := &DoctorHandlers{chatProvider: "router", chatAPIKey: "", chatEndpoint: ""}
	got := h.checkAgentLLMAPIKey(context.Background())
	if got.Status != "OK" {
		t.Fatalf("router provider with delegated credentials must not fault: %+v", got)
	}
}

// TestEndpointNeedsKey_EmptyEndpoint is the narrower root-cause assertion:
// there is no upstream to authenticate against, so no key can be required.
func TestEndpointNeedsKey_EmptyEndpoint(t *testing.T) {
	for _, ep := range []string{"", "   "} {
		if endpointNeedsKey(ep) {
			t.Errorf("endpointNeedsKey(%q) = true, want false (no upstream to authenticate to)", ep)
		}
	}
	// Negative control: a real remote endpoint still requires a key.
	if !endpointNeedsKey("https://api.openai.com/v1") {
		t.Error("a real remote endpoint must still require a key")
	}
	// Loopback still exempt.
	if endpointNeedsKey("http://127.0.0.1:11434/v1") {
		t.Error("loopback endpoint must remain exempt")
	}
}

// TestCheckAgentLLMAPIKey_DirectProviderStillFaults is the negative control for
// the router short-circuit: a NON-router provider with an empty key against a
// real upstream is still the fresh-install "Invalid API key" failure the check
// exists to catch.
func TestCheckAgentLLMAPIKey_DirectProviderStillFaults(t *testing.T) {
	h := &DoctorHandlers{chatProvider: "openai", chatAPIKey: "", chatEndpoint: "https://api.openai.com/v1"}
	got := h.checkAgentLLMAPIKey(context.Background())
	if got.Status != "ERROR" {
		t.Fatalf("empty key against a real upstream must still ERROR: %+v", got)
	}
}
