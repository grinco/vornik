package api

// Regression tests for hasWebhookSignatureForPath (2026-05-29
// audit-agent finding). Pre-fix the auth middleware accepted any
// of the 3 HMAC signature headers for any webhook path —
// X-Slack-Signature against /api/v1/webhooks/{project}/{src}
// passed the auth gate, then the handler rejected with a
// signature-specific 401 that an attacker could distinguish from
// a 404 on a missing project, enumerating valid project/source
// pairs without an API key. The fix scopes the accepted header
// to the path's actual verifier.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// reqWithHeader builds a minimal request for the header-matching
// table-driven test below. Only the path + header pair matter for
// this contract; method + body are irrelevant.
func reqWithHeader(path, header, value string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, nil)
	if header != "" {
		r.Header.Set(header, value)
	}
	return r
}

func TestHasWebhookSignatureForPath(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		header string
		value  string
		want   bool
	}{
		// Slack endpoint: only X-Slack-Signature counts.
		{"slack_accepts_slack_sig", "/api/v1/slack/webhook", "X-Slack-Signature", "v0=abc", true},
		{"slack_rejects_vornik_sig", "/api/v1/slack/webhook", "X-Vornik-Signature", "abc", false},
		{"slack_rejects_github_sig", "/api/v1/slack/webhook", "X-Hub-Signature-256", "sha256=abc", false},
		{"slack_rejects_empty", "/api/v1/slack/webhook", "", "", false},

		// GitHub endpoint: only X-Hub-Signature-256 counts.
		{"github_accepts_github_sig", "/api/v1/github-app/webhook", "X-Hub-Signature-256", "sha256=abc", true},
		{"github_rejects_vornik_sig", "/api/v1/github-app/webhook", "X-Vornik-Signature", "abc", false},
		{"github_rejects_slack_sig", "/api/v1/github-app/webhook", "X-Slack-Signature", "v0=abc", false},

		// Generic ingest path: X-Vornik-Signature + X-Hub-Signature-256.
		// X-Slack-Signature MUST NOT bypass (the agent's reported gap).
		{"generic_accepts_vornik_sig", "/api/v1/webhooks/proj/src", "X-Vornik-Signature", "abc", true},
		{"generic_accepts_github_sig", "/api/v1/webhooks/proj/src", "X-Hub-Signature-256", "sha256=abc", true},
		{"generic_rejects_slack_sig", "/api/v1/webhooks/proj/src", "X-Slack-Signature", "v0=abc", false},

		// Whitespace-only header value MUST NOT count.
		{"slack_rejects_whitespace_value", "/api/v1/slack/webhook", "X-Slack-Signature", "   ", false},

		// Non-webhook path always returns false.
		{"unknown_path_rejects_anything", "/api/v1/healthz", "X-Slack-Signature", "v0=abc", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hasWebhookSignatureForPath(tc.path, reqWithHeader(tc.path, tc.header, tc.value))
			if got != tc.want {
				t.Errorf("hasWebhookSignatureForPath(%q, %s=%q) = %v, want %v",
					tc.path, tc.header, tc.value, got, tc.want)
			}
		})
	}
}
