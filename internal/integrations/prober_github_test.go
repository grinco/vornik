package integrations

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testPrivateKeyPEM generates a fresh RSA key and PEM-encodes it (PKCS#1,
// matching what a downloaded GitHub App private key looks like).
func testPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	return string(pem.EncodeToMemory(block))
}

func TestGitHubAppProber_Kind(t *testing.T) {
	p := newGitHubAppProber(http.DefaultClient, 0)
	if p.Kind() != "github_app" {
		t.Errorf("Kind() = %q, want github_app", p.Kind())
	}
}

func TestGitHubAppProber_Probe_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_faketoken","expires_at":"2099-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	p := newGitHubAppProber(srv.Client(), 2*time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Kind: "github_app", Values: map[string]string{
		"app_id":           "123",
		"installation_id":  "456",
		"private_key_path": testPrivateKeyPEM(t),
		"api_base_url":     srv.URL,
	}})

	if !res.OK || res.Outcome != OutcomeOK {
		t.Fatalf("res = %+v, want OK/OutcomeOK", res)
	}
}

func TestGitHubAppProber_Probe_InvalidCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	p := newGitHubAppProber(srv.Client(), 2*time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{
		"app_id":           "123",
		"installation_id":  "456",
		"private_key_path": testPrivateKeyPEM(t),
		"api_base_url":     srv.URL,
	}})

	if res.OK || res.Outcome != OutcomeFail {
		t.Fatalf("res = %+v, want !OK/OutcomeFail for a 404 (bad installation id)", res)
	}
}

func TestGitHubAppProber_Probe_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()

	p := newGitHubAppProber(srv.Client(), 2*time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{
		"app_id":           "123",
		"installation_id":  "456",
		"private_key_path": testPrivateKeyPEM(t),
		"api_base_url":     srv.URL,
	}})
	if res.OK || res.Outcome != OutcomeFail {
		t.Fatalf("res = %+v, want !OK/OutcomeFail for a 401", res)
	}
}

func TestGitHubAppProber_Probe_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer srv.Close()

	p := newGitHubAppProber(srv.Client(), 2*time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{
		"app_id":           "123",
		"installation_id":  "456",
		"private_key_path": testPrivateKeyPEM(t),
		"api_base_url":     srv.URL,
	}})
	if res.OK || res.Outcome != OutcomeError {
		t.Fatalf("res = %+v, want !OK/OutcomeError for a 429", res)
	}
}

func TestGitHubAppProber_Probe_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newGitHubAppProber(srv.Client(), 2*time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{
		"app_id":           "123",
		"installation_id":  "456",
		"private_key_path": testPrivateKeyPEM(t),
		"api_base_url":     srv.URL,
	}})
	if res.OK || res.Outcome != OutcomeError {
		t.Fatalf("res = %+v, want !OK/OutcomeError for a 5xx", res)
	}
}

func TestGitHubAppProber_Probe_InvalidPrivateKey(t *testing.T) {
	p := newGitHubAppProber(http.DefaultClient, 2*time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{
		"app_id":           "123",
		"installation_id":  "456",
		"private_key_path": "not a pem key",
	}})
	if res.OK || res.Outcome != OutcomeFail {
		t.Fatalf("res = %+v, want !OK/OutcomeFail for an unparseable private key", res)
	}
}

func TestGitHubAppProber_Probe_MissingFields(t *testing.T) {
	p := newGitHubAppProber(http.DefaultClient, 2*time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{}})
	if res.OK || res.Outcome != OutcomeFail {
		t.Fatalf("res = %+v, want !OK/OutcomeFail for missing required fields", res)
	}
	if len(res.Failures) == 0 {
		t.Error("expected CheckFailures for missing fields")
	}
}

// TestGitHubAppProber_Probe_NeverEchoesPrivateKey — the PEM key must never
// appear in Summary/Detail even on failure.
func TestGitHubAppProber_Probe_NeverEchoesPrivateKey(t *testing.T) {
	key := testPrivateKeyPEM(t)
	p := newGitHubAppProber(&http.Client{Timeout: 200 * time.Millisecond}, 200*time.Millisecond)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{
		"app_id":           "123",
		"installation_id":  "456",
		"private_key_path": key,
		"api_base_url":     "http://127.0.0.1:1",
	}})
	if strings.Contains(res.Summary, key) || strings.Contains(res.Detail, key) {
		t.Fatalf("ProbeResult leaked the private key")
	}
}

func TestGitHubAppProber_Probe_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_faketoken","expires_at":"2099-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	p := newGitHubAppProber(&http.Client{Timeout: 5 * time.Millisecond}, 5*time.Millisecond)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{
		"app_id":           "123",
		"installation_id":  "456",
		"private_key_path": testPrivateKeyPEM(t),
		"api_base_url":     srv.URL,
	}})
	if res.OK || res.Outcome != OutcomeError {
		t.Fatalf("res = %+v, want !OK/OutcomeError on timeout", res)
	}
}
