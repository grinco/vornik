package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	os.Stdout = w
	err = fn()
	_ = w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return buf.String(), err
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	body := `[{"name":"broker","type":"http","url":"http://127.0.0.1:8788/sse","allowed_tools":["get_quote"]}]`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write mcp config: %v", err)
	}

	servers, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(servers) != 1 || servers[0].Name != "broker" || servers[0].URL != "http://127.0.0.1:8788/sse" {
		t.Fatalf("unexpected servers: %#v", servers)
	}
	if len(servers[0].AllowedTools) != 1 || servers[0].AllowedTools[0] != "get_quote" {
		t.Fatalf("unexpected allowed tools: %#v", servers[0].AllowedTools)
	}
}

func TestLoadConfigRejectsInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(`not json`), 0o644); err != nil {
		t.Fatalf("write mcp config: %v", err)
	}

	_, err := loadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "parse mcp.json") {
		t.Fatalf("loadConfig invalid error = %v, want parse mcp.json", err)
	}
}

func TestHTTPDiscover(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tools":[{"type":"function","function":{"name":"broker__get_quote","description":"quote","parameters":{"type":"object"}}}]}`))
	}))
	defer srv.Close()

	out, err := captureStdout(t, func() error {
		return httpDiscover(context.Background(), srv.URL+"/", "p1")
	})
	if err != nil {
		t.Fatalf("httpDiscover: %v", err)
	}
	if gotPath != "/api/v1/projects/p1/mcp/tools" {
		t.Fatalf("daemon path = %q", gotPath)
	}
	var tools []map[string]any
	if err := json.Unmarshal([]byte(out), &tools); err != nil {
		t.Fatalf("discover stdout is not JSON: %v; body=%s", err, out)
	}
	if len(tools) != 1 {
		t.Fatalf("discover tools len = %d, want 1", len(tools))
	}
}

func TestHTTPCall(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"quote: SPY 600"}`))
	}))
	defer srv.Close()

	out, err := captureStdout(t, func() error {
		return httpCall(context.Background(), srv.URL, "p1", "broker__get_quote", `{"symbol":"SPY"}`)
	})
	if err != nil {
		t.Fatalf("httpCall: %v", err)
	}
	if gotPath != "/api/v1/projects/p1/mcp/tools/call" {
		t.Fatalf("daemon path = %q", gotPath)
	}
	if gotBody["name"] != "broker__get_quote" {
		t.Fatalf("request name = %#v", gotBody["name"])
	}
	args, ok := gotBody["arguments"].(map[string]any)
	if !ok || args["symbol"] != "SPY" {
		t.Fatalf("request arguments = %#v", gotBody["arguments"])
	}
	if out != "quote: SPY 600" {
		t.Fatalf("stdout = %q", out)
	}
}

// TestHTTPDiscoverSendsAuthHeader — the daemon's AuthMiddleware
// rejects every request lacking a credential when api.auth_enabled
// is true. mcp-bridge must forward VORNIK_API_KEY (or VORNIK_LLM_API_KEY
// as fallback) as a Bearer token, otherwise discover 401s and the
// agent gets an empty tools list (observed 2026-05-28 on task
// task_20260528111635 — "mcp-bridge: daemon returned 401: Missing API key").
func TestHTTPDiscoverSendsAuthHeader(t *testing.T) {
	t.Setenv(envAPIKey, "internal-system-key-123")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tools":[]}`))
	}))
	defer srv.Close()

	_, err := captureStdout(t, func() error {
		return httpDiscover(context.Background(), srv.URL, "p1")
	})
	if err != nil {
		t.Fatalf("httpDiscover: %v", err)
	}
	if gotAuth != "Bearer internal-system-key-123" {
		t.Fatalf("Authorization header = %q, want Bearer internal-system-key-123", gotAuth)
	}
}

