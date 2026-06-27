// Code in this file was extracted from server.go to keep the
// per-page handlers grouped with their data types.

package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
)

func (s *Server) ArtifactDownload(w http.ResponseWriter, r *http.Request) {
	artifactID := r.URL.Path[len("/artifacts/"):]
	if artifactID == "" || s.artifactRepo == nil {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	artifact, err := s.artifactRepo.Get(ctx, artifactID)
	if err != nil || artifact == nil {
		http.NotFound(w, r)
		return
	}
	// Project-scope check — a scoped key for project A must not
	// download project B's artifact by guessing the ID. The path-
	// traversal guard below stops filesystem-level escape but does
	// nothing for tenant/project ownership. Legacy artifacts with
	// empty ProjectID bypass per the in-tree convention (admin /
	// auth-off visible).
	if artifact.ProjectID != "" && !api.RequestAllowsProject(r, artifact.ProjectID) {
		http.NotFound(w, r)
		return
	}

	if artifact.StoragePath == "" {
		http.Error(w, "Artifact has no storage path", http.StatusNotFound)
		return
	}

	// Defense-in-depth on the legacy direct-disk path: validate
	// that the stored path stays under the configured base. Skipped
	// when artifactReader is wired — the backend layer enforces its
	// own key safety (LocalBackend rejects traversal via safepath,
	// S3 keys have no directory semantics to escape), and the
	// recorded StoragePath may legitimately not look like an
	// absolute filesystem path under S3.
	if s.artifactReader == nil && s.artifactBasePath != "" {
		cleanBase := filepath.Clean(s.artifactBasePath)
		cleanPath := filepath.Clean(artifact.StoragePath)
		rel, relErr := filepath.Rel(cleanBase, cleanPath)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			s.logger.Error().
				Str("artifact_id", artifactID).
				Str("storage_path", artifact.StoragePath).
				Str("base_path", s.artifactBasePath).
				Msg("ArtifactDownload: path escapes artifact base — refusing to serve")
			http.Error(w, "Artifact path invalid", http.StatusForbidden)
			return
		}
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(artifact.Name)))
	if artifact.MimeType != nil && *artifact.MimeType != "" {
		w.Header().Set("Content-Type", *artifact.MimeType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}

	// Phase-4 storage abstraction: stream via the backend-aware
	// Store when wired so S3-backed deployments serve the same way.
	// Fall back to http.ServeFile on the legacy direct-disk path —
	// gives us the conditional GET / range request handling the
	// stdlib does for free on filesystem deployments.
	if s.artifactReader == nil {
		if _, statErr := os.Stat(artifact.StoragePath); os.IsNotExist(statErr) {
			http.Error(w, "Artifact file not found on disk", http.StatusNotFound)
			return
		}
		http.ServeFile(w, r, artifact.StoragePath)
		return
	}

	rc, openErr := s.artifactReader.Open(ctx, artifact.ID)
	if openErr != nil {
		// Treat missing-from-backend as 404; other errors as 500.
		if isNotFoundError(openErr) {
			http.Error(w, "Artifact not found in storage backend", http.StatusNotFound)
			return
		}
		s.logger.Error().Err(openErr).
			Str("artifact_id", artifactID).
			Msg("ArtifactDownload: backend Open failed")
		http.Error(w, "Artifact read failed", http.StatusInternalServerError)
		return
	}
	defer func() { _ = rc.Close() }()
	if _, copyErr := io.Copy(w, rc); copyErr != nil {
		// Headers already sent; just log and stop.
		s.logger.Warn().Err(copyErr).
			Str("artifact_id", artifactID).
			Msg("ArtifactDownload: copy to client aborted")
	}
}

// isNotFoundError reports whether err signals a missing object at
// the backend layer. The artifacts package wraps every driver's
// not-found error in artifacts.ErrNotFound; checking message
// substring keeps this file from depending on that package.
func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "not found") || strings.Contains(msg, "ErrNotFound")
}
