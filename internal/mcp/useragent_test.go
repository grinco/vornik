package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/version"
)

// jsonRPCEcho replies to any JSON-RPC request with an empty successful
// result, so a test can assert on the REQUEST the client sent.
func jsonRPCEcho(t *testing.T, capture func(*http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture(r)
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{},
		})
	}))
}

// TestUserAgent_DefaultsToBuildVersion asserts the package emits a real
// identity even when nothing called SetVersion (a unit-test binary, a tool
// that constructs a Client directly).
func TestUserAgent_DefaultsToBuildVersion(t *testing.T) {
	assert.Equal(t, version.UserAgent(version.Default), userAgent())
}

// TestSetVersion_ReflectedInUserAgent proves the daemon's build version
// reaches the header, which is what makes the UA useful for vendor-side
// support triage.
func TestSetVersion_ReflectedInUserAgent(t *testing.T) {
	t.Cleanup(func() { SetVersion("") })
	SetVersion("2026.8.0-1-gdeadbeef")
	assert.Equal(t, "Vornik/2026.8.0-1-gdeadbeef (+https://vornik.io)", userAgent())
}

// TestStreamableHTTP_SendsVornikUserAgent — F3 regression, request path 1 of 3.
func TestStreamableHTTP_SendsVornikUserAgent(t *testing.T) {
	var got string
	srv := jsonRPCEcho(t, func(r *http.Request) { got = r.Header.Get("User-Agent") })
	defer srv.Close()

	c := &Client{
		config:     ServerConfig{Name: "s", Transport: "streamable-http", URL: srv.URL},
		logger:     zerolog.Nop(),
		httpClient: srv.Client(),
	}
	_, err := c.callStreamableHTTP(context.Background(), "tools/list", map[string]any{})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(got, "Vornik/"), "User-Agent = %q", got)
	assert.NotContains(t, got, "Go-http-client")
}

// TestStreamableHTTPNotify_SendsVornikUserAgent — request path 2 of 3. The
// notifications/initialized envelope is a separate request builder and was
// the easiest one to miss.
func TestStreamableHTTPNotify_SendsVornikUserAgent(t *testing.T) {
	var got string
	srv := jsonRPCEcho(t, func(r *http.Request) { got = r.Header.Get("User-Agent") })
	defer srv.Close()

	c := &Client{
		config:     ServerConfig{Name: "s", Transport: "streamable-http", URL: srv.URL},
		logger:     zerolog.Nop(),
		httpClient: srv.Client(),
	}
	require.NoError(t, c.notify("notifications/initialized", nil))
	assert.True(t, strings.HasPrefix(got, "Vornik/"), "User-Agent = %q", got)
}

// TestSSE_SendsVornikUserAgent — request path 3 of 3.
func TestSSE_SendsVornikUserAgent(t *testing.T) {
	var got string
	srv := jsonRPCEcho(t, func(r *http.Request) { got = r.Header.Get("User-Agent") })
	defer srv.Close()

	c := &Client{
		config:     ServerConfig{Name: "s", Transport: "sse", URL: srv.URL + "/sse"},
		logger:     zerolog.Nop(),
		httpClient: srv.Client(),
	}
	_, err := c.callSSE(context.Background(), "tools/list", map[string]any{})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(got, "Vornik/"), "User-Agent = %q", got)
	assert.NotContains(t, got, "Go-http-client")
}

// TestUserAgent_OperatorOverrideWins keeps an escape hatch: a deployment
// behind a picky proxy can pin its own UA through the per-server Headers map,
// and the default must not clobber it. The guarantee this design makes is
// "never Go's anonymous default", not "always exactly ours".
func TestUserAgent_OperatorOverrideWins(t *testing.T) {
	var got string
	srv := jsonRPCEcho(t, func(r *http.Request) { got = r.Header.Get("User-Agent") })
	defer srv.Close()

	c := &Client{
		config: ServerConfig{
			Name: "s", Transport: "streamable-http", URL: srv.URL,
			Headers: map[string]string{"User-Agent": "Custom/9"},
		},
		logger:     zerolog.Nop(),
		httpClient: srv.Client(),
	}
	_, err := c.callStreamableHTTP(context.Background(), "tools/list", map[string]any{})
	require.NoError(t, err)
	assert.Equal(t, "Custom/9", got)
}
