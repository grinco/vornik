package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// InstinctRepository is the SQLite mirror of the instinct layer
// repository (migrations 85/86). Behaviour parity with the Postgres
// side is proven by the shared repotest.RunInstinctSuite.
//
// Parity notes:
//   - trigger_json is a BLOB carrying raw JSON bytes.
//   - timestamps are TEXT (RFC3339Nano, UTC) — lexically sortable, so
//     max(last_seen_at, excluded.last_seen_at) preserves the
//     "latest corroboration wins" rule the Postgres GREATEST does.
//   - support/contradict counts use SUM(CASE …) rather than COUNT(*)
//     FILTER for driver portability.
type InstinctRepository struct {
	db DBTX
}

func NewInstinctRepository(db DBTX) *InstinctRepository {
	return &InstinctRepository{db: db}
}

// Upsert inserts or updates by (scope, project_id, trigger_key) and
// returns the resolved row ID. Leaves the derived counters /
// confidence untouched.
func (r *InstinctRepository) Upsert(ctx context.Context, in *persistence.Instinct) (string, error) {
	if in == nil {
		return "", fmt.Errorf("instinct is nil")
	}
	if in.Domain == "" || in.Action == "" || in.TriggerKey == "" {
		return "", fmt.Errorf("instinct domain, action and trigger_key are required")
	}
	if in.ID == "" {
		in.ID = persistence.GenerateID("ins")
	}
	if in.Scope == "" {
		in.Scope = persistence.InstinctScopeProject
	}
	if in.Source == "" {
		in.Source = persistence.InstinctSourceObserver
	}
	if in.Status == "" {
		in.Status = persistence.InstinctStatusCandidate
	}
	now := time.Now().UTC()
	if in.CreatedAt.IsZero() {
		in.CreatedAt = now
	}
	if in.LastSeenAt.IsZero() {
		in.LastSeenAt = now
	}

	var id string
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO instincts (
			id, scope, project_id, domain, trigger_key, trigger_json,
			action, confidence, support_count, contradict_count,
			source, status, distill_model,
			created_at, updated_at, last_seen_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, 0, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (scope, project_id, trigger_key)
		DO UPDATE SET
			domain        = excluded.domain,
			trigger_json  = excluded.trigger_json,
			action        = excluded.action,
			source        = excluded.source,
			distill_model = excluded.distill_model,
			updated_at    = excluded.updated_at,
			last_seen_at  = max(instincts.last_seen_at, excluded.last_seen_at)
		RETURNING id
	`,
		in.ID, in.Scope, in.ProjectID, in.Domain, in.TriggerKey, nullableBlob(in.Trigger),
		in.Action,
		in.Source, in.Status, in.DistillModel,
		sqliteTime(in.CreatedAt), sqliteTime(in.LastSeenAt), sqliteTime(in.LastSeenAt),
	).Scan(&id)
	if err != nil {
		return "", err
	}
	in.ID = id
	return id, nil
}

// AddEvidence records one outcome, idempotent on (instinct_id,
// outcome_id). Returns true only when a new row was inserted.
func (r *InstinctRepository) AddEvidence(ctx context.Context, ev *persistence.InstinctEvidence) (bool, error) {
	if ev == nil {
		return false, fmt.Errorf("evidence is nil")
	}
	if ev.InstinctID == "" || ev.OutcomeID == "" {
		return false, fmt.Errorf("evidence instinct_id and outcome_id are required")
	}
	if ev.Polarity == "" {
		ev.Polarity = persistence.InstinctPolaritySupport
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now().UTC()
	}
	// W6 per-action evidence partitioning: tag the evidence with the instinct
	// action it corroborates; resolve from the instinct's current action when
	// the caller leaves it unset (mirrors the Postgres repo).
	if ev.Action == "" {
		if err := r.db.QueryRowContext(ctx,
			`SELECT action FROM instincts WHERE id = ?`, ev.InstinctID,
		).Scan(&ev.Action); err != nil {
			return false, err
		}
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO instinct_evidence (instinct_id, outcome_id, polarity, action, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (instinct_id, outcome_id) DO NOTHING
	`, ev.InstinctID, ev.OutcomeID, ev.Polarity, ev.Action, sqliteTime(ev.CreatedAt))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RecordActionVersion appends one action-transition history row (W6
// versioning). Append-only; snapshots a displaced action's final state.
func (r *InstinctRepository) RecordActionVersion(ctx context.Context, v *persistence.InstinctActionVersion) error {
	if v == nil {
		return fmt.Errorf("action version is nil")
	}
	if v.InstinctID == "" || v.Action == "" || v.Reason == "" {
		return fmt.Errorf("action version instinct_id, action and reason are required")
	}
	if v.ID == "" {
		v.ID = persistence.GenerateID("iav")
	}
	if v.RecordedAt.IsZero() {
		v.RecordedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO instinct_action_history (
			id, instinct_id, action, confidence, support_count, contradict_count, reason, recorded_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, v.ID, v.InstinctID, v.Action, v.Confidence, v.SupportCount, v.ContradictCount, v.Reason, sqliteTime(v.RecordedAt))
	return err
}

// ListActionHistory returns an instinct's action-transition history,
// newest first, capped at limit (<=0 → 100).
func (r *InstinctRepository) ListActionHistory(ctx context.Context, instinctID string, limit int) ([]*persistence.InstinctActionVersion, error) {
	if instinctID == "" {
		return nil, fmt.Errorf("instinct id required")
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, instinct_id, action, confidence, support_count, contradict_count, reason, recorded_at
		FROM instinct_action_history
		WHERE instinct_id = ?
		ORDER BY recorded_at DESC, id DESC
		LIMIT ?
	`, instinctID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.InstinctActionVersion
	for rows.Next() {
		var v persistence.InstinctActionVersion
		var recordedAt sqlTime
		if err := rows.Scan(&v.ID, &v.InstinctID, &v.Action, &v.Confidence,
			&v.SupportCount, &v.ContradictCount, &v.Reason, &recordedAt); err != nil {
			return nil, err
		}
		v.RecordedAt = recordedAt.Time
		out = append(out, &v)
	}
	return out, rows.Err()
}

// RecomputeConfidence recounts evidence, recomputes confidence via the
// injected scorer, and persists counts + confidence + derived status.
func (r *InstinctRepository) RecomputeConfidence(ctx context.Context, instinctID string, score persistence.InstinctScorer) error {
	if instinctID == "" {
		return fmt.Errorf("instinct id required")
	}
	if score == nil {
		return fmt.Errorf("scorer is nil")
	}

	var lastSeen, createdAt sqlTime
	var status, triggerKey, distillModel, action string
	if err := r.db.QueryRowContext(ctx, `
		SELECT last_seen_at, created_at, status, trigger_key, distill_model, action FROM instincts WHERE id = ?
	`, instinctID).Scan(&lastSeen, &createdAt, &status, &triggerKey, &distillModel, &action); err != nil {
		return err
	}

	// W6 per-action evidence partitioning: count only evidence recorded under
	// the instinct's CURRENT action, so a replaced action doesn't inherit the
	// displaced action's evidence (parity with the Postgres repo).
	var support, contradict int
	if err := r.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN polarity = 'support'    THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN polarity = 'contradict' THEN 1 ELSE 0 END), 0)
		FROM instinct_evidence
		WHERE instinct_id = ? AND action = ?
	`, instinctID, action).Scan(&support, &contradict); err != nil {
		return err
	}

	// ProjectCount drives the active→promoted (cross-project)
	// transition: how many distinct projects already hold this
	// trigger_key at active/promoted. Counted before this row's own
	// status is recomputed, so a freshly-promoting row doesn't
	// double-count itself.
	projectCount, err := r.CountActiveProjects(ctx, triggerKey)
	if err != nil {
		return err
	}

	// Application feedback (slice 7): aggregate instinct_applications into
	// the three score buckets. failed+rejected both mean "surfacing didn't
	// help"; accepted is intermediate (the eventual succeeded/failed row is
	// what counts) and is excluded.
	var appSucceeded, appFailed, appIgnored int
	if err := r.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN result = 'succeeded'              THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN result IN ('failed', 'rejected')  THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN result = 'ignored'                THEN 1 ELSE 0 END), 0)
		FROM instinct_applications
		WHERE instinct_id = ?
	`, instinctID).Scan(&appSucceeded, &appFailed, &appIgnored); err != nil {
		return err
	}

	confidence, nextStatus := score.Score(persistence.InstinctScoreInput{
		SupportCount:    support,
		ContradictCount: contradict,
		LastSeenAt:      lastSeen.Time,
		CreatedAt:       createdAt.Time,
		CurrentStatus:   status,
		ProjectCount:    projectCount,
		Distilled:       distillModel != "",
		AppSucceeded:    appSucceeded,
		AppFailed:       appFailed,
		AppIgnored:      appIgnored,
	})

	_, err = r.db.ExecContext(ctx, `
		UPDATE instincts
		SET support_count = ?, contradict_count = ?, confidence = ?, status = ?, updated_at = ?
		WHERE id = ?
	`, support, contradict, confidence, nextStatus, sqliteTime(time.Now().UTC()), instinctID)
	return err
}

// Get returns one instinct by ID, or persistence.ErrNotFound.
func (r *InstinctRepository) Get(ctx context.Context, id string) (*persistence.Instinct, error) {
	row := r.db.QueryRowContext(ctx, instinctSelectColumns+` FROM instincts WHERE id = ?`, id)
	return scanInstinct(row)
}

// List returns instincts matching the filter, highest confidence first.
func (r *InstinctRepository) List(ctx context.Context, filter persistence.InstinctFilter) ([]*persistence.Instinct, error) {
	var b strings.Builder
	b.WriteString(instinctSelectColumns + ` FROM instincts WHERE 1=1`)
	args := make([]any, 0, 5)
	if filter.ProjectID != nil {
		b.WriteString(" AND project_id = ?")
		args = append(args, *filter.ProjectID)
	}
	if filter.Scope != nil {
		b.WriteString(" AND scope = ?")
		args = append(args, *filter.Scope)
	}
	if filter.Domain != nil {
		b.WriteString(" AND domain = ?")
		args = append(args, *filter.Domain)
	}
	if filter.Status != nil {
		b.WriteString(" AND status = ?")
		args = append(args, *filter.Status)
	}
	if filter.TriggerKey != nil {
		b.WriteString(" AND trigger_key = ?")
		args = append(args, *filter.TriggerKey)
	}
	if filter.MinConfidence != nil {
		b.WriteString(" AND confidence >= ?")
		args = append(args, *filter.MinConfidence)
	}
	b.WriteString(" ORDER BY confidence DESC, last_seen_at DESC")
	if filter.PageSize > 0 {
		b.WriteString(" LIMIT ?")
		args = append(args, filter.PageSize)
	}
	if filter.Offset > 0 {
		b.WriteString(" OFFSET ?")
		args = append(args, filter.Offset)
	}

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.Instinct
	for rows.Next() {
		in, err := scanInstinct(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// CountActiveProjects returns the number of distinct project_ids that
// hold an instinct with the given trigger_key at status active or
// promoted. Backs the cross-project promotion transition.
func (r *InstinctRepository) CountActiveProjects(ctx context.Context, triggerKey string) (int, error) {
	if triggerKey == "" {
		return 0, nil
	}
	var n int
	// scope = 'project' excludes the global mirror row (project_id '')
	// so a once-promoted trigger doesn't inflate its own project count.
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT project_id) FROM instincts
		WHERE trigger_key = ? AND scope = 'project' AND status IN ('active','promoted')
	`, triggerKey).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// CountByDomainStatus returns the live instinct population grouped by
// (domain, status). One row per non-empty bucket; absent buckets are
// zero by definition.
func (r *InstinctRepository) CountByDomainStatus(ctx context.Context) ([]persistence.InstinctDomainStatusCount, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT domain, status, COUNT(*)
		FROM instincts
		GROUP BY domain, status
		ORDER BY domain, status
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []persistence.InstinctDomainStatusCount
	for rows.Next() {
		var c persistence.InstinctDomainStatusCount
		if err := rows.Scan(&c.Domain, &c.Status, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Retire flips an instinct to status='retired'.
func (r *InstinctRepository) Retire(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE instincts SET status = ?, updated_at = ? WHERE id = ?
	`, persistence.InstinctStatusRetired, sqliteTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// RecordApplication appends one application/feedback row.
func (r *InstinctRepository) RecordApplication(ctx context.Context, app *persistence.InstinctApplication) error {
	if app == nil {
		return fmt.Errorf("application is nil")
	}
	if app.InstinctID == "" || app.Surface == "" || app.Result == "" {
		return fmt.Errorf("application instinct_id, surface and result are required")
	}
	if app.ID == "" {
		app.ID = persistence.GenerateID("insapp")
	}
	if app.AppliedAt.IsZero() {
		app.AppliedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO instinct_applications (id, instinct_id, task_id, surface, result, applied_at, execution_id, step_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, app.ID, app.InstinctID, app.TaskID, app.Surface, app.Result, sqliteTime(app.AppliedAt), app.ExecutionID, app.StepID)
	return err
}

// ListApplications returns application rows for one instinct, newest first.
func (r *InstinctRepository) ListApplications(ctx context.Context, instinctID string, limit int) ([]*persistence.InstinctApplication, error) {
	query := `
		SELECT id, instinct_id, task_id, surface, result, applied_at, execution_id, step_id
		FROM instinct_applications
		WHERE instinct_id = ?
		ORDER BY applied_at DESC
	`
	args := []any{instinctID}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.InstinctApplication
	for rows.Next() {
		var (
			a         persistence.InstinctApplication
			appliedAt sqlTime
		)
		if err := rows.Scan(&a.ID, &a.InstinctID, &a.TaskID, &a.Surface, &a.Result, &appliedAt, &a.ExecutionID, &a.StepID); err != nil {
			return nil, err
		}
		a.AppliedAt = appliedAt.Time
		out = append(out, &a)
	}
	return out, rows.Err()
}

// ListPendingRecoveryApplications returns surfaced-but-unresolved
// lead_recovery applications (oldest first) for the RecoveryResolver.
func (r *InstinctRepository) ListPendingRecoveryApplications(ctx context.Context, limit int) ([]*persistence.InstinctApplication, error) {
	if limit <= 0 {
		limit = 500
	}
	// Pending = surfaced-but-unresolved: 'ignored' (advisory) OR
	// 'auto_applied' (v2 prompt-level directive). Both await the resolver.
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, instinct_id, task_id, surface, result, applied_at, execution_id, step_id
		FROM instinct_applications
		WHERE surface = ? AND result IN (?, ?) AND execution_id != ''
		ORDER BY applied_at ASC
		LIMIT ?
	`, persistence.InstinctSurfaceLeadRecovery, persistence.InstinctResultIgnored, persistence.InstinctResultAutoApplied, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.InstinctApplication
	for rows.Next() {
		var (
			a         persistence.InstinctApplication
			appliedAt sqlTime
		)
		if err := rows.Scan(&a.ID, &a.InstinctID, &a.TaskID, &a.Surface, &a.Result, &appliedAt, &a.ExecutionID, &a.StepID); err != nil {
			return nil, err
		}
		a.AppliedAt = appliedAt.Time
		out = append(out, &a)
	}
	return out, rows.Err()
}

// ResolveApplication flips a still-'ignored' application row to result
// in place. Returns ErrNotFound when no ignored row matches. There is
// no updated_at column on instinct_applications, so only result is set.
func (r *InstinctRepository) ResolveApplication(ctx context.Context, id string, result string) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE instinct_applications SET result = ?
		WHERE id = ? AND result IN (?, ?)
	`, result, id, persistence.InstinctResultIgnored, persistence.InstinctResultAutoApplied)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// ListApplicationCounts returns per-instinct application tallies for a
// batch of IDs in a single GROUP BY. failed+rejected collapse into
// Failed; accepted is excluded. Empty/nil input runs no SQL.
func (r *InstinctRepository) ListApplicationCounts(ctx context.Context, instinctIDs []string) (map[string]*persistence.InstinctApplicationCounts, error) {
	out := map[string]*persistence.InstinctApplicationCounts{}
	if len(instinctIDs) == 0 {
		return out, nil
	}
	var b strings.Builder
	b.WriteString(`
		SELECT instinct_id,
		       COALESCE(SUM(CASE WHEN result = 'succeeded' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN result IN ('failed','rejected') THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN result IN ('ignored','auto_applied') THEN 1 ELSE 0 END), 0)
		FROM instinct_applications
		WHERE instinct_id IN (`)
	args := make([]any, 0, len(instinctIDs))
	for i, id := range instinctIDs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		args = append(args, id)
	}
	b.WriteString(") GROUP BY instinct_id")

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var c persistence.InstinctApplicationCounts
		if err := rows.Scan(&c.InstinctID, &c.Succeeded, &c.Failed, &c.Ignored); err != nil {
			return nil, err
		}
		cp := c
		out[c.InstinctID] = &cp
	}
	return out, rows.Err()
}

const instinctSelectColumns = `
	SELECT id, scope, project_id, domain, trigger_key, trigger_json,
	       action, confidence, support_count, contradict_count,
	       source, status, distill_model, created_at, updated_at, last_seen_at`

func scanInstinct(scanner interface {
	Scan(dest ...any) error
}) (*persistence.Instinct, error) {
	var (
		in        persistence.Instinct
		trigger   []byte
		createdAt sqlTime
		updatedAt sqlTime
		lastSeen  sqlTime
		distill   sql.NullString
	)
	if err := scanner.Scan(
		&in.ID, &in.Scope, &in.ProjectID, &in.Domain, &in.TriggerKey, &trigger,
		&in.Action, &in.Confidence, &in.SupportCount, &in.ContradictCount,
		&in.Source, &in.Status, &distill, &createdAt, &updatedAt, &lastSeen,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, persistence.ErrNotFound
		}
		return nil, err
	}
	if len(trigger) > 0 {
		in.Trigger = trigger
	}
	in.DistillModel = distill.String
	in.CreatedAt = createdAt.Time
	in.UpdatedAt = updatedAt.Time
	in.LastSeenAt = lastSeen.Time
	return &in, nil
}

var _ persistence.InstinctRepository = (*InstinctRepository)(nil)
