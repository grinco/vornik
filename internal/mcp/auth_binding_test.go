package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
)

// Regression: P0 connector-auth silent degradation, observed on
// vornik-marketing 2026-08-25 and reproduced against the production daemon the
// same day.
//
// applyMCPOAuthToken resolved the bearer ONCE, at wiring time, into the static
// AuthHeaders map. initMCP — the only thing that rewires — runs at boot, on
// config reload, and on OAuth consent. Atlassian's access token lives 8 hours;
// the measured gap between two rewires on the production daemon was 58h41m. So
// the daemon presented a dead bearer for ~51 hours while every status surface
// reported the grant healthy.
//
// The fix is that a GRANT-derived credential binds per request. See
// https://docs.vornik.io §3.1.
func TestAuthHeaderProviderIsConsultedOnEveryRequest(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer srv.Close()

	var n int64
	c := &Client{
		config: ServerConfig{
			Name:      "atlassian",
			Transport: "streamable-http",
			URL:       srv.URL,
			AuthHeaderProvider: func(context.Context) (map[string]string, error) {
				i := atomic.AddInt64(&n, 1)
				return map[string]string{"Authorization": "Bearer token-" + string(rune('0'+i))}, nil
			},
		},
		logger: zerolog.Nop(),
	}

	for i := 0; i < 2; i++ {
		req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if err := c.applyConfigHeaders(context.Background(), req); err != nil {
			t.Fatalf("applyConfigHeaders: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		_ = resp.Body.Close()
	}

	if len(seen) != 2 {
		t.Fatalf("want 2 requests, got %d", len(seen))
	}
	if seen[0] == seen[1] {
		t.Fatalf("the bearer was frozen across requests (%q both times) — "+
			"a grant-derived credential must be resolved per call", seen[0])
	}
}

// A provider error is FATAL to the call. Falling back to an unauthenticated
// request is precisely the silent degradation this design removes: the vendor
// answers 401, the agent narrates it, and the task reports success.
func TestAuthHeaderProviderErrorFailsTheCall(t *testing.T) {
	sentinel := errors.New("mcp oauth grant needs operator reconnect")
	c := &Client{
		config: ServerConfig{
			Name:      "atlassian",
			Transport: "streamable-http",
			URL:       "http://127.0.0.1:1",
			AuthHeaderProvider: func(context.Context) (map[string]string, error) {
				return nil, sentinel
			},
		},
		logger: zerolog.Nop(),
	}
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:1", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	err = c.applyConfigHeaders(context.Background(), req)
	if err == nil {
		t.Fatal("a credential that cannot be resolved must fail the call, not degrade it to unauthenticated")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("want the provider's error wrapped, got %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("no credential must be attached on the error path, got %q", got)
	}
}

// "Not connected yet" is not an error: AccessToken returns ("", nil) for a
// server the operator has never consented to. The server must still register
// and the request must still go out, exactly as before — withholding it makes
// the tools vanish from every agent's catalog and reads as a missing
// integration rather than a missing consent (auth design §8).
func TestAuthHeaderProviderNilMapIsNotAnError(t *testing.T) {
	c := &Client{
		config: ServerConfig{
			Name:      "atlassian",
			Transport: "streamable-http",
			AuthHeaderProvider: func(context.Context) (map[string]string, error) {
				return nil, nil
			},
		},
		logger: zerolog.Nop(),
	}
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:1", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if err := c.applyConfigHeaders(context.Background(), req); err != nil {
		t.Fatalf("an unconnected oauth server must not fail the call: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("want no Authorization header, got %q", got)
	}
}

// A static credential (mode: static / header / stdio env) stays bound at wiring
// time — config only changes through a reload, which rewires anyway. The
// provider is the exception for grant-derived credentials, not a replacement.
func TestStaticAuthHeadersStillApplyWhenNoProvider(t *testing.T) {
	c := &Client{
		config: ServerConfig{
			Name:        "scraper",
			Transport:   "streamable-http",
			AuthHeaders: map[string]string{"Authorization": "Bearer static-value"},
		},
		logger: zerolog.Nop(),
	}
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:1", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if err := c.applyConfigHeaders(context.Background(), req); err != nil {
		t.Fatalf("applyConfigHeaders: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer static-value" {
		t.Fatalf("static auth header lost: %q", got)
	}
}
