package integrations

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/slack"
)

func withSlackTestAuthTestURL(t *testing.T, url string) {
	t.Helper()
	orig := slack.AuthTestURL
	slack.AuthTestURL = url
	t.Cleanup(func() { slack.AuthTestURL = orig })
}

func TestSlackProber_Kind(t *testing.T) {
	p := newSlackProber(http.DefaultClient, 0)
	if p.Kind() != "slack" {
		t.Errorf("Kind() = %q, want slack", p.Kind())
	}
}

func TestSlackProber_Probe_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"team":"Acme","user":"botuser"}`))
	}))
	defer srv.Close()
	withSlackTestAuthTestURL(t, srv.URL)

	p := newSlackProber(srv.Client(), 2*time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Kind: "slack", Values: map[string]string{"bot_token_env": "xoxb-fake"}})

	if !res.OK || res.Outcome != OutcomeOK {
		t.Fatalf("res = %+v, want OK/OutcomeOK", res)
	}
	if !strings.Contains(res.Summary, "Acme") || !strings.Contains(res.Summary, "botuser") {
		t.Errorf("Summary = %q, want it to mention Acme and botuser", res.Summary)
	}
}

func TestSlackProber_Probe_InvalidAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
	}))
	defer srv.Close()
	withSlackTestAuthTestURL(t, srv.URL)

	p := newSlackProber(srv.Client(), 2*time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{"bot_token_env": "xoxb-bad"}})

	if res.OK || res.Outcome != OutcomeFail {
		t.Fatalf("res = %+v, want !OK/OutcomeFail for invalid_auth", res)
	}
	if len(res.Failures) == 0 {
		t.Error("expected at least one CheckFailure")
	}
}

func TestSlackProber_Probe_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
	}))
	defer srv.Close()
	withSlackTestAuthTestURL(t, srv.URL)

	p := newSlackProber(srv.Client(), 2*time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{"bot_token_env": "xoxb-some"}})
	if res.OK || res.Outcome != OutcomeError {
		t.Fatalf("res = %+v, want !OK/OutcomeError for ratelimited", res)
	}
}

func TestSlackProber_Probe_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	withSlackTestAuthTestURL(t, srv.URL)

	p := newSlackProber(srv.Client(), 2*time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{"bot_token_env": "xoxb-some"}})
	if res.OK || res.Outcome != OutcomeError {
		t.Fatalf("res = %+v, want !OK/OutcomeError for a 5xx", res)
	}
}

func TestSlackProber_Probe_Malformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()
	withSlackTestAuthTestURL(t, srv.URL)

	p := newSlackProber(srv.Client(), 2*time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{"bot_token_env": "xoxb-some"}})
	if res.OK || res.Outcome != OutcomeError {
		t.Fatalf("res = %+v, want !OK/OutcomeError for a malformed body", res)
	}
}

func TestSlackProber_Probe_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ok":true,"team":"Acme","user":"botuser"}`))
	}))
	defer srv.Close()
	withSlackTestAuthTestURL(t, srv.URL)

	p := newSlackProber(srv.Client(), 5*time.Millisecond)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{"bot_token_env": "xoxb-some"}})
	if res.OK || res.Outcome != OutcomeError {
		t.Fatalf("res = %+v, want !OK/OutcomeError on timeout", res)
	}
}

func TestSlackProber_Probe_NeverEchoesToken(t *testing.T) {
	const distinctiveSecret = "xoxb-VeryDistinctiveSlackTokenForLeakTest"
	withSlackTestAuthTestURL(t, "http://127.0.0.1:1")

	p := newSlackProber(&http.Client{Timeout: 200 * time.Millisecond}, 200*time.Millisecond)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{"bot_token_env": distinctiveSecret}})

	if strings.Contains(res.Summary, distinctiveSecret) || strings.Contains(res.Detail, distinctiveSecret) {
		t.Fatalf("ProbeResult leaked the token: Summary=%q Detail=%q", res.Summary, res.Detail)
	}
}

func TestSlackProber_Probe_MissingToken(t *testing.T) {
	p := newSlackProber(http.DefaultClient, time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{}})
	if res.OK || res.Outcome != OutcomeFail {
		t.Fatalf("res = %+v, want !OK/OutcomeFail for a missing required field", res)
	}
}
