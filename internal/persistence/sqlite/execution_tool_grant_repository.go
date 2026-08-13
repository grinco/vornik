package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ExecutionToolGrantRepository is the SQLite twin of the Postgres repo. The JSON
// columns are TEXT here and JSONB there; the decoded shape is identical, which is
// what the shared contract suite asserts.
type ExecutionToolGrantRepository struct {
	db DBTX
}

// NewExecutionToolGrantRepository constructs the repo over the shared DBTX. The
// tool-name columns are TEXT in this backend; the decoded shape matches its
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
	modified := sqliteTimePtr(g.CeilingModifiedAt)
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO execution_tool_grants
		    (id, execution_id, project_id, step_id, role, requested_tools, accepted,
		     refused_tools, is_escalation, ceiling_hash, ceiling_modified_at, actor, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		g.ID, g.ExecutionID, g.ProjectID, g.StepID, g.Role, string(req), g.Accepted,
		string(ref), g.IsEscalation, g.CeilingHash, modified, g.Actor, sqliteTime(g.CreatedAt))
	if err != nil {
		return err
	}
	return nil
}

// Current returns the newest ACCEPTED grant for a step, or nil when there is none.
// Refused rows are skipped: a refused request must not narrow anything, or a grant
// naming one invalid tool could starve the step.
func (r *ExecutionToolGrantRepository) Current(ctx context.Context, executionID, stepID string) (*persistence.ExecutionToolGrant, error) {
	if executionID == "" || stepID == "" {
		return nil, nil
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT id, execution_id, project_id, step_id, role, requested_tools, accepted,
		       refused_tools, is_escalation, ceiling_hash, ceiling_modified_at, actor, created_at
		FROM execution_tool_grants
		WHERE execution_id = ? AND step_id = ? AND accepted = 1
		ORDER BY created_at DESC, id DESC
		LIMIT 1`, executionID, stepID)

	var (
		g        persistence.ExecutionToolGrant
		req, ref string
		modified sql.NullString
		created  string
	)
	err := row.Scan(&g.ID, &g.ExecutionID, &g.ProjectID, &g.StepID, &g.Role, &req, &g.Accepted,
		&ref, &g.IsEscalation, &g.CeilingHash, &modified, &g.Actor, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(req), &g.RequestedTools); err != nil {
		return nil, fmt.Errorf("decode requested_tools for %s: %w", g.ID, err)
	}
	if ref != "" {
		_ = json.Unmarshal([]byte(ref), &g.RefusedTools)
	}
	if modified.Valid && modified.String != "" {
		if t, perr := parseSqliteTime(modified.String); perr == nil {
			g.CeilingModifiedAt = &t
		}
	}
	if t, perr := parseSqliteTime(created); perr == nil {
		g.CreatedAt = t
	}
	return &g, nil
}

// EscalationCount counts escalation rows for a step, refused attempts included — the
// limit bounds audited cycles, and a refused cycle costs the same write.
func (r *ExecutionToolGrantRepository) EscalationCount(ctx context.Context, executionID, stepID string) (int, error) {
	if executionID == "" || stepID == "" {
		return 0, nil
	}
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM execution_tool_grants
		WHERE execution_id = ? AND step_id = ? AND is_escalation = 1`,
		executionID, stepID).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
