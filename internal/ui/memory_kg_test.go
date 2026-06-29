package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/admin"
	"vornik.io/vornik/internal/persistence"
)

// adminMemoryRequest builds a GET /memory/ request marked admin. The KG
// widget is gated `{{if and .IsAdmin .KG.Enabled}}` (its stats are
// instance-wide, so it's admin/auth-off-only); a bare httptest request is
// neither admin nor auth-off, so without this the panel is hidden and the
// widget assertions can't see it.
func adminMemoryRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/memory/", nil)
	return req.WithContext(admin.ContextWithAdmin(req.Context(), "session:test-admin"))
}

// TestMemoryIndex_WithKGStatsRendersWidget — when the chunk-graph
// repo is wired and returns non-empty stats, the KG widget on the
// memory landing page should populate with chunk counts + entity
// types.
func TestMemoryIndex_WithKGStatsRendersWidget(t *testing.T) {
	root := writeSwarmFixture(t)
	srv, _ := swarmEditServer(t, root)
	srv.chunkGraph = &fakeChunkGraph{
		stats: &persistence.KGStats{
			ChunksPending: 5,
			ChunksDone:    95,
			Entities:      42,
			Edges:         88,
			Mentions:      150,
			EntitiesByType: map[string]int{
				"person":       12,
				"organization": 8,
				"location":     5,
			},
		},
	}
	rec := httptest.NewRecorder()
	srv.Memory(rec, adminMemoryRequest())
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	// Entity type names should surface in the rendered legend.
	assert.Contains(t, body, "person")
	assert.Contains(t, body, "organization")
}

// TestMemoryIndex_KGStatsZeroTotalAvoidsDivByZero — when both
// pending and done are zero, the % calc must not divide by zero.
func TestMemoryIndex_KGStatsZeroTotalAvoidsDivByZero(t *testing.T) {
	root := writeSwarmFixture(t)
	srv, _ := swarmEditServer(t, root)
	srv.chunkGraph = &fakeChunkGraph{
		stats: &persistence.KGStats{}, // all zeros
	}
	rec := httptest.NewRecorder()
	// Admin request so the KG widget actually renders and the PercentDone
	// div-by-zero guard is genuinely exercised (not skipped behind the gate).
	srv.Memory(rec, adminMemoryRequest())
	require.Equal(t, http.StatusOK, rec.Code)
}
