package api

import (
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/ratelimit"
)

// Coverage-sweep tests for trivial pure helpers + Server option
// setters in this package. Each test pins one observable
// contract; failures should describe the specific regression in
// human-readable terms.

// TestExtractCheckpointID_FindsMessagesSegment — the conversational-
// task answer route encodes the checkpoint id between `messages/`
// and `/answer`. Helper plucks it out. Pin a representative URL
// shape so a future route refactor surfaces here.
func TestExtractCheckpointID_FindsMessagesSegment(t *testing.T) {
	cases := map[string]string{
		"/api/v1/projects/p/tasks/t/messages/cp-1/answer":  "cp-1",
		"/api/v1/projects/p/tasks/t/messages/cp-abc-2/foo": "cp-abc-2",
		"/api/v1/projects/p/tasks/t/messages":              "", // no id segment after messages/
		"/api/v1/projects/p/tasks/t":                       "", // no /messages
	}
	for url, want := range cases {
		req := httptest.NewRequest("POST", url, nil)
		if got := extractCheckpointID(req); got != want {
			t.Errorf("extractCheckpointID(%q) = %q, want %q", url, got, want)
		}
	}
}

// TestIsEnvSourcedRaw_SkipsTemplateRefs — the secret-hygiene
// doctor needs to treat `${VAR}` references as non-secrets
// (their actual value is supplied by the environment at start).
// Empty strings also pass (nothing to leak). Anything else is
// candidate-secret material.
func TestIsEnvSourcedRaw_SkipsTemplateRefs(t *testing.T) {
	cases := map[string]bool{
		"":              true,
		"   ":           true,
		"${SECRET}":     true,
		"${X}":          true,
		"sk-real-token": false,
		"$SECRET":       false, // un-braced form is NOT considered safe by this check
		"prefix${X}":    false, // partial template; doctor flags
	}
	for in, want := range cases {
		if got := isEnvSourcedRaw(in); got != want {
			t.Errorf("isEnvSourcedRaw(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestLooksLikeRawSecret_RejectsPlaceholders — the doctor's
// false-positive defence: things like "CHANGE_ME" / "<your-key>"
// must NOT trip the secret-leak warning, otherwise operators
// learn to ignore the check.
func TestLooksLikeRawSecret_RejectsPlaceholders(t *testing.T) {
	for _, placeholder := range []string{
		"", "   ",
		"${ENV_VAR}",
		"CHANGE_ME",
		"changeme",
		"YOUR_KEY_HERE",
		"<your-token>",
		"<replace-me>",
		"dev-key-123",
	} {
		if looksLikeRawSecret(placeholder) {
			t.Errorf("looksLikeRawSecret(%q) flagged a placeholder", placeholder)
		}
	}
}

// TestLooksLikeRawSecret_FlagsRealKeys — the positive path. Real
// API keys and tokens (~32+ chars of mixed characters) MUST trip
// the heuristic.
func TestLooksLikeRawSecret_FlagsRealKeys(t *testing.T) {
	for _, candidate := range []string{
		"sk-1234567890abcdefghijklmnopqrstuv",
		"AKIAIOSFODNN7EXAMPLE",                     // looks like an AWS key
		"ghp_1234567890abcdefghijklmnopqrstuvwxyz", // looks like a GitHub PAT
	} {
		if !looksLikeRawSecret(candidate) {
			t.Errorf("looksLikeRawSecret(%q) missed a real-shape secret", candidate)
		}
	}
}

// TestServerOptionSetters — every option setter in api.go is a
// 2-line `s.field = v; return` shape. Driving them through
// NewServer in one pass exercises ~40 LOC at once for cheap.
// The point isn't to pin the setters' behaviour individually
// (they're trivial) but to make sure none of them panics on a
// nil server / zero-value arg, and to bump coverage.
func TestServerOptionSetters(t *testing.T) {
	// Pick options that are safe to wire with nil/zero values.
	// Each one sets a field; the server stays usable.
	opts := []ServerOption{
		WithAPIKeyRepository(nil),
		WithGistReader(nil),
		WithAPIKeyLimiter(ratelimit.NewAPIKeyLimiter()),
	}
	s := NewServer(opts...)
	if s == nil {
		t.Fatal("NewServer returned nil")
	}
}

// TestAuthConfigOptions — same shape as ServerOptions, but for
// the AuthConfigOption family. Exercises the trio of helper
// option setters added during slice 1 of the rate-limit work.
func TestAuthConfigOptions(t *testing.T) {
	c := BuildAuthConfig(nil,
		WithAPIKeyLookup(nil),
		WithAPIKeyToucher(nil),
		WithAuthAPIKeyLimiter(ratelimit.NewAPIKeyLimiter()),
	)
	if c.APIKeyLimiter == nil {
		t.Error("WithAuthAPIKeyLimiter did not wire the limiter")
	}
}

// TestIsPublicEndpoint_CoreHealthPaths — the access-log + auth
// short-circuit reads on this helper. Pin the canonical set so
// a future "skip /v1/embeddings too" doesn't accidentally also
// skip /api/v1/tasks.
func TestIsPublicEndpoint_CoreHealthPaths(t *testing.T) {
	publicPaths := []string{
		"/healthz", "/readyz", "/health/live", "/health/ready", "/metrics",
	}
	for _, p := range publicPaths {
		if !isPublicEndpoint(p) {
			t.Errorf("isPublicEndpoint(%q) = false, want true", p)
		}
	}
	// Real API paths must NOT be public.
	for _, p := range []string{"/api/v1/tasks", "/api/v1/projects", "/ui/", "/"} {
		if isPublicEndpoint(p) {
			t.Errorf("isPublicEndpoint(%q) = true, leaks an auth bypass", p)
		}
	}
}

// TestIsWebhookEndpoint_PathPrefix — webhook routes get the
// HMAC-or-key relaxation. Pin the prefix-match shape so a
// route rename doesn't silently break webhook auth.
func TestIsWebhookEndpoint_PathPrefix(t *testing.T) {
	if !isWebhookEndpoint("/api/v1/webhooks/github") {
		t.Error("known webhook path not matched")
	}
	if isWebhookEndpoint("/api/v1/tasks") {
		t.Error("non-webhook path classified as webhook")
	}
	if isWebhookEndpoint("") {
		t.Error("empty path classified as webhook")
	}
}
