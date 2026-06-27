package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"vornik.io/vornik/internal/memory"
)

// MemoryStats handles GET /api/v1/memory/stats. Unscoped (returns all
// projects) so operators get one shot at "what's the state of RAG
// across everything"; a --project filter is applied client-side in the
// vornikctl CLI because the underlying repo query already returns all
// rows cheaply.
func (s *Server) MemoryStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only GET is supported")
		return
	}
	if s.memoryStats == nil {
		respondError(w, http.StatusServiceUnavailable, "MEMORY_DISABLED", "memory stats is not enabled")
		return
	}
	rows, err := s.memoryStats.Stats(r.Context())
	if err != nil {
		s.logger.Warn().Err(err).Msg("memory stats failed")
		respondError(w, http.StatusInternalServerError, "STATS_ERROR", "memory stats failed")
		return
	}
	if rows == nil {
		rows = []MemoryProjectStats{}
	}
	if allowed, scoped := requestScopedProjectSet(r); scoped {
		filtered := rows[:0]
		for _, row := range rows {
			if allowed[row.ProjectID] {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"projects": rows,
		"total":    len(rows),
	})
}

// MemoryCacheStats handles GET /api/v1/memory/cache-stats. Returns
// both embedding cache (Phase D) and response cache (Phase E)
// summaries so the CLI can render them side-by-side. Each block's
// Enabled flag distinguishes "feature off" from "no rows yet" —
// the CLI uses Enabled to pick "disabled" vs "0 rows" wording.
func (s *Server) MemoryCacheStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only GET is supported")
		return
	}
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.memoryCacheStats == nil {
		respondError(w, http.StatusServiceUnavailable, "CACHE_STATS_DISABLED",
			"memory cache stats not wired on this daemon")
		return
	}
	embed, embedErr := s.memoryCacheStats.EmbeddingCacheStats(r.Context())
	if embedErr != nil {
		s.logger.Warn().Err(embedErr).Msg("embedding cache stats failed")
	}
	resp, respErr := s.memoryCacheStats.ResponseCacheStats(r.Context())
	if respErr != nil {
		s.logger.Warn().Err(respErr).Msg("response cache stats failed")
	}
	// Both errors → 500. One error → return the half that worked +
	// a warning header. Operators see the partial result rather than
	// "everything's broken".
	if embedErr != nil && respErr != nil {
		respondError(w, http.StatusInternalServerError, "CACHE_STATS_ERROR",
			"both cache stats queries failed")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"embedding_cache": embed,
		"response_cache":  resp,
	})
}

// MemoryBackfillTitles handles POST /api/v1/memory/backfill-titles.
// One call processes one batch (default 10, capped at 100 to keep the
// HTTP round-trip bounded — the CLI loops). Optional ?count=true is
// a cheap probe that returns only the Remaining field, used by
// --dry-run.
func (s *Server) MemoryBackfillTitles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only POST is supported")
		return
	}
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.memoryTitleBackfiller == nil {
		respondError(w, http.StatusServiceUnavailable, "MEMORY_TITLER_DISABLED", "memory title backfill is not enabled — set memory.titler.enabled=true and restart")
		return
	}

	if r.URL.Query().Get("count") == "true" {
		remaining, err := s.memoryTitleBackfiller.CountRemaining(r.Context())
		if err != nil {
			s.logger.Warn().Err(err).Msg("memory backfill: count failed")
			respondError(w, http.StatusInternalServerError, "BACKFILL_ERROR", "count failed")
			return
		}
		respondJSON(w, http.StatusOK, MemoryTitleBackfillResult{Remaining: remaining})
		return
	}

	batchSize := 10
	if s := r.URL.Query().Get("batch_size"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			batchSize = n
		}
	}
	if batchSize > 100 {
		batchSize = 100
	}

	res, err := s.memoryTitleBackfiller.BackfillBatch(r.Context(), batchSize)
	if err != nil {
		s.logger.Warn().Err(err).Msg("memory backfill: batch failed")
		respondError(w, http.StatusInternalServerError, "BACKFILL_ERROR", err.Error())
		return
	}
	respondJSON(w, http.StatusOK, res)
}

