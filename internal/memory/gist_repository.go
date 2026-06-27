package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrGistNotFound is returned by GetGist when the loop hasn't
// populated a row for that project yet. Consumers (HTTP, CLI, UI)
// map this to a 404 / "no gist yet" empty-state rather than a
// 500.
var ErrGistNotFound = errors.New("memory: no gist for project")

// PersistedGist is the shape stored in project_gists. Distinct
// from ProjectGist (the in-memory Consolidator output) so the
// persistence concerns — `generated_at`, `duration_ms` — stay on
// the storage side while the library remains pure.
type PersistedGist struct {
	ProjectID     string
	Terms         []TermFrequency
	ChunksScanned int
	GeneratedAt   time.Time
	DurationMs    int
	// Narrative is the LLM-tier short summary written by
	// LLMConsolidateWorker. Empty when the LLM tier hasn't run
	// for this project yet — the API/UI degrade to the
	// term-frequency display without complaint.
	Narrative string
	// NarrativeModel records the model that produced Narrative.
	// Empty when Narrative is empty.
	NarrativeModel string
	// NarrativeGeneratedAt is the wall-clock time the LLM tier
	// last wrote Narrative. Zero when Narrative is empty.
	NarrativeGeneratedAt time.Time
}

// UpsertGist writes the gist for a project, overwriting any prior
// row. Each consolidation tick replaces the snapshot in place —
// PK on project_id means there's only ever one current row per
// project (no history; the library is cheap enough to recompute).
func (r *Repository) UpsertGist(ctx context.Context, g *PersistedGist) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("memory: gist upsert: repository not configured")
	}
	if g == nil || g.ProjectID == "" {
		return fmt.Errorf("memory: gist upsert: missing project id")
	}
	termsJSON, err := json.Marshal(g.Terms)
	if err != nil {
		return fmt.Errorf("memory: gist upsert: marshal terms: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO project_gists (
		    project_id, terms_json, chunks_scanned, generated_at, duration_ms
		) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (project_id) DO UPDATE SET
		    terms_json     = EXCLUDED.terms_json,
		    chunks_scanned = EXCLUDED.chunks_scanned,
		    generated_at   = EXCLUDED.generated_at,
		    duration_ms    = EXCLUDED.duration_ms`,
		g.ProjectID, termsJSON, g.ChunksScanned, g.GeneratedAt, g.DurationMs,
	)
	if err != nil {
		return fmt.Errorf("memory: gist upsert: %w", err)
	}
	return nil
}

// GetGist returns the stored row for a project. ErrGistNotFound
// distinguishes "loop hasn't run yet" from real DB errors so the
// API layer can render an empty-state without bubbling 500s.
// Narrative / NarrativeModel / NarrativeGeneratedAt are populated
// when the LLM tier has run; otherwise they're zero values
// (empty string, zero time) — callers branch on Narrative != ""
// to decide whether to render the natural-language summary.
func (r *Repository) GetGist(ctx context.Context, projectID string) (*PersistedGist, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("memory: gist get: repository not configured")
	}
	if projectID == "" {
		return nil, fmt.Errorf("memory: gist get: missing project id")
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT project_id, terms_json, chunks_scanned, generated_at, duration_ms,
		       narrative, narrative_model, narrative_generated_at
		FROM project_gists
		WHERE project_id = $1`, projectID)
	var (
		out        PersistedGist
		termsJSON  []byte
		narrative  sql.NullString
		narModel   sql.NullString
		narGenerAt sql.NullTime
	)
	if err := row.Scan(&out.ProjectID, &termsJSON, &out.ChunksScanned, &out.GeneratedAt, &out.DurationMs,
		&narrative, &narModel, &narGenerAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrGistNotFound
		}
		return nil, fmt.Errorf("memory: gist get: %w", err)
	}
	if err := json.Unmarshal(termsJSON, &out.Terms); err != nil {
		return nil, fmt.Errorf("memory: gist get: unmarshal terms: %w", err)
	}
	if narrative.Valid {
		out.Narrative = narrative.String
	}
	if narModel.Valid {
		out.NarrativeModel = narModel.String
	}
	if narGenerAt.Valid {
		out.NarrativeGeneratedAt = narGenerAt.Time
	}
	return &out, nil
}

// UpsertNarrative writes only the LLM-tier narrative columns,
// leaving terms_json + chunks_scanned + generated_at +
// duration_ms untouched. Returns ErrGistNotFound when no
// term-frequency row exists yet for the project (the LLM tier
// always layers on top of the LLM-free pass; running the LLM
// tier against a missing project means the LLM-free worker
// hasn't fired yet, and the right answer is to wait).
func (r *Repository) UpsertNarrative(ctx context.Context, projectID, narrative, model string, generatedAt time.Time) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("memory: narrative upsert: repository not configured")
	}
	if projectID == "" {
		return fmt.Errorf("memory: narrative upsert: missing project id")
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE project_gists
		SET narrative = $2,
		    narrative_model = $3,
		    narrative_generated_at = $4
		WHERE project_id = $1`,
		projectID, narrative, model, generatedAt,
	)
	if err != nil {
		return fmt.Errorf("memory: narrative upsert: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("memory: narrative upsert: rows affected: %w", err)
	}
	if aff == 0 {
		return ErrGistNotFound
	}
	return nil
}
