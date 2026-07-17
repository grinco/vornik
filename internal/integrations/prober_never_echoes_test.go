package integrations

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// These success-path guards pin the design's "Summary and Detail are secret-free"
// invariant (review-20260716-b1ab): the probers set Detail only on failure
// paths today, so the invariant was untested on success. Each test makes the
// provider's SUCCESS response body echo the credential; the prober must still
// build its result from provider identity and never surface the secret. A future
// change that starts populating Summary/Detail from the response body fails here.

func assertNoSecretEcho(t *testing.T, res ProbeResult, secret string) {
	t.Helper()
	if !res.OK {
		t.Fatalf("expected a successful probe, got %+v", res)
	}
	if strings.Contains(res.Summary, secret) {
		t.Errorf("Summary echoed the secret: %q", res.Summary)
	}
	if strings.Contains(res.Detail, secret) {
		t.Errorf("Detail echoed the secret: %q", res.Detail)
	}
	for _, f := range res.Failures {
		if strings.Contains(f.Reason, secret) {
			t.Errorf("Failure.Reason echoed the secret: %q", f.Reason)
		}
	}
}

func TestTelegramProber_SuccessNeverEchoesSecret(t *testing.T) {
	const secret = "SUPERSECRETBOTTOKEN0123456789"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"username":"my_bot","token_echo":"` + secret + `"}}`))
	}))
	defer srv.Close()
	withTelegramTestBaseURL(t, srv.URL)
	res := newTelegramProber(srv.Client(), 2*time.Second).Probe(context.Background(),
		CandidateConfig{Kind: "telegram", Values: map[string]string{"bot_token": secret}})
	assertNoSecretEcho(t, res, secret)
}

func TestSlackProber_SuccessNeverEchoesSecret(t *testing.T) {
	const secret = "xoxb-SUPERSECRET-0123456789ABCDEF"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"team":"Acme","user":"botuser","token_echo":"` + secret + `"}`))
	}))
	defer srv.Close()
	withSlackTestAuthTestURL(t, srv.URL)
	res := newSlackProber(srv.Client(), 2*time.Second).Probe(context.Background(),
		CandidateConfig{Kind: "slack", Values: map[string]string{"bot_token_env": secret}})
	assertNoSecretEcho(t, res, secret)
}

func TestGitHubAppProber_SuccessNeverEchoesSecret(t *testing.T) {
	// The installation-token endpoint's SUCCESS body carries the minted token —
	// the prober must not surface it (it builds Summary from the expiry, not the
	// token).
	const minted = "ghs_SUPERSECRETMINTEDTOKEN0123456789"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"` + minted + `","expires_at":"2099-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()
	res := newGitHubAppProber(srv.Client(), 2*time.Second).Probe(context.Background(),
		CandidateConfig{Kind: "github_app", Values: map[string]string{
			"app_id":           "123",
			"installation_id":  "456",
			"private_key_path": testPrivateKeyPEM(t),
			"api_base_url":     srv.URL,
		}})
	assertNoSecretEcho(t, res, minted)
}
