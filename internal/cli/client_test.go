package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewClient(t *testing.T) {
	client := NewClient("http://localhost:8080", "test-key")
	if client.baseURL != "http://localhost:8080" {
		t.Errorf("expected baseURL http://localhost:8080, got %s", client.baseURL)
	}
	if client.apiKey != "test-key" {
		t.Errorf("expected apiKey test-key, got %s", client.apiKey)
	}
	if client.httpClient == nil {
		t.Error("expected httpClient to be initialized")
	}
}

func TestClientDo(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify headers
		if r.Header.Get("Content-Type") != "application/json" {
			t.Error("expected Content-Type header to be application/json")
		}
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Error("expected X-API-Key header to be test-key")
		}
		if r.URL.Path != "/test" {
			t.Errorf("expected path /test, got %s", r.URL.Path)
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer testServer.Close()

	client := NewClient(testServer.URL, "test-key")
	resp, err := client.Do(http.MethodGet, "/test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
}

func TestClientGet(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET request, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer testServer.Close()

	client := NewClient(testServer.URL, "test-key")
	resp, err := client.Get("/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
}

func TestClientPost(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST request, got %s", r.Method)
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "created"})
	}))
	defer testServer.Close()

	client := NewClient(testServer.URL, "test-key")
	resp, err := client.Post("/test", map[string]string{"name": "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("expected status 202, got %d", resp.StatusCode)
	}
}

func TestAPIError(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code":    "VALIDATION_ERROR",
			"message": "invalid input",
		})
	}))
	defer testServer.Close()

	client := NewClient(testServer.URL, "test-key")
	resp, err := client.Get("/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	err = ParseAPIError(resp)
	if err == nil {
		t.Fatal("expected API error")
	}

	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected APIError type, got %T", err)
	}

	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", apiErr.StatusCode)
	}
	if apiErr.Code != "VALIDATION_ERROR" {
		t.Errorf("expected code VALIDATION_ERROR, got %s", apiErr.Code)
	}
	if apiErr.Message != "invalid input" {
		t.Errorf("expected message 'invalid input', got %s", apiErr.Message)
	}
}

func TestNewClientWithTimeout_ExplicitTimeoutHonoured(t *testing.T) {
	c := NewClientWithTimeout("http://x", "k", 7*time.Second)
	if c.httpClient.Timeout != 7*time.Second {
		t.Errorf("Timeout = %v, want 7s", c.httpClient.Timeout)
	}
}

func TestNewClientWithTimeout_ZeroDisablesDeadline(t *testing.T) {
	// "0" must keep the http.Client.Timeout at zero (no deadline)
	// so streaming endpoints stay open. Anchors the documented
	// contract operators rely on when they set VORNIK_API_TIMEOUT=0.
	c := NewClientWithTimeout("http://x", "k", 0)
	if c.httpClient.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 (deadline disabled)", c.httpClient.Timeout)
	}
}

func TestClientFromEnv_DefaultsApply(t *testing.T) {
	t.Setenv("VORNIK_API_URL", "")
	t.Setenv("VORNIK_API_KEY", "")
	t.Setenv("VORNIK_API_TIMEOUT", "")
	c := ClientFromEnv()
	if c.baseURL != DefaultAPIURL {
		t.Errorf("baseURL = %q, want %q", c.baseURL, DefaultAPIURL)
	}
	if c.apiKey != DefaultAPIKey {
		t.Errorf("apiKey = %q, want %q", c.apiKey, DefaultAPIKey)
	}
	if c.httpClient.Timeout != DefaultAPITimeout {
		t.Errorf("Timeout = %v, want %v", c.httpClient.Timeout, DefaultAPITimeout)
	}
}

func TestClientFromEnv_TimeoutOverrideAppliesGoDuration(t *testing.T) {
	t.Setenv("VORNIK_API_TIMEOUT", "5m")
	c := ClientFromEnv()
	if c.httpClient.Timeout != 5*time.Minute {
		t.Errorf("VORNIK_API_TIMEOUT=5m → Timeout = %v, want 5m", c.httpClient.Timeout)
	}
}

func TestClientFromEnv_TimeoutZeroDisablesDeadline(t *testing.T) {
	// "0" is the documented escape hatch for streaming endpoints
	// where the operator explicitly wants no deadline.
	t.Setenv("VORNIK_API_TIMEOUT", "0")
	c := ClientFromEnv()
	if c.httpClient.Timeout != 0 {
		t.Errorf("VORNIK_API_TIMEOUT=0 → Timeout = %v, want 0", c.httpClient.Timeout)
	}
}

func TestClientFromEnv_TimeoutNoneDisablesDeadline(t *testing.T) {
	// Mirrors "0" but is friendlier to read in shell scripts.
	t.Setenv("VORNIK_API_TIMEOUT", "none")
	c := ClientFromEnv()
	if c.httpClient.Timeout != 0 {
		t.Errorf(`VORNIK_API_TIMEOUT="none" → Timeout = %v, want 0`, c.httpClient.Timeout)
	}
}

func TestClientFromEnv_BadTimeoutFallsBackToDefault(t *testing.T) {
	// A malformed value must NOT crash the CLI — operators see a
	// stderr warning and the default timeout is preserved.
	t.Setenv("VORNIK_API_TIMEOUT", "yesterday")
	c := ClientFromEnv()
	if c.httpClient.Timeout != DefaultAPITimeout {
		t.Errorf("bad VORNIK_API_TIMEOUT must fall back to default; got Timeout = %v", c.httpClient.Timeout)
	}
}