// TestHTTPCallSendsAuthHeader — mirror of TestHTTPDiscoverSendsAuthHeader
// for the call path. tool-call traffic is much higher volume than
// discover; missing auth here would manifest as silent 404s in the
// agent loop ("unknown tool: foo") because the daemon never gets the
// chance to dispatch.
func TestHTTPCallSendsAuthHeader(t *testing.T) {
	t.Setenv(envAPIKey, "internal-system-key-456")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"ok"}`))
	}))
	defer srv.Close()

	_, err := captureStdout(t, func() error {
		return httpCall(context.Background(), srv.URL, "p1", "broker__get_quote", `{}`)
	})
	if err != nil {
		t.Fatalf("httpCall: %v", err)
	}
	if gotAuth != "Bearer internal-system-key-456" {
		t.Fatalf("Authorization header = %q, want Bearer internal-system-key-456", gotAuth)
	}
}

// TestHTTPDiscoverFallsBackToLLMKey — some agent shapes inject
// VORNIK_LLM_API_KEY but not VORNIK_API_KEY (older builds, or
// custom warm-pool configs). daemonBearerToken must fall back so
// the bridge keeps working across container versions.
func TestHTTPDiscoverFallsBackToLLMKey(t *testing.T) {
	t.Setenv(envAPIKey, "")
	t.Setenv(envLLMAPIKey, "llm-fallback-key")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tools":[]}`))
	}))
	defer srv.Close()

	_, err := captureStdout(t, func() error {
		return httpDiscover(context.Background(), srv.URL, "p1")
	})
	if err != nil {
		t.Fatalf("httpDiscover: %v", err)
	}
	if gotAuth != "Bearer llm-fallback-key" {
		t.Fatalf("Authorization header = %q, want fallback to VORNIK_LLM_API_KEY", gotAuth)
	}
}

// TestHTTPDiscoverNoKeyNoHeader — auth_enabled=false deployments
// have no key configured anywhere. The bridge must NOT send an
// Authorization header in that case (an empty Bearer would still
// trip the middleware's "Missing API key" path).
func TestHTTPDiscoverNoKeyNoHeader(t *testing.T) {
	t.Setenv(envAPIKey, "")
	t.Setenv(envLLMAPIKey, "")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tools":[]}`))
	}))
	defer srv.Close()

	_, err := captureStdout(t, func() error {
		return httpDiscover(context.Background(), srv.URL, "p1")
	})
	if err != nil {
		t.Fatalf("httpDiscover: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization header = %q, want empty when no key env is set", gotAuth)
	}
}

func TestHTTPCallPropagatesDaemonError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer srv.Close()

	_, err := captureStdout(t, func() error {
		return httpCall(context.Background(), srv.URL, "p1", "broker__get_quote", `{}`)
	})
	if err == nil || !strings.Contains(err.Error(), "daemon returned 502") {
		t.Fatalf("httpCall error = %v, want daemon returned 502", err)
	}
}

// TestHTTPCallRejectsInvalidJSON — pre-fix the marshal error from
// embedding an invalid json.RawMessage was discarded
// (`body, _ := json.Marshal(...)`), so the daemon got an empty
// POST and returned 400. Now we validate up-front and return a
// typed error before any HTTP traffic, so a hallucinated /
// truncated tool-call argument is surfaced clearly.
func TestHTTPCallRejectsInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream must not be hit when argsJSON is invalid")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, err := captureStdout(t, func() error {
		return httpCall(context.Background(), srv.URL, "p1", "broker__get_quote", `{"missing_close":`)
	})
	if err == nil {
		t.Fatalf("expected error on invalid JSON argsJSON, got nil")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("expected 'not valid JSON' in error, got: %v", err)
	}
}

// TestHTTPCallTrimsLargeArgsPreviewInError — defensive: if the LLM
// hallucinates a giant argument string, the error message must
// not echo the entire payload back into the audit log.
func TestHTTPCallTrimsLargeArgsPreviewInError(t *testing.T) {
	huge := strings.Repeat("x", 10*1024)
	_, err := captureStdout(t, func() error {
		return httpCall(context.Background(), "http://unused", "p1", "broker__get_quote", huge)
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if len(err.Error()) > 500 {
		t.Errorf("error message length = %d, want truncated under 500 chars", len(err.Error()))
	}
}

// TestDaemonHTTP covers the unix:// vs http(s):// transport selection
// added for the daemon-only network policy (Step B).
func TestDaemonHTTP(t *testing.T) {
	// http(s) base is trimmed of a trailing slash; client is plain.
	base, client := daemonHTTP("http://host.containers.internal:8080/", 60*time.Second)
	if base != "http://host.containers.internal:8080" {
		t.Errorf("http base = %q, want trimmed", base)
	}
	if client == nil || client.Timeout != 60*time.Second {
		t.Errorf("http client misconfigured: %+v", client)
	}
	if client.Transport != nil {
		t.Errorf("http client should use default transport, got custom %T", client.Transport)
	}

	// unix:// yields a synthetic http://unix base + a socket-dialing transport.
	base, client = daemonHTTP("unix:///run/vornik/vornik.sock", 60*time.Second)
	if base != "http://unix" {
		t.Errorf("unix base = %q, want http://unix", base)
	}
	if client == nil || client.Transport == nil {
		t.Fatalf("unix client must carry a custom dialing transport")
	}
}
