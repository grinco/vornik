package integrations

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/telegram"
)

func withTelegramTestBaseURL(t *testing.T, url string) {
	t.Helper()
	orig := telegram.APIBaseURL
	telegram.APIBaseURL = url
	t.Cleanup(func() { telegram.APIBaseURL = orig })
}

func TestTelegramProber_Kind(t *testing.T) {
	p := newTelegramProber(http.DefaultClient, 0)
	if p.Kind() != "telegram" {
		t.Errorf("Kind() = %q, want telegram", p.Kind())
	}
}

func TestTelegramProber_Probe_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"username":"my_bot"}}`))
	}))
	defer srv.Close()
	withTelegramTestBaseURL(t, srv.URL)

	p := newTelegramProber(srv.Client(), 2*time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Kind: "telegram", Values: map[string]string{"bot_token": "faketoken"}})

	if !res.OK || res.Outcome != OutcomeOK {
		t.Fatalf("res = %+v, want OK/OutcomeOK", res)
	}
	if !strings.Contains(res.Summary, "@my_bot") {
		t.Errorf("Summary = %q, want it to mention @my_bot", res.Summary)
	}
	if res.Kind != "telegram" {
		t.Errorf("Kind = %q, want telegram", res.Kind)
	}
}

func TestTelegramProber_Probe_InvalidToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
	}))
	defer srv.Close()
	withTelegramTestBaseURL(t, srv.URL)

	p := newTelegramProber(srv.Client(), 2*time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{"bot_token": "badtoken"}})

	if res.OK || res.Outcome != OutcomeFail {
		t.Fatalf("res = %+v, want !OK/OutcomeFail for a 401", res)
	}
	if len(res.Failures) == 0 {
		t.Error("expected at least one CheckFailure for an invalid token")
	}
}

func TestTelegramProber_Probe_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests"}`))
	}))
	defer srv.Close()
	withTelegramTestBaseURL(t, srv.URL)

	p := newTelegramProber(srv.Client(), 2*time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{"bot_token": "sometoken"}})

	if res.OK || res.Outcome != OutcomeError {
		t.Fatalf("res = %+v, want !OK/OutcomeError for a 429 (transient, not proof of invalidity)", res)
	}
}

func TestTelegramProber_Probe_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":500,"description":"boom"}`))
	}))
	defer srv.Close()
	withTelegramTestBaseURL(t, srv.URL)

	p := newTelegramProber(srv.Client(), 2*time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{"bot_token": "sometoken"}})
	if res.OK || res.Outcome != OutcomeError {
		t.Fatalf("res = %+v, want !OK/OutcomeError for a 5xx", res)
	}
}

func TestTelegramProber_Probe_Malformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()
	withTelegramTestBaseURL(t, srv.URL)

	p := newTelegramProber(srv.Client(), 2*time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{"bot_token": "sometoken"}})
	if res.OK || res.Outcome != OutcomeError {
		t.Fatalf("res = %+v, want !OK/OutcomeError for a malformed body", res)
	}
}

func TestTelegramProber_Probe_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"username":"my_bot"}}`))
	}))
	defer srv.Close()
	withTelegramTestBaseURL(t, srv.URL)

	p := newTelegramProber(srv.Client(), 5*time.Millisecond)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{"bot_token": "sometoken"}})
	if res.OK || res.Outcome != OutcomeError {
		t.Fatalf("res = %+v, want !OK/OutcomeError on timeout", res)
	}
}

// TestTelegramProber_Probe_NeverEchoesToken is the design §8 "Summary/
// Detail-no-echo" assertion: feed a distinctive token, force a failure,
// assert the token never appears in Summary or Detail.
func TestTelegramProber_Probe_NeverEchoesToken(t *testing.T) {
	const distinctiveSecret = "999888:AAVeryDistinctiveTokenForLeakTestXYZ"
	withTelegramTestBaseURL(t, "http://127.0.0.1:1")

	p := newTelegramProber(&http.Client{Timeout: 200 * time.Millisecond}, 200*time.Millisecond)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{"bot_token": distinctiveSecret}})

	if strings.Contains(res.Summary, distinctiveSecret) || strings.Contains(res.Detail, distinctiveSecret) {
		t.Fatalf("ProbeResult leaked the token: Summary=%q Detail=%q", res.Summary, res.Detail)
	}
	for _, f := range res.Failures {
		if strings.Contains(f.Reason, distinctiveSecret) {
			t.Fatalf("CheckFailure leaked the token: %+v", f)
		}
	}
}

func TestTelegramProber_Probe_MissingToken(t *testing.T) {
	p := newTelegramProber(http.DefaultClient, time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{}})
	if res.OK || res.Outcome != OutcomeFail {
		t.Fatalf("res = %+v, want !OK/OutcomeFail for a missing required field", res)
	}
}