// MemoryReclassifyLLM handles POST /api/v1/memory/reclassify-llm.
// One call processes one batch (default 10, capped at 50 to keep
// the HTTP round-trip bounded — the CLI loops). Required query
// parameter: project. Optional ?count=true is a cheap probe that
// returns only the Remaining field, used by --dry-run.
func (s *Server) MemoryReclassifyLLM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only POST is supported")
		return
	}
	if s.memoryClassifyBackfiller == nil {
		respondError(w, http.StatusServiceUnavailable, "MEMORY_CLASSIFIER_DISABLED", "memory LLM classifier is not enabled — set memory.classifier.enabled=true and restart")
		return
	}
	projectID := r.URL.Query().Get("project")
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "project query parameter is required")
		return
	}
	if !requestAllowsProject(r, projectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "project not allowed")
		return
	}

	if r.URL.Query().Get("count") == "true" {
		remaining, err := s.memoryClassifyBackfiller.CountRemaining(r.Context(), projectID)
		if err != nil {
			s.logger.Warn().Err(err).Msg("memory reclassify-llm: count failed")
			respondError(w, http.StatusInternalServerError, "RECLASSIFY_ERROR", "count failed")
			return
		}
		respondJSON(w, http.StatusOK, MemoryClassifyBackfillResult{Remaining: remaining})
		return
	}

	batchSize := 10
	if bs := r.URL.Query().Get("batch_size"); bs != "" {
		if n, err := strconv.Atoi(bs); err == nil && n > 0 {
			batchSize = n
		}
	}
	if batchSize > 50 {
		batchSize = 50
	}

	res, err := s.memoryClassifyBackfiller.BackfillBatch(r.Context(), projectID, batchSize)
	if err != nil {
		s.logger.Warn().Err(err).Msg("memory reclassify-llm: batch failed")
		respondError(w, http.StatusInternalServerError, "RECLASSIFY_ERROR", "reclassify failed")
		return
	}
	respondJSON(w, http.StatusOK, res)
}

// MemoryRegraph handles POST /api/v1/memory/regraph?project=X[&dry_run=true].
// Re-flags every project chunk that produced zero published edges so
// the KG worker reprocesses them with current pipeline logic. Returns
// the count of chunks affected (or the count that WOULD be affected
// when dry_run=true).
//
// Use case: after a KG-pipeline fix (e.g. evidence-substring
// normalisation, 2026-05-25) the operator wants the existing isolated
// entities to actually benefit, not just future ingest. Without this
// surface the fix only helps new chunks until the operator manually
// flips flags.
//
// Project-scoped (not global) so a fix only re-runs against the
// project the operator opts in. Same admin-gate matrix as
// /reclassify-llm: requestAllowsProject is the canonical scope check.
func (s *Server) MemoryRegraph(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only POST is supported")
		return
	}
	if s.memoryGraphReflagger == nil {
		respondError(w, http.StatusServiceUnavailable, "MEMORY_REGRAPH_DISABLED",
			"KG re-flag endpoint not wired on this deployment")
		return
	}
	projectID := r.URL.Query().Get("project")
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "project query parameter is required")
		return
	}
	if !requestAllowsProject(r, projectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "project not allowed")
		return
	}
	dryRun := r.URL.Query().Get("dry_run") == "true"

	n, err := s.memoryGraphReflagger.ReflagChunksMissingEdges(r.Context(), projectID, dryRun)
	if err != nil {
		s.logger.Warn().Err(err).Str("project", projectID).Msg("memory regraph: reflag failed")
		respondError(w, http.StatusInternalServerError, "REGRAPH_ERROR", "regraph failed")
		return
	}
	respondJSON(w, http.StatusOK, MemoryGraphReflagResult{
		Project:   projectID,
		DryRun:    dryRun,
		ReFlagged: n,
	})
}

// memorySearchResponse is the JSON response for GET /api/v1/projects/{id}/memory/search.
type memorySearchResponse struct {
	Results []MemorySearchResult `json:"results"`
}

// MemorySearch handles GET /api/v1/projects/{projectId}/memory/search?q=<query>&limit=<n>.
func (s *Server) MemorySearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only GET is supported")
		return
	}

	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}
	// Defense in depth: the global ProjectAuthMiddleware already guards
	// project-scoped endpoints, but this branch is separately exposed
	// and it's cheap to re-check. Forbidden API keys return the same
	// shape as other project-scoped endpoints.
	if !requestAllowsProject(r, projectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "project not allowed")
		return
	}

	q := r.URL.Query().Get("q")
	if q == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "query parameter 'q' is required")
		return
	}

	limit := 10
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 50 {
		limit = 50
	}

	if s.memorySearcher == nil {
		respondError(w, http.StatusServiceUnavailable, "MEMORY_DISABLED", "memory search is not enabled")
		return
	}

	// B-15: stamp retrieval context so the audit row carries who
	// did the search. Without this every REST-API recall produced an
	// audit row with NULL actor_kind / actor_id, muddying the
	// dashboards that split agent vs companion vs operator-direct
	// retrievals. ActorID empty for static-keys-only is fine — the
	// audit row gets actor_kind="rest_api" + NULL actor_id, which
	// still distinguishes it from agent/companion paths.
	ctx := memory.WithRetrievalContext(r.Context(), &memory.RetrievalContext{
		ActorKind: "rest_api",
		ActorID:   APIKeyIDFromContext(r.Context()),
	})

	results, err := s.memorySearcher.Search(ctx, projectID, q, limit)
	if err != nil {
		s.logger.Warn().
			Err(err).
			Str("project_id", projectID).
			Str("query", q).
			Msg("memory search failed")
		respondError(w, http.StatusInternalServerError, "SEARCH_ERROR", "memory search failed")
		return
	}

	if results == nil {
		results = []MemorySearchResult{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(memorySearchResponse{Results: results})
}
