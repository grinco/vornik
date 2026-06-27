package postgres

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// InstinctRepository persists the continuous-learning instinct layer
// (migrations 85/86). Writes come from the leader-elected extraction
// worker in internal/instinct; reads power the API / CLI / UI. Every
// mutating method is idempotent so the worker can safely re-scan an
// overlapping window — see the persistence.InstinctRepository contract.
type InstinctRepository struct {
	db DBTX
}

// NewInstinctRepository creates a new repository.
func NewInstinctRepository(db DBTX) *InstinctRepository {
	return &InstinctRepository{db: db}
}

// Upsert inserts a new instinct or updates the existing row for the
// same (scope, project_id, trigger_key). It returns the resolved row
// ID. support_count / contradict_count / confidence are intentionally
// left untouched — RecomputeConfidence owns them.
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
	trigger := in.Trigger
	if len(trigger) == 0 {
		trigger = []byte("{}")
	}

	var id string
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO instincts (
			id, scope, project_id, domain, trigger_key, trigger_json,
			action, source, status, distill_model,
			created_at, updated_at, last_seen_at
		) VALUES (
			$1, $2, $3, $4, $5, $6::jsonb,
			$7, $8, $9, $10,
			$11, $11, $12
		)
		ON CONFLICT (scope, project_id, trigger_key)
		DO UPDATE SET
			domain        = EXCLUDED.domain,
			trigger_json  = EXCLUDED.trigger_json,
			action        = EXCLUDED.action,
			source        = EXCLUDED.source,
			distill_model = EXCLUDED.distill_model,
			updated_at    = EXCLUDED.updated_at,
			last_seen_at  = GREATEST(instincts.last_seen_at, EXCLUDED.last_seen_at)
		RETURNING id
	`,
		in.ID, in.Scope, in.ProjectID, in.Domain, in.TriggerKey, string(trigger),
		in.Action, in.Source, in.Status, in.DistillModel,
		in.CreatedAt, in.LastSeenAt,
	).Scan(&id)
	if err != nil {
		return "", mapDBError(err)
	}
	in.ID = id
	return id, nil
}

// AddEvidence records one corroborating/contradicting outcome.
// Idempotent on (instinct_id, outcome_id). Returns true only when a
// new row was actually inserted.
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
	// action it corroborates. The worker passes ev.Action; resolve from the
	// instinct's current action when a caller leaves it unset so evidence is
	// never recorded action-less (which RecomputeConfidence would then ignore).
	if ev.Action == "" {
		if err := r.db.QueryRowContext(ctx,
			`SELECT action FROM instincts WHERE id = $1`, ev.InstinctID,
		).Scan(&ev.Action); err != nil {
			return false, mapDBError(err)
		}
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO instinct_evidence (instinct_id, outcome_id, polarity, action, created_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (instinct_id, outcome_id) DO NOTHING
	`, ev.InstinctID, ev.OutcomeID, ev.Polarity, ev.Action, ev.CreatedAt)
	if err != nil {
		return false, mapDBError(err)
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
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, v.ID, v.InstinctID, v.Action, v.Confidence, v.SupportCount, v.ContradictCount, v.Reason, v.RecordedAt)
	return mapDBError(err)
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
		WHERE instinct_id = $1
		ORDER BY recorded_at DESC, id DESC
		LIMIT $2
	`, instinctID, limit)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.InstinctActionVersion
	for rows.Next() {
		var v persistence.InstinctActionVersion
		if err := rows.Scan(&v.ID, &v.InstinctID, &v.Action, &v.Confidence,
			&v.SupportCount, &v.ContradictCount, &v.Reason, &v.RecordedAt); err != nil {
			return nil, mapDBError(err)
		}
		out = append(out, &v)
	}
	return out, mapDBError(rows.Err())
}

// RecomputeConfidence recounts the evidence, recomputes the confidence
// via the injected scorer, and persists the counts, confidence, and
// derived status. Runs in a single transaction so a concurrent
// AddEvidence can't interleave a stale count.
func (r *InstinctRepository) RecomputeConfidence(ctx context.Context, instinctID string, score persistence.InstinctScorer) error {
	if instinctID == "" {
		return fmt.Errorf("instinct id required")
	}
	if score == nil {
		return fmt.Errorf("scorer is nil")
	}

	var lastSeen, createdAt time.Time
	var status, triggerKey, distillModel, action string
	if err := r.db.QueryRowContext(ctx, `
		SELECT last_seen_at, created_at, status, trigger_key, distill_model, action FROM instincts WHERE id = $1
	`, instinctID).Scan(&lastSeen, &createdAt, &status, &triggerKey, &distillModel, &action); err != nil {
		return mapDBError(err)
	}

	// W6 per-action evidence partitioning: count only evidence recorded under
	// the instinct's CURRENT action. When a cross-project conflict replaces
	// the global action, the displaced action's evidence (a different `action`
	// value) no longer counts — so the new action's confidence reflects only
	// its own corroboration, without deleting the prior evidence rows.
	var support, contradict int
	if err := r.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE polarity = 'support'),
			COUNT(*) FILTER (WHERE polarity = 'contradict')
		FROM instinct_evidence
		WHERE instinct_id = $1 AND action = $2
	`, instinctID, action).Scan(&support, &contradict); err != nil {
		return mapDBError(err)
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
			COUNT(*) FILTER (WHERE result = 'succeeded'),
			COUNT(*) FILTER (WHERE result IN ('failed', 'rejected')),
			COUNT(*) FILTER (WHERE result IN ('ignored', 'auto_applied'))
		FROM instinct_applications
		WHERE instinct_id = $1
	`, instinctID).Scan(&appSucceeded, &appFailed, &appIgnored); err != nil {
		return mapDBError(err)
	}

	confidence, nextStatus := score.Score(persistence.InstinctScoreInput{
		SupportCount:    support,
		ContradictCount: contradict,
		LastSeenAt:      lastSeen,
		CreatedAt:       createdAt,
		CurrentStatus:   status,
		ProjectCount:    projectCount,
		Distilled:       distillModel != "",
		AppSucceeded:    appSucceeded,
		AppFailed:       appFailed,
		AppIgnored:      appIgnored,
	})

	_, err = r.db.ExecContext(ctx, `
		UPDATE instincts
		SET support_count    = $2,
		    contradict_count = $3,
		    confidence       = $4,
		    status           = $5,
		    updated_at       = NOW()
		WHERE id = $1
	`, instinctID, support, contradict, confidence, nextStatus)
	return mapDBError(err)
}

// Get returns one instinct by ID, or persistence.ErrNotFound.
func (r *InstinctRepository) Get(ctx context.Context, id string) (*persistence.Instinct, error) {
	row := r.db.QueryRowContext(ctx, instinctSelectColumns+` FROM instincts WHERE id = $1`, id)
	return scanInstinct(row)
}

// List returns instincts matching the filter, highest confidence first.
func (r *InstinctRepository) List(ctx context.Context, filter persistence.InstinctFilter) ([]*persistence.Instinct, error) {
	query := instinctSelectColumns + ` FROM instincts WHERE 1=1`
	args := make([]any, 0, 5)
	argPos := 1
	if filter.ProjectID != nil {
		query += fmt.Sprintf(" AND project_id = $%d", argPos)
		args = append(args, *filter.ProjectID)
		argPos++
	}
	if filter.Scope != nil {
		query += fmt.Sprintf(" AND scope = $%d", argPos)
		args = append(args, *filter.Scope)
		argPos++
	}
	if filter.Domain != nil {
		query += fmt.Sprintf(" AND domain = $%d", argPos)
		args = append(args, *filter.Domain)
		argPos++
	}
	if filter.Status != nil {
		query += fmt.Sprintf(" AND status = $%d", argPos)
		args = append(args, *filter.Status)
		argPos++
	}
	if filter.TriggerKey != nil {
		query += fmt.Sprintf(" AND trigger_key = $%d", argPos)
		args = append(args, *filter.TriggerKey)
		argPos++
	}
	if filter.MinConfidence != nil {
		query += fmt.Sprintf(" AND confidence >= $%d", argPos)
		args = append(args, *filter.MinConfidence)
		argPos++
	}
	query += " ORDER BY confidence DESC, last_seen_at DESC"
	if filter.PageSize > 0 {
		query += fmt.Sprintf(" LIMIT $%d", argPos)
		args = append(args, filter.PageSize)
		argPos++
	}
	if filter.Offset > 0 {
		query += fmt.Sprintf(" OFFSET $%d", argPos)
		args = append(args, filter.Offset)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err)
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
		WHERE trigger_key = $1 AND scope = 'project' AND status IN ('active','promoted')
	`, triggerKey).Scan(&n)
	if err != nil {
		return 0, mapDBError(err)
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
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []persistence.InstinctDomainStatusCount
	for rows.Next() {
		var c persistence.InstinctDomainStatusCount
		if err := rows.Scan(&c.Domain, &c.Status, &c.Count); err != nil {
			return nil, mapDBError(err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Retire flips an instinct to status='retired'. Returns ErrNotFound
// when no row matches.
func (r *InstinctRepository) Retire(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE instincts SET status = $2, updated_at = NOW() WHERE id = $1
	`, id, persistence.InstinctStatusRetired)
	if err != nil {
		return mapDBError(err)
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
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, app.ID, app.InstinctID, app.TaskID, app.Surface, app.Result, app.AppliedAt, app.ExecutionID, app.StepID)
	return mapDBError(err)
}

// ListApplications returns application rows for one instinct, newest
// first. limit <= 0 means no cap.
func (r *InstinctRepository) ListApplications(ctx context.Context, instinctID string, limit int) ([]*persistence.InstinctApplication, error) {
	query := `
		SELECT id, instinct_id, task_id, surface, result, applied_at, execution_id, step_id
		FROM instinct_applications
		WHERE instinct_id = $1
		ORDER BY applied_at DESC
	`
	args := []any{instinctID}
	if limit > 0 {
		query += " LIMIT $2"
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.InstinctApplication
	for rows.Next() {
		var a persistence.InstinctApplication
		if err := rows.Scan(&a.ID, &a.InstinctID, &a.TaskID, &a.Surface, &a.Result, &a.AppliedAt, &a.ExecutionID, &a.StepID); err != nil {
			return nil, mapDBError(err)
		}
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
	// 'auto_applied' (v2 prompt-level directive). Both await the resolver's
	// flip to succeeded/failed from the step outcome.
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, instinct_id, task_id, surface, result, applied_at, execution_id, step_id
		FROM instinct_applications
		WHERE surface = $1 AND result IN ($2, $3) AND execution_id != ''
		ORDER BY applied_at ASC
		LIMIT $4
	`, persistence.InstinctSurfaceLeadRecovery, persistence.InstinctResultIgnored, persistence.InstinctResultAutoApplied, limit)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.InstinctApplication
	for rows.Next() {
		var a persistence.InstinctApplication
		if err := rows.Scan(&a.ID, &a.InstinctID, &a.TaskID, &a.Surface, &a.Result, &a.AppliedAt, &a.ExecutionID, &a.StepID); err != nil {
			return nil, mapDBError(err)
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// ResolveApplication flips a still-'ignored' application row to result
// in place. Returns ErrNotFound when no ignored row matches. There is
// no updated_at column on instinct_applications, so only result is set.
func (r *InstinctRepository) ResolveApplication(ctx context.Context, id string, result string) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE instinct_applications SET result = $2
		WHERE id = $1 AND result IN ($3, $4)
	`, id, result, persistence.InstinctResultIgnored, persistence.InstinctResultAutoApplied)
	if err != nil {
		return mapDBError(err)
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
		       COUNT(*) FILTER (WHERE result = 'succeeded'),
		       COUNT(*) FILTER (WHERE result IN ('failed','rejected')),
		       COUNT(*) FILTER (WHERE result = 'ignored')
		FROM instinct_applications
		WHERE instinct_id IN (`)
	args := make([]any, 0, len(instinctIDs))
	for i, id := range instinctIDs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("$" + strconv.Itoa(i+1))
		args = append(args, id)
	}
	b.WriteString(") GROUP BY instinct_id")

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var c persistence.InstinctApplicationCounts
		if err := rows.Scan(&c.InstinctID, &c.Succeeded, &c.Failed, &c.Ignored); err != nil {
			return nil, mapDBError(err)
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
	var in persistence.Instinct
	var trigger []byte
	if err := scanner.Scan(
		&in.ID, &in.Scope, &in.ProjectID, &in.Domain, &in.TriggerKey, &trigger,
		&in.Action, &in.Confidence, &in.SupportCount, &in.ContradictCount,
		&in.Source, &in.Status, &in.DistillModel, &in.CreatedAt, &in.UpdatedAt, &in.LastSeenAt,
	); err != nil {
		return nil, mapDBError(err)
	}
	if len(trigger) > 0 {
		in.Trigger = trigger
	}
	return &in, nil
}

// ensure interface compliance at compile time.
var _ persistence.InstinctRepository = (*InstinctRepository)(nil)
