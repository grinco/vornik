package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"vornik.io/vornik/internal/memory"
)

// GistReader is the narrow surface the gist handler needs. Kept
// as a one-method interface so the service container can wire the
// memory.Repository without dragging the full memory surface into
// the api Server struct.
type GistReader interface {
	GetGist(ctx context.Context, projectID string) (*memory.PersistedGist, error)
}

// gistResponse is the wire shape. ProjectID + meta + ranked
// []Term mirror the in-memory ProjectGist; we add `generated_at`
// and `duration_ms` so operators can see "how stale is this
// gist?" without a separate metrics call. The narrative_* group
// surfaces the LLM-tier summary when present; absent fields
// (omitempty) signal "LLM tier hasn't run / is disabled".
type gistResponse struct {
	ProjectID            string        `json:"project_id"`
	ChunksScanned        int           `json:"chunks_scanned"`
	Terms                []gistTermRow `json:"terms"`
	GeneratedAt          time.Time     `json:"generated_at"`
	DurationMs           int           `json:"duration_ms"`
	Narrative            string        `json:"narrative,omitempty"`
	NarrativeModel       string        `json:"narrative_model,omitempty"`
	NarrativeGeneratedAt *time.Time    `json:"narrative_generated_at,omitempty"`
}

type gistTermRow struct {
	Term  string `json:"term"`
	Count int    `json:"count"`
}

// GetProjectGist serves GET /api/v1/projects/{id}/gist.
//
//   - 200 with the gist payload when the consolidate loop has
//     populated a row.
//   - 404 GIST_NOT_FOUND when the loop hasn't run yet for this
//     project — operators see "no gist yet, wait ~10 min" rather
//     than a confusing 500.
//   - 503 GIST_NOT_CONFIGURED when the reader isn't wired (memory
//     subsystem off entirely).
func (s *Server) GetProjectGist(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}
	if s.gistReader == nil {
		respondError(w, http.StatusServiceUnavailable, "GIST_NOT_CONFIGURED",
			"project gist surface not configured (memory subsystem disabled?)")
		return
	}
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}
	gist, err := s.gistReader.GetGist(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, memory.ErrGistNotFound) {
			respondError(w, http.StatusNotFound, "GIST_NOT_FOUND",
				"no gist for this project yet — the consolidate loop runs every ~10 minutes")
			return
		}
		s.logger.Warn().Err(err).Str("project", projectID).
			Msg("project gist: read failed")
		respondError(w, http.StatusInternalServerError, "DB_ERROR", "failed to read gist")
		return
	}
	out := gistResponse{
		ProjectID:      gist.ProjectID,
		ChunksScanned:  gist.ChunksScanned,
		Terms:          make([]gistTermRow, 0, len(gist.Terms)),
		GeneratedAt:    gist.GeneratedAt,
		DurationMs:     gist.DurationMs,
		Narrative:      gist.Narrative,
		NarrativeModel: gist.NarrativeModel,
	}
	if !gist.NarrativeGeneratedAt.IsZero() {
		t := gist.NarrativeGeneratedAt
		out.NarrativeGeneratedAt = &t
	}
	for _, t := range gist.Terms {
		out.Terms = append(out.Terms, gistTermRow{Term: t.Term, Count: t.Count})
	}
	respondJSON(w, http.StatusOK, out)
}
