// Package cli provides command-line interface commands for vornik.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultAPIURL is the default base URL for the vornik API.
const DefaultAPIURL = "http://localhost:8080"

// DefaultAPIKey is the default API key for development.
const DefaultAPIKey = "vornik-dev-key"

// DefaultAPITimeout is the request deadline applied to every CLI
// → daemon call when no explicit override is set. 30s is generous
// for the common read paths (list / get / status) but too tight
// for endpoints that internally invoke an LLM (doctor prompt-lint,
// memory reclassify on large corpora, judge dry-runs).
// Operators can override per-invocation via the VORNIK_API_TIMEOUT
// env var (e.g. VORNIK_API_TIMEOUT=10m vornikctl doctor prompt-lint).
const DefaultAPITimeout = 30 * time.Second

// Client provides HTTP client functionality for the vornik API.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewClient creates a new API client with the given base URL and
// API key. Uses DefaultAPITimeout for the HTTP client deadline.
// Callers needing a longer deadline (LLM-bound endpoints) should
// either set VORNIK_API_TIMEOUT before constructing via
// ClientFromEnv, or build a client via NewClientWithTimeout.
func NewClient(baseURL, apiKey string) *Client {
	return NewClientWithTimeout(baseURL, apiKey, DefaultAPITimeout)
}

// NewClientWithTimeout is the explicit-timeout constructor.
// A zero or negative timeout disables the deadline entirely;
// useful for streaming endpoints (SSE / log tails) where the
// request stays open by design.
func NewClientWithTimeout(baseURL, apiKey string, timeout time.Duration) *Client {
	hc := &http.Client{}
	if timeout > 0 {
		hc.Timeout = timeout
	}
	return &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		httpClient: hc,
	}
}

// ClientFromEnv creates a new API client using environment variables.
// Environment variables:
//   - VORNIK_API_URL: Base URL for the API (default: http://localhost:8080)
//   - VORNIK_API_KEY: API key for authentication (default: vornik-dev-key)
//   - VORNIK_API_TIMEOUT: Request deadline as a Go duration string
//     (e.g. "5m", "10m"). Default DefaultAPITimeout (30s).
//     "0" or "none" disables the deadline.
func ClientFromEnv() *Client {
	baseURL := os.Getenv("VORNIK_API_URL")
	if baseURL == "" {
		baseURL = DefaultAPIURL
	}

	// Resolution order: VORNIK_API_KEY env (explicit; wins for CI /
	// scripted use) → OS keychain (recommended for interactive operators,
	// via `vornikctl auth login`) → built-in default. A keychain error
	// (e.g. no secret service on a headless box) falls through silently;
	// `vornikctl auth status` surfaces it explicitly.
	apiKey := os.Getenv("VORNIK_API_KEY")
	if apiKey == "" {
		if stored, err := LoadStoredAPIKey(); err == nil && stored != "" {
			apiKey = stored
		}
	}
	if apiKey == "" {
		apiKey = DefaultAPIKey
	}

	timeout := DefaultAPITimeout
	if v := strings.TrimSpace(os.Getenv("VORNIK_API_TIMEOUT")); v != "" {
		// Accept "0" and "none" as explicit no-deadline markers so
		// operators don't have to know that time.ParseDuration
		// rejects "none".
		if v == "0" || strings.EqualFold(v, "none") {
			timeout = 0
		} else if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
			timeout = parsed
		} else {
			// Bad value: fall back to default + warn so the operator
			// sees they tried to override something. stderr keeps
			// stdout clean for scripts.
			fmt.Fprintf(os.Stderr, "vornikctl: ignoring invalid VORNIK_API_TIMEOUT=%q (use a Go duration like \"10m\", or \"0\"/\"none\" to disable)\n", v)
		}
	}

	return NewClientWithTimeout(baseURL, apiKey, timeout)
}

// Do performs an HTTP request to the API.
func (c *Client) Do(method, path string, body interface{}) (*http.Response, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	url := c.baseURL + path
	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	return resp, nil
}

// Get performs a GET request to the API.
func (c *Client) Get(path string) (*http.Response, error) {
	return c.Do(http.MethodGet, path, nil)
}

// Post performs a POST request to the API.
func (c *Client) Post(path string, body interface{}) (*http.Response, error) {
	return c.Do(http.MethodPost, path, body)
}

// Delete performs a DELETE request to the API.
func (c *Client) Delete(path string) (*http.Response, error) {
	return c.Do(http.MethodDelete, path, nil)
}

// APIError represents an error response from the API.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("API error %d (%s): %s", e.StatusCode, e.Code, e.Message)
	case e.Message != "":
		return fmt.Sprintf("API error %d: %s", e.StatusCode, e.Message)
	case e.Code != "":
		return fmt.Sprintf("API error %d: %s", e.StatusCode, e.Code)
	default:
		return fmt.Sprintf("API error %d", e.StatusCode)
	}
}

// ParseAPIError parses an error response from the API. Handles both the
// canonical nested shape the daemon emits (`{"error":{"code","message"}}`
// — see internal/api/middleware.go:respondError) and a flat fallback so
// older server builds don't explode the CLI. An empty body, or a body
// that isn't JSON at all, yields a generic status-only error rather than
// the "API error 404: - -" silence the previous flat-only decoder
// produced whenever the server returned the documented shape.
func ParseAPIError(resp *http.Response) error {
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("HTTP %d: failed to read error response: %w", resp.StatusCode, err)
	}

	apiErr := &APIError{StatusCode: resp.StatusCode}

	if len(bytes.TrimSpace(body)) == 0 {
		apiErr.Message = http.StatusText(resp.StatusCode)
		return apiErr
	}

	// Canonical nested shape.
	var nested struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &nested); err == nil && (nested.Error.Code != "" || nested.Error.Message != "") {
		apiErr.Code = nested.Error.Code
		apiErr.Message = nested.Error.Message
		return apiErr
	}

	// Flat fallback (covers older servers / hand-crafted error responses).
	var flat struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &flat); err == nil && (flat.Code != "" || flat.Message != "") {
		apiErr.Code = flat.Code
		apiErr.Message = flat.Message
		return apiErr
	}

	// Not JSON or no recognised fields — surface the raw body so the user
	// at least sees the server's message instead of "- -".
	apiErr.Message = string(bytes.TrimSpace(body))
	return apiErr
}
