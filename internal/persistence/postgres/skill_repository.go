package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// SkillRepository is the PostgreSQL persistence.SkillRepository — the
// knowledge-skill store (LLD 2026-07-07-knowledge-skill-store-design,
// migration 113). repo_scope "" is persisted as NULL; tags/roles are
// JSON-encoded TEXT for backend parity with SQLite.
type SkillRepository struct {
	db DBTX
}

// NewSkillRepository constructs a SkillRepository over db.
func NewSkillRepository(db DBTX) *SkillRepository { return &SkillRepository{db: db} }

const pgSkillColumns = `id, project_id, repo_scope, name, description, body, body_sha256,
	domain, tags, roles, maturity, version, origin_client, origin_task, author,
	usage_fired, usage_worked, usage_corrected, last_fired_at, created_at, updated_at,
	is_global, embedding, embedding_model, supersedes_id, distinct_justification`

func encodeSkillList(v []string) string {
	if v == nil {
		return "[]"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func decodeSkillList(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func pgNullStr(v string) interface{} {
	if v == "" {
		return nil
	}
	return v
}

// Create inserts a new skill, returning ErrSkillNameConflict on a duplicate
// (project_id, repo_scope, name).
func (r *SkillRepository) Create(ctx context.Context, s *persistence.Skill) error {
	if _, err := r.Get(ctx, s.ProjectID, s.RepoScope, s.Name); err == nil {
		return persistence.ErrSkillNameConflict
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return err
	}
	return r.insert(ctx, s)
}

func (r *SkillRepository) insert(ctx context.Context, s *persistence.Skill) error {
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = now
	}
	if s.Maturity == "" {
		s.Maturity = persistence.SkillMaturityDraft
	}
	if s.Version == 0 {
		s.Version = 1
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO project_skills (`+pgSkillColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26)`,
		s.ID, s.ProjectID, pgNullStr(s.RepoScope), s.Name, s.Description, s.Body, s.BodySHA256,
		pgNullStr(s.Domain), encodeSkillList(s.Tags), encodeSkillList(s.Roles),
		s.Maturity, s.Version, pgNullStr(s.OriginClient), pgNullStr(s.OriginTask), pgNullStr(s.Author),
		s.UsageFired, s.UsageWorked, s.UsageCorrected, s.LastFiredAt,
		s.CreatedAt, s.UpdatedAt, s.IsGlobal,
		persistence.EncodeSkillVector(s.Embedding), s.EmbeddingModel,
		s.SupersedesID, s.DistinctJustification,
	)
	return mapDBError(err)
}

// Upsert inserts a skill or, when its natural key already exists, bumps the
// version, replaces the mutable fields, and resets maturity to draft.
func (r *SkillRepository) Upsert(ctx context.Context, s *persistence.Skill) (*persistence.Skill, error) {
	existing, err := r.Get(ctx, s.ProjectID, s.RepoScope, s.Name)
	if errors.Is(err, persistence.ErrNotFound) {
		if err := r.insert(ctx, s); err != nil {
			return nil, err
		}
		return r.Get(ctx, s.ProjectID, s.RepoScope, s.Name)
	}
	if err != nil {
		return nil, err
	}
	// Archive the body we are about to destroy. §6 binds approval to a body
	// hash, so overwriting an approved body in place removes the only copy of
	// what the operator sanctioned. Archiving first means a failure here
	// aborts the edit rather than losing the prior text.
	if err := r.archiveVersion(ctx, existing); err != nil {
		return nil, err
	}
	_, err = r.db.ExecContext(ctx, `
		UPDATE project_skills SET
			description = $1, body = $2, body_sha256 = $3, domain = $4,
			tags = $5, roles = $6, maturity = $7, version = $8,
			origin_client = $9, origin_task = $10, author = $11, updated_at = $12,
			embedding = $14, embedding_model = $15,
			supersedes_id = $16, distinct_justification = $17
		WHERE id = $13`,
		s.Description, s.Body, s.BodySHA256, pgNullStr(s.Domain),
		encodeSkillList(s.Tags), encodeSkillList(s.Roles),
		persistence.SkillMaturityDraft, existing.Version+1,
		pgNullStr(s.OriginClient), pgNullStr(s.OriginTask), pgNullStr(s.Author),
		time.Now().UTC(), existing.ID,
		persistence.EncodeSkillVector(s.Embedding), s.EmbeddingModel,
		s.SupersedesID, s.DistinctJustification,
	)
	if err != nil {
		return nil, mapDBError(err)
	}
	return r.GetByID(ctx, existing.ID)
}

// archiveVersion appends a skill's current body to project_skill_versions.
//
// Idempotent on (skill_id, version): ON CONFLICT DO NOTHING, so a retried
// Upsert cannot fail on a duplicate archive row. The archived text for a given
// version never changes, which is what makes that safe.
func (r *SkillRepository) archiveVersion(ctx context.Context, s *persistence.Skill) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO project_skill_versions
			(id, skill_id, version, name, description, body, body_sha256, maturity, archived_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (skill_id, version) DO NOTHING`,
		persistence.GenerateID("skillver"), s.ID, s.Version, s.Name, s.Description,
		s.Body, s.BodySHA256, s.Maturity, time.Now().UTC())
	return mapDBError(err)
}

// ListVersions returns archived prior bodies, newest first.
func (r *SkillRepository) ListVersions(ctx context.Context, skillID string) ([]*persistence.SkillVersion, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, skill_id, version, name, description, body, body_sha256, maturity, archived_at
		FROM project_skill_versions WHERE skill_id = $1 ORDER BY version DESC`, skillID)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.SkillVersion
	for rows.Next() {
		var v persistence.SkillVersion
		if err := rows.Scan(&v.ID, &v.SkillID, &v.Version, &v.Name, &v.Description,
			&v.Body, &v.BodySHA256, &v.Maturity, &v.ArchivedAt); err != nil {
			return nil, err
		}
		out = append(out, &v)
	}
	return out, rows.Err()
}

// GetByID fetches a skill by primary key, returning ErrNotFound if absent.
func (r *SkillRepository) GetByID(ctx context.Context, id string) (*persistence.Skill, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+pgSkillColumns+` FROM project_skills WHERE id = $1`, id)
	return scanPGSkillRow(row)
}

// Get fetches a skill by its scope-qualified natural key, returning
// ErrNotFound if absent.
func (r *SkillRepository) Get(ctx context.Context, projectID, repoScope, name string) (*persistence.Skill, error) {
	var (
		q    string
		args []interface{}
	)
	if repoScope == "" {
		q = `SELECT ` + pgSkillColumns + ` FROM project_skills
			WHERE project_id = $1 AND repo_scope IS NULL AND name = $2`
		args = []interface{}{projectID, name}
	} else {
		q = `SELECT ` + pgSkillColumns + ` FROM project_skills
			WHERE project_id = $1 AND repo_scope = $2 AND name = $3`
		args = []interface{}{projectID, repoScope, name}
	}
	return scanPGSkillRow(r.db.QueryRowContext(ctx, q, args...))
}

// List returns skills matching the filter, newest-updated first.
func (r *SkillRepository) List(ctx context.Context, projectID string, f persistence.SkillListFilter) ([]*persistence.Skill, error) {
	var b strings.Builder
	// IncludeGlobal widens to global skills, but ONLY for a non-empty
	// projectID — an empty project must never match all rows (guard).
	if f.IncludeGlobal && projectID != "" {
		b.WriteString(`SELECT ` + pgSkillColumns + ` FROM project_skills WHERE (project_id = $1 OR is_global = true)`)
	} else {
		b.WriteString(`SELECT ` + pgSkillColumns + ` FROM project_skills WHERE project_id = $1`)
	}
	args := []interface{}{projectID}
	pos := 2
	next := func(v interface{}) string {
		args = append(args, v)
		p := fmt.Sprintf("$%d", pos)
		pos++
		return p
	}

	if f.RepoScope != "" {
		if f.StrictScope {
			b.WriteString(` AND (repo_scope = ` + next(f.RepoScope) + ` OR repo_scope = '*')`)
		} else {
			b.WriteString(` AND (repo_scope = ` + next(f.RepoScope) + ` OR repo_scope = '*' OR repo_scope IS NULL)`)
		}
	}
	if len(f.Maturities) > 0 {
		parts := make([]string, 0, len(f.Maturities))
		for _, m := range f.Maturities {
			parts = append(parts, next(m))
		}
		b.WriteString(` AND maturity IN (` + strings.Join(parts, ",") + `)`)
	}
	if f.Domain != "" {
		b.WriteString(` AND domain = ` + next(f.Domain))
	}
	if f.Role != "" {
		b.WriteString(` AND (roles = '[]' OR roles LIKE ` + next(`%"`+f.Role+`"%`) + `)`)
	}
	b.WriteString(` ORDER BY updated_at DESC`)
	if f.Limit > 0 {
		b.WriteString(` LIMIT ` + next(f.Limit))
	}

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.Skill
	for rows.Next() {
		s, err := scanPGSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListForMaturityScan returns all active/trusted skills across projects.
func (r *SkillRepository) ListForMaturityScan(ctx context.Context) ([]*persistence.Skill, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+pgSkillColumns+`
		FROM project_skills WHERE maturity IN ('active','trusted')`)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.Skill
	for rows.Next() {
		s, err := scanPGSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListDrafts returns all draft skills across projects, oldest first.
func (r *SkillRepository) ListDrafts(ctx context.Context, limit int) ([]*persistence.Skill, error) {
	q := `SELECT ` + pgSkillColumns + ` FROM project_skills WHERE maturity = 'draft' ORDER BY created_at ASC`
	if limit > 0 {
		q += ` LIMIT $1`
		rows, err := r.db.QueryContext(ctx, q, limit)
		return scanPGSkillList(rows, err)
	}
	rows, err := r.db.QueryContext(ctx, q)
	return scanPGSkillList(rows, err)
}

func scanPGSkillList(rows *sql.Rows, err error) ([]*persistence.Skill, error) {
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.Skill
	for rows.Next() {
		s, serr := scanPGSkill(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListAcrossProjects returns skills from every project in the given
// maturity states (empty = any), newest-updated first. Powers the admin
// skills browser.
func (r *SkillRepository) ListAcrossProjects(ctx context.Context, maturities []string, limit int) ([]*persistence.Skill, error) {
	var b strings.Builder
	b.WriteString(`SELECT ` + pgSkillColumns + ` FROM project_skills`)
	args := []interface{}{}
	pos := 1
	next := func(v interface{}) string {
		args = append(args, v)
		p := fmt.Sprintf("$%d", pos)
		pos++
		return p
	}
	if len(maturities) > 0 {
		parts := make([]string, 0, len(maturities))
		for _, m := range maturities {
			parts = append(parts, next(m))
		}
		b.WriteString(` WHERE maturity IN (` + strings.Join(parts, ",") + `)`)
	}
	b.WriteString(` ORDER BY updated_at DESC`)
	if limit > 0 {
		b.WriteString(` LIMIT ` + next(limit))
	}
	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	return scanPGSkillList(rows, err)
}

// SetMaturity transitions a skill to the given maturity state.
func (r *SkillRepository) SetMaturity(ctx context.Context, id, maturity string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE project_skills SET maturity = $1, updated_at = $2 WHERE id = $3`,
		maturity, time.Now().UTC(), id)
	if err != nil {
		return mapDBError(err)
	}
	return pgErrIfNoRows(res)
}

// SetGlobal flips a skill's cross-project reach without touching maturity.
func (r *SkillRepository) SetGlobal(ctx context.Context, id string, global bool) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE project_skills SET is_global = $1, updated_at = $2 WHERE id = $3`,
		global, time.Now().UTC(), id)
	if err != nil {
		return mapDBError(err)
	}
	return pgErrIfNoRows(res)
}

// SetEmbedding stores the dedup-preflight vector + its model. Deliberately
// does NOT touch updated_at: the injection index and skill_audit both order by
// it, and a lazy backfill of derived data must not reshuffle either or make an
// untouched skill look freshly edited.
func (r *SkillRepository) SetEmbedding(ctx context.Context, id string, embedding []float32, model string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE project_skills SET embedding = $1, embedding_model = $2 WHERE id = $3`,
		persistence.EncodeSkillVector(embedding), model, id)
	if err != nil {
		return mapDBError(err)
	}
	return pgErrIfNoRows(res)
}

// RecordFeedback increments the usage counter for the given signal and, for
// "fired", stamps last_fired_at.
func (r *SkillRepository) RecordFeedback(ctx context.Context, id, signal string) error {
	col, err := pgSkillUsageColumn(signal)
	if err != nil {
		return err
	}
	q := `UPDATE project_skills SET ` + col + ` = ` + col + ` + 1`
	args := []interface{}{}
	pos := 1
	if signal == persistence.SkillSignalFired {
		q += fmt.Sprintf(`, last_fired_at = $%d`, pos)
		args = append(args, time.Now().UTC())
		pos++
	}
	q += fmt.Sprintf(` WHERE id = $%d`, pos)
	args = append(args, id)
	res, err := r.db.ExecContext(ctx, q, args...)
	if err != nil {
		return mapDBError(err)
	}
	return pgErrIfNoRows(res)
}

// pgSkillUsageColumn whitelists signal → column so the interpolated
// name can never be attacker-controlled.
func pgSkillUsageColumn(signal string) (string, error) {
	switch signal {
	case persistence.SkillSignalFired:
		return "usage_fired", nil
	case persistence.SkillSignalWorked:
		return "usage_worked", nil
	case persistence.SkillSignalCorrected:
		return "usage_corrected", nil
	default:
		return "", fmt.Errorf("postgres: unknown skill feedback signal %q", signal)
	}
}

type pgSkillScanner interface {
	Scan(dest ...interface{}) error
}

func scanPGSkillRow(row *sql.Row) (*persistence.Skill, error) {
	s, err := scanPGSkill(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	return s, err
}

func scanPGSkill(sc pgSkillScanner) (*persistence.Skill, error) {
	var (
		s          persistence.Skill
		repoScope  sql.NullString
		domain     sql.NullString
		tags       string
		roles      string
		originCl   sql.NullString
		originTask sql.NullString
		author     sql.NullString
		lastFired  sql.NullTime
		embedding  sql.NullString
		embModel   sql.NullString
		supersedes sql.NullString
		distinctJ  sql.NullString
	)
	if err := sc.Scan(
		&s.ID, &s.ProjectID, &repoScope, &s.Name, &s.Description, &s.Body, &s.BodySHA256,
		&domain, &tags, &roles, &s.Maturity, &s.Version, &originCl, &originTask, &author,
		&s.UsageFired, &s.UsageWorked, &s.UsageCorrected, &lastFired, &s.CreatedAt, &s.UpdatedAt,
		&s.IsGlobal, &embedding, &embModel, &supersedes, &distinctJ,
	); err != nil {
		return nil, err
	}
	s.Embedding = persistence.DecodeSkillVector(embedding.String)
	s.EmbeddingModel = embModel.String
	s.SupersedesID = supersedes.String
	s.DistinctJustification = distinctJ.String
	s.RepoScope = repoScope.String
	s.Domain = domain.String
	s.Tags = decodeSkillList(tags)
	s.Roles = decodeSkillList(roles)
	s.OriginClient = originCl.String
	s.OriginTask = originTask.String
	s.Author = author.String
	if lastFired.Valid {
		t := lastFired.Time
		s.LastFiredAt = &t
	}
	return &s, nil
}

func pgErrIfNoRows(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return persistence.ErrNotFound
	}
	return nil
}
