package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ExecutionToolGrantRepository persists per-execution tool grants into
// execution_tool_grants (migration 160).
//
// Append-only: Record inserts, nothing updates. The current grant is the newest
// accepted row for (execution_id, step_id) and everything older is the audit trail.
type ExecutionToolGrantRepository struct {
	db DBTX
}

// NewExecutionToolGrantRepository constructs the repo over the shared DBTX. The
// tool-name columns are JSONB in this backend; the decoded shape matches its
// twin, which is what the shared contract suite asserts.
func NewExecutionToolGrantRepository(db DBTX) *ExecutionToolGrantRepository {
	return &ExecutionToolGrantRepository{db: db}
}

// Record appends one grant or escalation decision, accepted or refused. A refused
// request is recorded too — a rejected privilege request is exactly what an audit
// trail is for.
func (r *ExecutionToolGrantRepository) Record(ctx context.Context, g *persistence.ExecutionToolGrant) error {
	if g == nil {
		return fmt.Errorf("nil tool grant")
	}
	if g.ExecutionID == "" || g.StepID == "" {
		// Both scope the grant. Without them a row cannot be found again, which
		// would make it an audit entry nobody can act on.
		return fmt.Errorf("tool grant needs execution_id and step_id")
	}
	if g.ID == "" {
		g.ID = persistence.GenerateID("tgrant")
	}
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now().UTC()
	}
	req, err := json.Marshal(nonNilStrings(g.RequestedTools))
	if err != nil {
		return fmt.Errorf("marshal requested_tools: %w", err)
	}
	ref, err := json.Marshal(nonNilStrings(g.RefusedTools))
	if err != nil {
		return fmt.Errorf("marshal refused_tools: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO execution_tool_grants
		    (id, execution_id, project_id, step_id, role, requested_tools, accepted,
		     refused_tools, is_escalation, ceiling_hash, ceiling_modified_at, actor, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		g.ID, g.ExecutionID, g.ProjectID, g.StepID, g.Role, req, g.Accepted,
		ref, g.IsEscalation, g.CeilingHash, g.CeilingModifiedAt, g.Actor, g.CreatedAt)
	if err != nil {
		return mapDBError(err)
	}
	return nil
}

// Current returns the newest ACCEPTED grant for a step, or nil when there is none.
//
// Refused rows are skipped deliberately: a refused request must not narrow anything,
// or a hostile grant naming one invalid tool could starve a step of everything.
func (r *ExecutionToolGrantRepository) Current(ctx context.Context, executionID, stepID string) (*persistence.ExecutionToolGrant, error) {
	if executionID == "" || stepID == "" {
		return nil, nil
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT id, execution_id, project_id, step_id, role, requested_tools, accepted,
		       refused_tools, is_escalation, ceiling_hash, ceiling_modified_at, actor, created_at
		FROM execution_tool_grants
		WHERE execution_id = $1 AND step_id = $2 AND accepted = TRUE
		ORDER BY created_at DESC, id DESC
		LIMIT 1`, executionID, stepID)

	var (
		g        persistence.ExecutionToolGrant
		req, ref []byte
		modified sql.NullTime
	)
	err := row.Scan(&g.ID, &g.ExecutionID, &g.ProjectID, &g.StepID, &g.Role, &req, &g.Accepted,
		&ref, &g.IsEscalation, &g.CeilingHash, &modified, &g.Actor, &g.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := json.Unmarshal(req, &g.RequestedTools); err != nil {
		return nil, fmt.Errorf("decode requested_tools for %s: %w", g.ID, err)
	}
	if len(ref) > 0 {
		_ = json.Unmarshal(ref, &g.RefusedTools)
	}
	if modified.Valid {
		t := modified.Time
		g.CeilingModifiedAt = &t
	}
	return &g, nil
}

// EscalationCount counts escalation rows for a step, refused ones included — the
// limit exists to bound audited cycles, and a refused cycle costs the same write.
func (r *ExecutionToolGrantRepository) EscalationCount(ctx context.Context, executionID, stepID string) (int, error) {
	if executionID == "" || stepID == "" {
		return 0, nil
	}
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM execution_tool_grants
		WHERE execution_id = $1 AND step_id = $2 AND is_escalation = TRUE`,
		executionID, stepID).Scan(&n)
	if err != nil {
		return 0, mapDBError(err)
	}
	return n, nil
}

// nonNilStrings keeps a nil slice out of the JSON column so a row always decodes to
// an array — a NULL there would make "no tools requested" and "column never written"
// indistinguishable.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
