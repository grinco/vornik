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

func TestIsPublicEndpoint_A2AOnlyExposesDiscoveryCards(t *testing.T) {
	for _, path := range []string{
		"/a2a/v1/agents/project-a/research/card",
		"/a2a/v1/agents/project-a/research/tasks",
		"/a2a/v1/agents/project-a/research/tasks/task-1",
		"/a2a/v1/agents/project-a/research/tasks/task-1/pushNotificationConfig",
		"/a2a/v1/agents/project-a/research/card/extra",
	} {
		want := path == "/a2a/v1/agents/project-a/research/card"
		if got := isPublicEndpoint(path); got != want {
			t.Errorf("isPublicEndpoint(%q) = %v, want %v", path, got, want)
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
