package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

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
	req := httptest.NewRequest(http.MethodGet, "/memory/", nil)
	rec := httptest.NewRecorder()
	srv.Memory(rec, req)
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
	req := httptest.NewRequest(http.MethodGet, "/memory/", nil)
	rec := httptest.NewRecorder()
	srv.Memory(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}
