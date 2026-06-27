package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcp"
)

// stubMCPRegistry returns a fixed snapshot. Lets the test exercise
// the JSON shape without standing up a real Connect()/tools/list
// chain.
type stubMCPRegistry struct {
	snap []mcp.ServerSnapshot
}

func (s *stubMCPRegistry) Snapshot(_ context.Context) []mcp.ServerSnapshot {
	return s.snap
}

// TestServer_ListMCPServers_HappyPath covers the documented JSON
// contract: each configured server is emitted in alphabetical order
// with its reachable flag, tool list, and last_checked_at. The
// parallel project-form agent consumes this surface — locking
// the shape here is what keeps it from drifting.
func TestServer_ListMCPServers_HappyPath(t *testing.T) {
	checked := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	reg := &stubMCPRegistry{snap: []mcp.ServerSnapshot{
		{
			Name:          "scraper",
			Transport:     "sse",
			URL:           "http://127.0.0.1:8787",
			Reachable:     true,
			LastCheckedAt: checked,
			Tools: []mcp.Tool{
				{Name: "web_fetch", Description: "Fetch a URL"},
				{Name: "ical_events", Description: "Parse iCal"},
			},
		},
		{
			Name:          "broken",
			Transport:     "sse",
			URL:           "http://127.0.0.1:9999",
			Reachable:     false,
			Error:         "connection refused",
			LastCheckedAt: checked,
		},
	}}
	server := NewServer(WithLogger(zerolog.Nop()), WithMCPRegistry(reg))

	req := authDisabledReq(httptest.NewRequest(http.MethodGet, "/api/v1/mcp/servers", nil))
	rec := httptest.NewRecorder()
	server.ListMCPServers(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp mcpServersResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Servers, 2)

	// Order is whatever the registry hands us; the stub returned
	// scraper first.
	require.Equal(t, "scraper", resp.Servers[0].Name)
	require.True(t, resp.Servers[0].Reachable)
	require.Empty(t, resp.Servers[0].Error)
	require.Len(t, resp.Servers[0].Tools, 2)
	require.Equal(t, "web_fetch", resp.Servers[0].Tools[0].Name)
	require.Equal(t, "Fetch a URL", resp.Servers[0].Tools[0].Description)
	require.Equal(t, checked, resp.Servers[0].LastCheckedAt)

	// Unreachable server still appears — operators need to see it.
	require.Equal(t, "broken", resp.Servers[1].Name)
	require.False(t, resp.Servers[1].Reachable)
	require.Equal(t, "connection refused", resp.Servers[1].Error)
	// Tools is nil on the wire for unreachable rows so consumers
	// can tell "server up, advertises nothing" (empty slice) from
	// "server down" (nil) without reading the reachable flag.
	require.Nil(t, resp.Servers[1].Tools)
}

// TestServer_ListMCPServers_NoRegistry confirms the endpoint is
// safe on deployments that don't declare a daemon-level mcp block.
// Returns 200 + empty array rather than 503 — operators get a
// clean "nothing configured" page rather than an error.
func TestServer_ListMCPServers_NoRegistry(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()))

	req := authDisabledReq(httptest.NewRequest(http.MethodGet, "/api/v1/mcp/servers", nil))
	rec := httptest.NewRecorder()
	server.ListMCPServers(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"servers":[]`)
}

// TestServer_ListMCPServers_ReachableEmpty differentiates the
// "server up, no tools" state from the "server down" state. A
// reachable server with an empty advertised catalog must serialize
// as tools: [] (not null) so JSON consumers can branch cleanly.
func TestServer_ListMCPServers_ReachableEmpty(t *testing.T) {
	reg := &stubMCPRegistry{snap: []mcp.ServerSnapshot{
		{Name: "empty", Transport: "sse", URL: "http://example", Reachable: true, Tools: nil},
	}}
	server := NewServer(WithLogger(zerolog.Nop()), WithMCPRegistry(reg))

	req := authDisabledReq(httptest.NewRequest(http.MethodGet, "/api/v1/mcp/servers", nil))
	rec := httptest.NewRecorder()
	server.ListMCPServers(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	// Empty array, not null.
	require.Contains(t, rec.Body.String(), `"tools":[]`)
	require.NotContains(t, rec.Body.String(), `"tools":null`)
}

func TestServer_ListMCPServers_RequiresAdmin(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()), WithAdminConfig(config.AdminConfig{
		Enabled:     true,
		AllowedKeys: []string{"sk-admin"},
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/servers", nil)
	req = req.WithContext(context.WithValue(req.Context(), apiKeyKey, "sk-project"))
	rec := httptest.NewRecorder()
	server.ListMCPServers(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
}
